package selector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"miroxy/core/cred"
	"miroxy/core/upstream"
	"miroxy/internal/types"
)

type credState int

const (
	stateHealthy     credState = iota
	stateCoolingDown           // circuit-broken: too many 5xx/network failures
	stateRateLimited           // cooling from a 429; recovers automatically
)

const defaultRateWindow = time.Minute

// defaultRateLimitTiers is the escalating per-key cooldown schedule for 429 responses.
// Index 0 is used on the first failure; the last element caps all subsequent failures.
var defaultRateLimitTiers = []time.Duration{
	10 * time.Second,
	30 * time.Second,
	60 * time.Second,
	120 * time.Second,
	300 * time.Second,
}

type credEntry struct {
	id     string
	source CredentialSource

	state             credState
	inFlight          int64
	failures          int
	rateLimitFailures int
	coolEnd           time.Time

	recentRequests []time.Time
	recentTokens   []tokenSample
}

// tokenSample is one post-response token observation, used by the TPM
// sliding window. Unlike recentRequests (one timestamp per attempt, known at
// Select time), token counts are only known after the upstream response
// arrives, so they're recorded separately via CredPool.RecordTokens.
type tokenSample struct {
	at     time.Time
	tokens int64
}

func (e *credEntry) available(now time.Time) bool {
	if e.state == stateCoolingDown || e.state == stateRateLimited {
		return now.After(e.coolEnd)
	}
	return true
}

// String renders a credState for serialization (see CredHealthSnapshot).
func (s credState) String() string {
	switch s {
	case stateCoolingDown:
		return "cooling_down"
	case stateRateLimited:
		return "rate_limited"
	default:
		return "healthy"
	}
}

func parseCredState(s string) credState {
	switch s {
	case "cooling_down":
		return stateCoolingDown
	case "rate_limited":
		return stateRateLimited
	default:
		return stateHealthy
	}
}

// CredSpec is a named upstream credential entry passed to NewCredPool.
type CredSpec struct {
	Name   string           // display name used in logs and error messages
	Source CredentialSource // supplies the live credential value on demand
}

// CredPoolConfig configures a CredPool.
type CredPoolConfig struct {
	Keys          []CredSpec
	Upstream      upstream.UpstreamAdapter // embedded in each ExecutionPlan
	ProviderModel string                   // embedded in each ExecutionPlan
	Strategy      string                   // "round_robin" | "least_requests" (default: round_robin)
	Threshold     int                      // consecutive failures before circuit-break (default: 5)
	Cooldown      time.Duration            // circuit-break cooldown (default: 60s)

	// Proactive rate limiting — sliding window per credential.
	RateLimitRPM  int           // per-credential requests-per-minute cap; 0 = disabled
	RateSoftLimit int           // rotate at this count before hitting the cap
	RateWindow    time.Duration // window duration; leave zero for 1-minute default

	// RateLimitTPM caps total (input+output) tokens per credential within
	// RateWindow; 0 = disabled. Unlike RPM, this has no soft-limit rotation —
	// token counts are only known after the response, so there's nothing to
	// reserve at Select time (see CredPool.RecordTokens).
	RateLimitTPM int

	// RateLimitTiers overrides the escalating 429 cooldown schedule.
	// Leave nil to use the built-in schedule (10s → 30s → 60s → 120s → 300s).
	RateLimitTiers []time.Duration
}

// CredPool holds a set of credentials (API keys or OAuth tokens), selects healthy
// ones by strategy, and manages 429 cooldown and circuit-break. Implements Selector.
type CredPool struct {
	mu    sync.Mutex
	creds []*credEntry

	strategy  string
	threshold int
	cooldown  time.Duration
	counter   uint64

	upstream      upstream.UpstreamAdapter
	providerModel string

	rateLimit     int
	rateSoftLimit int
	rateWindow    time.Duration
	tpmLimit      int

	rateLimitTiers []time.Duration
}

