package selector

import (
	"context"
	"time"

	"miroxy/core/upstream"
	"miroxy/internal/types"
)

// TargetSelector wraps a CredPool with a specific Translator and ProviderModel.
// Multiple model_routes entries can share the same CredPool via separate
// TargetSelectors — credential rotation, circuit-break, and 429 accounting
// are shared; the translator and model name are per-entry.
type TargetSelector struct {
	pool          *CredPool
	upstream     upstream.UpstreamAdapter
	providerModel string
}

// NewTargetSelector creates a TargetSelector around an existing CredPool.
func NewTargetSelector(pool *CredPool, trans upstream.UpstreamAdapter, providerModel string) *TargetSelector {
	return &TargetSelector{
		pool:          pool,
		upstream:      trans,
		providerModel: providerModel,
	}
}

func (t *TargetSelector) Select(ctx context.Context, req *types.MessageRequest) (*ExecutionPlan, error) {
	plan, err := t.pool.Select(ctx, req)
	if err != nil {
		return nil, err
	}
	plan.Model = t.providerModel
	plan.Upstream = t.upstream
	return plan, nil
}

func (t *TargetSelector) Release(plan *ExecutionPlan, err error) {
	t.pool.Release(plan, err)
}

// TakeRateLimited implements probeCapable so prober.go can probe rate-limited
// keys. Delegates to the inner pool and fills Translator on each plan.
func (t *TargetSelector) TakeRateLimited(ctx context.Context) []*ExecutionPlan {
	plans := t.pool.TakeRateLimited(ctx)
	for _, p := range plans {
		p.Upstream = t.upstream
		p.Model = t.providerModel
	}
	return plans
}

// EarliestCooldown delegates to the inner pool.
func (t *TargetSelector) EarliestCooldown() (time.Time, bool) {
	return t.pool.EarliestCooldown()
}
