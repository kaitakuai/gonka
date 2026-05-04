package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	devshardpkg "devshard"
	"devshard/bridge"
	"devshard/transport"
	"devshard/types"
	"devshard/user"
)

type RuntimeConfig struct {
	ID              string `json:"id"`
	PrivateKeyHex   string `json:"private_key,omitempty"`
	PrivateKeyEnv   string `json:"private_key_env,omitempty"`
	Model           string `json:"model,omitempty"`
	StoragePath     string `json:"storage_path,omitempty"`
	ProtocolVersion string `json:"protocol_version,omitempty"`
}

type Gateway struct {
	runtimes           map[string]*devshardRuntime
	runtimeOrder       []*devshardRuntime
	limiter            *GatewayLimiter
	participantLimiter *ParticipantRequestLimiter
	phaseGate          *ChainPhaseGate
	escrowChecker      *EscrowChecker
	metrics            *DevshardMetrics
	capacity           *CapacityState
	settings           GatewaySettings
	store              *GatewayStore
	perf               *PerfTracker
	perfStore          *PerfStore
	baseStorageDir     string
	rotatorStop        chan struct{}
	rotatorDone        chan struct{}
	finalizeMu         sync.Mutex
	settlementMu       sync.Mutex
	settlementInFlight map[string]struct{}
	mu                 sync.Mutex
	roundRobinSeed     atomic.Uint64
}

type devshardRuntime struct {
	id              string
	model           string
	handler         http.Handler
	proxy           *Proxy
	session         *user.Session
	participantKeys []string
	// participantSlotCounts maps a participant key to the number of
	// slots in this escrow's group held by that host. Used by the
	// CapacityState to compute share(h,e). Length differs from
	// participantKeys when one host occupies multiple slots in the
	// same escrow.
	participantSlotCounts map[string]int

	active         atomic.Bool
	activeRequests atomic.Int64
	reservedTokens atomic.Int64

	activeConfigured bool
}

type runtimeStatus struct {
	ID                   string `json:"id"`
	Model                string `json:"model"`
	Active               bool   `json:"active"`
	Phase                string `json:"phase,omitempty"`
	Nonce                uint64 `json:"nonce,omitempty"`
	Balance              uint64 `json:"balance,omitempty"`
	ProtocolVersion      string `json:"protocol_version,omitempty"`
	ActiveRequests       int64  `json:"active_requests"`
	ReservedTokens       int64  `json:"reserved_tokens"`
	ChainPhase           string `json:"chain_phase,omitempty"`
	ConfirmationPoCPhase string `json:"confirmation_poc_phase,omitempty"`
	RequestsBlocked      bool   `json:"requests_blocked"`
	BlockReason          string `json:"block_reason,omitempty"`
}

type gatewayCapacityStatus struct {
	TotalWeight              float64            `json:"total_weight"`
	BaselineWeight           float64            `json:"baseline_weight"`
	LostWeight               float64            `json:"lost_weight"`
	ScaleFactor              float64            `json:"scale_factor"`
	AvailablePercent         float64            `json:"available_percent"`
	LostPercent              float64            `json:"lost_percent"`
	HostCount                int                `json:"host_count"`
	AvailableHostCount       int                `json:"available_host_count"`
	UnavailableHostCount     int                `json:"unavailable_host_count"`
	CurrentWeightMatched     int                `json:"current_weight_matched_hosts"`
	CurrentWeightFallback    int                `json:"current_weight_fallback_hosts"`
	BaselineWeightMatched    int                `json:"baseline_weight_matched_hosts"`
	BaselineWeightFallback   int                `json:"baseline_weight_fallback_hosts"`
	ObservedCurrentWeightKey int                `json:"observed_current_weight_keys"`
	ObservedFullWeightKey    int                `json:"observed_full_weight_keys"`
	EscrowWeights            map[string]float64 `json:"escrow_weights"`
}

var (
	DefaultRequestMaxTokens uint64 = 10_000

	errRuntimePrivateKeyMissing = errors.New("private key missing")
)

type UnsupportedModelError struct {
	Model     string
	Supported []string
}

func (e *UnsupportedModelError) Error() string {
	if len(e.Supported) == 0 {
		return fmt.Sprintf("unsupported model %q", e.Model)
	}
	return fmt.Sprintf("unsupported model %q; supported models: %s", e.Model, strings.Join(e.Supported, ", "))
}

func newRuntimeMux(proxy *Proxy) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", proxy.handleSwaggerUI)
	mux.HandleFunc("GET /openapi.json", proxy.handleOpenAPISpec)
	mux.HandleFunc("/v1/chat/completions", proxy.handleChatCompletions)
	mux.HandleFunc("POST /v1/finalize", proxy.handleFinalize)
	mux.HandleFunc("GET /v1/finalize", proxy.handleGetFinalize)
	mux.HandleFunc("GET /v1/status", proxy.handleStatus)
	mux.HandleFunc("GET /v1/state", proxy.handleState)
	mux.HandleFunc("GET /v1/debug/pending", proxy.handleDebugPending)
	mux.HandleFunc("GET /v1/debug/state", proxy.handleDebugState)
	mux.HandleFunc("GET /v1/debug/perf", proxy.handleDebugPerf)
	mux.HandleFunc("GET /v1/debug/signatures", proxy.handleDebugSignatures)
	mux.HandleFunc("POST /v1/debug/signatures/collect", proxy.handleCollectSignatures)
	return mux
}