func NewCredPool(cfg CredPoolConfig) *CredPool {
	entries := make([]*credEntry, len(cfg.Keys))
	for i, k := range cfg.Keys {
		name := k.Name
		if name == "" {
			name = fmt.Sprintf("key_%d", i)
		}
		entries[i] = &credEntry{id: name, source: k.Source}
	}

	strategy := cfg.Strategy
	if strategy == "" {
		strategy = "round_robin"
	}
	threshold := cfg.Threshold
	if threshold <= 0 {
		threshold = 5
	}
	cooldown := cfg.Cooldown
	if cooldown <= 0 {
		cooldown = 60 * time.Second
	}
	rateWindow := cfg.RateWindow
	if rateWindow <= 0 {
		rateWindow = defaultRateWindow
	}
	softLimit := cfg.RateSoftLimit
	if cfg.RateLimitRPM > 0 && softLimit <= 0 {
		softLimit = cfg.RateLimitRPM - 2
		if softLimit < 1 {
			softLimit = 1
		}
	}
	tiers := cfg.RateLimitTiers
	if len(tiers) == 0 {
		tiers = defaultRateLimitTiers
	}

	return &CredPool{
		creds:          entries,
		strategy:       strategy,
		threshold:      threshold,
		cooldown:       cooldown,
		upstream:       cfg.Upstream,
		providerModel:  cfg.ProviderModel,
		rateLimit:      cfg.RateLimitRPM,
		rateSoftLimit:  softLimit,
		rateWindow:     rateWindow,
		tpmLimit:       cfg.RateLimitTPM,
		rateLimitTiers: tiers,
	}
}

func (p *CredPool) Select(ctx context.Context, _ *types.MessageRequest) (*ExecutionPlan, error) {
	p.mu.Lock()

	now := time.Now()

	// Recover credentials whose cooldown has expired.
	// rateLimitFailures is intentionally NOT reset here; it only resets on a
	// successful request so the escalating schedule persists across cooldowns.
	for _, e := range p.creds {
		if (e.state == stateCoolingDown || e.state == stateRateLimited) && now.After(e.coolEnd) {
			e.state = stateHealthy
			e.failures = 0
			slog.Info("credential recovered from cooldown", "key_id", e.id)
		}
	}

	if p.rateLimit > 0 {
		for _, e := range p.creds {
			p.pruneWindow(e, now)
		}
	}
	if p.tpmLimit > 0 {
		for _, e := range p.creds {
			p.pruneTokenWindow(e, now)
		}
	}

	// First pass: healthy, under the soft RPM limit, and under the TPM cap.
	// Cheapest check first — available() is a state/time comparison, the RPM
	// length compare is next, the TPM sum-over-window is the priciest and
	// only runs when the cheaper checks already passed (&& short-circuits).
	selected := p.selectByCriteria(func(e *credEntry) bool {
		return e.available(now) &&
			(p.rateLimit == 0 || len(e.recentRequests) < p.rateSoftLimit) &&
			(p.tpmLimit == 0 || p.tokensInWindow(e) < int64(p.tpmLimit))
	})

	// Second pass: all over the soft RPM limit and/or TPM cap — use best
	// available as reactive backstop, ignoring both proactive limits.
	if selected == nil && (p.rateLimit > 0 || p.tpmLimit > 0) {
		selected = p.selectByCriteria(func(e *credEntry) bool {
			return e.available(now)
		})
		if selected != nil {
			slog.Warn("all credentials at soft rate/token limit, using best available",
				"key_id", selected.id,
				"requests_in_window", len(selected.recentRequests),
				"tokens_in_window", p.tokensInWindow(selected),
				"soft_limit", p.rateSoftLimit,
				"rate_limit", p.rateLimit,
				"tpm_limit", p.tpmLimit,
			)
		}
	}

	if selected == nil {
		p.mu.Unlock()
		return nil, ErrNoSelection
	}

	selected.inFlight++
	if p.rateLimit > 0 {
		selected.recentRequests = append(selected.recentRequests, now)
	}
	slog.Debug("credential acquired",
		"key_id", selected.id,
		"strategy", p.strategy,
		"in_flight", selected.inFlight,
		"requests_in_window", len(selected.recentRequests),
	)

	p.mu.Unlock() // release before potential IO in source.Credential

	credential, err := selected.source.Credential(ctx)
	if err != nil {
		p.mu.Lock()
		selected.inFlight--
		selected.failures++
		if selected.failures >= p.threshold {
			selected.state = stateCoolingDown
			selected.coolEnd = time.Now().Add(p.cooldown)
			slog.Warn("credential circuit-broken (source error)",
				"key_id", selected.id, "failures", selected.failures,
				"cooldown_until", selected.coolEnd.Format(time.RFC3339))
		}
		p.mu.Unlock()
		return nil, fmt.Errorf("credential %s: %w", selected.id, err)
	}

	return &ExecutionPlan{
		SelectionID: selected.id,
		Credential:  credential,
		Model:       p.providerModel,
		Upstream:    p.upstream,
	}, nil
}

