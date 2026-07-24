package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"encoding/json"
	ccomp "miroxy/core/compress"
	"miroxy/core/cred"
	"miroxy/core/dispatch"
	coredown "miroxy/core/downstream"
	"miroxy/core/ir"
	"miroxy/core/rpc"
	"miroxy/core/selector"
	coreup "miroxy/core/upstream"
	"miroxy/internal/auth"
	"miroxy/internal/compress"
	"miroxy/internal/config"
	intcred "miroxy/internal/cred"
	intdown "miroxy/internal/downstream"
	"miroxy/internal/dump"
	"miroxy/internal/idgen"
	"miroxy/internal/localstate"
	"miroxy/internal/pipeline"
	introuter "miroxy/internal/router"
	"miroxy/internal/stats"
	"miroxy/internal/types"
	intup "miroxy/internal/upstream"
	intwarden "miroxy/internal/warden"
)

// routingState is swapped atomically on hot reload.
// All maps are read together so they stay consistent.
type routingState struct {
	selectors map[string]selector.Selector
	timeouts  map[string]time.Duration
	// passthroughSelectors handles models not in model_routes.
	// Keyed by provider name ("anthropic", "openai", "gemini").
	// Built from credpools whose family is known — either an explicit
	// upstream_model_type: tag, or derived from a model_routes reference
	// (see cfg.CredpoolFamilies / resolveCredpoolFamilies).
	passthroughSelectors map[string]selector.Selector
	// usageAcc holds one entry per credstone-backed named pool, keyed by the
	// same "credsource:"+poolName SelectionID the pool uses internally, so
	// UpstreamExecutor can look up plan.SelectionID directly. Empty when
	// credsource is disabled.
	usageAcc map[string]*intcred.UsageAccumulator
}

// Server is the miroxy HTTP server.
type Server struct {
	mux           *http.ServeMux
	cfgPath       string                        // config file path for reload
	cfg           atomic.Pointer[config.Config] // swapped atomically on reload
	routing       atomic.Pointer[routingState]  // selectors + timeouts, swapped atomically
	router        *introuter.BuiltinRouter      // model-name → RouteTarget; see internal/router
	pipe          *pipeline.Pipeline
	dispatcher    dispatch.Dispatcher
	inFlight      atomic.Int64     // count of active requests, used for graceful shutdown logging
	dumpStore     dump.Store       // nil when dump is disabled
	tokenStats    *stats.Registry  // in-process token usage counters; never nil
	compressStats *ccomp.Stats     // nil when compress is disabled
	wardenStats   *intwarden.Stats // nil when warden is disabled
	startTime     time.Time

	// Proxy lifecycle — used by the serve loop and admin stop/start endpoints.
	proxyRunning atomic.Bool
	stopProxyCh  chan struct{} // buffered(1); admin writes, serve loop reads
	startProxyCh chan struct{} // buffered(1); admin writes, serve loop reads

	// credSourceCancel stops the current generation's CredSource background
	// pollers (see buildRoutingState). Not read on the request path — only
	// touched by New, Reload, and Close, which are never concurrent with
	// each other — so it needs no atomic/mutex guard.
	credSourceCancel context.CancelFunc

	// localStateStore is opened once at New() time (nil when local_state is
	// disabled or sidecar.credsource is enabled) and reused across Reload —
	// changing local_state config requires a restart, same as server.port.
	localStateStore *localstate.Store

	// wardenFlushCancel stops the periodic warden-stats-to-local_state flush
	// loop started in New() (nil when warden or local_state is disabled).
	// Warden isn't reload-aware, so unlike credSourceCancel this has a
	// simple 1:1 lifetime with the process, not a per-generation one.
	wardenFlushCancel context.CancelFunc

	// tokenFlushCancel/compressFlushCancel are the same pattern as
	// wardenFlushCancel, for token-usage and compression stats respectively.
	// tokenStats is always active (unconditional in newBase), so its flush
	// loop only depends on local_state being enabled; compressStats is nil
	// unless compress.enabled is set too.
	tokenFlushCancel    context.CancelFunc
	compressFlushCancel context.CancelFunc
}

// credentialFromConfig wraps a raw key into the appropriate typed Credential.
func credentialFromConfig(keyValue, authStyle string) cred.Credential {
	switch authStyle {
	case "bearer":
		return &cred.HeaderCredential{Header: "Authorization", Value: "Bearer " + keyValue}
	case "api_key":
		return &cred.HeaderCredential{Header: "x-api-key", Value: keyValue}
	case "none":
		return &cred.HeaderCredential{Header: "", Value: ""}
	default: // "query_key" or empty → Gemini-style ?key=
		return &cred.QueryCredential{Param: "key", Value: keyValue}
	}
}

// buildCredSpecsFromPool builds CredSpec slice from a CredPoolCfg using a given authStyle.
// poolName is used only for the oauth_refresh multi-replica warning below.
func buildCredSpecsFromPool(kp config.CredPoolCfg, authStyle, poolName string) []selector.CredSpec {
	if kp.Type == "oauth_refresh" && len(kp.Keys) > 0 {
		intcred.WarnIfMultiReplicaUnsafe(poolName)
	}
	specs := make([]selector.CredSpec, 0, len(kp.Keys))
	for _, k := range kp.Keys {
		// validateConfig already warned and counted this key as unusable —
		// skip it here too so an empty credential never reaches Select().
		if !k.IsUsable(authStyle) {
			continue
		}
		var src selector.CredentialSource
		switch {
		case kp.Type == "oauth_refresh":
			src = intcred.NewOAuthSource(kp.ClientID, kp.ClientSecret, k.Key)
		case authStyle == "sigv4":
			// SigV4Credential.Apply signs the request in place (see
			// core/cred/credential.go) — no AWS SDK or separate dispatch
			// path involved.
			src = selector.NewStaticSource(&cred.SigV4Credential{
				AccessKeyID:     k.AccessKeyID,
				SecretAccessKey: k.SecretAccessKey,
				SessionToken:    k.SessionToken,
				Region:          kp.Region,
				Service:         kp.Service,
			})
		default:
			src = selector.NewStaticSource(credentialFromConfig(k.Key, authStyle))
		}
		specs = append(specs, selector.CredSpec{Name: k.Name, Source: src})
	}
	return specs
}