func buildRuntime(cfg RuntimeConfig, chainREST, defaultModel string, perf *PerfTracker) (*devshardRuntime, error) {
	keyHex := strings.TrimSpace(cfg.PrivateKeyHex)
	if keyHex == "" && cfg.PrivateKeyEnv != "" {
		keyHex = strings.TrimSpace(os.Getenv(cfg.PrivateKeyEnv))
	}
	if keyHex == "" {
		return nil, fmt.Errorf("runtime %s: %w", cfg.ID, errRuntimePrivateKeyMissing)
	}

	model := cfg.Model
	if model == "" {
		model = defaultModel
	}

	if err := os.MkdirAll(filepath.Dir(cfg.StoragePath), 0o755); err != nil {
		return nil, fmt.Errorf("runtime %s: create storage dir: %w", cfg.ID, err)
	}

	if perf == nil {
		perf = NewPerfTracker(nil)
	}

	pv, pvErr := types.ParseProtocolVersion(cfg.ProtocolVersion)
	if pvErr != nil {
		return nil, fmt.Errorf("runtime %s: %w", cfg.ID, pvErr)
	}

	br := newRESTBridgeForProtocol(chainREST, pv)
	routePrefix := devshardpkg.ResolveHostRoutePrefix(pv, os.Getenv("DEVSHARD_ROUTE_PREFIX"))
	session, sm, err := user.NewHTTPSession(user.HTTPSessionConfig{
		PrivateKeyHex:    keyHex,
		EscrowID:         cfg.ID,
		Bridge:           br,
		StoragePath:      cfg.StoragePath,
		RoutePrefix:      routePrefix,
		RequestAdmission: sharedParticipantRequestLimiter,
		ProtocolVersion:  pv,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime %s: create session: %w", cfg.ID, err)
	}
	if err := perf.BackfillLegacyEscrowSamples(cfg.ID, cfg.StoragePath, session.HostParticipantKeyList()); err != nil {
		log.Printf("runtime %s: backfill legacy perf samples: %v", cfg.ID, err)
	}

	redundancy := NewRedundancyWithThrottle(
		session,
		perf,
		len(session.Clients()),
		model,
		sharedParticipantRequestLimiter.IsBlocked,
	)
	redundancy.participantLimiter = sharedParticipantRequestLimiter
	proxy := &Proxy{
		session:    session,
		sm:         sm,
		escrowID:   cfg.ID,
		model:      model,
		redundancy: redundancy,
		perf:       perf,
	}

	rt := &devshardRuntime{
		id:                    cfg.ID,
		model:                 model,
		handler:               newRuntimeMux(proxy),
		proxy:                 proxy,
		session:               session,
		participantKeys:       session.ParticipantKeys(),
		participantSlotCounts: hostSlotCounts(session.HostParticipantKeyList()),
	}
	rt.active.Store(true)
	rt.activeConfigured = true
	return rt, nil
}

func newRESTBridgeForProtocol(chainREST string, pv types.ProtocolVersion) *bridge.RESTBridge {
	if pv == types.ProtocolV0211 {
		return bridge.NewRESTBridge(chainREST, bridge.WithEscrowEndpoint("subnet_escrow"))
	}
	return bridge.NewRESTBridge(chainREST)
}

// hostSlotCounts builds a slot-count map from a per-slot participant
// key list. Empty keys (uncommon, but possible if a slot lacks a
// validator address) are skipped.
func hostSlotCounts(perSlotKeys []string) map[string]int {
	counts := make(map[string]int, len(perSlotKeys))
	for _, key := range perSlotKeys {
		if key == "" {
			continue
		}
		counts[key]++
	}
	return counts
}

func (rt *devshardRuntime) close() error {
	if rt.session != nil {
		rt.session.Close()
	}
	return nil
}

func (rt *devshardRuntime) acceptsNewInferences() (bool, string) {
	if rt == nil || !rt.active.Load() {
		return false, "inactive"
	}
	if rt.proxy == nil || rt.proxy.sm == nil {
		return true, ""
	}
	phase := rt.proxy.sm.Phase()
	if phase == types.PhaseActive {
		return true, ""
	}
	return false, fmt.Sprintf("phase=%s", sessionPhaseLabel(phase))
}

func sessionPhaseLabel(phase types.SessionPhase) string {
	switch phase {
	case types.PhaseActive:
		return "active"
	case types.PhaseFinalizing:
		return "finalizing"
	case types.PhaseSettlement:
		return "settlement"
	default:
		return fmt.Sprintf("unknown(%d)", phase)
	}
}

func (rt *devshardRuntime) snapshot() runtimeStatus {
	status := runtimeStatus{
		ID:             rt.id,
		Model:          rt.model,
		Active:         rt.active.Load(),
		ActiveRequests: rt.activeRequests.Load(),
		ReservedTokens: rt.reservedTokens.Load(),
	}
	if rt.proxy != nil && rt.proxy.sm != nil && rt.proxy.session != nil {
		phase := rt.proxy.sm.Phase()
		status.Phase = sessionPhaseLabel(phase)
		st := rt.proxy.sm.SnapshotState()
		status.Nonce = rt.proxy.session.Nonce()
		status.Balance = st.Balance
		status.ProtocolVersion = string(rt.proxy.sm.ProtocolVersion())
	}
	if rt.proxy != nil && rt.proxy.phaseGate != nil {
		snapshot := rt.proxy.phaseGate.Snapshot()
		status.ChainPhase = snapshot.EpochPhase
		status.ConfirmationPoCPhase = snapshot.ConfirmationPoCPhase
		status.RequestsBlocked = snapshot.RequestsBlocked
		status.BlockReason = snapshot.BlockReason
	}
	return status
}

// TODO: the (reservedTokens*1000 + activeRequests) formula is missleading,
// let's just leave activeRequests here, and leave a todo comment, that
// we might need to change it, so that if limits for tokens or cuncurrent
// requests are set, we need to measure if the escrow is further from
// the limists

// load returns the capacity-aware load score for this runtime. Lower
// is better; the picker selects the runtime with the smallest load.
//
// Score is simply activeRequests / W(e):
//   - activeRequests is the live count of in-flight inferences this
//     runtime owns (incremented on dispatch, decremented on
//     completion). It's the most direct, low-latency signal of "is
//     this runtime busy right now".
//   - W(e) is the runtime's effective capacity: the sum of available
//     host weights, accounting for chain-side weight, share within the
//     escrow, PoC preservation, and reactive throttle.
//
// Reserved tokens (the historical "I expect this many tokens to flow
// through me soon" hint) used to dominate the score; we no longer mix
// them in because (a) they're a noisy estimate, (b) the participant
// limiter already kills hosts that get hot, and (c) keeping the score
// to one quantity makes load-balance debugging tractable.
//
// A weight <= 0 means the escrow currently has no usable hosts (every
// host is throttled or PoC-excluded). Returning +Inf pushes it to the
// back of the queue without removing it from the candidate set, which
// preserves the existing fall-back semantics if every escrow degrades
// simultaneously.
func (rt *devshardRuntime) load(weight float64) float64 {
	if weight <= 0 {
		return math.Inf(+1)
	}
	return float64(rt.activeRequests.Load()) / weight
}

func NewGateway(runtimes []*devshardRuntime, limiter *GatewayLimiter, defaultModel string) *Gateway {
	byID := make(map[string]*devshardRuntime, len(runtimes))
	for _, rt := range runtimes {
		if !rt.activeConfigured {
			rt.active.Store(true)
			rt.activeConfigured = true
		}
		byID[rt.id] = rt
	}
	g := &Gateway{
		runtimes:           byID,
		runtimeOrder:       runtimes,
		limiter:            limiter,
		participantLimiter: sharedParticipantRequestLimiter,
		metrics:            NewDevshardMetrics(),
		capacity:           NewCapacityState(),
		settings: GatewaySettings{
			DefaultModel: defaultModel,
		},
		settlementInFlight: make(map[string]struct{}),
	}
	g.participantLimiter.SetMetrics(g.metrics)
	g.metrics.AttachGateway(g)
	g.attachCapacityLiveAvailability()
	for _, rt := range runtimes {
		g.attachRuntimeSharedState(rt)
	}
	return g
}

func NewManagedGateway(runtimes []*devshardRuntime, limiter *GatewayLimiter, settings GatewaySettings, baseStorageDir string, store *GatewayStore, perfArgs ...*PerfTracker) *Gateway {
	settings = settings.WithTuningDefaults()
	applyGatewayTuningSettings(settings)
	g := NewGateway(runtimes, limiter, settings.DefaultModel)
	g.settings = settings
	g.baseStorageDir = baseStorageDir
	g.store = store
	if len(perfArgs) > 0 && perfArgs[0] != nil {
		g.perf = perfArgs[0]
	}
	g.phaseGate = NewChainPhaseGate(settings.PublicAPI, 0)
	if g.phaseGate != nil {
		g.phaseGate.SetPreservedSnapshotBaseURL(settings.ChainREST)
	}
	if g.phaseGate != nil {
		for _, rt := range g.runtimeOrder {
			g.attachRuntimeSharedState(rt)
		}
		g.attachCapacityStateToPhaseGate()
		g.phaseGate.Start()
	}
	g.escrowChecker = NewEscrowChecker(func() string {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.settings.ChainREST
	})
	for _, rt := range g.runtimeOrder {
		g.attachEscrowChecker(rt)
	}
	g.startEscrowRotatorIfEnabled()
	go g.balanceCheckLoop()
	return g
}

func (g *Gateway) attachRuntimeSharedState(rt *devshardRuntime) {
	if g == nil || rt == nil {
		return
	}
	if rt.proxy != nil {
		rt.proxy.phaseGate = g.phaseGate
	}
	g.attachMetrics(rt)
	g.attachEscrowChecker(rt)
	if g.capacity != nil {
		g.capacity.SetEscrowMembership(rt.id, rt.participantSlotCounts)
	}
}

const (
	balanceCheckInterval                = 30 * time.Second
	balanceMinimumThreshold      uint64 = 1_000_000
	nonceDeactivationLimit       uint64 = 19_800
	autoSettlementRetryInterval         = 10 * time.Second
	autoSettlementAttemptTimeout        = 5 * time.Minute
	autoSettlementMaxAttempts           = 30
)

// checkBalances scans all active runtimes and deactivates any whose
// escrow is close to exhausting its usable balance or nonce budget.
func (g *Gateway) checkBalances() {
	g.mu.Lock()
	runtimes := make([]*devshardRuntime, len(g.runtimeOrder))
	copy(runtimes, g.runtimeOrder)
	g.mu.Unlock()

	for _, rt := range runtimes {
		if rt == nil || !rt.active.Load() || rt.proxy == nil || rt.proxy.sm == nil {
			continue
		}
		balance := rt.proxy.sm.Balance()
		if balance < balanceMinimumThreshold {
			log.Printf("escrow_balance_low escrow=%s balance=%d threshold=%d — deactivating",
				rt.id, balance, balanceMinimumThreshold)
			g.deactivateAndSettleDevshardByID(rt.id, "low_balance")
			continue
		}
		nonce := rt.proxy.sm.LatestNonce()
		if nonce >= nonceDeactivationLimit {
			log.Printf("escrow_nonce_high escrow=%s nonce=%d limit=%d — deactivating",
				rt.id, nonce, nonceDeactivationLimit)
			g.deactivateAndSettleDevshardByID(rt.id, "high_nonce")
		}
	}
}

// balanceCheckLoop periodically checks each active runtime's escrow limits.
func (g *Gateway) balanceCheckLoop() {
	g.checkBalances()
	ticker := time.NewTicker(balanceCheckInterval)
	defer ticker.Stop()
	for range ticker.C {
		g.checkBalances()
	}
}

// attachCapacityStateToPhaseGate wires the capacity state into the
// chain phase poll loop. Two channels are wired:
//
//   - Live availability source: the picker pulls per-host availability
//     from the participant limiter on every EscrowWeight call so a 503
//     (or recovery) shrinks/restores W(e) on the very next request,
//     without waiting for the next phase poll. Availability is binary
//     with hysteresis to full bucket recovery (see
//     ParticipantRequestLimiter.IsAvailable).
//   - Phase-gate snapshot push: chain-reported weights and PoC
//     preserved set on every refresh, plus a scale-hook callback that
//     pushes the latest W_tot/W_ref ratio to the GatewayLimiter.
func (g *Gateway) attachCapacityStateToPhaseGate() {
	if g == nil || g.phaseGate == nil || g.capacity == nil {
		return
	}
	g.attachCapacityLiveAvailability()
	scaleHook := func(scale float64) {
		if g.limiter == nil {
			return
		}
		g.limiter.ApplyScaleFactor(scale)
	}
	g.phaseGate.SetCapacityState(g.capacity, scaleHook)
}

func (g *Gateway) attachCapacityLiveAvailability() {
	if g == nil || g.capacity == nil {
		return
	}
	if g.participantLimiter == nil {
		g.capacity.SetLiveAvailable(nil)
		return
	}
	g.capacity.SetLiveAvailable(g.participantLimiter.IsAvailable)
}

func (g *Gateway) refreshCapacityScale() {
	if g == nil || g.capacity == nil || g.limiter == nil {
		return
	}
	if !g.limiter.HasConfiguredLimits() {
		return
	}
	g.limiter.ApplyScaleFactor(g.capacity.ScaleFactorAcrossModels())
}

func (g *Gateway) capacityStatus() gatewayCapacityStatus {
	if g == nil || g.capacity == nil {
		return gatewayCapacityStatus{}
	}
	snap := g.capacity.Snapshot()
	lost := snap.BaselineWeight - snap.TotalWeight
	if lost < 0 {
		lost = 0
	}
	availablePercent := snap.ScaleFactor * 100
	lostPercent := 100 - availablePercent
	if lostPercent < 0 {
		lostPercent = 0
	}
	return gatewayCapacityStatus{
		TotalWeight:              snap.TotalWeight,
		BaselineWeight:           snap.BaselineWeight,
		LostWeight:               lost,
		ScaleFactor:              snap.ScaleFactor,
		AvailablePercent:         availablePercent,
		LostPercent:              lostPercent,
		HostCount:                snap.HostCount,
		AvailableHostCount:       snap.AvailableHostCount,
		UnavailableHostCount:     snap.UnavailableHostCount,
		CurrentWeightMatched:     snap.CurrentWeightMatched,
		CurrentWeightFallback:    snap.CurrentWeightFallback,
		BaselineWeightMatched:    snap.BaselineWeightMatched,
		BaselineWeightFallback:   snap.BaselineWeightFallback,
		ObservedCurrentWeightKey: snap.ObservedCurrentWeightKey,
		ObservedFullWeightKey:    snap.ObservedFullWeightKey,
		EscrowWeights:            snap.EscrowWeights,
	}
}

func (g *Gateway) Close() error {
	var firstErr error
	if g.phaseGate != nil {
		g.phaseGate.Stop()
	}
	g.stopEscrowRotator()
	for _, rt := range g.runtimeOrder {
		if err := rt.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if g.perfStore != nil {
		if err := g.perfStore.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", g.metrics.Handler())
	mux.HandleFunc("/v1/chat/completions", g.handlePooledChat)
	mux.HandleFunc("/v1/status", g.handlePooledStatus)
	mux.HandleFunc("/v1/admin/state", g.handleAdminState)
	mux.HandleFunc("/v1/admin/settings", g.handleAdminSettings)
	mux.HandleFunc("/v1/admin/devshards", g.handleAdminDevshards)
	mux.HandleFunc("/v1/admin/devshards/", g.handleAdminDevshardAction)
	mux.HandleFunc("/v1/admin/escrows", g.handleAdminEscrows)
	mux.HandleFunc("/v1/admin/participants/unquarantine", g.handleAdminUnquarantine)
	mux.HandleFunc("/v1/finalize", g.handleSingleOnly)
	mux.HandleFunc("/v1/state", g.handleSingleOnly)
	mux.HandleFunc("/v1/debug/pending", g.handleSingleOnly)
	mux.HandleFunc("/v1/debug/state", g.handleSingleOnly)
	mux.HandleFunc("/v1/debug/perf", g.handleSingleOnly)
	mux.HandleFunc("/v1/debug/signatures", g.handleSingleOnly)
	mux.HandleFunc("/v1/debug/signatures/collect", g.handleSingleOnly)
	mux.HandleFunc("/devshard/", g.handleDevshard)
	return mux
}

func (g *Gateway) handlePooledStatus(w http.ResponseWriter, r *http.Request) {
	g.refreshCapacityScale()
	g.mu.Lock()
	runtimes := append([]*devshardRuntime(nil), g.runtimeOrder...)
	g.mu.Unlock()
	if len(runtimes) == 1 {
		runtimes[0].handler.ServeHTTP(w, r)
		return
	}

	statuses := make([]runtimeStatus, 0, len(runtimes))
	for _, rt := range runtimes {
		statuses = append(statuses, rt.snapshot())
	}
	writeJSON(w, map[string]any{
		"mode":      "gateway",
		"devshards": statuses,
		"limiter":   g.limiter.Snapshot(),
		"capacity":  g.capacityStatus(),
		"runtimes":  len(runtimes),
	})
}

func (g *Gateway) handleSingleOnly(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	runtimes := append([]*devshardRuntime(nil), g.runtimeOrder...)
	g.mu.Unlock()
	if len(runtimes) == 1 {
		if r.URL.Path == "/v1/finalize" && r.Method == http.MethodPost {
			g.finalizeMu.Lock()
			defer g.finalizeMu.Unlock()
			log.Printf("gateway_finalize_lock_acquired escrow=%s path=%s", runtimes[0].id, r.URL.Path)
		}
		runtimes[0].handler.ServeHTTP(w, r)
		return
	}
	http.Error(w, `{"error":{"message":"use /devshard/{id} prefix for this endpoint when multiple devshards are configured"}}`, http.StatusBadRequest)
}

func (g *Gateway) handlePooledChat(w http.ResponseWriter, r *http.Request) {
	ctx, _ := ensureRequestLogContext(r.Context())
	r = r.WithContext(ctx)
	body, model, inputTokens, err := parseChatReservation(r)
	if err != nil {
		logRequestStage(ctx, "gateway_parse_failed", "error", err)
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), chatRequestErrorStatus(err, http.StatusBadRequest))
		return
	}
	logRequestStage(ctx, "gateway_request_received", "model", firstNonEmpty(model, g.settings.DefaultModel), "input_tokens", inputTokens)

	if capacityAwareLimitsEnabled() || !relaxedPoCBypassActive() {
		g.refreshCapacityScale()
		limitModel := firstNonEmpty(model, g.settings.DefaultModel)
		if err := g.limiter.AcquireForModel(limitModel, inputTokens, g.capacity.LimitShareForModel(limitModel)); err != nil {
			g.metrics.RecordLimitRejection(limiterReasonLabel(err))
			logRequestStage(ctx, "gateway_limiter_rejected", "reason", limiterReasonLabel(err), "input_tokens", inputTokens)
			http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusTooManyRequests)
			return
		}
		defer g.limiter.ReleaseForModel(limitModel, inputTokens)
		logRequestStage(ctx, "gateway_limiter_acquired", "input_tokens", inputTokens)
	} else {
		logRequestStage(ctx, "gateway_limiter_bypassed_during_poc", "input_tokens", inputTokens, "reason", currentPoCPhaseReason())
	}

	rt, err := g.reserveRuntimeForModel(model, inputTokens)
	if err != nil {
		logRequestStage(ctx, "gateway_runtime_select_failed", "error", err)
		if isParticipantRateLimitError(err) {
			g.metrics.RecordParticipantLimitRejection("pooled_route")
		}
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), gatewayStatusCodeForError(err))
		return
	}
	defer g.releaseRuntime(rt, inputTokens)
	logRequestStage(ctx, "gateway_runtime_selected", "escrow", rt.id)

	g.serveChatToRuntime(rt, "/v1/chat/completions", body, w, r)
}

