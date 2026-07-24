package selector

import (
	"context"
	"errors"
	"testing"
)

// TestRoutingSelector_Fallback_HeterogeneousProtocols guards the claim that a
// single model_routes fallback chain may span providers on different
// protocols, and each Select() call surfaces whichever target actually
// answered — not a protocol fixed once for the whole route. dispatchFor
// (internal/server/upstream.go) relies on this: it reads plan.Protocol fresh
// per retry attempt, so attempt 1 landing on an Anthropic-compatible target
// and attempt 2 falling through to a Gemini target must each report their
// own target's protocol, independently.
func TestRoutingSelector_Fallback_HeterogeneousProtocols(t *testing.T) {
	anthropicTarget := &stubSelector{id: "bedrock-anthropic", protocol: "anthropic"}
	geminiTarget := &stubSelector{id: "gemini-pool", protocol: "gemini"}
	rs := NewRoutingSelector(RoutingSelectorConfig{
		Strategy:  "fallback",
		Selectors: []Selector{anthropicTarget, geminiTarget},
	})
	req := msgReq("hello")
	ctx := context.Background()

	// Both targets healthy: fallback always tries the first target first.
	plan, err := rs.Select(ctx, req, "m")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if plan.Protocol != "anthropic" {
		t.Fatalf("attempt 1: got protocol %q, want %q", plan.Protocol, "anthropic")
	}

	// First target exhausted (simulates all its credentials cooling down/
	// circuit-broken) — fallback must advance to the second target, whose
	// protocol differs, and report ITS protocol, not the first target's.
	anthropicTarget.unavailable = true
	plan, err = rs.Select(ctx, req, "m")
	if err != nil {
		t.Fatalf("Select after first target exhausted: %v", err)
	}
	if plan.Protocol != "gemini" {
		t.Fatalf("attempt 2: got protocol %q, want %q", plan.Protocol, "gemini")
	}

	// Both exhausted: ErrNoSelection propagates, no stale plan reused.
	geminiTarget.unavailable = true
	if _, err := rs.Select(ctx, req, "m"); !errors.Is(err, ErrNoSelection) {
		t.Fatalf("expected ErrNoSelection when all targets exhausted, got %v", err)
	}
}
