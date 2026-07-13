package observability

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

// Per-node snapshot ceilings (contract D7): a broken or malicious node must
// not balloon the merged output.
const (
	maxBodyBytes     = 2 << 20 // 2 MiB
	maxSeriesPerNode = 5000
)

// errNotExposed marks a 404 (GONKA_METRICS=off or a pre-metrics image);
// such nodes are absent from the output entirely per the schema contract.
var errNotExposed = errors.New("mlnode does not expose metrics")

// MLNodeTarget identifies one mlnode and the URL of its Prometheus /metrics.
type MLNodeTarget struct {
	ID  string
	URL string
}

// MLNodeMetricsConfig tunes the aggregating handler.
type MLNodeMetricsConfig struct {
	// CacheTTL is how long a merged snapshot is served before a rebuild.
	CacheTTL time.Duration
	// ScrapeTimeout bounds each per-mlnode fetch.
	ScrapeTimeout time.Duration
}

func (c MLNodeMetricsConfig) withDefaults() MLNodeMetricsConfig {
	if c.CacheTTL <= 0 {
		c.CacheTTL = 10 * time.Second
	}
	if c.ScrapeTimeout <= 0 {
		c.ScrapeTimeout = 3 * time.Second
	}
	return c
}

// MLNodeMetricsHandler returns an http.Handler that federates the metrics of
// every mlnode returned by list() into one exposition: each node's exporter
// is scraped concurrently, every series gets a node_id label, families are
// merged by name (naive text concatenation breaks Prometheus on duplicate
// HELP/TYPE and colliding series) and mlnode_up{node_id} reports scrape
// health. Rebuilds are cached for CacheTTL and single-flighted, so public
// scrapes cost the mlnodes at most one fan-out per TTL.
func MLNodeMetricsHandler(list func() ([]MLNodeTarget, error), cfg MLNodeMetricsConfig) http.Handler {
	cfg = cfg.withDefaults()
	a := &mlnodeAggregator{
		list:   list,
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.ScrapeTimeout},
	}
	return http.HandlerFunc(a.serve)
}

type mlnodeAggregator struct {
	list   func() ([]MLNodeTarget, error)
	cfg    MLNodeMetricsConfig
	client *http.Client

	buildMu  sync.Mutex // single-flights rebuilds so scrapes coalesce
	mu       sync.Mutex // guards the cache fields below
	cached   []byte
	cachedAt time.Time
}

func (a *mlnodeAggregator) serve(w http.ResponseWriter, r *http.Request) {
	body, err := a.render(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", string(expfmt.FmtText))
	_, _ = w.Write(body)
}

func (a *mlnodeAggregator) render(ctx context.Context) ([]byte, error) {
	if b := a.fresh(); b != nil {
		return b, nil
	}
	// Single-flight: concurrent scrapers wait, then read the fresh cache.
	a.buildMu.Lock()
	defer a.buildMu.Unlock()
	if b := a.fresh(); b != nil {
		return b, nil
	}
	body, err := a.build(ctx)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.cached, a.cachedAt = body, time.Now()
	a.mu.Unlock()
	return body, nil
}

func (a *mlnodeAggregator) fresh() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cached != nil && time.Since(a.cachedAt) < a.cfg.CacheTTL {
		return a.cached
	}
	return nil
}

func (a *mlnodeAggregator) build(ctx context.Context) ([]byte, error) {
	targets, err := a.list()
	if err != nil {
		return nil, fmt.Errorf("enumerate mlnodes: %w", err)
	}

	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		merged = map[string]*dto.MetricFamily{}
		up     = make([]*dto.Metric, 0, len(targets))
	)
	for _, t := range targets {
		t := t
		wg.Add(1)
		go func() {
			defer wg.Done()
			fams, scrapeErr := a.scrapeOne(ctx, t)
			mu.Lock()
			defer mu.Unlock()
			if errors.Is(scrapeErr, errNotExposed) {
				return // node absent entirely, no up series
			}
			up = append(up, upMetric(t.ID, scrapeErr == nil))
			if scrapeErr != nil {
				return
			}
			for name, fam := range fams {
				if existing, ok := merged[name]; ok {
					// same family from another node: HELP/TYPE written once
					existing.Metric = append(existing.Metric, fam.Metric...)
				} else {
					merged[name] = fam
				}
			}
		}()
	}
	wg.Wait()

	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic output so scrape diffs are stable

	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.FmtText)
	sort.Slice(up, func(i, j int) bool { return up[i].Label[0].GetValue() < up[j].Label[0].GetValue() })
	if err := enc.Encode(upFamily(up)); err != nil {
		return nil, err
	}
	for _, name := range names {
		if err := enc.Encode(merged[name]); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func (a *mlnodeAggregator) scrapeOne(ctx context.Context, t MLNodeTarget) (map[string]*dto.MetricFamily, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotExposed
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mlnode %s: status %d", t.ID, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("mlnode %s: read metrics: %w", t.ID, err)
	}
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("mlnode %s: metrics exceed %d bytes, snapshot rejected", t.ID, maxBodyBytes)
	}
	parser := expfmt.TextParser{}
	fams, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mlnode %s: parse metrics: %w", t.ID, err)
	}
	total := 0
	for _, fam := range fams {
		total += len(fam.Metric)
	}
	if total > maxSeriesPerNode {
		return nil, fmt.Errorf("mlnode %s: %d series exceed the %d per-node cap, snapshot rejected", t.ID, total, maxSeriesPerNode)
	}
	for _, fam := range fams {
		for _, m := range fam.Metric {
			m.Label = append(m.Label, &dto.LabelPair{Name: strp("node_id"), Value: strp(t.ID)})
		}
	}
	return fams, nil
}

func upFamily(metrics []*dto.Metric) *dto.MetricFamily {
	return &dto.MetricFamily{
		Name:   strp("mlnode_up"),
		Help:   strp("Whether the mlnode's metrics endpoint was reachable on the last scrape (1 = up)."),
		Type:   dto.MetricType_GAUGE.Enum(),
		Metric: metrics,
	}
}

func upMetric(id string, ok bool) *dto.Metric {
	v := 0.0
	if ok {
		v = 1.0
	}
	return &dto.Metric{
		Label: []*dto.LabelPair{{Name: strp("node_id"), Value: strp(id)}},
		Gauge: &dto.Gauge{Value: &v},
	}
}

func strp(s string) *string { return &s }