func (g *Gateway) handleDevshard(w http.ResponseWriter, r *http.Request) {
	ctx, _ := ensureRequestLogContext(r.Context())
	r = r.WithContext(ctx)
	devshardID, innerPath, ok := parseDevshardPath(r.URL.Path)
	if !ok {
		logRequestStage(ctx, "gateway_devshard_path_invalid", "path", r.URL.Path)
		http.NotFound(w, r)
		return
	}
	logRequestStage(ctx, "gateway_devshard_request_received", "escrow", devshardID, "path", innerPath)

	g.mu.Lock()
	rt, ok := g.runtimes[devshardID]
	g.mu.Unlock()
	if !ok {
		logRequestStage(ctx, "gateway_devshard_not_found", "escrow", devshardID)
		http.Error(w, fmt.Sprintf(`{"error":{"message":"unknown devshard %s"}}`, devshardID), http.StatusNotFound)
		return
	}

	if innerPath == "/v1/chat/completions" {
		if ok, reason := rt.acceptsNewInferences(); !ok {
			logRequestStage(ctx, "gateway_devshard_unavailable", "escrow", devshardID, "reason", reason)
			http.Error(w, fmt.Sprintf(`{"error":{"message":"devshard %s is unavailable for new inferences: %s"}}`, devshardID, reason), http.StatusConflict)
			return
		}
		body, model, inputTokens, err := parseChatReservation(r)
		if err != nil {
			logRequestStage(ctx, "gateway_devshard_parse_failed", "escrow", devshardID, "error", err)
			http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), chatRequestErrorStatus(err, http.StatusBadRequest))
			return
		}
		if err := rt.validateRequestedModel(model); err != nil {
			logRequestStage(ctx, "gateway_devshard_model_rejected", "escrow", devshardID, "model", model, "error", err)
			http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), gatewayStatusCodeForError(err))
			return
		}
		if capacityAwareLimitsEnabled() || !relaxedPoCBypassActive() {
			g.refreshCapacityScale()
			limitModel := firstNonEmpty(model, rt.model, g.settings.DefaultModel)
			if err := g.limiter.AcquireForModel(limitModel, inputTokens, g.capacity.LimitShareForModel(limitModel)); err != nil {
				g.metrics.RecordLimitRejection(limiterReasonLabel(err))
				logRequestStage(ctx, "gateway_devshard_limiter_rejected", "escrow", devshardID, "reason", limiterReasonLabel(err), "input_tokens", inputTokens)
				http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusTooManyRequests)
				return
			}
			defer g.limiter.ReleaseForModel(limitModel, inputTokens)
			logRequestStage(ctx, "gateway_devshard_limiter_acquired", "escrow", devshardID, "input_tokens", inputTokens)
		} else {
			logRequestStage(ctx, "gateway_devshard_limiter_bypassed_during_poc", "escrow", devshardID, "input_tokens", inputTokens, "reason", currentPoCPhaseReason())
		}

		g.reserveRuntime(rt, inputTokens)
		defer g.releaseRuntime(rt, inputTokens)
		logRequestStage(ctx, "gateway_devshard_runtime_selected", "escrow", devshardID, "input_tokens", inputTokens)

		g.serveChatToRuntime(rt, innerPath, body, w, r)
		return
	}
	if innerPath == "/v1/finalize" && r.Method == http.MethodPost {
		if rt.activeRequests.Load() > 0 {
			http.Error(w, fmt.Sprintf(`{"error":{"message":"devshard %s has active requests"}}`, devshardID), http.StatusConflict)
			return
		}
		g.finalizeMu.Lock()
		defer g.finalizeMu.Unlock()
		log.Printf("gateway_finalize_lock_acquired escrow=%s path=%s", devshardID, r.URL.Path)
		req := cloneRequestWithBody(r, nil)
		req.URL.Path = innerPath
		req.URL.RawPath = innerPath
		req.RequestURI = innerPath
		w.Header().Set("X-Devshard-ID", devshardID)
		capture := &gatewayStatusResponseWriter{ResponseWriter: w}
		rt.handler.ServeHTTP(capture, req)
		if status := capture.statusCode(); status >= 200 && status < 300 {
			g.markDevshardInactiveAfterFinalize(devshardID, rt)
		}
		return
	}

	req := cloneRequestWithBody(r, nil)
	req.URL.Path = innerPath
	req.URL.RawPath = innerPath
	req.RequestURI = innerPath
	w.Header().Set("X-Devshard-ID", devshardID)
	rt.handler.ServeHTTP(w, req)
}

type gatewayStatusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *gatewayStatusResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *gatewayStatusResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *gatewayStatusResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (g *Gateway) markDevshardInactiveAfterFinalize(id string, rt *devshardRuntime) {
	rt.active.Store(false)
	if g.store == nil {
		return
	}
	if err := g.store.SetDevshardActive(id, false); err != nil {
		log.Printf("finalize: persist deactivation for devshard %s: %v", id, err)
	}
}

func (g *Gateway) serveChatToRuntime(rt *devshardRuntime, path string, body []byte, w http.ResponseWriter, r *http.Request) {
	req := cloneRequestWithBody(r, body)
	req.URL.Path = path
	req.URL.RawPath = path
	req.RequestURI = path
	w.Header().Set("X-Devshard-ID", rt.id)
	logRequestStage(req.Context(), "gateway_request_forwarded", "escrow", rt.id, "path", path)
	rt.handler.ServeHTTP(w, req)
}

func (g *Gateway) reserveRuntimeForModel(requestModel string, inputTokens int64) (*devshardRuntime, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	var candidates []*devshardRuntime
	for _, rt := range g.runtimeOrder {
		ok, reason := rt.acceptsNewInferences()
		if !ok {
			if rt != nil {
				log.Printf("gateway: skipping escrow %s for new inference: %s", rt.id, reason)
			}
			continue
		}
		candidates = append(candidates, rt)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no devshard runtimes available for new inferences")
	}
	if requestModel != "" {
		var matching []*devshardRuntime
		for _, rt := range candidates {
			if rt.model == requestModel {
				matching = append(matching, rt)
			}
		}
		if len(matching) == 0 {
			return nil, &UnsupportedModelError{Model: requestModel, Supported: supportedModels(candidates)}
		}
		candidates = matching
	}

	bestScore := g.runtimeLoad(candidates[0], requestModel)
	best := []*devshardRuntime{candidates[0]}
	for _, rt := range candidates[1:] {
		score := g.runtimeLoad(rt, requestModel)
		switch {
		case score < bestScore:
			bestScore = score
			best = []*devshardRuntime{rt}
		case score == bestScore:
			best = append(best, rt)
		}
	}

	// All candidates score +Inf only when every escrow's W(e) == 0 -
	// i.e. every host is PoC-excluded or fully throttled. Surface this
	// as a participant-rate-limit error so callers see the existing
	// 429 path instead of dispatching a request that is guaranteed to
	// fail upstream. We deliberately don't enumerate which hosts caused
	// it: a host can have W(e)==0 for many reasons (chain weight 0, PoC
	// exclusion, reactive throttle, share rounding) and surfacing only
	// the throttled subset would mislead operators about the root
	// cause. Per-escrow W(e) is logged below for diagnostics.
	if math.IsInf(bestScore, +1) {
		log.Printf(
			"gateway: all %d candidate escrow(s) at zero capacity, returning 429; per-escrow weights: %s",
			len(candidates), g.formatCandidateWeightsLocked(candidates, requestModel),
		)
		return nil, &EscrowParticipantRateLimitError{}
	}

	chosen := best[0]
	if len(best) > 1 {
		idx := int(g.roundRobinSeed.Add(1)-1) % len(best)
		chosen = best[idx]
	}
	g.reserveRuntimeLocked(chosen, inputTokens)
	if g.metrics != nil {
		g.metrics.RecordPickerChoice(chosen.id, chosen.model)
	}
	return chosen, nil
}

// formatCandidateWeightsLocked returns a compact "id=W(e)" diagnostic
// string for log output when every escrow scored +Inf. Operators use
// this to tell whether the cause was a system-wide PoC pause (every
// W(e) == 0 simultaneously), a single hot escrow (one weight low),
// or a missing capacity-model registration (HasEscrow false).
func (g *Gateway) formatCandidateWeightsLocked(candidates []*devshardRuntime, requestModel string) string {
	parts := make([]string, 0, len(candidates))
	for _, rt := range candidates {
		if g.capacity != nil && g.capacity.HasEscrow(rt.id) {
			model := firstNonEmpty(requestModel, rt.model)
			parts = append(parts, fmt.Sprintf("%s=%g", rt.id, g.capacity.EscrowWeightForModel(rt.id, model)))
		} else {
			parts = append(parts, fmt.Sprintf("%s=unregistered", rt.id))
		}
	}
	return strings.Join(parts, " ")
}

// runtimeLoad bridges the gateway and the devshardRuntime: it pulls the
// effective weight W(e) from the CapacityState and feeds it into the
// runtime's load formula. Kept on the gateway so the runtime stays
// free of state dependencies.
//
// Fallback rules:
//   - No capacity state attached, or escrow not registered with the
//     state (no slot/membership info): use neutral weight 1.0 so the
//     picker degrades to a pure activeRequests comparison.
//   - Escrow registered but W(e) == 0 (every host is PoC-excluded or
//     fully throttled): honor the 0 so the runtime drops to +Inf load
//     and stops receiving traffic until at least one host recovers.
func (g *Gateway) runtimeLoad(rt *devshardRuntime, requestModel string) float64 {
	if g == nil || g.capacity == nil || !g.capacity.HasEscrow(rt.id) {
		return rt.load(1.0)
	}
	return rt.load(g.capacity.EscrowWeightForModel(rt.id, firstNonEmpty(requestModel, rt.model)))
}

func (g *Gateway) reserveRuntime(rt *devshardRuntime, inputTokens int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reserveRuntimeLocked(rt, inputTokens)
}

func (g *Gateway) reserveRuntimeLocked(rt *devshardRuntime, inputTokens int64) {
	rt.activeRequests.Add(1)
	rt.reservedTokens.Add(inputTokens)
}

func (g *Gateway) releaseRuntime(rt *devshardRuntime, inputTokens int64) {
	rt.activeRequests.Add(-1)
	rt.reservedTokens.Add(-inputTokens)
}

func (rt *devshardRuntime) validateRequestedModel(requestModel string) error {
	if rt == nil || requestModel == "" || requestModel == rt.model {
		return nil
	}
	return &UnsupportedModelError{Model: requestModel, Supported: []string{rt.model}}
}

func supportedModels(runtimes []*devshardRuntime) []string {
	models := make([]string, 0, len(runtimes))
	for _, rt := range runtimes {
		if rt == nil || rt.model == "" || slices.Contains(models, rt.model) {
			continue
		}
		models = append(models, rt.model)
	}
	return models
}

func gatewayStatusCodeForError(err error) int {
	var unsupportedModelErr *UnsupportedModelError
	if errors.As(err, &unsupportedModelErr) {
		return http.StatusBadRequest
	}
	if isParticipantRateLimitError(err) {
		return http.StatusTooManyRequests
	}
	var admissionErr *RequestAdmissionError
	if errors.As(err, &admissionErr) {
		return http.StatusServiceUnavailable
	}
	var upstreamErr *transport.UpstreamStatusError
	if errors.As(err, &upstreamErr) && isParticipantThrottleStatus(upstreamErr.StatusCode) {
		return http.StatusTooManyRequests
	}
	return http.StatusBadGateway
}

func isParticipantRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	var participantErr *ParticipantRateLimitError
	if errors.As(err, &participantErr) {
		return true
	}
	var escrowErr *EscrowParticipantRateLimitError
	return errors.As(err, &escrowErr)
}

func parseDevshardPath(path string) (devshardID, innerPath string, ok bool) {
	trimmed := strings.TrimPrefix(path, "/devshard/")
	if trimmed == path {
		return "", "", false
	}
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 || parts[0] == "" {
		return "", "", false
	}
	return parts[0], "/" + parts[1], true
}

func cloneRequestWithBody(r *http.Request, body []byte) *http.Request {
	req := r.Clone(r.Context())
	req.URL = cloneURL(r.URL)
	if body != nil {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}
	return req
}

func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return &url.URL{}
	}
	clone := *u
	return &clone
}

func parseChatReservation(r *http.Request) ([]byte, string, int64, error) {
	updatedBody, req, err := prepareChatRequestBody(r)
	if err != nil {
		return nil, "", 0, err
	}

	inputTokens := estimatePromptTokens(updatedBody)
	return updatedBody, req.Model, inputTokens, nil
}

func estimatePromptTokens(body []byte) int64 {
	if len(body) == 0 {
		return 1
	}
	// Approximate tokenizer: 1 token ~= 4 bytes. Good enough for admission control.
	estimate := (len(body) + 3) / 4
	if estimate < 1 {
		estimate = 1
	}
	return int64(estimate)
}

func resolveRuntimeConfigs(singleEscrowID, singleKeyHex, singleModel, singleStoragePath string) ([]RuntimeConfig, error) {
	if raw := strings.TrimSpace(os.Getenv("DEVSHARDS_JSON")); raw != "" {
		var runtimes []RuntimeConfig
		if err := json.Unmarshal([]byte(raw), &runtimes); err != nil {
			return nil, fmt.Errorf("parse DEVSHARDS_JSON: %w", err)
		}
		return runtimes, nil
	}

	if singleEscrowID == "" || singleKeyHex == "" {
		return nil, fmt.Errorf("--private-key/--escrow-id or DEVSHARD_PRIVATE_KEY/DEVSHARD_ESCROW_ID required")
	}

	return []RuntimeConfig{{
		ID:            singleEscrowID,
		PrivateKeyHex: singleKeyHex,
		Model:         singleModel,
		StoragePath:   singleStoragePath,
	}}, nil
}

