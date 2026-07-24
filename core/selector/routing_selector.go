package selector

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"miroxy/core/ir"
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

	affinity *affinityMap // nil unless Sticky was set; no-op for "fallback"
}

// RoutingSelectorConfig configures a RoutingSelector.
type RoutingSelectorConfig struct {
	Strategy  string // fallback | round_robin | least_requests; default fallback
	Selectors []Selector

	// Sticky keeps a conversation on the same target across calls. No
	// effect on "fallback", which is already fixed-order every time.
	Sticky    bool
	StickyTTL time.Duration // idle expiry for a sticky binding; default 30m
}

// NewRoutingSelector creates a RoutingSelector from cfg.
func NewRoutingSelector(cfg RoutingSelectorConfig) *RoutingSelector {
	strategy := cfg.Strategy
	if strategy == "" {
		strategy = "fallback"
	}
	targets := make([]*routingTarget, len(cfg.Selectors))
	for i, s := range cfg.Selectors {
		targets[i] = &routingTarget{sel: s}
	}
	r := &RoutingSelector{strategy: strategy, targets: targets}
	if cfg.Sticky {
		ttl := cfg.StickyTTL
		if ttl <= 0 {
			ttl = 30 * time.Minute
		}
		r.affinity = newAffinityMap(ttl)
	}
	return r
}

func (r *RoutingSelector) Select(ctx context.Context, req *ir.IRRequest, model string) (*ExecutionPlan, error) {
	switch r.strategy {
	case "round_robin":
		return r.selectRoundRobin(ctx, req, model)
	case "least_requests":
		return r.selectLeastRequests(ctx, req, model)
	default:
		return r.selectFallback(ctx, req, model)
	}
}

// Release calls plan.ReleaseHook, which RoutingSelector.Select sets to the
// Release method of whichever inner selector provided the plan.
func (r *RoutingSelector) Release(plan *ExecutionPlan, err error) {
	if plan.ReleaseHook != nil {
		plan.ReleaseHook(plan, err)
	}
}

func (r *RoutingSelector) selectFallback(ctx context.Context, req *ir.IRRequest, model string) (*ExecutionPlan, error) {
	for _, t := range r.targets {
		plan, err := t.sel.Select(ctx, req, model)
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

func (r *RoutingSelector) selectRoundRobin(ctx context.Context, req *ir.IRRequest, model string) (*ExecutionPlan, error) {
	key := r.sessionKey(req, model)
	if key != "" {
		if plan, ok := r.trySticky(ctx, req, model, key); ok {
			return plan, nil
		}
	}

	n := uint64(len(r.targets))
	startIdx := r.rrIdx.Add(1) % n
	for i := uint64(0); i < n; i++ {
		idx := (startIdx + i) % n
		t := r.targets[idx]
		plan, err := t.sel.Select(ctx, req, model)
		if errors.Is(err, ErrNoSelection) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if key != "" {
			r.affinity.Set(key, strconv.Itoa(int(idx)))
		}
		return r.wrapPlan(plan, t), nil
	}
	return nil, ErrNoSelection
}

func (r *RoutingSelector) selectLeastRequests(ctx context.Context, req *ir.IRRequest, model string) (*ExecutionPlan, error) {
	key := r.sessionKey(req, model)
	if key != "" {
		if plan, ok := r.trySticky(ctx, req, model, key); ok {
			return plan, nil
		}
	}

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
		plan, err := t.sel.Select(ctx, req, model)
		if errors.Is(err, ErrNoSelection) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if key != "" {
			r.affinity.Set(key, strconv.Itoa(idx))
		}
		return r.wrapPlan(plan, t), nil
	}
	return nil, ErrNoSelection
}

// sessionKey returns "" when sticky is disabled, so callers can use it
// directly as the "skip stickiness" sentinel.
func (r *RoutingSelector) sessionKey(req *ir.IRRequest, model string) string {
	if r.affinity == nil {
		return ""
	}
	return SessionKeyFromRequest(req, model)
}

// trySticky attempts the target bound to key. ok is false on no binding, an
// out-of-range index, or nothing available — callers fall through on false.
func (r *RoutingSelector) trySticky(ctx context.Context, req *ir.IRRequest, model, key string) (*ExecutionPlan, bool) {
	idxStr, ok := r.affinity.Get(key)
	if !ok {
		return nil, false
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || idx >= len(r.targets) {
		return nil, false
	}
	t := r.targets[idx]
	plan, err := t.sel.Select(ctx, req, model)
	if err != nil {
		return nil, false
	}
	return r.wrapPlan(plan, t), true
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
