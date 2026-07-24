package server

import (
	"context"
	"net/http"
	"testing"

	"miroxy/core/cred"
	"miroxy/core/ir"
	"miroxy/core/selector"
	coreup "miroxy/core/upstream"
)

// markerAdapter is a minimal UpstreamAdapter test double — dispatchFor tests
// only need to distinguish "which adapter was picked", never actually invoke it.
type markerAdapter struct{ name string }

func (m *markerAdapter) ToUpstream(context.Context, *ir.IRRequest, cred.Credential) (*http.Request, error) {
	return nil, nil
}
func (m *markerAdapter) ToUpstreamStream(context.Context, *ir.IRRequest, cred.Credential) (*http.Request, error) {
	return nil, nil
}
func (m *markerAdapter) FromUpstream(*http.Response) (*ir.IRResponse, error) { return nil, nil }
func (m *markerAdapter) StreamFromUpstream(context.Context, *http.Response, string, string) (<-chan ir.StreamEvent, error) {
	return nil, nil
}

func TestDispatchFor_ProtocolMatch_UsesPassthroughWithRawBody(t *testing.T) {
	real := &markerAdapter{name: "real"}
	raw := &markerAdapter{name: "raw"}
	plan := &selector.ExecutionPlan{Upstream: real, PassthroughUpstream: raw, Protocol: "openai"}

	ctx, got, mode := dispatchFor(context.Background(), plan, "openai", []byte(`{"model":"gpt"}`), nil)

	if got != raw {
		t.Fatalf("got adapter %v, want the passthrough adapter", got)
	}
	if mode != DispatchRaw {
		t.Fatalf("got mode %v, want DispatchRaw", mode)
	}
	body, ok := coreup.RawBodyFromContext(ctx)
	if !ok || string(body) != `{"model":"gpt"}` {
		t.Fatalf("raw body not attached to ctx: ok=%v body=%q", ok, body)
	}
}

func TestDispatchFor_ProtocolMatch_AttachesRawHeaders(t *testing.T) {
	real := &markerAdapter{name: "real"}
	raw := &markerAdapter{name: "raw"}
	plan := &selector.ExecutionPlan{Upstream: real, PassthroughUpstream: raw, Protocol: "anthropic"}
	headers := http.Header{"Anthropic-Version": []string{"2023-06-01"}}

	ctx, _, _ := dispatchFor(context.Background(), plan, "anthropic", []byte(`{}`), headers)

	got, ok := coreup.RawHeadersFromContext(ctx)
	if !ok || got.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatalf("raw headers not attached to ctx: ok=%v headers=%v", ok, got)
	}
}

func TestDispatchFor_ProtocolMismatch_UsesRealAdapter(t *testing.T) {
	real := &markerAdapter{name: "real"}
	raw := &markerAdapter{name: "raw"}
	plan := &selector.ExecutionPlan{Upstream: real, PassthroughUpstream: raw, Protocol: "gemini"}

	ctx, got, mode := dispatchFor(context.Background(), plan, "openai", []byte(`{"model":"gpt"}`), nil)

	if got != real {
		t.Fatalf("got adapter %v, want the real transform adapter", got)
	}
	if mode != DispatchIR {
		t.Fatalf("got mode %v, want DispatchIR", mode)
	}
	if _, ok := coreup.RawBodyFromContext(ctx); ok {
		t.Fatal("raw body should not be attached to ctx when protocols mismatch")
	}
}

func TestDispatchFor_ForcePassthrough_IgnoresProtocolMismatch(t *testing.T) {
	real := &markerAdapter{name: "real"}
	raw := &markerAdapter{name: "raw"}
	plan := &selector.ExecutionPlan{Upstream: real, PassthroughUpstream: raw, Protocol: "gemini", ForcePassthrough: true}

	_, got, mode := dispatchFor(context.Background(), plan, "openai", []byte(`{}`), nil)

	if got != raw {
		t.Fatalf("got adapter %v, want passthrough forced regardless of protocol", got)
	}
	if mode != DispatchRaw {
		t.Fatalf("got mode %v, want DispatchRaw", mode)
	}
}

func TestDispatchFor_NoPassthroughAdapter_FallsBackToReal(t *testing.T) {
	real := &markerAdapter{name: "real"}
	plan := &selector.ExecutionPlan{Upstream: real, Protocol: "openai"}

	_, got, mode := dispatchFor(context.Background(), plan, "openai", []byte(`{}`), nil)

	if got != real {
		t.Fatalf("got adapter %v, want real adapter when PassthroughUpstream is nil", got)
	}
	if mode != DispatchIR {
		t.Fatalf("got mode %v, want DispatchIR", mode)
	}
}

func TestDispatchFor_EmptyClientProtocol_NeverMatches(t *testing.T) {
	real := &markerAdapter{name: "real"}
	raw := &markerAdapter{name: "raw"}
	plan := &selector.ExecutionPlan{Upstream: real, PassthroughUpstream: raw, Protocol: ""}

	_, got, mode := dispatchFor(context.Background(), plan, "", []byte(`{}`), nil)

	if got != real {
		t.Fatal("empty client protocol must never match an empty target protocol into passthrough")
	}
	if mode != DispatchIR {
		t.Fatalf("got mode %v, want DispatchIR", mode)
	}
}

// TestDispatchFor_HeterogeneousFallbackChain_PerAttemptModeDiffers guards the
// architectural claim that a single model_routes fallback chain spanning
// providers on different protocols dispatches each retry attempt on its own
// terms — not on a decision cached once for the whole route. Attempt 1 lands
// on a target whose protocol matches the client's (passthrough); attempt 2,
// having fallen through to a different provider, must independently resolve
// to IR translation even though both attempts share the same clientProtocol
// and the same retry loop.
func TestDispatchFor_HeterogeneousFallbackChain_PerAttemptModeDiffers(t *testing.T) {
	clientProtocol := "anthropic"

	attempt1Plan := &selector.ExecutionPlan{
		Upstream:            &markerAdapter{name: "anthropic-real"},
		PassthroughUpstream: &markerAdapter{name: "anthropic-raw"},
		Protocol:            "anthropic", // matches client — this attempt's target is Anthropic-compatible
	}
	attempt2Plan := &selector.ExecutionPlan{
		Upstream: &markerAdapter{name: "gemini-real"},
		Protocol: "gemini", // fallback target is a different provider/protocol entirely
	}

	_, adapter1, mode1 := dispatchFor(context.Background(), attempt1Plan, clientProtocol, []byte(`{}`), nil)
	if mode1 != DispatchRaw || adapter1 != attempt1Plan.PassthroughUpstream {
		t.Fatalf("attempt 1: got adapter=%v mode=%v, want passthrough/DispatchRaw", adapter1, mode1)
	}

	_, adapter2, mode2 := dispatchFor(context.Background(), attempt2Plan, clientProtocol, []byte(`{}`), nil)
	if mode2 != DispatchIR || adapter2 != attempt2Plan.Upstream {
		t.Fatalf("attempt 2: got adapter=%v mode=%v, want real/DispatchIR", adapter2, mode2)
	}
}
