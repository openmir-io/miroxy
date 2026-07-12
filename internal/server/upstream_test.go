package server

import (
	"context"
	"net/http"
	"testing"

	"miroxy/core/cred"
	"miroxy/core/selector"
	coreup "miroxy/core/upstream"
	"miroxy/internal/types"
)

// markerAdapter is a minimal UpstreamAdapter test double — dispatchFor tests
// only need to distinguish "which adapter was picked", never actually invoke it.
type markerAdapter struct{ name string }

func (m *markerAdapter) ToUpstream(context.Context, *types.MessageRequest, cred.Credential) (*http.Request, error) {
	return nil, nil
}
func (m *markerAdapter) ToUpstreamStream(context.Context, *types.MessageRequest, cred.Credential) (*http.Request, error) {
	return nil, nil
}
func (m *markerAdapter) FromUpstream(*http.Response) (*types.MessageResponse, error) { return nil, nil }
func (m *markerAdapter) StreamFromUpstream(context.Context, *http.Response, string, string) (<-chan types.SSEEvent, error) {
	return nil, nil
}

func TestDispatchFor_ProtocolMatch_UsesPassthroughWithRawBody(t *testing.T) {
	real := &markerAdapter{name: "real"}
	raw := &markerAdapter{name: "raw"}
	plan := &selector.ExecutionPlan{Upstream: real, PassthroughUpstream: raw, Protocol: "openai"}

	ctx, got := dispatchFor(context.Background(), plan, "openai", []byte(`{"model":"gpt"}`))

	if got != raw {
		t.Fatalf("got adapter %v, want the passthrough adapter", got)
	}
	body, ok := coreup.RawBodyFromContext(ctx)
	if !ok || string(body) != `{"model":"gpt"}` {
		t.Fatalf("raw body not attached to ctx: ok=%v body=%q", ok, body)
	}
}

func TestDispatchFor_ProtocolMismatch_UsesRealAdapter(t *testing.T) {
	real := &markerAdapter{name: "real"}
	raw := &markerAdapter{name: "raw"}
	plan := &selector.ExecutionPlan{Upstream: real, PassthroughUpstream: raw, Protocol: "gemini"}

	ctx, got := dispatchFor(context.Background(), plan, "openai", []byte(`{"model":"gpt"}`))

	if got != real {
		t.Fatalf("got adapter %v, want the real transform adapter", got)
	}
	if _, ok := coreup.RawBodyFromContext(ctx); ok {
		t.Fatal("raw body should not be attached to ctx when protocols mismatch")
	}
}

func TestDispatchFor_ForcePassthrough_IgnoresProtocolMismatch(t *testing.T) {
	real := &markerAdapter{name: "real"}
	raw := &markerAdapter{name: "raw"}
	plan := &selector.ExecutionPlan{Upstream: real, PassthroughUpstream: raw, Protocol: "gemini", ForcePassthrough: true}

	_, got := dispatchFor(context.Background(), plan, "openai", []byte(`{}`))

	if got != raw {
		t.Fatalf("got adapter %v, want passthrough forced regardless of protocol", got)
	}
}

func TestDispatchFor_NoPassthroughAdapter_FallsBackToReal(t *testing.T) {
	real := &markerAdapter{name: "real"}
	plan := &selector.ExecutionPlan{Upstream: real, Protocol: "openai"}

	_, got := dispatchFor(context.Background(), plan, "openai", []byte(`{}`))

	if got != real {
		t.Fatalf("got adapter %v, want real adapter when PassthroughUpstream is nil", got)
	}
}

func TestDispatchFor_EmptyClientProtocol_NeverMatches(t *testing.T) {
	real := &markerAdapter{name: "real"}
	raw := &markerAdapter{name: "raw"}
	plan := &selector.ExecutionPlan{Upstream: real, PassthroughUpstream: raw, Protocol: ""}

	_, got := dispatchFor(context.Background(), plan, "", []byte(`{}`))

	if got != real {
		t.Fatal("empty client protocol must never match an empty target protocol into passthrough")
	}
}