// newCredPool builds a CredPool from a CredPoolCfg and authStyle.
//
// When credClient is non-nil (credsource.enabled), one additional CredSpec
// sourcing from credstone (poolID = poolName) is appended alongside the
// pool's local keys, and its background health poller is started against
// pollerCtx. A pool not registered in credstone (or temporarily exhausted
// there) simply parks that one entry — the pool's local keys keep serving,
// giving the fallback behavior for free via CredPool's existing per-entry
// circuit-break.
//
// The returned *intcred.UsageAccumulator is nil unless credClient is non-nil;
// callers key it by the same "credsource:"+poolName SelectionID the pool
// uses internally, so UpstreamExecutor can look it up by plan.SelectionID
// with no extra bookkeeping.
func newCredPool(
	kp config.CredPoolCfg,
	authStyle string,
	poolName string,
	credClient *intcred.CredstoneClient,
	syncInterval time.Duration,
	pollerCtx context.Context,
) (*selector.CredPool, *intcred.UsageAccumulator) {
	specs := buildCredSpecsFromPool(kp, authStyle, poolName)
	var usage *intcred.UsageAccumulator
	if credClient != nil {
		cs := intcred.NewCredSource(credClient, poolName)
		cs.StartPoller(pollerCtx, syncInterval)
		usage = intcred.NewUsageAccumulator(credClient, poolName)
		usage.StartFlusher(pollerCtx, syncInterval)
		cs.SetUsageAccumulator(usage)
		specs = append(specs, selector.CredSpec{Name: "credsource:" + poolName, Source: cs})
	}
	pool := selector.NewCredPool(selector.CredPoolConfig{
		Name:            poolName,
		Keys:            specs,
		Strategy:        kp.Strategy,
		Threshold:       kp.CircuitBreakThreshold,
		Cooldown:        time.Duration(kp.CooldownSeconds) * time.Second,
		RateLimitRPM:    kp.RateLimitRPM,
		RateSoftLimit:   kp.RateSoftLimit,
		RateLimitTPM:    kp.RateLimitTPM,
		Sticky:          kp.Sticky,
		StickyTTL:       time.Duration(kp.StickyTTLSeconds) * time.Second,
		RoundRobinBatch: kp.RoundRobinBatchSize,
	})
	return pool, usage
}

// resolveProto resolves the effective outgoing protocol for a model entry.
func resolveProto(m config.ModelEntry) (proto string) {
	proto = m.Protocol
	if proto == "" {
		proto = m.ProviderRef
	}
	return proto
}

// namedPoolAuthStyle returns a named pool's auth_style, resolved by
// resolveProviders from its provider_ref. Returns "bearer" as a defensive
// default for a pool that somehow has neither (shouldn't happen for a
// config that passed validateConfig).
func namedPoolAuthStyle(poolName string, cfg *config.Config) string {
	if kp, ok := cfg.CredPools[poolName]; ok && kp.AuthStyle != "" {
		return kp.AuthStyle
	}
	return "bearer"
}

// namedPoolProvider returns a named credpool's provider_ref, or "" if unset.
func namedPoolProvider(poolName string, cfg *config.Config) string {
	return cfg.CredPools[poolName].ProviderRef
}

// buildRoutingState builds selectors and timeouts from a config.
//
// The returned context.CancelFunc stops every CredSource poller started
// during this build; it is a no-op when credsource is disabled. Callers
// must invoke it once this routingState is replaced (on Reload) or the
// server shuts down, so pollers from superseded generations don't leak.
func buildRoutingState(cfg *config.Config, localStore *localstate.Store) (*routingState, context.CancelFunc) {
	pollerCtx, cancelPollers := context.WithCancel(context.Background())

	var credClient *intcred.CredstoneClient
	if cfg.Sidecar.CredSource.Enabled {
		credClient = intcred.NewCredstoneClient(cfg.Sidecar.CredSource.BaseURL, cfg.Sidecar.CredSource.AuthToken)
		slog.Info("credsource enabled — credentials also served by credstone", "base_url", cfg.Sidecar.CredSource.BaseURL)
	}
	syncInterval := time.Duration(cfg.Sidecar.CredSource.SyncInterval) * time.Second

	namedPools := make(map[string]*selector.CredPool, len(cfg.CredPools))
	usageAcc := make(map[string]*intcred.UsageAccumulator)
	for name, kp := range cfg.CredPools {
		authStyle := namedPoolAuthStyle(name, cfg)
		pool, usage := newCredPool(kp, authStyle, name, credClient, syncInterval, pollerCtx)
		namedPools[name] = pool
		if usage != nil {
			usageAcc["credsource:"+name] = usage
		}
		if localStore != nil {
			pool.RestoreHealth(loadHealthSnapshot(localStore, name))
			startHealthSnapshotLoop(pollerCtx, pool, localStore, name)
		}
	}
	sels := make(map[string]selector.Selector, len(cfg.ModelRoutes))
	timeouts := make(map[string]time.Duration, len(cfg.ModelRoutes))
	for _, m := range cfg.ModelRoutes {
		sels[m.ModelName] = buildModelSelector(m, namedPools, cfg)
		timeouts[m.ModelName] = modelTimeout(m)
	}

	// Build native-vendor passthrough selectors: credpools that opted in via
	// native_passthrough: true serve requests for a model name that is
	// globally unique to one vendor (claude-*/gpt-*/gemini-*) but matched no
	// model_routes entry — see LookupModel step 4. Gated by the root
	// native_passthrough_enable switch so a client's default model picker
	// can't silently reach real vendor credentials unless the operator
	// explicitly turns this on. Multiple pools for the same protocol are
	// grouped under one round-robin RoutingSelector.
	passthrough := make(map[string]selector.Selector)
	if cfg.NativePassthroughEnable {
		byProtocol := make(map[string][]selector.Selector)
		for poolName, kp := range cfg.CredPools {
			if !kp.NativePassthrough {
				continue
			}
			pool, ok := namedPools[poolName]
			if !ok {
				continue
			}
			// "passthrough" here means the client's raw model name is
			// forwarded as-is (no model_routes entry matched it) — unrelated
			// to protocol passthrough. The real-vs-raw protocol decision is
			// still made per request in dispatchFor, same as every other
			// target.
			upstream := intup.Get(kp.Protocol, "", kp.APIBase)
			rawEndpoint, rawStreamEndpoint := intup.PassthroughEndpoints(kp.Protocol, "", kp.APIBase)
			rawAdapter := intup.NewPassthrough(rawEndpoint, rawStreamEndpoint, "")
			byProtocol[kp.Protocol] = append(byProtocol[kp.Protocol], selector.NewTargetSelector(pool, upstream, "", kp.Protocol, rawAdapter, false))
			slog.Info("native passthrough pool registered", "protocol", kp.Protocol, "pool", poolName)
		}
		for protocol, sels := range byProtocol {
			passthrough[protocol] = selector.NewRoutingSelector(selector.RoutingSelectorConfig{
				Strategy:  "round_robin",
				Selectors: sels,
			})
		}
	}

	return &routingState{selectors: sels, timeouts: timeouts, passthroughSelectors: passthrough, usageAcc: usageAcc}, cancelPollers
}

