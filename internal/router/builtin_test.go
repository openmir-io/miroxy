package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"miroxy/core/ir"
	"miroxy/core/selector"
	"miroxy/internal/config"
)

// fakeSelector is a minimal selector.Selector test double — BuiltinRouter
// never calls its methods, it only needs to round-trip the same instance.
type fakeSelector struct{ name string }

func (f *fakeSelector) Select(context.Context, *ir.IRRequest, string) (*selector.ExecutionPlan, error) {
	return nil, nil
}
func (f *fakeSelector) Release(*selector.ExecutionPlan, error) {}

func newTestRouter(t *testing.T) *BuiltinRouter {
	t.Helper()
	r := NewBuiltinRouter(nil)
	r.UpdateConfig(&config.Config{
		ModelRoutes: []config.ModelEntry{
			{ModelName: "claude-sonnet", ProviderRef: "anthropic", UpstreamModel: "claude-sonnet-real"},
		},
	})
	r.UpdateRouting(&RoutingTable{
		Selectors: map[string]selector.Selector{"claude-sonnet": &fakeSelector{name: "sonnet"}},
		Timeouts:  map[string]time.Duration{"claude-sonnet": 30 * time.Second},
		PassthroughSelectors: map[string]selector.Selector{
			"gemini": &fakeSelector{name: "gemini-passthrough"},
		},
	})
	return r
}

func TestBuiltinRouter_Route_ExactMatch(t *testing.T) {
	r := newTestRouter(t)

	target, err := r.Route(context.Background(), "claude-sonnet")
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if target.Model.Name != "claude-sonnet" || target.Model.UpstreamModel != "claude-sonnet-real" || target.Model.Provider != "anthropic" {
		t.Errorf("target.Model = %+v, want claude-sonnet/claude-sonnet-real/anthropic", target.Model)
	}
	if target.Timeout != 30*time.Second {
		t.Errorf("target.Timeout = %v, want 30s", target.Timeout)
	}
	if fs, ok := target.Selector.(*fakeSelector); !ok || fs.name != "sonnet" {
		t.Errorf("target.Selector = %+v, want the sonnet fakeSelector", target.Selector)
	}
}

func TestBuiltinRouter_Route_UnknownModel(t *testing.T) {
	r := newTestRouter(t)

	_, err := r.Route(context.Background(), "does-not-exist")
	if !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("err = %v, want ErrModelNotFound", err)
	}
}

func TestBuiltinRouter_Route_PassthroughFallback(t *testing.T) {
	r := newTestRouter(t)

	// This model_routes entry has a ProviderRef tag but no matching entry in
	// RoutingTable.Selectors (only "claude-sonnet" does) — exercises the
	// fallback-to-PassthroughSelectors branch in Route, the same branch
	// LookupModel's own provider-inference step (config.go step 4) would
	// also land in for a model name matching no model_routes entry at all.
	r.UpdateConfig(&config.Config{
		ModelRoutes: []config.ModelEntry{
			{ModelName: "gemini-1.5-flash", ProviderRef: "gemini"},
		},
	})

	target, err := r.Route(context.Background(), "gemini-1.5-flash")
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	fs, ok := target.Selector.(*fakeSelector)
	if !ok || fs.name != "gemini-passthrough" {
		t.Errorf("target.Selector = %+v, want the gemini passthrough fakeSelector", target.Selector)
	}
	// No upstream_model configured -> the original requested model name is
	// forwarded upstream as-is.
	if target.Model.UpstreamModel != "gemini-1.5-flash" {
		t.Errorf("target.Model.UpstreamModel = %q, want the original requested model name", target.Model.UpstreamModel)
	}
}

func TestBuiltinRouter_UpdateRouting_IsAtomicAndLiveForNewRequests(t *testing.T) {
	r := newTestRouter(t)

	r.UpdateRouting(&RoutingTable{
		Selectors: map[string]selector.Selector{"claude-sonnet": &fakeSelector{name: "replaced"}},
		Timeouts:  map[string]time.Duration{"claude-sonnet": 5 * time.Second},
	})

	target, err := r.Route(context.Background(), "claude-sonnet")
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	fs, ok := target.Selector.(*fakeSelector)
	if !ok || fs.name != "replaced" {
		t.Errorf("target.Selector = %+v, want the replaced fakeSelector — UpdateRouting should take effect immediately", target.Selector)
	}
}