func defaultStoragePath(baseStorageDir, escrowID string) string {
	return filepath.Join(baseStorageDir, fmt.Sprintf("escrow-%s", escrowID), "state.db")
}

type adminDevshardRequest struct {
	ID              string `json:"id"`
	PrivateKey      string `json:"private_key,omitempty"`
	PrivateKeyEnv   string `json:"private_key_env,omitempty"`
	Model           string `json:"model,omitempty"`
	StoragePath     string `json:"storage_path,omitempty"`
	ProtocolVersion string `json:"protocol_version,omitempty"`
}

type adminCreateEscrowRequest struct {
	PrivateKey      string `json:"private_key,omitempty"`
	PrivateKeyEnv   string `json:"private_key_env,omitempty"`
	Amount          uint64 `json:"amount"`
	ModelID         string `json:"model_id,omitempty"`
	Register        *bool  `json:"register,omitempty"`
	StoragePath     string `json:"storage_path,omitempty"`
	ProtocolVersion string `json:"protocol_version,omitempty"`
	ChainID         string `json:"chain_id,omitempty"`
	FeeDenom        string `json:"fee_denom,omitempty"`
	FeeAmount       uint64 `json:"fee_amount,omitempty"`
	GasLimit        uint64 `json:"gas_limit,omitempty"`
}

type adminSettleEscrowRequest struct {
	PrivateKey    string `json:"private_key,omitempty"`
	PrivateKeyEnv string `json:"private_key_env,omitempty"`
	ChainID       string `json:"chain_id,omitempty"`
	FeeDenom      string `json:"fee_denom,omitempty"`
	FeeAmount     uint64 `json:"fee_amount,omitempty"`
	GasLimit      uint64 `json:"gas_limit,omitempty"`
}

type adminSettingsRequest struct {
	ChainREST               *string                          `json:"chain_rest,omitempty"`
	PublicAPI               *string                          `json:"public_api,omitempty"`
	DefaultModel            *string                          `json:"default_model,omitempty"`
	MaxConcurrentRequests   *int64                           `json:"max_concurrent_requests,omitempty"`
	MaxInputTokensInFlight  *int64                           `json:"max_input_tokens_in_flight,omitempty"`
	DefaultRequestMaxTokens *uint64                          `json:"default_request_max_tokens,omitempty"`
	Disabled                *adminGatewayDisabledRequest     `json:"disabled,omitempty"`
	ParticipantThrottle     *adminParticipantThrottleRequest `json:"participant_throttle,omitempty"`
	Redundancy              *adminRedundancyRequest          `json:"redundancy,omitempty"`
	Perf                    *adminPerfRequest                `json:"perf,omitempty"`
	EscrowRotation          *adminEscrowRotationRequest      `json:"escrow_rotation,omitempty"`
}

type adminGatewayDisabledRequest struct {
	Enabled *bool   `json:"enabled,omitempty"`
	Message *string `json:"message,omitempty"`
}

type adminParticipantThrottleRequest struct {
	RequestBurst                   *int   `json:"request_burst,omitempty"`
	RecoveryPerMinute              *int   `json:"recovery_per_minute,omitempty"`
	HTTPQuarantineMS               *int64 `json:"http_quarantine_ms,omitempty"`
	TransportFailureQuarantineMS   *int64 `json:"transport_failure_quarantine_ms,omitempty"`
	EmptyStreamQuarantineMS        *int64 `json:"empty_stream_quarantine_ms,omitempty"`
	StalledWinnerQuarantineMS      *int64 `json:"stalled_winner_quarantine_ms,omitempty"`
	EmptyStreamQuarantineThreshold *int   `json:"empty_stream_threshold,omitempty"`
}

type adminRedundancyRequest struct {
	ReceiptTimeoutMS             *int64   `json:"receipt_timeout_ms,omitempty"`
	FirstTokenTimeoutFloorMS     *int64   `json:"first_token_timeout_floor_ms,omitempty"`
	PerInputTokenFirstTokenLagMS *int64   `json:"per_input_token_first_token_lag_ms,omitempty"`
	InterChunkStallTimeoutMS     *int64   `json:"inter_chunk_stall_timeout_ms,omitempty"`
	NonStreamResponseFloorMS     *int64   `json:"non_stream_response_floor_ms,omitempty"`
	PerInputTokenResponseLagMS   *int64   `json:"per_input_token_response_lag_ms,omitempty"`
	SecondaryWaitAfterWinnerMS   *int64   `json:"secondary_wait_after_winner_ms,omitempty"`
	ParallelAdvantageThreshold   *float64 `json:"parallel_advantage_threshold,omitempty"`
	UnresponsiveThreshold        *float64 `json:"unresponsive_threshold,omitempty"`
}

type adminPerfRequest struct {
	SampleSize *int   `json:"sample_size,omitempty"`
	WindowMS   *int64 `json:"window_ms,omitempty"`
}

type adminEscrowRotationRequest struct {
	Enabled       *bool   `json:"enabled,omitempty"`
	PrePoCBlocks  *int64  `json:"pre_poc_blocks,omitempty"`
	TempCount     *int    `json:"temp_count,omitempty"`
	TargetCount   *int    `json:"target_count,omitempty"`
	Amount        *uint64 `json:"amount,omitempty"`
	ModelID       *string `json:"model_id,omitempty"`
	PrivateKeyEnv *string `json:"private_key_env,omitempty"`
}

func (g *Gateway) handleAdminState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	g.refreshCapacityScale()
	if g.store == nil {
		http.Error(w, `{"error":{"message":"gateway state store unavailable"}}`, http.StatusServiceUnavailable)
		return
	}
	state, ok, err := g.store.LoadState()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if !ok {
		writeJSON(w, map[string]any{
			"settings":  g.settings,
			"devshards": []GatewayDevshardState{},
			"limiter":   g.limiter.Snapshot(),
			"capacity":  g.capacityStatus(),
		})
		return
	}

	g.mu.Lock()
	runtimeByID := make(map[string]runtimeStatus, len(g.runtimeOrder))
	for _, rt := range g.runtimeOrder {
		runtimeByID[rt.id] = rt.snapshot()
	}
	g.mu.Unlock()

	type adminDevshardView struct {
		GatewayDevshardState
		Runtime *runtimeStatus `json:"runtime,omitempty"`
	}
	views := make([]adminDevshardView, 0, len(state.Devshards))
	for _, devshard := range state.Devshards {
		view := adminDevshardView{GatewayDevshardState: devshard}
		if snapshot, ok := runtimeByID[devshard.ID]; ok {
			s := snapshot
			view.Runtime = &s
		}
		views = append(views, view)
	}
	writeJSON(w, map[string]any{
		"settings":  state.Settings,
		"devshards": views,
		"limiter":   g.limiter.Snapshot(),
		"capacity":  g.capacityStatus(),
	})
}

func (g *Gateway) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	if g.store == nil {
		http.Error(w, `{"error":{"message":"gateway state store unavailable"}}`, http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		g.mu.Lock()
		settings := g.settings
		g.mu.Unlock()
		writeJSON(w, settings)
	case http.MethodPost:
		var req adminSettingsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusBadRequest)
			return
		}

		g.mu.Lock()
		settings := g.settings
		if req.ChainREST != nil {
			settings.ChainREST = strings.TrimSpace(*req.ChainREST)
		}
		if req.PublicAPI != nil {
			settings.PublicAPI = strings.TrimSpace(*req.PublicAPI)
		}
		if req.DefaultModel != nil {
			settings.DefaultModel = strings.TrimSpace(*req.DefaultModel)
		}
		if req.MaxConcurrentRequests != nil {
			settings.MaxConcurrentRequests = *req.MaxConcurrentRequests
		}
		if req.MaxInputTokensInFlight != nil {
			settings.MaxInputTokensInFlight = *req.MaxInputTokensInFlight
		}
		if req.DefaultRequestMaxTokens != nil {
			settings.DefaultRequestMaxTokens = *req.DefaultRequestMaxTokens
		}
		if req.Disabled != nil {
			applyGatewayDisabledRequest(&settings.Disabled, req.Disabled)
		}
		if req.ParticipantThrottle != nil {
			applyParticipantThrottleRequest(&settings.ParticipantThrottle, req.ParticipantThrottle)
		}
		if req.Redundancy != nil {
			applyRedundancyRequest(&settings.Redundancy, req.Redundancy)
		}
		if req.Perf != nil {
			applyPerfRequest(&settings.Perf, req.Perf)
		}
		if req.EscrowRotation != nil {
			applyEscrowRotationRequest(&settings.EscrowRotation, req.EscrowRotation)
		}
		if err := validateGatewaySettings(settings); err != nil {
			g.mu.Unlock()
			http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusBadRequest)
			return
		}
		if err := g.store.UpdateSettings(settings); err != nil {
			g.mu.Unlock()
			http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusInternalServerError)
			return
		}
		g.settings = settings
		if g.phaseGate != nil {
			g.phaseGate.Stop()
		}
		g.phaseGate = NewChainPhaseGate(settings.PublicAPI, 0)
		if g.phaseGate != nil {
			g.phaseGate.SetPreservedSnapshotBaseURL(settings.ChainREST)
		}
		for _, rt := range g.runtimeOrder {
			g.attachRuntimeSharedState(rt)
		}
		if g.phaseGate != nil {
			g.attachCapacityStateToPhaseGate()
			g.phaseGate.Start()
		}
		g.limiter.UpdateLimits(settings.MaxConcurrentRequests, settings.MaxInputTokensInFlight)
		DefaultRequestMaxTokens = settings.DefaultRequestMaxTokens
		applyGatewayTuningSettings(settings)
		if g.perf != nil {
			g.perf.ResizeRings()
		}
		if settings.EscrowRotation.Enabled {
			g.startEscrowRotatorLocked()
		} else {
			g.stopEscrowRotatorLocked()
		}
		g.mu.Unlock()

		writeJSON(w, settings)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func applyGatewayDisabledRequest(settings *GatewayDisabledSettings, req *adminGatewayDisabledRequest) {
	if req.Enabled != nil {
		settings.Enabled = *req.Enabled
	}
	if req.Message != nil {
		settings.Message = strings.TrimSpace(*req.Message)
	}
	*settings = settings.WithDefaults()
}