func (p *CredPool) Release(plan *ExecutionPlan, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var e *credEntry
	for _, c := range p.creds {
		if c.id == plan.SelectionID {
			e = c
			break
		}
	}
	if e == nil {
		return
	}

	if e.inFlight > 0 {
		e.inFlight--
	}

	var rlErr *RateLimitError
	var soErr *ServerOverloadError
	switch {
	case errors.As(err, &rlErr):
		// 429: don't touch the circuit-break failure counter.
		cooldown := p.rateLimitBackoff(e.rateLimitFailures)
		if rlErr.RetryAfter > 0 {
			cooldown = rlErr.RetryAfter
		}
		e.rateLimitFailures++
		if e.state == stateHealthy {
			e.state = stateRateLimited
			e.coolEnd = time.Now().Add(cooldown)
			slog.Warn("credential rate-limited (429), backing off",
				"key_id", e.id,
				"rl_failures", e.rateLimitFailures,
				"cooldown", cooldown,
				"cooldown_until", e.coolEnd.Format(time.RFC3339),
			)
		}

	case errors.As(err, &soErr):
		// Transient 5xx (e.g. 503 "model overloaded"): park the key briefly so
		// the next Select() routes to a different key. Does NOT touch
		// rateLimitFailures — the 429 escalation schedule stays clean.
		cooldown := soErr.RetryAfter
		if cooldown <= 0 {
			cooldown = 5 * time.Second
		}
		if e.state == stateHealthy {
			e.state = stateRateLimited
			e.coolEnd = time.Now().Add(cooldown)
			slog.Debug("credential parked (server overload)",
				"key_id", e.id,
				"cooldown", cooldown,
				"cooldown_until", e.coolEnd.Format(time.RFC3339),
			)
		}

	case err != nil:
		e.failures++
		slog.Debug("credential failure recorded",
			"key_id", e.id, "failures", e.failures, "threshold", p.threshold, "error", err)
		if e.failures >= p.threshold {
			e.state = stateCoolingDown
			e.coolEnd = time.Now().Add(p.cooldown)
			slog.Warn("credential circuit-broken",
				"key_id", e.id, "failures", e.failures, "cooldown_until", e.coolEnd.Format(time.RFC3339))
		}

	default:
		// Success: reset both counters so the next failure restarts from tier 0.
		e.failures = 0
		e.rateLimitFailures = 0
	}

	// Optional hook: sources that need to report the outcome upstream (e.g.
	// a credstone-backed CredSource returning a lease) implement this
	// interface. Does not change the CredentialSource contract itself —
	// most sources (static keys, OAuth) don't implement it and are
	// unaffected.
	if r, ok := e.source.(outcomeReporter); ok {
		r.ReportOutcome(plan.Credential, err)
	}
}

// outcomeReporter is an optional extension to CredentialSource. A source
// that implements it receives the outcome of each Select/Release cycle for
// credentials it produced — used by credstone-backed sources to return
// leases with outcome flags.
type outcomeReporter interface {
	ReportOutcome(c cred.Credential, err error)
}

func (p *CredPool) rateLimitBackoff(failures int) time.Duration {
	idx := failures
	if idx >= len(p.rateLimitTiers) {
		idx = len(p.rateLimitTiers) - 1
	}
	return p.rateLimitTiers[idx]
}

// TakeRateLimited temporarily lifts the rate-limit cooldown on every rate-limited
// credential and returns them as ExecutionPlans ready for a probe request.
// The caller MUST Release each plan: pass nil on success, &RateLimitError{} on failure.
// Credentials whose source returns an error are silently skipped (kept rate-limited).
// Returns nil when no credentials are rate-limited.
func (p *CredPool) TakeRateLimited(ctx context.Context) []*ExecutionPlan {
	type candidate struct {
		entry *credEntry
		src   CredentialSource
	}

	p.mu.Lock()
	var candidates []candidate
	for _, e := range p.creds {
		if e.state == stateRateLimited {
			e.state = stateHealthy
			e.inFlight++
			candidates = append(candidates, candidate{entry: e, src: e.source})
		}
	}
	p.mu.Unlock()

	var plans []*ExecutionPlan
	for _, c := range candidates {
		cred, err := c.src.Credential(ctx)
		if err != nil {
			slog.Warn("probe: credential unavailable, skipping", "key_id", c.entry.id, "error", err)
			p.mu.Lock()
			c.entry.state = stateRateLimited
			c.entry.inFlight--
			p.mu.Unlock()
			continue
		}
		plans = append(plans, &ExecutionPlan{
			SelectionID: c.entry.id,
			Credential:  cred,
			Model:       p.providerModel,
			Upstream:    p.upstream,
		})
	}
	return plans
}

