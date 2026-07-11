package cred

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corecred "miroxy/core/cred"
	"miroxy/core/selector"
)

// CredSource is a selector.CredentialSource backed by a credstone pool.
//
// Each Credential() call issues CredstoneClient.Acquire to obtain a live
// credential. The call is skipped (fast-fail) when the last poll found zero
// healthy entries, so the surrounding CredPool immediately tries the next
// local key without spending a round-trip on credstone.
//
// CredSource also implements the optional outcomeReporter interface that
// core/selector.CredPool.Release checks via type assertion — this is how
// lease outcomes (429 / 5xx / error / success) get reported back to
// credstone on the normal Selector.Release path, without any change to the
// CredentialSource interface itself.
type CredSource struct {
	client *CredstoneClient
	poolID string
	usage  *UsageAccumulator // optional; nil unless SetUsageAccumulator is called

	mu          sync.RWMutex
	lastHealthy int
}

var _ selector.CredentialSource = (*CredSource)(nil)

// NewCredSource creates a CredSource for the given credstone pool.
// lastHealthy starts at 0 (fast-fail) until the first StartPoller poll
// succeeds.
func NewCredSource(client *CredstoneClient, poolID string) *CredSource {
	return &CredSource{client: client, poolID: poolID}
}

// SetUsageAccumulator attaches an optional rpd/tpd usage accumulator. When
// set, every ReportOutcome call also records a request delta for this pool's
// next flush to credstone.
func (cs *CredSource) SetUsageAccumulator(ua *UsageAccumulator) {
	cs.usage = ua
}

// Credential implements selector.CredentialSource.
func (cs *CredSource) Credential(ctx context.Context) (corecred.Credential, error) {
	if !cs.IsHealthy() {
		return nil, fmt.Errorf("credstone pool %q: no healthy entries (last poll)", cs.poolID)
	}

	resp, err := cs.client.Acquire(ctx, cs.poolID)
	if err != nil {
		slog.Warn("credstone Acquire failed", "pool", cs.poolID, "error", err)
		return nil, fmt.Errorf("credstone pool %q: %w", cs.poolID, err)
	}

	inner, err := credentialFromAcquire(resp)
	if err != nil {
		slog.Warn("credstone Acquire: unusable credential kind", "pool", cs.poolID, "kind", resp.Kind)
		return nil, err
	}

	return &leasedCredential{Credential: inner, leaseID: resp.LeaseID}, nil
}

// ReportOutcome maps a Selector.Release outcome to credstone's Release call.
// c is the exact credential value this CredSource returned from the
// matching Credential() call; its lease ID is extracted via type assertion.
// Never propagates an error — logs WARNING only, matching how the local
// CredPool already treats source failures.
func (cs *CredSource) ReportOutcome(c corecred.Credential, err error) {
	lc, ok := c.(*leasedCredential)
	if !ok {
		return // not a credential this CredSource produced
	}

	if cs.usage != nil {
		cs.usage.AddRequest()
	}

	// CredstoneClient.Release handles an empty leaseID gracefully (DEBUG log,
	// no HTTP call) — see the known-gap note there.
	req := releaseRequest{LeaseID: lc.leaseID}
	var rlErr *selector.RateLimitError
	var soErr *selector.ServerOverloadError
	switch {
	case errors.As(err, &rlErr):
		req.RateLimited = true
		if rlErr.RetryAfter > 0 {
			req.RetryAfterSeconds = int(rlErr.RetryAfter.Seconds())
		}
	case errors.As(err, &soErr):
		req.ServerOverload = true
	case err != nil:
		req.CallError = true
	}

	// Fire-and-forget: this runs inline inside CredPool.Release, on the
	// executor's retry-loop goroutine. A slow or unreachable credstone must
	// never add latency before the next Select() attempt, so the actual HTTP
	// call happens in its own goroutine rather than blocking here.
	go func() {
		if relErr := cs.client.Release(context.Background(), req); relErr != nil {
			slog.Warn("credstone Release failed", "lease_id", lc.leaseID, "error", relErr)
		}
	}()
}

// IsHealthy reports whether the most recent poll found any healthy entries.
// Used to fast-fail Credential() without a network round-trip.
func (cs *CredSource) IsHealthy() bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.lastHealthy > 0
}

// StartPoller polls credstone's PoolStatus every interval, keeping IsHealthy
// current. Call once; the goroutine runs until ctx is cancelled. The first
// poll fires immediately (before the first wait) so the source has an
// accurate health state before serving any traffic.
func (cs *CredSource) StartPoller(ctx context.Context, interval time.Duration) {
	go cs.pollLoop(ctx, interval)
}

func (cs *CredSource) pollLoop(ctx context.Context, interval time.Duration) {
	cs.poll(ctx)
	t := time.NewTimer(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cs.poll(ctx)
			t.Reset(interval)
		}
	}
}

func (cs *CredSource) poll(ctx context.Context) {
	resp, err := cs.client.PoolStatus(ctx, cs.poolID)
	if err != nil {
		// Preserve the previous healthy count on transient errors — a brief
		// credstone blip should not immediately cut off credstone-sourced
		// traffic.
		slog.Warn("credstone PoolStatus poll failed", "pool", cs.poolID, "error", err)
		return
	}

	for _, p := range resp.Pools {
		if p.PoolID != cs.poolID {
			continue
		}
		cs.mu.Lock()
		cs.lastHealthy = p.Healthy
		cs.mu.Unlock()
		slog.Debug("credstone pool status",
			"pool", cs.poolID, "healthy", p.Healthy,
			"rate_limited", p.RateLimited, "cooling_down", p.CoolingDown, "in_flight", p.InFlight)
		return
	}
	// Pool absent from the response — not registered in credstone (or
	// credstone returned no pools at all). Treat as zero healthy so this
	// entry parks locally and the credpool's other (local) entries carry the
	// load; this is the fallback path, not an error.
	cs.mu.Lock()
	cs.lastHealthy = 0
	cs.mu.Unlock()
}

// credentialFromAcquire maps a credstone acquireResponse to a miroxy
// Credential, based on its transport kind.
func credentialFromAcquire(resp *acquireResponse) (corecred.Credential, error) {
	switch resp.Kind {
	case "CREDENTIAL_KIND_HEADER":
		header := resp.HeaderName
		if header == "" {
			header = "Authorization"
		}
		value := resp.Value
		if header == "Authorization" {
			value = "Bearer " + resp.Value
		}
		return &corecred.HeaderCredential{Header: header, Value: value}, nil
	case "CREDENTIAL_KIND_QUERY":
		param := resp.ParamName
		if param == "" {
			param = "key"
		}
		return &corecred.QueryCredential{Param: param, Value: resp.Value}, nil
	default:
		return nil, fmt.Errorf("credstone: unsupported credential kind %q", resp.Kind)
	}
}

// leasedCredential wraps a Credential with the lease ID needed to report its
// outcome back to credstone via CredSource.ReportOutcome.
type leasedCredential struct {
	corecred.Credential
	leaseID string
}