func applyParticipantThrottleRequest(settings *ParticipantThrottleSettings, req *adminParticipantThrottleRequest) {
	if req.RequestBurst != nil {
		settings.RequestBurst = *req.RequestBurst
	}
	if req.RecoveryPerMinute != nil {
		settings.RecoveryPerMinute = *req.RecoveryPerMinute
	}
	if req.HTTPQuarantineMS != nil {
		settings.HTTPQuarantineMS = *req.HTTPQuarantineMS
	}
	if req.TransportFailureQuarantineMS != nil {
		settings.TransportFailureQuarantineMS = *req.TransportFailureQuarantineMS
	}
	if req.EmptyStreamQuarantineMS != nil {
		settings.EmptyStreamQuarantineMS = *req.EmptyStreamQuarantineMS
	}
	if req.StalledWinnerQuarantineMS != nil {
		settings.StalledWinnerQuarantineMS = *req.StalledWinnerQuarantineMS
	}
	if req.EmptyStreamQuarantineThreshold != nil {
		settings.EmptyStreamQuarantineThreshold = *req.EmptyStreamQuarantineThreshold
	}
}

func applyRedundancyRequest(settings *RedundancySettings, req *adminRedundancyRequest) {
	if req.ReceiptTimeoutMS != nil {
		settings.ReceiptTimeoutMS = *req.ReceiptTimeoutMS
	}
	if req.FirstTokenTimeoutFloorMS != nil {
		settings.FirstTokenTimeoutFloorMS = *req.FirstTokenTimeoutFloorMS
	}
	if req.PerInputTokenFirstTokenLagMS != nil {
		settings.PerInputTokenFirstTokenLagMS = *req.PerInputTokenFirstTokenLagMS
	}
	if req.InterChunkStallTimeoutMS != nil {
		settings.InterChunkStallTimeoutMS = *req.InterChunkStallTimeoutMS
	}
	if req.NonStreamResponseFloorMS != nil {
		settings.NonStreamResponseFloorMS = *req.NonStreamResponseFloorMS
	}
	if req.PerInputTokenResponseLagMS != nil {
		settings.PerInputTokenResponseLagMS = *req.PerInputTokenResponseLagMS
	}
	if req.SecondaryWaitAfterWinnerMS != nil {
		settings.SecondaryWaitAfterWinnerMS = *req.SecondaryWaitAfterWinnerMS
	}
	if req.ParallelAdvantageThreshold != nil {
		settings.ParallelAdvantageThreshold = *req.ParallelAdvantageThreshold
	}
	if req.UnresponsiveThreshold != nil {
		settings.UnresponsiveThreshold = *req.UnresponsiveThreshold
	}
}

func applyPerfRequest(settings *PerfSettings, req *adminPerfRequest) {
	if req.SampleSize != nil {
		settings.SampleSize = *req.SampleSize
	}
	if req.WindowMS != nil {
		settings.WindowMS = *req.WindowMS
	}
}

func applyEscrowRotationRequest(settings *EscrowRotationSettings, req *adminEscrowRotationRequest) {
	if req.Enabled != nil {
		settings.Enabled = *req.Enabled
	}
	if req.PrePoCBlocks != nil {
		settings.PrePoCBlocks = *req.PrePoCBlocks
	}
	if req.TempCount != nil {
		settings.TempCount = *req.TempCount
	}
	if req.TargetCount != nil {
		settings.TargetCount = *req.TargetCount
	}
	if req.Amount != nil {
		settings.Amount = *req.Amount
	}
	if req.ModelID != nil {
		settings.ModelID = strings.TrimSpace(*req.ModelID)
	}
	if req.PrivateKeyEnv != nil {
		settings.PrivateKeyEnv = strings.TrimSpace(*req.PrivateKeyEnv)
	}
}

func validateGatewaySettings(settings GatewaySettings) error {
	p := settings.ParticipantThrottle
	switch {
	case p.RequestBurst <= 0:
		return fmt.Errorf("participant_throttle.request_burst must be > 0")
	case p.RecoveryPerMinute <= 0:
		return fmt.Errorf("participant_throttle.recovery_per_minute must be > 0")
	case p.HTTPQuarantineMS <= 0:
		return fmt.Errorf("participant_throttle.http_quarantine_ms must be > 0")
	case p.TransportFailureQuarantineMS <= 0:
		return fmt.Errorf("participant_throttle.transport_failure_quarantine_ms must be > 0")
	case p.EmptyStreamQuarantineMS <= 0:
		return fmt.Errorf("participant_throttle.empty_stream_quarantine_ms must be > 0")
	case p.StalledWinnerQuarantineMS <= 0:
		return fmt.Errorf("participant_throttle.stalled_winner_quarantine_ms must be > 0")
	case p.EmptyStreamQuarantineThreshold <= 0:
		return fmt.Errorf("participant_throttle.empty_stream_threshold must be > 0")
	}
	r := settings.Redundancy
	switch {
	case r.ReceiptTimeoutMS <= 0:
		return fmt.Errorf("redundancy.receipt_timeout_ms must be > 0")
	case r.FirstTokenTimeoutFloorMS <= 0:
		return fmt.Errorf("redundancy.first_token_timeout_floor_ms must be > 0")
	case r.PerInputTokenFirstTokenLagMS < 0:
		return fmt.Errorf("redundancy.per_input_token_first_token_lag_ms must be >= 0")
	case r.InterChunkStallTimeoutMS < 0:
		return fmt.Errorf("redundancy.inter_chunk_stall_timeout_ms must be >= 0")
	case r.NonStreamResponseFloorMS <= 0:
		return fmt.Errorf("redundancy.non_stream_response_floor_ms must be > 0")
	case r.PerInputTokenResponseLagMS < 0:
		return fmt.Errorf("redundancy.per_input_token_response_lag_ms must be >= 0")
	case r.SecondaryWaitAfterWinnerMS <= 0:
		return fmt.Errorf("redundancy.secondary_wait_after_winner_ms must be > 0")
	case r.ParallelAdvantageThreshold <= 0 || r.ParallelAdvantageThreshold >= 1:
		return fmt.Errorf("redundancy.parallel_advantage_threshold must be > 0 and < 1")
	case r.UnresponsiveThreshold <= 0 || r.UnresponsiveThreshold > 1:
		return fmt.Errorf("redundancy.unresponsive_threshold must be > 0 and <= 1")
	}
	perf := settings.Perf
	switch {
	case perf.SampleSize <= 0:
		return fmt.Errorf("perf.sample_size must be > 0")
	case perf.WindowMS <= 0:
		return fmt.Errorf("perf.window_ms must be > 0")
	}
	rotation := settings.EscrowRotation
	if rotation.Enabled {
		switch {
		case rotation.PrePoCBlocks <= 0:
			return fmt.Errorf("escrow_rotation.pre_poc_blocks must be > 0")
		case rotation.TempCount <= 0:
			return fmt.Errorf("escrow_rotation.temp_count must be > 0")
		case rotation.TargetCount <= 0:
			return fmt.Errorf("escrow_rotation.target_count must be > 0")
		case rotation.Amount == 0:
			return fmt.Errorf("escrow_rotation.amount must be > 0 when rotation is enabled")
		case strings.TrimSpace(rotation.PrivateKeyEnv) == "":
			return fmt.Errorf("escrow_rotation.private_key_env is required when rotation is enabled")
		}
	}
	return nil
}

func applyGatewayTuningSettings(settings GatewaySettings) {
	settings = settings.WithTuningDefaults()
	sharedParticipantRequestLimiter.UpdateSettings(settings.ParticipantThrottle)
	ApplyRedundancySettings(settings.Redundancy)
	ApplyPerfSettings(settings.Perf)
}