// New creates a production Server.
func New(cfg *config.Config, cfgPath string) *Server {
	if cfg.Server.ModelDiscovery != "strict" {
		tryInjectAnthropicModels(cfg)
		tryInjectOpenAIModels(cfg)
	}

	s := newBase(cfg, cfgPath)

	// Wire dump store when enabled.
	if cfg.Dump.Enabled {
		if ds, err := dump.NewJSONLStoreWithLimits(cfg.Dump.Path, cfg.Dump.MaxSizeMB, cfg.Dump.MaxBackups); err != nil {
			slog.Warn("dump: failed to open store, dump disabled", "error", err)
		} else {
			s.dumpStore = ds
			slog.Info("dump enabled", "path", cfg.Dump.Path,
				"max_size_mb", cfg.Dump.MaxSizeMB, "max_backups", cfg.Dump.MaxBackups)
		}
	}

	s.localStateStore = openLocalStateStore(cfg)
	if s.localStateStore != nil {
		if persisted, ok := s.localStateStore.LoadTokenStats(); ok {
			s.tokenStats.Restore(tokenStatsFromPersisted(persisted))
		}
		ctx, cancel := context.WithCancel(context.Background())
		startTokenStatsFlushLoop(ctx, s.tokenStats, s.localStateStore)
		s.tokenFlushCancel = cancel
	}

	rt, cancelPollers := buildRoutingState(cfg, s.localStateStore)
	s.routing.Store(rt)
	s.credSourceCancel = cancelPollers
	s.router.UpdateConfig(cfg)
	s.router.UpdateRouting(&introuter.RoutingTable{
		Selectors:            rt.selectors,
		Timeouts:             rt.timeouts,
		PassthroughSelectors: rt.passthroughSelectors,
	})

	probers := make(map[string]*keyProber, len(cfg.ModelRoutes))
	for _, m := range cfg.ModelRoutes {
		probers[m.ModelName] = newKeyProber(m.ModelName, rt.selectors[m.ModelName], s.dispatcher)
	}

	plugins := []pipeline.Plugin{newUpstreamExecutor(probers, s.tokenStats, rt.usageAcc)}
	if cfg.Warden.Enabled {
		wp, ws := buildWardenPlugin(&cfg.Warden, s.localStateStore)
		s.wardenStats = ws
		plugins = append([]pipeline.Plugin{wp}, plugins...)
		if s.localStateStore != nil {
			ctx, cancel := context.WithCancel(context.Background())
			startWardenStatsFlushLoop(ctx, ws, s.localStateStore)
			s.wardenFlushCancel = cancel
		}
	}
	if cfg.Compress.Enabled {
		cp, cs := buildCompressPlugin(&cfg.Compress, s.localStateStore)
		s.compressStats = cs
		plugins = append([]pipeline.Plugin{cp}, plugins...)
		if s.localStateStore != nil {
			ctx, cancel := context.WithCancel(context.Background())
			startCompressStatsFlushLoop(ctx, cs, s.localStateStore)
			s.compressFlushCancel = cancel
		}
	}
	// CommandPlugin runs at priority 5 (before everything else).
	cmdCfg := pipeline.CommandConfig{Disabled: cfg.Server.Commands.Disabled, AllowDump: cfg.Server.Commands.AllowDump}
	plugins = append([]pipeline.Plugin{pipeline.NewCommandPlugin(s, cmdCfg)}, plugins...)
	s.pipe = pipeline.New(plugins)
	s.registerRoutes(cfg)
	return s
}

// openLocalStateStore opens the standalone-mode health cache per
// LocalStateConfig, or returns nil when it doesn't apply. sidecar.credsource
// always wins: credstone is already the authoritative, cross-restart source
// of credential health, so a local disk cache would add nothing but a second
// copy that can disagree with it — local_state is ignored (with a warning)
// rather than silently doing something in that mode.
func openLocalStateStore(cfg *config.Config) *localstate.Store {
	if cfg.Sidecar.CredSource.Enabled {
		if cfg.LocalState.Enabled {
			slog.Warn("local_state.enabled is ignored while sidecar.credsource.enabled is true — " +
				"credstone is already the authoritative source of credential health")
		}
		return nil
	}
	if !cfg.LocalState.Enabled {
		return nil
	}
	path := cfg.LocalState.Path
	if path == "" {
		path = "./miroxy-local-state.db"
	}
	slog.Info("local_state enabled — credential health, warden, token, and compression stats will be cached to disk", "path", path)
	return localstate.Open(path)
}

// healthSnapshotInterval is how often each credpool's health state is
// mirrored to the local_state store. Not currently configurable — a fixed
// default keeps the config surface small; revisit if someone needs to tune it.
const healthSnapshotInterval = 30 * time.Second

// startHealthSnapshotLoop periodically mirrors pool's per-credential health
// to store under poolName, so a standalone-mode restart doesn't immediately
// re-hammer a credential that was still cooling down. Runs until ctx is
// cancelled — same per-generation lifecycle as CredSource's poller and
// UsageAccumulator's flusher — and does one best-effort final flush on exit,
// which is how both Reload (superseding this generation) and Close
// (shutdown) get a last snapshot without any extra call site.
func startHealthSnapshotLoop(ctx context.Context, pool *selector.CredPool, store *localstate.Store, poolName string) {
	go func() {
		t := time.NewTicker(healthSnapshotInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				flushHealthSnapshot(pool, store, poolName)
				return
			case <-t.C:
				flushHealthSnapshot(pool, store, poolName)
			}
		}
	}()
}

func flushHealthSnapshot(pool *selector.CredPool, store *localstate.Store, poolName string) {
	snap := pool.HealthSnapshot()
	converted := make(map[string]localstate.CredHealth, len(snap))
	for id, h := range snap {
		converted[id] = localstate.CredHealth{
			State:             h.State,
			CoolEndUnixNano:   h.CoolEnd.UnixNano(),
			RateLimitFailures: h.RateLimitFailures,
			Failures:          h.Failures,
		}
	}
	if err := store.SaveAllCredHealth(poolName, converted); err != nil {
		slog.Warn("local_state: health snapshot flush failed", "pool", poolName, "error", err)
	}
}

