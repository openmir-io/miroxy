package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"miroxy/core/cred"
	"miroxy/core/ir"
	coreup "miroxy/core/upstream"
)

// TestPassthroughEndpoints_OpenAIFamilyAppendsChatCompletions guards a real
// bug: PassthroughAdapter used to be built with the bare api_base (no
// suffix), so a passthrough-eligible attempt against an OpenAI-protocol
// target (e.g. Groq: base_url https://api.groq.com/openai/v1) posted
// straight to the base URL and got "Unknown request URL" back — the real
// transform adapter appends /chat/completions itself, but nothing told
// PassthroughAdapter to do the same.
func TestPassthroughEndpoints_OpenAIFamilyAppendsChatCompletions(t *testing.T) {
	for _, proto := range []string{"openai", "deepseek", "grok", "glm"} {
		ep, streamEp := PassthroughEndpoints(proto, "some-model", "https://api.groq.com/openai/v1")
		want := "https://api.groq.com/openai/v1/chat/completions"
		if ep != want {
			t.Errorf("%s: endpoint = %q, want %q", proto, ep, want)
		}
		if streamEp != want {
			t.Errorf("%s: streamEndpoint = %q, want %q", proto, streamEp, want)
		}
	}
}

func TestPassthroughEndpoints_OpenAIFamily_TrimsTrailingSlash(t *testing.T) {
	ep, _ := PassthroughEndpoints("openai", "m", "https://api.groq.com/openai/v1/")
	if want := "https://api.groq.com/openai/v1/chat/completions"; ep != want {
		t.Errorf("endpoint = %q, want %q", ep, want)
	}
}

// TestPassthroughEndpoints_Anthropic_AppendsMessagesPath guards that anthropic
// passthrough bakes in /v1/messages from a bare host, matching every other protocol.
func TestPassthroughEndpoints_Anthropic_AppendsMessagesPath(t *testing.T) {
	ep, streamEp := PassthroughEndpoints("anthropic", "claude-x", "https://api.anthropic.com")
	want := "https://api.anthropic.com/v1/messages"
	if ep != want || streamEp != want {
		t.Errorf("endpoint/streamEndpoint = %q/%q, want %q/%q", ep, streamEp, want, want)
	}
}

// TestPassthroughEndpoints_Gemini_BakesModelIntoPath guards that gemini
// passthrough gets the model-specific path + distinct streaming suffix the
// real GeminiAdapter builds, not a bare api_base.
func TestPassthroughEndpoints_Gemini_BakesModelIntoPath(t *testing.T) {
	ep, streamEp := PassthroughEndpoints("gemini", "gemini-2.5-flash", "https://generativelanguage.googleapis.com")
	wantEp := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent"
	wantStream := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse"
	if ep != wantEp {
		t.Errorf("endpoint = %q, want %q", ep, wantEp)
	}
	if streamEp != wantStream {
		t.Errorf("streamEndpoint = %q, want %q", streamEp, wantStream)
	}
}

func TestPassthroughEndpoints_Gemini_DefaultsBaseWhenEmpty(t *testing.T) {
	ep, _ := PassthroughEndpoints("", "gemini-2.5-flash", "")
	want := defaultGeminiBase + "/v1beta/models/gemini-2.5-flash:generateContent"
	if ep != want {
		t.Errorf("endpoint = %q, want %q", ep, want)
	}
}