func (g *Gateway) handleAdminDevshards(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		g.handleAdminState(w, r)
	case http.MethodPost:
		g.handleAdminAddDevshard(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (g *Gateway) handleAdminEscrows(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if g.store == nil {
		http.Error(w, `{"error":{"message":"gateway state store unavailable"}}`, http.StatusServiceUnavailable)
		return
	}
	var req adminCreateEscrowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusBadRequest)
		return
	}
	if req.Amount == 0 {
		http.Error(w, `{"error":{"message":"amount is required"}}`, http.StatusBadRequest)
		return
	}
	modelID := strings.TrimSpace(req.ModelID)
	if modelID == "" {
		modelID = g.settings.DefaultModel
	}
	privateKeyEnv := strings.TrimSpace(req.PrivateKeyEnv)
	if strings.TrimSpace(req.PrivateKey) == "" && privateKeyEnv == "" && strings.TrimSpace(os.Getenv("DEVSHARD_PRIVATE_KEY")) != "" {
		privateKeyEnv = "DEVSHARD_PRIVATE_KEY"
	}
	signer, keyHex, err := signerFromRequestKey(req.PrivateKey, privateKeyEnv)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusBadRequest)
		return
	}
	txClient, err := newGatewayRESTChainTxClient(g.settings, req.ChainID, req.FeeDenom, req.FeeAmount, req.GasLimit)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusBadRequest)
		return
	}
	result, err := txClient.CreateDevshardEscrow(r.Context(), signer, req.Amount, modelID)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusBadGateway)
		return
	}

	register := true
	if req.Register != nil {
		register = *req.Register
	}
	response := map[string]any{
		"escrow_id":  result.EscrowID,
		"tx_hash":    result.TxHash,
		"creator":    result.Creator,
		"registered": register,
	}
	if !register {
		writeJSON(w, response)
		return
	}

	record := GatewayDevshardState{
		RuntimeConfig: RuntimeConfig{
			ID:              strconv.FormatUint(result.EscrowID, 10),
			Model:           modelID,
			StoragePath:     strings.TrimSpace(req.StoragePath),
			ProtocolVersion: strings.TrimSpace(req.ProtocolVersion),
		},
		Active: true,
	}
	if strings.TrimSpace(req.PrivateKey) != "" {
		record.PrivateKeyHex = keyHex
	} else {
		record.PrivateKeyEnv = privateKeyEnv
	}
	if err := g.addCreatedEscrowRuntime(record); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q,"escrow_id":%q,"tx_hash":%q}}`, err.Error(), record.ID, result.TxHash), http.StatusInternalServerError)
		return
	}
	response["id"] = record.ID
	response["model"] = record.Model
	response["storage_path"] = record.StoragePath
	writeJSON(w, response)
}

func newGatewayRESTChainTxClient(settings GatewaySettings, chainID, feeDenom string, feeAmount, gasLimit uint64) (*RESTChainTxClient, error) {
	return NewRESTChainTxClient(RESTChainTxConfig{
		BaseURL:      settings.ChainREST,
		TxQueryURL:   firstNonEmpty(os.Getenv("DEVSHARD_TX_QUERY_REST"), "http://node1.gonka.ai:8000/chain-api"),
		ChainID:      firstNonEmpty(chainID, os.Getenv("DEVSHARD_CHAIN_ID")),
		FeeDenom:     firstNonEmpty(feeDenom, os.Getenv("DEVSHARD_TX_FEE_DENOM")),
		FeeAmount:    firstNonZeroUint64(feeAmount, uint64(readInt64Env("DEVSHARD_TX_FEE_AMOUNT", int64(defaultTxFeeAmount)))),
		GasLimit:     firstNonZeroUint64(gasLimit, uint64(readInt64Env("DEVSHARD_TX_GAS_LIMIT", int64(defaultTxGasLimit)))),
		PollInterval: txSettingDurationMS(os.Getenv("DEVSHARD_TX_POLL_INTERVAL_MS"), defaultTxPollInterval),
		PollTimeout:  txSettingDurationMS(os.Getenv("DEVSHARD_TX_POLL_TIMEOUT_MS"), defaultTxPollTimeout),
	})
}

func firstNonZeroUint64(values ...uint64) uint64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func (g *Gateway) addCreatedEscrowRuntime(record GatewayDevshardState) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	state, ok, err := g.store.LoadState()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("gateway state is not initialized")
	}
	if _, exists := g.runtimes[record.ID]; exists {
		return fmt.Errorf("devshard %s already exists", record.ID)
	}
	if record.Model == "" {
		record.Model = state.Settings.DefaultModel
	}
	if record.StoragePath == "" {
		record.StoragePath = defaultStoragePath(g.baseStorageDir, record.ID)
	}
	rt, err := gatewayRuntimeBuilder(record.RuntimeConfig, state.Settings.ChainREST, state.Settings.DefaultModel, g.perf)
	if err != nil {
		return err
	}
	if err := g.store.UpsertDevshard(record); err != nil {
		rt.close()
		return err
	}
	g.runtimes[record.ID] = rt
	g.runtimeOrder = append(g.runtimeOrder, rt)
	g.attachRuntimeSharedState(rt)
	g.sortRuntimeOrderLocked()
	return nil
}

func (g *Gateway) handleAdminDevshardAction(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/v1/admin/devshards/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	if len(parts) == 1 && r.Method == http.MethodDelete {
		g.handleAdminCleanDevshard(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "deactivate" && r.Method == http.MethodPost {
		g.handleAdminDeactivateDevshard(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "settle" && r.Method == http.MethodPost {
		g.handleAdminSettleDevshard(w, r, id)
		return
	}
	http.NotFound(w, r)
}

func (g *Gateway) handleAdminSettleDevshard(w http.ResponseWriter, r *http.Request, id string) {
	if g.store == nil {
		http.Error(w, `{"error":{"message":"gateway state store unavailable"}}`, http.StatusServiceUnavailable)
		return
	}
	var req adminSettleEscrowRequest
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(string(body)) != "" {
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusBadRequest)
			return
		}
	}

	result, err := g.settleDevshardOnChain(r.Context(), id, req)
	if err != nil {
		switch {
		case errors.Is(err, errDevshardBusy):
			http.Error(w, fmt.Sprintf(`{"error":{"message":"devshard %s has active requests"}}`, id), http.StatusConflict)
			return
		case strings.Contains(err.Error(), "is not active"):
			http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusNotFound)
			return
		case strings.Contains(err.Error(), "private key") || strings.Contains(err.Error(), "gateway state"):
			http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusBadRequest)
			return
		}
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{
		"id":        id,
		"escrow_id": result.EscrowID,
		"active":    false,
		"tx_hash":   result.TxHash,
		"settler":   result.Settler,
	})
}

func (g *Gateway) resolveDevshardSettlementKey(id string, req adminSettleEscrowRequest) (string, string, error) {
	if strings.TrimSpace(req.PrivateKey) != "" || strings.TrimSpace(req.PrivateKeyEnv) != "" {
		return req.PrivateKey, req.PrivateKeyEnv, nil
	}
	state, ok, err := g.store.LoadState()
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", fmt.Errorf("gateway state is not initialized")
	}
	record, found := findGatewayDevshard(state.Devshards, id)
	if !found {
		return "", "", fmt.Errorf("devshard %s not found", id)
	}
	if strings.TrimSpace(record.PrivateKeyHex) != "" || strings.TrimSpace(record.PrivateKeyEnv) != "" {
		return record.PrivateKeyHex, record.PrivateKeyEnv, nil
	}
	return "", "", fmt.Errorf("private_key or private_key_env is required")
}

func (g *Gateway) handleAdminAddDevshard(w http.ResponseWriter, r *http.Request) {
	if g.store == nil {
		http.Error(w, `{"error":{"message":"gateway state store unavailable"}}`, http.StatusServiceUnavailable)
		return
	}
	var req adminDevshardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusBadRequest)
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		http.Error(w, `{"error":{"message":"id is required"}}`, http.StatusBadRequest)
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	state, ok, err := g.store.LoadState()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, `{"error":{"message":"gateway state is not initialized"}}`, http.StatusServiceUnavailable)
		return
	}

	record, found := findGatewayDevshard(state.Devshards, req.ID)
	if found {
		if strings.TrimSpace(req.PrivateKey) != "" {
			record.PrivateKeyHex = strings.TrimSpace(req.PrivateKey)
		}
		if strings.TrimSpace(req.PrivateKeyEnv) != "" {
			record.PrivateKeyEnv = strings.TrimSpace(req.PrivateKeyEnv)
		}
		if strings.TrimSpace(req.Model) != "" {
			record.Model = strings.TrimSpace(req.Model)
		}
		if strings.TrimSpace(req.StoragePath) != "" {
			record.StoragePath = strings.TrimSpace(req.StoragePath)
		}
		if strings.TrimSpace(req.ProtocolVersion) != "" {
			record.ProtocolVersion = strings.TrimSpace(req.ProtocolVersion)
		}
		record.Active = true
	} else {
		hasKey := strings.TrimSpace(req.PrivateKey) != "" || strings.TrimSpace(req.PrivateKeyEnv) != ""
		if !hasKey {
			http.Error(w, `{"error":{"message":"private_key or private_key_env is required for a new devshard"}}`, http.StatusBadRequest)
			return
		}
		record = GatewayDevshardState{
			RuntimeConfig: RuntimeConfig{
				ID:              req.ID,
				PrivateKeyHex:   strings.TrimSpace(req.PrivateKey),
				PrivateKeyEnv:   strings.TrimSpace(req.PrivateKeyEnv),
				Model:           strings.TrimSpace(req.Model),
				StoragePath:     strings.TrimSpace(req.StoragePath),
				ProtocolVersion: strings.TrimSpace(req.ProtocolVersion),
			},
			Active: true,
		}
	}

	if existing, exists := g.runtimes[req.ID]; exists {
		if existing.active.Load() {
			http.Error(w, `{"error":{"message":"devshard already active"}}`, http.StatusConflict)
			return
		}
		if err := g.store.UpsertDevshard(record); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusInternalServerError)
			return
		}
		existing.active.Store(true)
		writeJSON(w, map[string]any{
			"id":           record.ID,
			"active":       true,
			"model":        record.Model,
			"storage_path": record.StoragePath,
		})
		return
	}

	if record.Model == "" {
		record.Model = state.Settings.DefaultModel
	}
	if record.StoragePath == "" {
		record.StoragePath = defaultStoragePath(g.baseStorageDir, record.ID)
	}

	rt, err := gatewayRuntimeBuilder(record.RuntimeConfig, state.Settings.ChainREST, state.Settings.DefaultModel, g.perf)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusBadRequest)
		return
	}
	if err := g.store.UpsertDevshard(record); err != nil {
		rt.close()
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusInternalServerError)
		return
	}
	g.runtimes[record.ID] = rt
	g.runtimeOrder = append(g.runtimeOrder, rt)
	g.attachRuntimeSharedState(rt)
	g.sortRuntimeOrderLocked()
	writeJSON(w, map[string]any{
		"id":           record.ID,
		"active":       true,
		"model":        record.Model,
		"storage_path": record.StoragePath,
	})
}

func (g *Gateway) handleAdminDeactivateDevshard(w http.ResponseWriter, r *http.Request, id string) {
	if g.store == nil {
		http.Error(w, `{"error":{"message":"gateway state store unavailable"}}`, http.StatusServiceUnavailable)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	rt, ok := g.runtimes[id]
	if !ok {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"devshard %s is not active"}}`, id), http.StatusNotFound)
		return
	}
	if err := g.store.SetDevshardActive(id, false); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusInternalServerError)
		return
	}
	rt.active.Store(false)
	writeJSON(w, map[string]any{
		"id":     id,
		"active": false,
	})
}

