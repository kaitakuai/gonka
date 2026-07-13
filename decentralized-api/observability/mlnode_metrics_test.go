package observability

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/common/expfmt"
)

// Same metric name from multiple mlnodes must merge into one valid,
// re-parseable exposition with node_id labels and correct up state.
func TestMLNodeMetricsHandler_MergesLabelsAndUp(t *testing.T) {
	body := func(val string) string {
		return "# HELP vllm_num_requests_running Running requests.\n" +
			"# TYPE vllm_num_requests_running gauge\n" +
			"vllm_num_requests_running " + val + "\n"
	}
	nodeA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body("3")))
	}))
	defer nodeA.Close()
	nodeB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body("7")))
	}))
	defer nodeB.Close()

	// unreachable node: closed server refuses connections fast
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	downURL := down.URL
	down.Close()

	list := func() ([]MLNodeTarget, error) {
		return []MLNodeTarget{
			{ID: "node-a", URL: nodeA.URL},
			{ID: "node-b", URL: nodeB.URL},
			{ID: "node-down", URL: downURL},
		}, nil
	}

	h := MLNodeMetricsHandler(list, MLNodeMetricsConfig{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/mlnodes/metrics", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	parser := expfmt.TextParser{}
	fams, err := parser.TextToMetricFamilies(strings.NewReader(rr.Body.String()))
	if err != nil {
		t.Fatalf("merged output is not valid Prometheus exposition: %v\n---\n%s", err, rr.Body.String())
	}

	fam, ok := fams["vllm_num_requests_running"]
	if !ok {
		t.Fatalf("merged output missing vllm_num_requests_running; got families: %v keys", len(fams))
	}
	if len(fam.Metric) != 2 {
		t.Fatalf("vllm_num_requests_running series = %d, want 2 (one per reachable node)", len(fam.Metric))
	}
	byNode := map[string]float64{}
	for _, m := range fam.Metric {
		var nodeID string
		for _, l := range m.Label {
			if l.GetName() == "node_id" {
				nodeID = l.GetValue()
			}
		}
		if nodeID == "" {
			t.Fatalf("series without node_id label: %v", m)
		}
		byNode[nodeID] = m.GetGauge().GetValue()
	}
	if byNode["node-a"] != 3 || byNode["node-b"] != 7 {
		t.Fatalf("values by node = %v, want node-a=3 node-b=7", byNode)
	}

	up, ok := fams["mlnode_up"]
	if !ok {
		t.Fatalf("merged output missing mlnode_up")
	}
	upByNode := map[string]float64{}
	for _, m := range up.Metric {
		for _, l := range m.Label {
			if l.GetName() == "node_id" {
				upByNode[l.GetValue()] = m.GetGauge().GetValue()
			}
		}
	}
	if upByNode["node-a"] != 1 || upByNode["node-b"] != 1 || upByNode["node-down"] != 0 {
		t.Fatalf("mlnode_up = %v, want node-a=1 node-b=1 node-down=0", upByNode)
	}
}

// A 404 node (metrics off / pre-metrics image) is absent entirely: no
// series, no mlnode_up.
func TestMLNodeMetricsHandler_NotExposedNodeIsAbsent(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# TYPE m gauge\nm 1\n"))
	}))
	defer up.Close()
	off := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer off.Close()

	list := func() ([]MLNodeTarget, error) {
		return []MLNodeTarget{
			{ID: "node-on", URL: up.URL},
			{ID: "node-off", URL: off.URL},
		}, nil
	}
	rr := httptest.NewRecorder()
	MLNodeMetricsHandler(list, MLNodeMetricsConfig{}).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rr.Body.String()
	if strings.Contains(body, "node-off") {
		t.Fatalf("404-node must be absent from the output entirely, got:\n%s", body)
	}
	if !strings.Contains(body, `mlnode_up{node_id="node-on"} 1`) {
		t.Fatalf("reachable node missing from mlnode_up:\n%s", body)
	}
}

// A node over the per-node series cap is rejected and reported down.
func TestMLNodeMetricsHandler_SeriesCapRejectsNode(t *testing.T) {
	var b strings.Builder
	b.WriteString("# TYPE flood gauge\n")
	for i := 0; i < maxSeriesPerNode+1; i++ {
		fmt.Fprintf(&b, "flood{i=\"%d\"} 1\n", i)
	}
	flood := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(b.String()))
	}))
	defer flood.Close()

	list := func() ([]MLNodeTarget, error) {
		return []MLNodeTarget{{ID: "node-flood", URL: flood.URL}}, nil
	}
	rr := httptest.NewRecorder()
	MLNodeMetricsHandler(list, MLNodeMetricsConfig{}).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rr.Body.String()
	if strings.Contains(body, "flood{") {
		t.Fatalf("over-cap node's series must be rejected, got some in output")
	}
	if !strings.Contains(body, `mlnode_up{node_id="node-flood"} 0`) {
		t.Fatalf("over-cap node must be reported down:\n%s", body)
	}
}
