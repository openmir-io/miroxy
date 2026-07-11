package selector

import (
	"context"
	"errors"
	"sort"
	"sync/atomic"

	"miroxy/internal/types"
)

// routingTarget is one upstream candidate within a RoutingSelector.
type routingTarget struct {
	sel      Selector
	inFlight atomic.Int64
}

// RoutingSelector distributes requests across multiple Selectors according to
// a strategy. Implements Selector so it can be used anywhere a Selector is expected.
//
// Strategy semantics:
//
//	fallback      — try targets in listed order; advance to next only on ErrNoSelection.
//	              Single-key 429 is handled inside each target's CredPool; it does NOT
//	              trigger a cross-target advance.
//	round_robin   — rotate across all targets. ErrNoSelection on one target skips it.
//	least_requests — send to the target with the fewest active in-flight requests.
type RoutingSelector struct {
	strategy string
	targets  []*routingTarget
	rrIdx    atomic.Uint64
}

// NewRoutingSelector creates a RoutingSelector with the given strategy and selectors.
// strategy defaults to "fallback" when empty.
func NewRoutingSelector(strategy string, selectors []Selector) *RoutingSelector {
	if strategy == "" {
		strategy = "fallback"
	}
	targets := make([]*routingTarget, len(selectors))
	for i, s := range selectors {
		targets[i] = &routingTarget{sel: s}
	}
	return &RoutingSelector{strategy: strategy, targets: targets}
}

func (r *RoutingSelector) Select(ctx context.Context, req *types.MessageRequest) (*ExecutionPlan, error) {
	switch r.strategy {
	case "round_robin":
		return r.selectRoundRobin(ctx, req)
	case "least_requests":
		return r.selectLeastRequests(ctx, req)
	default:
		return r.selectFallback(ctx, req)
	}
}

// Release calls plan.ReleaseHook, which RoutingSelector.Select sets to the
// Release method of whichever inner selector provided the plan.
func (r *RoutingSelector) Release(plan *ExecutionPlan, err error) {
	if plan.ReleaseHook != nil {
		plan.ReleaseHook(plan, err)
	}
}

func (r *RoutingSelector) selectFallback(ctx context.Context, req *types.MessageRequest) (*ExecutionPlan, error) {
	for _, t := range r.targets {
		plan, err := t.sel.Select(ctx, req)
		if errors.Is(err, ErrNoSelection) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return r.wrapPlan(plan, t), nil
	}
	return nil, ErrNoSelection
}

func (r *RoutingSelector) selectRoundRobin(ctx context.Context, req *types.MessageRequest) (*ExecutionPlan, error) {
	n := uint64(len(r.targets))
	startIdx := r.rrIdx.Add(1) % n
	for i := uint64(0); i < n; i++ {
		t := r.targets[(startIdx+i)%n]
		plan, err := t.sel.Select(ctx, req)
		if errors.Is(err, ErrNoSelection) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return r.wrapPlan(plan, t), nil
	}
	return nil, ErrNoSelection
}

func (r *RoutingSelector) selectLeastRequests(ctx context.Context, req *types.MessageRequest) (*ExecutionPlan, error) {
	// Sort by current inFlight ascending, try in that order.
	order := make([]int, len(r.targets))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		return r.targets[order[i]].inFlight.Load() < r.targets[order[j]].inFlight.Load()
	})
	for _, idx := range order {
		t := r.targets[idx]
		plan, err := t.sel.Select(ctx, req)
		if errors.Is(err, ErrNoSelection) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return r.wrapPlan(plan, t), nil
	}
	return nil, ErrNoSelection
}

// wrapPlan increments the target's inFlight counter and sets plan.ReleaseHook
// to decrement it and forward to the inner selector's Release.
func (r *RoutingSelector) wrapPlan(plan *ExecutionPlan, t *routingTarget) *ExecutionPlan {
	t.inFlight.Add(1)
	plan.ReleaseHook = func(p *ExecutionPlan, err error) {
		t.inFlight.Add(-1)
		t.sel.Release(p, err)
	}
	return plan
}

// TakeRateLimited implements probeCapable for the primary (first) target.
// Only the first target is probed because routing-level fallback handles
// the case where a provider is temporarily unreachable.
func (r *RoutingSelector) TakeRateLimited(ctx context.Context) []*ExecutionPlan {
	if len(r.targets) == 0 {
		return nil
	}
	type probeCapable interface {
		TakeRateLimited(ctx context.Context) []*ExecutionPlan
	}
	if pc, ok := r.targets[0].sel.(probeCapable); ok {
		return pc.TakeRateLimited(ctx)
	}
	return nil
}
