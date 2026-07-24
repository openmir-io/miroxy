package selector

import (
	"context"
	"time"

	"miroxy/core/ir"
	"miroxy/core/upstream"
)

// TargetSelector wraps a CredPool with a specific Translator and UpstreamModel.
// Multiple model_routes entries can share the same CredPool via separate
// TargetSelectors — credential rotation, circuit-break, and 429 accounting
// are shared; the translator and model name are per-entry.
type TargetSelector struct {
	pool          *CredPool
	upstream      upstream.UpstreamAdapter
	upstreamModel string

	// protocol/passthroughUpstream/forcePassthrough are embedded in each
	// ExecutionPlan so UpstreamExecutor can pick real transform vs raw
	// passthrough per attempt — see ExecutionPlan's doc comment.
	protocol            string
	passthroughUpstream upstream.UpstreamAdapter
	forcePassthrough    bool
}

// NewTargetSelector creates a TargetSelector around an existing CredPool.
// protocol is this target's static upstream wire protocol; passthroughUpstream
// is the raw-forwarding adapter to use when a request's actual client
// protocol matches it (or forcePassthrough is set unconditionally).
func NewTargetSelector(pool *CredPool, trans upstream.UpstreamAdapter, upstreamModel string, protocol string, passthroughUpstream upstream.UpstreamAdapter, forcePassthrough bool) *TargetSelector {
	return &TargetSelector{
		pool:                pool,
		upstream:            trans,
		upstreamModel:       upstreamModel,
		protocol:            protocol,
		passthroughUpstream: passthroughUpstream,
		forcePassthrough:    forcePassthrough,
	}
}

func (t *TargetSelector) Select(ctx context.Context, req *ir.IRRequest, model string) (*ExecutionPlan, error) {
	plan, err := t.pool.Select(ctx, req, model)
	if err != nil {
		return nil, err
	}
	plan.Model = t.upstreamModel
	plan.Upstream = t.upstream
	plan.Protocol = t.protocol
	plan.PassthroughUpstream = t.passthroughUpstream
	plan.ForcePassthrough = t.forcePassthrough
	return plan, nil
}

func (t *TargetSelector) Release(plan *ExecutionPlan, err error) {
	t.pool.Release(plan, err)
}

// TakeRateLimited implements probeCapable so prober.go can probe rate-limited
// keys. Delegates to the inner pool and fills in per-target plan fields.
func (t *TargetSelector) TakeRateLimited(ctx context.Context) []*ExecutionPlan {
	plans := t.pool.TakeRateLimited(ctx)
	for _, p := range plans {
		p.Upstream = t.upstream
		p.Model = t.upstreamModel
		p.Protocol = t.protocol
		p.PassthroughUpstream = t.passthroughUpstream
		p.ForcePassthrough = t.forcePassthrough
	}
	return plans
}

// EarliestCooldown delegates to the inner pool.
func (t *TargetSelector) EarliestCooldown() (time.Time, bool) {
	return t.pool.EarliestCooldown()
}