func TestRewriteModelField_ReplacesModelPreservesOtherFields(t *testing.T) {
	in := []byte(`{"model":"groq-test","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	out := rewriteModelField(in, "openai/gpt-oss-120b")

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if got["model"] != "openai/gpt-oss-120b" {
		t.Errorf("model = %v, want %q", got["model"], "openai/gpt-oss-120b")
	}
	if got["stream"] != true {
		t.Errorf("stream field lost: %v", got)
	}
	if _, ok := got["messages"]; !ok {
		t.Errorf("messages field lost: %v", got)
	}
}

func TestRewriteModelField_InvalidJSON_ReturnsOriginalUnchanged(t *testing.T) {
	in := []byte(`not json`)
	out := rewriteModelField(in, "whatever")
	if string(out) != string(in) {
		t.Errorf("expected untouched fallback, got %q", out)
	}
}

// TestPassthroughAdapter_ToUpstream_RewritesModelField guards the real bug:
// a client's request carries miroxy's own routing alias in "model" (e.g.
// "groq-test"), which a real upstream like Groq rejects outright
// ("model_not_found") — passthrough must still rewrite it to the
// configured upstream_model even though everything else forwards verbatim.
func TestPassthroughAdapter_ToUpstream_RewritesModelField(t *testing.T) {
	adapter := NewPassthrough("https://api.groq.com/openai/v1/chat/completions", "", "openai/gpt-oss-120b")
	ctx := coreup.WithRawBody(context.Background(), []byte(`{"model":"groq-test","messages":[{"role":"user","content":"hi"}]}`))

	httpReq, err := adapter.ToUpstream(ctx, &ir.IRRequest{}, &cred.HeaderCredential{Header: "Authorization", Value: "Bearer x"})
	if err != nil {
		t.Fatalf("ToUpstream: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if got["model"] != "openai/gpt-oss-120b" {
		t.Errorf("outgoing model = %v, want %q (the configured upstream_model, not the client's alias)", got["model"], "openai/gpt-oss-120b")
	}
}

// TestPassthroughAdapter_ToUpstream_ModelAlreadyMatches_LeavesBodyUntouched
// guards true byte-for-byte forwarding when the client's model field already
// equals upstream_model (e.g. model_name == upstream_model, or the native
// vendor passthrough case) — rewriteModelField's JSON round-trip would
// otherwise reorder keys for no reason even though nothing needs to change.
func TestPassthroughAdapter_ToUpstream_ModelAlreadyMatches_LeavesBodyUntouched(t *testing.T) {
	adapter := NewPassthrough("https://api.anthropic.com/v1/messages", "", "claude-opus-4-8")
	original := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`)
	ctx := coreup.WithRawBody(context.Background(), original)

	httpReq, err := adapter.ToUpstream(ctx, &ir.IRRequest{}, &cred.HeaderCredential{Header: "Authorization", Value: "Bearer x"})
	if err != nil {
		t.Fatalf("ToUpstream: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != string(original) {
		t.Errorf("body changed even though model already matched upstream_model: got %s, want %s", body, original)
	}
}

// TestPassthroughAdapter_ToUpstream_ForwardsClientHeaders guards that
// protocol-specific headers the body can't carry (e.g. Anthropic's required
// anthropic-version) survive passthrough, while auth/framing headers this
// adapter owns are not leaked from the client's original request.
func TestPassthroughAdapter_ToUpstream_ForwardsClientHeaders(t *testing.T) {
	adapter := NewPassthrough("https://api.anthropic.com/v1/messages", "", "")
	ctx := coreup.WithRawBody(context.Background(), []byte(`{"model":"claude-opus-4-8"}`))
	ctx = coreup.WithRawHeaders(ctx, http.Header{
		"Anthropic-Version": {"2023-06-01"},
		"Authorization":     {"Bearer client-side-token"},
		"Host":              {"localhost:9000"},
	})

	httpReq, err := adapter.ToUpstream(ctx, &ir.IRRequest{}, &cred.HeaderCredential{Header: "x-api-key", Value: "real-key"})
	if err != nil {
		t.Fatalf("ToUpstream: %v", err)
	}
	if got := httpReq.Header.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want forwarded value", got)
	}
	if got := httpReq.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want blocked (credential owns auth)", got)
	}
	if got := httpReq.Header.Get("x-api-key"); got != "real-key" {
		t.Errorf("x-api-key = %q, want the credential's value", got)
	}
}

// TestPassthroughAdapter_ToUpstream_EmptyUpstreamModel_LeavesBodyUnchanged
// guards the provider-keyed catch-all passthrough case (no model_routes
// entry matched — the client's raw model name IS the intended upstream
// model, so there is nothing configured to rewrite it to).
func TestPassthroughAdapter_ToUpstream_EmptyUpstreamModel_LeavesBodyUnchanged(t *testing.T) {
	adapter := NewPassthrough("https://api.example.com/v1/chat/completions", "", "")
	original := []byte(`{"model":"whatever-the-client-said","messages":[]}`)
	ctx := coreup.WithRawBody(context.Background(), original)

	httpReq, err := adapter.ToUpstream(ctx, &ir.IRRequest{}, &cred.HeaderCredential{Header: "Authorization", Value: "Bearer x"})
	if err != nil {
		t.Fatalf("ToUpstream: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != string(original) {
		t.Errorf("body changed with empty upstreamModel: got %s, want %s", body, original)
	}
}
