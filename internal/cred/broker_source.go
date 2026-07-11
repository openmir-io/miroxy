package cred

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	corecred "miroxy/core/cred"
	"miroxy/core/selector"
)

// BrokerSource is a selector.CredentialSource backed by a CredBroker pool.
//
// Each Credential() call issues CredBroker.Acquire to obtain a live credential.
// The call is skipped (fast-fail) when the last BrokerPoller poll found zero
// healthy entries, so miroxy's CredPool immediately tries the next local key
// without spending a round-trip to the broker.
//
// Lease release: the lease ID returned by Acquire is not currently forwarded to
// CredBroker.Release. Leases auto-expire after their TTL on the broker side.
// TODO: extend ExecutionPlan with a post-request hook so the executor can call
// Release(leaseID, rateLimited, serverOverload, callError) after each upstream
// call. This propagates 429/5xx outcomes to the broker in real time and enables
// multi-instance coordination without waiting for the TTL sweep.
type BrokerSource struct {
	broker  corecred.CredBroker
	poolID  string
	healthy atomic.Int32 // written by BrokerPoller; 0 = skip Acquire (zero-value safe)
}

var _ selector.CredentialSource = (*BrokerSource)(nil)

// NewBrokerSource creates a BrokerSource. healthy starts at 0 (fast-fail) until
// the first BrokerPoller.poll() succeeds.
func NewBrokerSource(broker corecred.CredBroker, poolID string) *BrokerSource {
	return &BrokerSource{broker: broker, poolID: poolID}
}

func (s *BrokerSource) Credential(ctx context.Context) (corecred.Credential, error) {
	if s.healthy.Load() == 0 {
		return nil, fmt.Errorf("cred broker pool %q: no healthy entries (last poll)", s.poolID)
	}
	_, c, err := s.broker.Acquire(ctx, s.poolID, "miroxy")
	if err != nil {
		return nil, fmt.Errorf("cred broker pool %q acquire: %w", s.poolID, err)
	}
	return c, nil
}

// BrokerPoller keeps a BrokerSource's healthy count current by calling
// CredBroker.HealthyCount on a fixed interval.
//
// Call Start once; the goroutine runs until ctx is cancelled.
// The first poll fires immediately inside Start so the source is ready before
// the server begins handling traffic.
type BrokerPoller struct {
	src      *BrokerSource
	broker   corecred.CredBroker
	poolID   string
	interval time.Duration
}

func NewBrokerPoller(src *BrokerSource, broker corecred.CredBroker, poolID string, interval time.Duration) *BrokerPoller {
	return &BrokerPoller{src: src, broker: broker, poolID: poolID, interval: interval}
}

func (p *BrokerPoller) Start(ctx context.Context) {
	go p.run(ctx)
}

func (p *BrokerPoller) run(ctx context.Context) {
	p.poll(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.poll(ctx)
		}
	}
}

func (p *BrokerPoller) poll(ctx context.Context) {
	n, err := p.broker.HealthyCount(ctx, p.poolID)
	if err != nil {
		// Preserve previous count on transient errors — a brief broker blip should
		// not immediately cut off all broker-sourced traffic.
		slog.Warn("cred broker poll failed", "pool", p.poolID, "err", err)
		return
	}
	old := p.src.healthy.Swap(int32(n))
	if old != int32(n) {
		slog.Info("cred broker pool health changed", "pool", p.poolID, "healthy", n)
	} else {
		slog.Debug("cred broker poll", "pool", p.poolID, "healthy", n)
	}
}