// loadHealthSnapshot reads poolName's persisted health snapshot and converts
// it to the selector package's snapshot type, ready for CredPool.RestoreHealth.
func loadHealthSnapshot(store *localstate.Store, poolName string) map[string]selector.CredHealthSnapshot {
	raw := store.LoadAllCredHealth(poolName)
	out := make(map[string]selector.CredHealthSnapshot, len(raw))
	for id, h := range raw {
		out[id] = selector.CredHealthSnapshot{
			State:             h.State,
			CoolEnd:           time.Unix(0, h.CoolEndUnixNano),
			RateLimitFailures: h.RateLimitFailures,
			Failures:          h.Failures,
		}
	}
	return out
}

// buildCompressPlugin constructs a CompressPlugin from the config block.
// buildCompressPlugin constructs a CompressPlugin from the config block. A
// fresh BuiltinCompressor+Stats is built each call. When store is non-nil, a
// prior run's persisted counters are restored before the plugin/stats are
// returned — before any request can reach them, same contract as
// buildWardenPlugin.
func buildCompressPlugin(cfg *config.CompressConfig, store *localstate.Store) (*compress.CompressPlugin, *ccomp.Stats) {
	bc := compress.BuiltinConfig{
		ToolResultBudget: cfg.ToolResultBudget,
		TotalBudget:      cfg.TotalBudget,
		WindowRecentKeep: cfg.WindowRecentKeep,
		AlignDynamic:     cfg.AlignDynamic,
		CCR:              compress.NewMemCCRStore(), // bbolt: swap when CCRPath != ""
	}
	comp := compress.NewBuiltinCompressor(bc)
	stats := comp.Stats()
	if store != nil {
		if persisted, ok := store.LoadCompressStats(); ok {
			stats.Restore(persisted.TotalRequests, persisted.TotalOriginal, persisted.TotalCompressed,
				compressModelsFromPersisted(persisted.Models))
		}
	}
	return compress.NewCompressPlugin(comp, cfg.Threshold), stats
}

// buildWardenPlugin constructs a WardenPlugin from the config block. A
// fresh BuiltinWarden+Stats is built each call, same as buildCompressPlugin
// above. When store is non-nil, a prior run's persisted counters are
// restored before the plugin/stats are returned — i.e. before any request
// can reach them, same contract as CredPool.RestoreHealth.
func buildWardenPlugin(cfg *config.WardenConfig, store *localstate.Store) (*intwarden.WardenPlugin, *intwarden.Stats) {
	wc := &intwarden.Config{
		Enabled:    cfg.Enabled,
		Mode:       cfg.Mode,
		Secrets:    cfg.Secrets == nil || *cfg.Secrets,
		PII:        cfg.PII == nil || *cfg.PII,
		Injection:  cfg.Injection == nil || *cfg.Injection,
		Jailbreak:  cfg.Jailbreak == nil || *cfg.Jailbreak,
		FailClosed: cfg.FailClosed,
	}
	w := intwarden.NewBuiltinWarden()
	w.UpdateConfig(wc)
	st := intwarden.NewStats()
	if store != nil {
		if persisted, ok := store.LoadWardenStats(); ok {
			st.Restore(wardenStatsFromPersisted(persisted))
		}
	}
	return intwarden.NewWardenPlugin(w, st), st
}

// wardenStatsFromPersisted converts the on-disk shape to the in-process
// StatsSnapshot shape Stats.Restore expects.
func wardenStatsFromPersisted(p localstate.WardenStats) intwarden.StatsSnapshot {
	var startedAt time.Time
	if p.StartedAtUnixNano != 0 {
		startedAt = time.Unix(0, p.StartedAtUnixNano)
	}
	return intwarden.StatsSnapshot{
		RequestsInspected: p.RequestsInspected,
		SecretsFound:      p.SecretsFound,
		PIIFound:          p.PIIFound,
		InjectionsBlocked: p.InjectionsBlocked,
		JailbreaksBlocked: p.JailbreaksBlocked,
		TokensVaulted:     p.TokensVaulted,
		ByType:            p.ByType,
		StartedAt:         startedAt,
	}
}

// healthSnapshotInterval (defined alongside startHealthSnapshotLoop) is
// reused for warden's flush cadence too — same tradeoff, same constant.

// startWardenStatsFlushLoop persists ws's counters to store every
// healthSnapshotInterval, plus once more when ctx is cancelled so Close()
// gets a final up-to-date snapshot. Warden isn't reload-aware today (Reload
// never rebuilds s.pipe), so ctx here has a simple 1:1 lifetime with the one
// warden instance built in New() — unlike the per-generation pollerCtx used
// for credential health.
func startWardenStatsFlushLoop(ctx context.Context, ws *intwarden.Stats, store *localstate.Store) {
	go func() {
		ticker := time.NewTicker(healthSnapshotInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				flushWardenStats(ws, store)
			case <-ctx.Done():
				flushWardenStats(ws, store)
				return
			}
		}
	}()
}

func flushWardenStats(ws *intwarden.Stats, store *localstate.Store) {
	snap := ws.Snapshot()
	if err := store.SaveWardenStats(localstate.WardenStats{
		RequestsInspected: snap.RequestsInspected,
		SecretsFound:      snap.SecretsFound,
		PIIFound:          snap.PIIFound,
		InjectionsBlocked: snap.InjectionsBlocked,
		JailbreaksBlocked: snap.JailbreaksBlocked,
		TokensVaulted:     snap.TokensVaulted,
		ByType:            snap.ByType,
		StartedAtUnixNano: snap.StartedAt.UnixNano(),
	}); err != nil {
		slog.Warn("warden: failed to flush stats to local_state", "error", err)
	}
}