// EarliestCooldown returns the soonest time a rate-limited credential will recover,
// and whether any such credential exists.
func (p *CredPool) EarliestCooldown() (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	var earliest time.Time
	found := false
	for _, e := range p.creds {
		if e.state == stateRateLimited && e.coolEnd.After(now) {
			if !found || e.coolEnd.Before(earliest) {
				earliest = e.coolEnd
				found = true
			}
		}
	}
	return earliest, found
}

func (p *CredPool) pruneWindow(e *credEntry, now time.Time) {
	cutoff := now.Add(-p.rateWindow)
	i := 0
	for i < len(e.recentRequests) && e.recentRequests[i].Before(cutoff) {
		i++
	}
	e.recentRequests = e.recentRequests[i:]
}

// pruneTokenWindow mirrors pruneWindow for the TPM sliding window.
func (p *CredPool) pruneTokenWindow(e *credEntry, now time.Time) {
	cutoff := now.Add(-p.rateWindow)
	i := 0
	for i < len(e.recentTokens) && e.recentTokens[i].at.Before(cutoff) {
		i++
	}
	e.recentTokens = e.recentTokens[i:]
}

// tokensInWindow sums token samples still inside the window. Caller must
// already hold p.mu (called from Select, which does).
func (p *CredPool) tokensInWindow(e *credEntry) int64 {
	var sum int64
	for _, s := range e.recentTokens {
		sum += s.tokens
	}
	return sum
}

// RecordTokens appends a post-response token-usage sample for selectionID,
// feeding the TPM sliding window checked on the next Select(). Deliberately
// not part of the Selector interface — token counts are only known after the
// upstream response, well after Release has already run, so callers
// type-assert for this optional capability (same pattern as probeCapable /
// outcomeReporter) instead of it being a Selector method every implementation
// must carry.
// CredHealthSnapshot is a serializable snapshot of one credential's health
// state, used only for optional local persistence (internal/localstate) —
// never read on the request-serving hot path. core/selector stays IO-free;
// the actual disk read/write happens in internal/server + internal/localstate.
type CredHealthSnapshot struct {
	State             string // credState.String() — "healthy" | "cooling_down" | "rate_limited"
	CoolEnd           time.Time
	RateLimitFailures int
	Failures          int
}

// HealthSnapshot returns a snapshot of every credential's current health
// state, keyed by SelectionID (credEntry.id).
func (p *CredPool) HealthSnapshot() map[string]CredHealthSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]CredHealthSnapshot, len(p.creds))
	for _, e := range p.creds {
		out[e.id] = CredHealthSnapshot{
			State:             e.state.String(),
			CoolEnd:           e.coolEnd,
			RateLimitFailures: e.rateLimitFailures,
			Failures:          e.failures,
		}
	}
	return out
}

// RestoreHealth seeds each credential's health state from a prior snapshot.
// Call once, right after NewCredPool, before serving traffic. An entry whose
// CoolEnd has already passed is restored as-is — available(now) already
// treats a past coolEnd as healthy, so no special-casing is needed here.
// Entries not present in snap (e.g. keys added to the pool since the
// snapshot was taken) are left at their zero-value (healthy) state.
func (p *CredPool) RestoreHealth(snap map[string]CredHealthSnapshot) {
	if len(snap) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.creds {
		h, ok := snap[e.id]
		if !ok {
			continue
		}
		e.state = parseCredState(h.State)
		e.coolEnd = h.CoolEnd
		e.rateLimitFailures = h.RateLimitFailures
		e.failures = h.Failures
	}
}

func (p *CredPool) RecordTokens(selectionID string, tokens int64) {
	if tokens <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tpmLimit == 0 {
		return
	}
	for _, e := range p.creds {
		if e.id == selectionID {
			e.recentTokens = append(e.recentTokens, tokenSample{at: time.Now(), tokens: tokens})
			return
		}
	}
}

func (p *CredPool) selectByCriteria(eligible func(*credEntry) bool) *credEntry {
	switch p.strategy {
	case "least_requests":
		return p.leastRequestsFiltered(eligible)
	default:
		return p.roundRobinFiltered(eligible)
	}
}

func (p *CredPool) roundRobinFiltered(eligible func(*credEntry) bool) *credEntry {
	n := uint64(len(p.creds))
	for range p.creds {
		p.counter++
		e := p.creds[p.counter%n]
		if eligible(e) {
			return e
		}
	}
	return nil
}

func (p *CredPool) leastRequestsFiltered(eligible func(*credEntry) bool) *credEntry {
	var best *credEntry
	for _, e := range p.creds {
		if !eligible(e) {
			continue
		}
		if best == nil || e.inFlight < best.inFlight {
			best = e
		}
	}
	return best
}
