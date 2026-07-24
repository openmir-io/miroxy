package unit_test

import (
	"context"
	"testing"
	"time"

	"miroxy/core/ir"
	"miroxy/core/selector"
)

func newStickyPool(ttl time.Duration, keys ...string) *selector.CredPool {
	return selector.NewCredPool(selector.CredPoolConfig{
		Keys:      toSpecs(keys...),
		Strategy:  "round_robin",
		Threshold: 3,
		Cooldown:  5 * time.Second,
		Sticky:    true,
		StickyTTL: ttl,
	})
}

func conversation(text string) *ir.IRRequest {
	return &ir.IRRequest{
		Messages: []ir.IRMessage{{Role: "user", Parts: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: text}}}}},
	}
}

const testModel = "test-model"

func TestSticky_ReusesSameCredentialAcrossCalls(t *testing.T) {
	pool := newStickyPool(time.Minute, "key-a", "key-b", "key-c")
	req := conversation("hello there")
	ctx := context.Background()

	first, err := pool.Select(ctx, req, testModel)
	if err != nil {
		t.Fatal(err)
	}
	pool.Release(first, nil)

	for i := 0; i < 5; i++ {
		plan, err := pool.Select(ctx, req, testModel)
		if err != nil {
			t.Fatalf("Select[%d]: %v", i, err)
		}
		if plan.SelectionID != first.SelectionID {
			t.Errorf("call %d: expected sticky reuse of %s, got %s", i, first.SelectionID, plan.SelectionID)
		}
		pool.Release(plan, nil)
	}
}

func TestSticky_DifferentConversationsSpreadAcrossKeys(t *testing.T) {
	pool := newStickyPool(time.Minute, "key-a", "key-b", "key-c")
	ctx := context.Background()

	seen := map[string]int{}
	for i := 0; i < 3; i++ {
		plan, err := pool.Select(ctx, conversation("conversation number"+string(rune('A'+i))), testModel)
		if err != nil {
			t.Fatal(err)
		}
		seen[plan.SelectionID]++
		pool.Release(plan, nil)
	}
	if len(seen) != 3 {
		t.Errorf("expected 3 distinct conversations to spread across 3 keys, got %d: %v", len(seen), seen)
	}
}

func TestSticky_RebindsWhenBoundCredentialIsRateLimited(t *testing.T) {
	pool := newStickyPool(time.Minute, "key-a", "key-b")
	req := conversation("hello there")
	ctx := context.Background()

	first, err := pool.Select(ctx, req, testModel)
	if err != nil {
		t.Fatal(err)
	}
	pool.Release(first, &selector.RateLimitError{RetryAfter: time.Hour})

	next, err := pool.Select(ctx, req, testModel)
	if err != nil {
		t.Fatal(err)
	}
	if next.SelectionID == first.SelectionID {
		t.Error("expected rebind to the other key once the sticky credential was rate-limited")
	}
	pool.Release(next, nil)

	again, err := pool.Select(ctx, req, testModel)
	if err != nil {
		t.Fatal(err)
	}
	if again.SelectionID != next.SelectionID {
		t.Errorf("expected the new binding to stick too, got %s want %s", again.SelectionID, next.SelectionID)
	}
	pool.Release(again, nil)
}

func TestSticky_NoOpWhenRequestHasNoMessages(t *testing.T) {
	pool := newStickyPool(time.Minute, "key-a", "key-b", "key-c")
	ctx := context.Background()

	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		plan, err := pool.Select(ctx, nil, testModel)
		if err != nil {
			t.Fatalf("Select[%d]: %v", i, err)
		}
		seen[plan.SelectionID]++
		pool.Release(plan, nil)
	}
	if len(seen) != 3 {
		t.Errorf("expected plain round-robin (3 keys) when there's nothing to fingerprint, got %d: %v", len(seen), seen)
	}
}

func TestSticky_DisabledByDefault(t *testing.T) {
	pool := newPool("round_robin", "key-a", "key-b", "key-c")
	req := conversation("hello there")
	ctx := context.Background()

	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		plan, err := pool.Select(ctx, req, testModel)
		if err != nil {
			t.Fatalf("Select[%d]: %v", i, err)
		}
		seen[plan.SelectionID]++
		pool.Release(plan, nil)
	}
	if len(seen) != 3 {
		t.Errorf("sticky:false should behave exactly like today's plain round-robin, got %d distinct keys: %v", len(seen), seen)
	}
}