// tokenStatsFromPersisted converts the on-disk shape to the argument list
// Registry.Restore expects.
func tokenStatsFromPersisted(p localstate.TokenStats) (totalIn, totalOut, totalReq int64, models []stats.ModelSnapshot) {
	models = make([]stats.ModelSnapshot, 0, len(p.Models))
	for _, m := range p.Models {
		pools := make([]stats.PoolSnapshot, 0, len(m.Pools))
		for _, pl := range m.Pools {
			keys := make([]stats.KeySnapshot, 0, len(pl.Keys))
			for _, k := range pl.Keys {
				keys = append(keys, stats.KeySnapshot{Name: k.Name, Input: k.Input, Output: k.Output, Requests: k.Requests})
			}
			pools = append(pools, stats.PoolSnapshot{Name: pl.Name, Input: pl.Input, Output: pl.Output, Requests: pl.Requests, Keys: keys})
		}
		models = append(models, stats.ModelSnapshot{Name: m.Name, Input: m.Input, Output: m.Output, Requests: m.Requests, Pools: pools})
	}
	return p.TotalInput, p.TotalOutput, p.TotalRequests, models
}

// startTokenStatsFlushLoop persists reg's counters to store every
// healthSnapshotInterval, plus once more when ctx is cancelled — same
// cadence/shutdown contract as startWardenStatsFlushLoop. tokenStats is
// always active (unconditional in newBase), so this loop's only gate is
// local_state being enabled, not any feature flag.
func startTokenStatsFlushLoop(ctx context.Context, reg *stats.Registry, store *localstate.Store) {
	go func() {
		ticker := time.NewTicker(healthSnapshotInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				flushTokenStats(reg, store)
			case <-ctx.Done():
				flushTokenStats(reg, store)
				return
			}
		}
	}()
}

func flushTokenStats(reg *stats.Registry, store *localstate.Store) {
	totalIn, totalOut, totalReq, models := reg.Snapshot()
	lsModels := make([]localstate.TokenModelStats, 0, len(models))
	for _, m := range models {
		pools := make([]localstate.TokenPoolStats, 0, len(m.Pools))
		for _, p := range m.Pools {
			keys := make([]localstate.TokenKeyStats, 0, len(p.Keys))
			for _, k := range p.Keys {
				keys = append(keys, localstate.TokenKeyStats{Name: k.Name, Input: k.Input, Output: k.Output, Requests: k.Requests})
			}
			pools = append(pools, localstate.TokenPoolStats{Name: p.Name, Input: p.Input, Output: p.Output, Requests: p.Requests, Keys: keys})
		}
		lsModels = append(lsModels, localstate.TokenModelStats{Name: m.Name, Input: m.Input, Output: m.Output, Requests: m.Requests, Pools: pools})
	}
	if err := store.SaveTokenStats(localstate.TokenStats{
		TotalInput: totalIn, TotalOutput: totalOut, TotalRequests: totalReq, Models: lsModels,
	}); err != nil {
		slog.Warn("token stats: failed to flush to local_state", "error", err)
	}
}

// compressModelsFromPersisted converts the on-disk per-model shape to the
// argument list ccomp.Stats.Restore expects.
func compressModelsFromPersisted(models []localstate.CompressModelStats) []ccomp.ModelSnapshot {
	out := make([]ccomp.ModelSnapshot, 0, len(models))
	for _, m := range models {
		out = append(out, ccomp.ModelSnapshot{
			Name: m.Name, Requests: m.Requests, OriginalTokens: m.OriginalTokens, CompressedTokens: m.CompressedTokens,
		})
	}
	return out
}

// startCompressStatsFlushLoop mirrors startTokenStatsFlushLoop for cs.
func startCompressStatsFlushLoop(ctx context.Context, cs *ccomp.Stats, store *localstate.Store) {
	go func() {
		ticker := time.NewTicker(healthSnapshotInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				flushCompressStats(cs, store)
			case <-ctx.Done():
				flushCompressStats(cs, store)
				return
			}
		}
	}()
}

func flushCompressStats(cs *ccomp.Stats, store *localstate.Store) {
	snap := cs.Snapshot()
	models := make([]localstate.CompressModelStats, 0, len(snap.Models))
	for _, m := range snap.Models {
		models = append(models, localstate.CompressModelStats{
			Name: m.Name, Requests: m.Requests, OriginalTokens: m.OriginalTokens, CompressedTokens: m.CompressedTokens,
		})
	}
	if err := store.SaveCompressStats(localstate.CompressStats{
		TotalRequests: snap.Requests, TotalOriginal: snap.OriginalTokens, TotalCompressed: snap.CompressedTokens, Models: models,
	}); err != nil {
		slog.Warn("compress stats: failed to flush to local_state", "error", err)
	}
}

// buildModelSelector creates the Selector for one model_routes entry.
func buildModelSelector(m config.ModelEntry, namedPools map[string]*selector.CredPool, cfg *config.Config) selector.Selector {
	if m.Routing != nil {
		return buildRoutingSelector(m, namedPools)
	}
	return buildSimpleSelector(m, namedPools, cfg)
}

// buildSimpleSelector handles a simple (single-provider) model_routes entry.
func buildSimpleSelector(m config.ModelEntry, namedPools map[string]*selector.CredPool, cfg *config.Config) selector.Selector {
	proto := resolveProto(m)
	trans := intup.Get(proto, m.UpstreamModel, m.APIBase)
	rawEndpoint, rawStreamEndpoint := intup.PassthroughEndpoints(proto, m.UpstreamModel, m.APIBase)
	rawAdapter := intup.NewPassthrough(rawEndpoint, rawStreamEndpoint, m.UpstreamModel)
	forcePassthrough := m.Mode == "passthrough"

	var pool *selector.CredPool
	if m.CredpoolRef != "" {
		pool = namedPools[m.CredpoolRef]
	} else {
		// Inline credpool — backward-compatible path.
		specs := buildCredSpecsFromPool(m.CredPool, m.AuthStyle, m.ModelName)
		pool = selector.NewCredPool(selector.CredPoolConfig{
			Name:                m.ModelName,
			Keys:                specs,
			Upstream:            trans, // kept for inline pools (prober compat)
			UpstreamModel:       m.UpstreamModel,
			Protocol:            proto,
			PassthroughUpstream: rawAdapter,
			ForcePassthrough:    forcePassthrough,
			Strategy:            m.CredPool.Strategy,
			Threshold:           m.CredPool.CircuitBreakThreshold,
			Cooldown:            time.Duration(m.CredPool.CooldownSeconds) * time.Second,
			RateLimitRPM:        m.CredPool.RateLimitRPM,
			RateSoftLimit:       m.CredPool.RateSoftLimit,
			RateLimitTPM:        m.CredPool.RateLimitTPM,
			Sticky:              m.CredPool.Sticky,
			StickyTTL:           time.Duration(m.CredPool.StickyTTLSeconds) * time.Second,
			RoundRobinBatch:     m.CredPool.RoundRobinBatchSize,
		})
		// Inline pool: translator is embedded in the pool, no TargetSelector needed.
		return pool
	}

	return selector.NewTargetSelector(pool, trans, m.UpstreamModel, proto, rawAdapter, forcePassthrough)
}

