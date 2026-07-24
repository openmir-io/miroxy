package selector

import (
	"context"
	"testing"
	"time"

	"miroxy/core/ir"
)

// stubSelector is a fake Selector for RoutingSelector tests: always returns
// a plan tagged with id, or ErrNoSelection when unavailable is set. protocol
// is optional — set it to simulate a target whose upstream wire protocol
// differs from other targets in the same fallback chain.
type stubSelector struct {
	id          string
	protocol    string
	unavailable bool
	selects     int
}

func (s *stubSelector) Select(ctx context.Context, req *ir.IRRequest, model string) (*ExecutionPlan, error) {
	s.selects++
	if s.unavailable {
		return nil, ErrNoSelection
	}
	return &ExecutionPlan{SelectionID: s.id, Protocol: s.protocol}, nil
}

func (s *stubSelector) Release(plan *ExecutionPlan, err error) {}

func TestRoutingSticky_ReusesSameTargetAcrossCalls(t *testing.T) {
	a := &stubSelector{id: "provider-a"}
	b := &stubSelector{id: "provider-b"}
	rs := NewRoutingSelector(RoutingSelectorConfig{
		Strategy:  "round_robin",
		Selectors: []Selector{a, b},
		Sticky:    true,
		StickyTTL: time.Minute,
	})
	req := msgReq("hello there")
	ctx := context.Background()

	first, err := rs.Select(ctx, req, "m")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		plan, err := rs.Select(ctx, req, "m")
		if err != nil {
			t.Fatalf("Select[%d]: %v", i, err)
		}
		if plan.SelectionID != first.SelectionID {
			t.Errorf("call %d: expected sticky reuse of %s, got %s", i, first.SelectionID, plan.SelectionID)
		}
	}
}

func TestRoutingSticky_RebindsWhenTargetUnavailable(t *testing.T) {
	a := &stubSelector{id: "provider-a"}
	b := &stubSelector{id: "provider-b"}
	rs := NewRoutingSelector(RoutingSelectorConfig{
		Strategy:  "round_robin",
		Selectors: []Selector{a, b},
		Sticky:    true,
		StickyTTL: time.Minute,
	})
	req := msgReq("hello there")
	ctx := context.Background()

	first, err := rs.Select(ctx, req, "m")
	if err != nil {
		t.Fatal(err)
	}
	if first.SelectionID == "provider-a" {
		a.unavailable = true
	} else {
		b.unavailable = true
	}

	next, err := rs.Select(ctx, req, "m")
	if err != nil {
		t.Fatal(err)
	}
	if next.SelectionID == first.SelectionID {
		t.Error("expected rebind to the other target once the sticky one became unavailable")
	}
}

func TestRoutingSticky_FallbackStrategyIgnoresSticky(t *testing.T) {
	a := &stubSelector{id: "provider-a"}
	b := &stubSelector{id: "provider-b"}
	rs := NewRoutingSelector(RoutingSelectorConfig{
		Strategy:  "fallback",
		Selectors: []Selector{a, b},
		Sticky:    true,
		StickyTTL: time.Minute,
	})
	req := msgReq("hello there")
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		plan, err := rs.Select(ctx, req, "m")
		if err != nil {
			t.Fatal(err)
		}
		if plan.SelectionID != "provider-a" {
			t.Errorf("call %d: fallback should always try targets in the same fixed order, got %s", i, plan.SelectionID)
		}
	}
}