func (g *Gateway) handleAdminCleanDevshard(w http.ResponseWriter, r *http.Request, id string) {
	if g.store == nil {
		http.Error(w, `{"error":{"message":"gateway state store unavailable"}}`, http.StatusServiceUnavailable)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	state, ok, err := g.store.LoadState()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, `{"error":{"message":"gateway state is not initialized"}}`, http.StatusServiceUnavailable)
		return
	}
	record, found := findGatewayDevshard(state.Devshards, id)
	if !found {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"devshard %s not found"}}`, id), http.StatusNotFound)
		return
	}
	if record.Active {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"devshard %s is active; deactivate it first"}}`, id), http.StatusConflict)
		return
	}
	if rt, ok := g.runtimes[id]; ok {
		if rt.activeRequests.Load() > 0 {
			http.Error(w, fmt.Sprintf(`{"error":{"message":"devshard %s has active requests"}}`, id), http.StatusConflict)
			return
		}
		delete(g.runtimes, id)
		g.runtimeOrder = removeRuntime(g.runtimeOrder, id)
		if g.capacity != nil {
			g.capacity.RemoveEscrow(id)
		}
		if err := rt.close(); err != nil {
			log.Printf("close devshard %s: %v", id, err)
		}
	}
	if err := g.store.DeleteDevshard(id); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if err := removeDevshardStorage(record.StoragePath, g.baseStorageDir); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"id":      id,
		"deleted": true,
	})
}

func (g *Gateway) handleAdminUnquarantine(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ParticipantKey string `json:"participant_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.ParticipantKey) == "" {
		http.Error(w, `{"error":{"message":"participant_key is required"}}`, http.StatusBadRequest)
		return
	}
	if g.participantLimiter == nil {
		http.Error(w, `{"error":{"message":"participant limiter not configured"}}`, http.StatusServiceUnavailable)
		return
	}
	cleared := g.participantLimiter.ClearQuarantine(req.ParticipantKey)
	writeJSON(w, map[string]any{
		"participant_key": req.ParticipantKey,
		"cleared":         cleared,
	})
}

func findGatewayDevshard(devshards []GatewayDevshardState, id string) (GatewayDevshardState, bool) {
	for _, devshard := range devshards {
		if devshard.ID == id {
			return devshard, true
		}
	}
	return GatewayDevshardState{}, false
}

func removeRuntime(runtimes []*devshardRuntime, id string) []*devshardRuntime {
	out := runtimes[:0]
	for _, rt := range runtimes {
		if rt.id != id {
			out = append(out, rt)
		}
	}
	return out
}

func (g *Gateway) sortRuntimeOrderLocked() {
	slices.SortFunc(g.runtimeOrder, func(a, b *devshardRuntime) int {
		return strings.Compare(a.id, b.id)
	})
}

func (g *Gateway) attachMetrics(rt *devshardRuntime) {
	if g == nil || g.metrics == nil || rt == nil || rt.proxy == nil || rt.proxy.redundancy == nil {
		return
	}
	rt.proxy.redundancy.metrics = g.metrics
	rt.proxy.redundancy.devshardID = rt.id
}

func (g *Gateway) attachEscrowChecker(rt *devshardRuntime) {
	if g == nil || rt == nil || rt.proxy == nil || rt.proxy.redundancy == nil {
		return
	}
	escrowID := rt.id
	protocol := rt.proxy.sm.ProtocolVersion()
	if g.escrowChecker != nil {
		rt.proxy.redundancy.onEscrowMissing = func() {
			go g.escrowChecker.TriggerCheckForProtocol(escrowID, protocol, func() {
				g.deactivateDevshardByID(escrowID)
			})
		}
	}
	rt.proxy.redundancy.onBalanceExhausted = func() {
		log.Printf("gateway_deactivating_exhausted_escrow escrow=%s", escrowID)
		g.deactivateAndSettleDevshardByID(escrowID, "balance_exhausted")
	}
}

// deactivateDevshardByID marks a devshard inactive in memory and persists the change.
// Safe to call from any goroutine.
func (g *Gateway) deactivateDevshardByID(id string) bool {
	return g.deactivateDevshardByIDWithReason(id, "escrow confirmed missing on chain")
}

func (g *Gateway) deactivateDevshardByIDWithReason(id, reason string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	rt, ok := g.runtimes[id]
	if !ok || !rt.active.Load() {
		return false
	}
	rt.active.Store(false)
	if g.store != nil {
		if err := g.store.SetDevshardActive(id, false); err != nil {
			log.Printf("escrow checker: persist deactivation for %s: %v", id, err)
		}
	}
	log.Printf("devshard %s deactivated: %s", id, reason)
	return true
}

func (g *Gateway) deactivateAndSettleDevshardByID(id, reason string) {
	if !g.deactivateDevshardByIDWithReason(id, reason) {
		return
	}
	g.scheduleAutoSettlement(id, reason)
}

func (g *Gateway) scheduleAutoSettlement(id, reason string) {
	if g.store == nil {
		log.Printf("auto_settle_skipped escrow=%s reason=%s error=missing_gateway_store", id, reason)
		return
	}

	g.settlementMu.Lock()
	if g.settlementInFlight == nil {
		g.settlementInFlight = make(map[string]struct{})
	}
	if _, exists := g.settlementInFlight[id]; exists {
		g.settlementMu.Unlock()
		return
	}
	g.settlementInFlight[id] = struct{}{}
	g.settlementMu.Unlock()

	go func() {
		defer func() {
			g.settlementMu.Lock()
			delete(g.settlementInFlight, id)
			g.settlementMu.Unlock()
		}()

		for attempt := 1; attempt <= autoSettlementMaxAttempts; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), autoSettlementAttemptTimeout)
			result, err := g.settleDevshardOnChain(ctx, id, adminSettleEscrowRequest{})
			cancel()
			if err == nil {
				log.Printf("auto_settle_submitted escrow=%s reason=%s tx_hash=%s settler=%s",
					id, reason, result.TxHash, result.Settler)
				return
			}
			log.Printf("auto_settle_failed escrow=%s reason=%s attempt=%d/%d error=%v",
				id, reason, attempt, autoSettlementMaxAttempts, err)
			if attempt == autoSettlementMaxAttempts {
				return
			}
			time.Sleep(autoSettlementRetryInterval)
		}
	}()
}

func removeDevshardStorage(storagePath, baseStorageDir string) error {
	if strings.TrimSpace(storagePath) == "" {
		return nil
	}
	storagePath = filepath.Clean(storagePath)
	baseStorageDir = filepath.Clean(baseStorageDir)
	if !strings.HasPrefix(storagePath, baseStorageDir+string(os.PathSeparator)) && storagePath != baseStorageDir {
		return fmt.Errorf("refusing to delete storage outside base dir: %s", storagePath)
	}
	parent := filepath.Dir(storagePath)
	if filepath.Base(storagePath) == "state.db" && strings.HasPrefix(parent, baseStorageDir+string(os.PathSeparator)) {
		return os.RemoveAll(parent)
	}
	for _, path := range []string{storagePath, storagePath + "-shm", storagePath + "-wal"} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	if err := os.Remove(parent); err != nil && !os.IsNotExist(err) {
		return nil
	}
	return nil
}

func finalizeRuntimeConfigs(runtimes []RuntimeConfig, defaultModel, baseStorageDir string) ([]RuntimeConfig, error) {
	out := make([]RuntimeConfig, 0, len(runtimes))
	seen := make(map[string]struct{}, len(runtimes))
	for _, cfg := range runtimes {
		cfg.ID = strings.TrimSpace(cfg.ID)
		if cfg.ID == "" {
			return nil, fmt.Errorf("runtime config missing id")
		}
		if _, ok := seen[cfg.ID]; ok {
			return nil, fmt.Errorf("duplicate runtime id %s", cfg.ID)
		}
		seen[cfg.ID] = struct{}{}
		if cfg.Model == "" {
			cfg.Model = defaultModel
		}
		if cfg.StoragePath == "" {
			cfg.StoragePath = defaultStoragePath(baseStorageDir, cfg.ID)
		}
		out = append(out, cfg)
	}
	slices.SortFunc(out, func(a, b RuntimeConfig) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

func buildRuntimes(configs []RuntimeConfig, chainREST, defaultModel string) ([]*devshardRuntime, error) {
	type result struct {
		idx int
		rt  *devshardRuntime
		err error
	}
	t0 := time.Now()
	perf := NewPerfTracker(nil)
	ch := make(chan result, len(configs))
	for i, cfg := range configs {
		go func(idx int, cfg RuntimeConfig) {
			rt, err := buildRuntime(cfg, chainREST, defaultModel, perf)
			ch <- result{idx, rt, err}
		}(i, cfg)
	}

	runtimes := make([]*devshardRuntime, len(configs))
	var firstErr error
	for range configs {
		res := <-ch
		if res.err != nil && firstErr == nil {
			firstErr = res.err
		}
		if res.rt != nil {
			runtimes[res.idx] = res.rt
			log.Printf("loaded devshard runtime escrow=%s model=%s storage=%s",
				configs[res.idx].ID, res.rt.model, configs[res.idx].StoragePath)
		}
	}
	if firstErr != nil {
		for _, rt := range runtimes {
			if rt != nil {
				rt.close()
			}
		}
		return nil, firstErr
	}
	log.Printf("build_runtimes_parallel count=%d total_elapsed_ms=%d", len(configs), time.Since(t0).Milliseconds())
	return runtimes, nil
}