// buildRoutingSelector handles a routing (multi-provider) model_routes entry.
func buildRoutingSelector(m config.ModelEntry, namedPools map[string]*selector.CredPool) selector.Selector {
	inner := make([]selector.Selector, 0, len(m.Routing.Targets))
	forcePassthrough := m.Mode == "passthrough"
	for _, t := range m.Routing.Targets {
		pool, ok := namedPools[t.CredpoolRef]
		if !ok {
			slog.Error("routing target references unknown credpool, skipping",
				"model", m.ModelName, "credpool_ref", t.CredpoolRef)
			continue
		}
		trans := intup.Get(t.Protocol, t.UpstreamModel, t.APIBase)
		rawEndpoint, rawStreamEndpoint := intup.PassthroughEndpoints(t.Protocol, t.UpstreamModel, t.APIBase)
		rawAdapter := intup.NewPassthrough(rawEndpoint, rawStreamEndpoint, t.UpstreamModel)
		// Each target's real-vs-passthrough choice is made per request, per
		// attempt, in dispatchFor — comparing the request's actual client
		// protocol against t.Protocol. This is what gives a round-robin
		// routing entry (e.g. gemini/anthropic/openai targets) correct
		// per-target behavior: transform for the two that differ from the
		// client's protocol, raw passthrough for the one that matches.
		inner = append(inner, selector.NewTargetSelector(pool, trans, t.UpstreamModel, t.Protocol, rawAdapter, forcePassthrough))
	}
	return selector.NewRoutingSelector(selector.RoutingSelectorConfig{
		Strategy:  m.Routing.Strategy,
		Selectors: inner,
		Sticky:    m.Routing.Sticky,
		StickyTTL: time.Duration(m.Routing.StickyTTLSeconds) * time.Second,
	})
}

// NewWithTranslators creates a Server with caller-supplied translators (integration tests).
func NewWithTranslators(cfg *config.Config, translators map[string]coreup.UpstreamAdapter) *Server {
	sels := make(map[string]selector.Selector, len(cfg.ModelRoutes))
	timeouts := make(map[string]time.Duration, len(cfg.ModelRoutes))
	for _, m := range cfg.ModelRoutes {
		trans := translators[m.ModelName]
		specs := buildCredSpecsFromPool(m.CredPool, m.AuthStyle, m.ModelName)
		pool := selector.NewCredPool(selector.CredPoolConfig{
			Name:            m.ModelName,
			Keys:            specs,
			Upstream:        trans,
			UpstreamModel:   m.UpstreamModel,
			Strategy:        m.CredPool.Strategy,
			Threshold:       m.CredPool.CircuitBreakThreshold,
			Cooldown:        time.Duration(m.CredPool.CooldownSeconds) * time.Second,
			RateLimitRPM:    m.CredPool.RateLimitRPM,
			RateSoftLimit:   m.CredPool.RateSoftLimit,
			RateLimitTPM:    m.CredPool.RateLimitTPM,
			Sticky:          m.CredPool.Sticky,
			StickyTTL:       time.Duration(m.CredPool.StickyTTLSeconds) * time.Second,
			RoundRobinBatch: m.CredPool.RoundRobinBatchSize,
		})
		sels[m.ModelName] = pool
		timeouts[m.ModelName] = modelTimeout(m)
	}
	s := newBase(cfg, "")
	s.routing.Store(&routingState{selectors: sels, timeouts: timeouts})
	s.router.UpdateConfig(cfg)
	s.router.UpdateRouting(&introuter.RoutingTable{Selectors: sels, Timeouts: timeouts})

	probers := make(map[string]*keyProber, len(cfg.ModelRoutes))
	for _, m := range cfg.ModelRoutes {
		probers[m.ModelName] = newKeyProber(m.ModelName, sels[m.ModelName], s.dispatcher)
	}
	s.pipe = pipeline.New([]pipeline.Plugin{newUpstreamExecutor(probers, s.tokenStats, nil)})
	s.registerRoutes(cfg)
	return s
}

// newBase allocates a Server with dispatcher and mux but no routing or pipeline yet.
func newBase(cfg *config.Config, cfgPath string) *Server {
	s := &Server{
		startTime:    time.Now(),
		tokenStats:   &stats.Registry{},
		mux:          http.NewServeMux(),
		cfgPath:      cfgPath,
		dispatcher:   rpc.NewHTTPDispatcher(&http.Client{}),
		stopProxyCh:  make(chan struct{}, 1),
		startProxyCh: make(chan struct{}, 1),
	}
	s.cfg.Store(cfg)
	s.router = introuter.NewBuiltinRouter(s.dispatcher)
	return s
}

// registerRoutes wires HTTP endpoints.  Each DownstreamAdapter registers its
// own path; adding a new client protocol requires no changes here.
func (s *Server) registerRoutes(cfg *config.Config) {
	authMW := auth.NewValidator(cfg.Auth.AllowedKeys).Middleware

	for _, a := range intdown.DefaultAdapters() {
		a := a // capture
		s.handleWithBareAlias(authMW, "POST", a.Path(), s.makeHandler(a))
	}

	s.handleWithBareAlias(authMW, "GET", "/v1/models", s.handleModels)
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if cfg.Metrics.Enabled {
		path := cfg.Metrics.Path
		if path == "" {
			path = "/metrics"
		}
		s.mux.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprintln(w, "# metrics stub — Prometheus integration coming in metrics phase")
		})
	}
}

// handleWithBareAlias registers fn (wrapped by mw) at path, and — when path
// starts with /v1 — also at the same path with that prefix stripped. Some
// OpenAI-compatible clients/tools construct the URL as base_url+"/chat/
// completions" without including /v1 in base_url; accepting both means a
// client that gets this detail wrong still works instead of a bare 404.
func (s *Server) handleWithBareAlias(mw func(http.Handler) http.Handler, method, path string, fn http.HandlerFunc) {
	handler := mw(fn)
	s.mux.Handle(method+" "+path, handler)
	if bare, ok := strings.CutPrefix(path, "/v1"); ok && bare != "" {
		s.mux.Handle(method+" "+bare, handler)
	}
}

// makeHandler returns an http.HandlerFunc that drives the full request cycle
// for one DownstreamAdapter: decode → model lookup → pipeline → encode.
// This single generic handler replaces the former handleMessages and
// handleChatCompletions methods.
func (s *Server) makeHandler(a coredown.DownstreamAdapter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			a.WriteError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(rawBody))

		req, model, err := a.Decode(r)
		if err != nil {
			slog.Debug("request decode/validate failed", "proto", a.Protocol(), "error", err)
			a.WriteError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		slog.Debug("request decoded", "proto", a.Protocol(),
			"model", model, "stream", req.Stream, "messages", len(req.Messages))
		// Dump raw_request (what the client sent, before pipeline processing).
		if b, err2 := dumpRequest(req); err2 == nil {
			dump.WriteIfEnabled(r.Context(), dump.Record{
				Dir:      dump.DirRawRequest,
				Protocol: a.Protocol(),
				Model:    model,
				Body:     b,
			})
		}

		target, err := s.router.Route(r.Context(), model)
		if err != nil {
			slog.Debug("model not found", "model", model)
			a.WriteError(w, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("unknown model %q — see GET /v1/models for available models", model))
			return
		}

		c := pipeline.NewContext(r.Context(), req, model, *target)
		c.ClientProtocol = a.Protocol()
		c.RawRequestBody = rawBody
		c.RawRequestHeaders = r.Header.Clone()
		c.EncodeRequest = func(req *ir.IRRequest) ([]byte, error) { return a.EncodeRequest(req, c.ClientModel) }

		if err := s.pipe.Run(c); err != nil {
			var pe *pipeline.PipelineError
			if errors.As(err, &pe) {
				slog.Debug("pipeline error", "proto", a.Protocol(),
					"status", pe.Status, "type", pe.ErrType, "msg", pe.Msg)
				if pe.RawBody != nil && target.Invisible {
					ct := pe.ContentType
					if ct == "" {
						ct = "application/json"
					}
					w.Header().Set("Content-Type", ct)
					w.WriteHeader(pe.Status)
					_, _ = w.Write(pe.RawBody)
				} else {
					a.WriteError(w, pe.Status, pe.ErrType, pe.Msg)
				}
			} else {
				slog.Debug("pipeline unexpected error", "error", err)
				a.WriteError(w, http.StatusInternalServerError, "api_error", err.Error())
			}
			return
		}

		// Raw passthrough streaming attempt (client and upstream protocols
		// matched) — relay upstream bytes verbatim, bypassing the canonical
		// IR stream-event channel entirely; the downstream adapter never reframes.
		if body, ct, status, ok := c.RawStream(); ok {
			writeRawStream(w, body, ct, status)
			c.ReleaseUpstream(nil)
			return
		}
		if c.StreamSrc() != nil {
			if err := a.WriteStream(r.Context(), w, c.ClientModel, c.StreamSrc()); err != nil {
				slog.Debug("stream delivery error", "proto", a.Protocol(), "error", err)
			}
			c.ReleaseUpstream(nil)
			return
		}
		msgID := idgen.NewMsgID()
		// Pipeline short-circuited with a sync response (e.g. CommandPlugin) but
		// the client requested streaming — delegate format to the adapter.
		if req.Stream {
			a.WriteResponseAsStream(r.Context(), w, c.Response, msgID, c.ClientModel)
			return
		}
		// Raw passthrough non-streaming attempt — write the verbatim upstream
		// body instead of re-encoding c.Response's other fields.
		if body, ct, status, ok := c.RawResponse(); ok {
			if ct == "" {
				ct = "application/json"
			}
			if status == 0 {
				status = http.StatusOK
			}
			w.Header().Set("Content-Type", ct)
			w.WriteHeader(status)
			_, _ = w.Write(body)
			return
		}
		a.WriteResponse(w, c.Response, msgID, c.ClientModel)
	}
}

// writeRawStream relays a raw passthrough upstream stream directly to the
// client, flushing after every chunk so it behaves like a real SSE stream
// even though nothing here understands its framing.
func writeRawStream(w http.ResponseWriter, body io.ReadCloser, contentType string, status int) {
	defer body.Close()
	if contentType == "" {
		contentType = "text/event-stream"
	}
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// ReloadResult summarises what changed after a config reload.
type ReloadResult struct {
	AddedModels   []string
	RemovedModels []string
	ChangedPools  []string
}

func (r *ReloadResult) String() string {
	if len(r.AddedModels) == 0 && len(r.RemovedModels) == 0 && len(r.ChangedPools) == 0 {
		return "no routing changes"
	}
	parts := []string{}
	if len(r.AddedModels) > 0 {
		parts = append(parts, fmt.Sprintf("+models:%v", r.AddedModels))
	}
	if len(r.RemovedModels) > 0 {
		parts = append(parts, fmt.Sprintf("-models:%v", r.RemovedModels))
	}
	if len(r.ChangedPools) > 0 {
		parts = append(parts, fmt.Sprintf("~pools:%v", r.ChangedPools))
	}
	return strings.Join(parts, " ")
}

// Reload re-reads the config file and atomically swaps routing state.
// Changes to server.port or admin.addr are rejected — those require a restart.
// In-flight requests complete against the old config; new requests use the new one.
func (s *Server) Reload() (*ReloadResult, error) {
	if s.cfgPath == "" {
		return nil, fmt.Errorf("reload not available: no config file path recorded")
	}
	newCfg, err := config.NewYAMLStore(s.cfgPath).Load()
	if err != nil {
		return nil, fmt.Errorf("reload: parse config: %w", err)
	}

	oldCfg := s.cfg.Load()

	// Reject changes that require a full restart.
	if effectivePort(newCfg) != effectivePort(oldCfg) {
		return nil, fmt.Errorf("reload rejected: server.port changed %d→%d (requires restart)",
			effectivePort(oldCfg), effectivePort(newCfg))
	}
	if effectiveAdminAddr(newCfg) != effectiveAdminAddr(oldCfg) {
		return nil, fmt.Errorf("reload rejected: admin.addr changed %s→%s (requires restart)",
			effectiveAdminAddr(oldCfg), effectiveAdminAddr(newCfg))
	}
	if newCfg.LocalState != oldCfg.LocalState || newCfg.Sidecar.CredSource.Enabled != oldCfg.Sidecar.CredSource.Enabled {
		return nil, fmt.Errorf("reload rejected: local_state or sidecar.credsource.enabled changed (requires restart) — " +
			"the local state store is opened once at startup and not rebuilt on reload")
	}

	newState, newCancelPollers := buildRoutingState(newCfg, s.localStateStore)
	oldState := s.routing.Load()
	result := diffRouting(oldState, newState)

	// Atomic swap — in-flight requests hold references to old selectors and
	// complete normally; GC reclaims old pools once all references drop.
	oldCancelPollers := s.credSourceCancel
	s.routing.Store(newState)
	s.cfg.Store(newCfg)
	s.credSourceCancel = newCancelPollers
	s.router.UpdateConfig(newCfg)
	s.router.UpdateRouting(&introuter.RoutingTable{
		Selectors:            newState.selectors,
		Timeouts:             newState.timeouts,
		PassthroughSelectors: newState.passthroughSelectors,
	})

	// Cancel the previous generation's CredSource pollers only after the new
	// state is live, so there's no gap in health-check freshness.
	if oldCancelPollers != nil {
		oldCancelPollers()
	}

	slog.Info("config reloaded", "changes", result.String(),
		"models", len(newCfg.ModelRoutes), "pools", len(newCfg.CredPools))
	return result, nil
}

// InFlightCount returns the number of requests currently being processed.
func (s *Server) InFlightCount() int64 { return s.inFlight.Load() }

// Close stops background goroutines owned by the Server — currently just
// the current generation's CredSource pollers. Call during graceful
// shutdown, alongside the HTTP server's own Shutdown.
func (s *Server) Close() {
	if s.credSourceCancel != nil {
		s.credSourceCancel()
	}
	if s.wardenFlushCancel != nil {
		s.wardenFlushCancel()
	}
	if s.tokenFlushCancel != nil {
		s.tokenFlushCancel()
	}
	if s.compressFlushCancel != nil {
		s.compressFlushCancel()
	}
	// The health-snapshot/warden-stats loops' final flush (triggered by the
	// cancels above) runs in its own goroutine and may not finish before
	// this Close call — acceptable: it's a best-effort cache, same
	// tolerance as every other local_state write.
	if s.localStateStore != nil {
		_ = s.localStateStore.Close()
	}
}

func effectivePort(cfg *config.Config) int {
	if cfg.Server.Port == 0 {
		return 8080
	}
	return cfg.Server.Port
}

func effectiveAdminAddr(cfg *config.Config) string {
	if cfg.Admin.Addr == "" {
		return "127.0.0.1:8090"
	}
	return cfg.Admin.Addr
}

// AdminEnabled returns true when the admin server should start.
// Defaults to true when admin.enabled is not set in config.
func AdminEnabled(cfg *config.Config) bool {
	if cfg.Admin.Enabled == nil {
		return true
	}
	return *cfg.Admin.Enabled
}

func diffRouting(old, new *routingState) *ReloadResult {
	r := &ReloadResult{}
	for name := range new.selectors {
		if _, exists := old.selectors[name]; !exists {
			r.AddedModels = append(r.AddedModels, name)
		}
	}
	for name := range old.selectors {
		if _, exists := new.selectors[name]; !exists {
			r.RemovedModels = append(r.RemovedModels, name)
		}
	}
	for name, nt := range new.timeouts {
		if ot, exists := old.timeouts[name]; exists && ot != nt {
			r.ChangedPools = append(r.ChangedPools, name)
		}
	}
	return r
}

// modelTimeout returns the request timeout for a model_routes entry.
// For routing entries, uses the maximum timeout across all targets.
func modelTimeout(m config.ModelEntry) time.Duration {
	if m.Routing != nil {
		var max int
		for _, t := range m.Routing.Targets {
			if t.TimeoutSeconds > max {
				max = t.TimeoutSeconds
			}
		}
		if max > 0 {
			return time.Duration(max) * time.Second
		}
		return 30 * time.Second
	}
	t := time.Duration(m.TimeoutSeconds) * time.Second
	if t <= 0 {
		t = 30 * time.Second
	}
	return t
}

// Handler returns the http.Handler for use with net/http or httptest.
func (s *Server) Handler() http.Handler { return s.requestLogger(s.mux) }

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (r *responseRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.inFlight.Add(1)
		defer s.inFlight.Add(-1)

		start := time.Now()

		// Inject trace_id into context when dump is enabled.
		ctx := r.Context()
		if s.dumpStore != nil {
			traceID := idgen.NewMsgID()[:8]
			ctx = dump.WithTrace(ctx, traceID, s.dumpStore)
			r = r.WithContext(ctx)
		}

		slog.Debug("request in", "method", r.Method, "path", r.URL.Path,
			"remote", r.RemoteAddr, "in_flight", s.inFlight.Load())
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.Info("request handled",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

// isClaudeCodeClient reports whether the request comes from Claude Code.
// Claude Code identifies itself via User-Agent: claude-code/<version>.
func isClaudeCodeClient(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("User-Agent"), "claude-code/")
}

// handleModels returns the configured model list.
// The response satisfies both Anthropic and OpenAI wire formats so clients
// using CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY can discover models.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	claudeCode := isClaudeCodeClient(r)
	data := make([]types.Model, 0, len(cfg.ModelRoutes))
	for _, m := range cfg.ModelRoutes {
		displayName := m.DisplayName
		if displayName == "" {
			displayName = m.ModelName
		}
		// For Claude Code: auto-prefix model IDs with "claude-" so they appear
		// in the /model picker (Claude Code only shows claude-* models).
		// Other clients receive the original model_name unchanged.
		id := m.ModelName
		if claudeCode && !strings.HasPrefix(id, "claude-") {
			id = "claude-" + id
		}
		data = append(data, types.Model{
			// Anthropic fields
			Type:        "model",
			ID:          id,
			DisplayName: displayName,
			CreatedAt:   "2025-01-01T00:00:00Z",
			// OpenAI compatibility fields (gateway model discovery)
			Object:  "model",
			Created: 1735689600, // 2025-01-01 00:00:00 UTC
			OwnedBy: "miroxy",
		})
	}
	resp := types.ModelsResponse{
		Object:  "list",
		Data:    data,
		HasMore: false,
	}
	if len(data) > 0 {
		resp.FirstID = data[0].ID
		resp.LastID = data[len(data)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

// ReloadText is the ServerRef implementation of Reload — returns a plain string.
func (s *Server) ReloadText() (string, error) {
	r, err := s.Reload()
	if err != nil {
		return "", err
	}
	return r.String(), nil
}

// dumpRequest serialises a MessageRequest to JSON for dump capture.
func dumpRequest(req *ir.IRRequest) ([]byte, error) {
	return json.Marshal(req)
}
