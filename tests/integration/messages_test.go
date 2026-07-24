package integration_test

import (
	"net/http"
	"strings"
	"testing"

	"miroxy/internal/types"
)

// TestNonStreaming_HappyPath is mandatory scenario 1 from the design doc:
// a non-streaming request reaches the stub, the stub returns fixed text,
// and the response is correctly shaped as an Anthropic MessageResponse.
func TestNonStreaming_HappyPath(t *testing.T) {
	stub := newStubGemini(t, nil, nil)
	defer stub.Close()

	miroxy := newTestServer(t, miroxyConfig{stubURL: stub.URL, keys: []string{"key-a"}})
	defer miroxy.Close()

	resp := doPost(t, miroxy.URL, nonStreamBody)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}

	var msg types.MessageResponse
	decodeJSON(t, resp.Body, &msg)

	if msg.Role != "assistant" {
		t.Errorf("role: got %q, want assistant", msg.Role)
	}
	if msg.StopReason != "end_turn" {
		t.Errorf("stop_reason: got %q, want end_turn", msg.StopReason)
	}
	if msg.Model != "claude-haiku" {
		t.Errorf("model echo: got %q, want claude-haiku", msg.Model)
	}
	if len(msg.Content) == 0 || msg.Content[0].Type != "text" || msg.Content[0].Text == "" {
		t.Errorf("content: %+v", msg.Content)
	}
	if msg.Usage.InputTokens <= 0 {
		t.Errorf("input_tokens should be > 0, got %d", msg.Usage.InputTokens)
	}
}

// TestNonStreaming_KeyFailover is mandatory scenario 2:
// keyA returns repeated 5xx → pool circuit-breaks keyA → keyB serves the request.
func TestNonStreaming_KeyFailover(t *testing.T) {
	const (
		keyA = "key-a"
		keyB = "key-b"
	)

	stub := newStubGemini(t, map[string]http.HandlerFunc{
		keyA: gemini500Handler,
		// keyB has no override → default success handler
	}, nil)
	defer stub.Close()

	miroxy := newTestServer(t, miroxyConfig{
		stubURL:   stub.URL,
		keys:      []string{keyA, keyB},
		threshold: 1, // circuit-break after 1 failure
	})
	defer miroxy.Close()

	resp := doPost(t, miroxy.URL, nonStreamBody)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after failover, got %d", resp.StatusCode)
	}

	var msg types.MessageResponse
	decodeJSON(t, resp.Body, &msg)

	if msg.Role != "assistant" {
		t.Errorf("role after failover: got %q, want assistant", msg.Role)
	}
}

// TestNonStreaming_AllKeysBroken returns 503 when all keys are circuit-broken.
func TestNonStreaming_AllKeysBroken(t *testing.T) {
	const keyA = "key-a"

	stub := newStubGemini(t, map[string]http.HandlerFunc{
		keyA: gemini500Handler,
	}, nil)
	defer stub.Close()

	miroxy := newTestServer(t, miroxyConfig{
		stubURL:   stub.URL,
		keys:      []string{keyA},
		threshold: 1,
	})
	defer miroxy.Close()

	// First request will exhaust retries against keyA and circuit-break it.
	resp1 := doPost(t, miroxy.URL, nonStreamBody)
	resp1.Body.Close()
	// Should be 502 (maxRetries exhausted) or 503 (no keys).

	// Second request: keyA is still cooling down → 503.
	resp2 := doPost(t, miroxy.URL, nonStreamBody)
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when all keys broken, got %d", resp2.StatusCode)
	}

	var errResp types.ErrorResponse
	decodeJSON(t, resp2.Body, &errResp)
	if errResp.Error.Type != "overloaded_error" {
		t.Errorf("error type: got %q, want overloaded_error", errResp.Error.Type)
	}
}

// TestNonStreaming_UnknownModel returns 400 with a hint.
func TestNonStreaming_UnknownModel(t *testing.T) {
	stub := newStubGemini(t, nil, nil)
	defer stub.Close()
	miroxy := newTestServer(t, miroxyConfig{stubURL: stub.URL})
	defer miroxy.Close()

	body := `{"model":"claude-opus-4","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	resp := doPost(t, miroxy.URL, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	var errResp types.ErrorResponse
	decodeJSON(t, resp.Body, &errResp)
	if errResp.Error.Type != "invalid_request_error" {
		t.Errorf("error type: got %q", errResp.Error.Type)
	}
}

// TestNonStreaming_ToolsTranslated verifies that tool definitions are forwarded
// to Gemini (not rejected with 501). The stub returns a plain text response.
func TestNonStreaming_ToolsTranslated(t *testing.T) {
	stub := newStubGemini(t, nil, nil)
	defer stub.Close()
	miroxy := newTestServer(t, miroxyConfig{stubURL: stub.URL})
	defer miroxy.Close()

	body := `{"model":"claude-haiku","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"tools":[{"name":"search","input_schema":{"type":"object","properties":{}}}]}`
	resp := doPost(t, miroxy.URL, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for tool request (tools now translated), got %d", resp.StatusCode)
	}
}

// TestNonStreaming_AuthRejected verifies missing/invalid bearer tokens get 401.
func TestNonStreaming_AuthRejected(t *testing.T) {
	stub := newStubGemini(t, nil, nil)
	defer stub.Close()
	miroxy := newTestServer(t, miroxyConfig{stubURL: stub.URL})
	defer miroxy.Close()

	req, _ := http.NewRequest(http.MethodPost, miroxy.URL+"/v1/messages", nil)
	req.Header.Set("Content-Type", "application/json")
	// No Authorization header.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth, got %d", resp.StatusCode)
	}
}

// TestGetModels returns the configured model list.
func TestGetModels(t *testing.T) {
	stub := newStubGemini(t, nil, nil)
	defer stub.Close()
	miroxy := newTestServer(t, miroxyConfig{stubURL: stub.URL})
	defer miroxy.Close()

	req, _ := http.NewRequest(http.MethodGet, miroxy.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testClientKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var models types.ModelsResponse
	decodeJSON(t, resp.Body, &models)

	// strict mode (default): only explicitly configured models are returned
	if len(models.Data) != 1 {
		t.Fatalf("strict mode: expected 1 model, got %d", len(models.Data))
	}
	m := models.Data[0]
	if m.ID != "claude-haiku" {
		t.Errorf("model ID: got %q, want claude-haiku", m.ID)
	}
	if m.Type != "model" {
		t.Errorf("model type: got %q, want model", m.Type)
	}
	if m.DisplayName == "" {
		t.Error("model display_name should not be empty")
	}
}

// TestNonStreaming_BarePathAliasWithoutV1 guards that a client which builds
// its request URL as base_url+"/messages" (omitting /v1 from base_url) still
// reaches the same handler as the documented /v1/messages path — some
// OpenAI-compatible tools get this detail wrong, and 404-ing them instead of
// serving the request is the exact friction that prompted this alias.
func TestNonStreaming_BarePathAliasWithoutV1(t *testing.T) {
	stub := newStubGemini(t, nil, nil)
	defer stub.Close()

	miroxy := newTestServer(t, miroxyConfig{stubURL: stub.URL, keys: []string{"key-a"}})
	defer miroxy.Close()

	req, err := http.NewRequest(http.MethodPost, miroxy.URL+"/messages", strings.NewReader(nonStreamBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testClientKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /messages (no /v1): expected 200, got %d", resp.StatusCode)
	}
	var msg types.MessageResponse
	decodeJSON(t, resp.Body, &msg)
	if msg.Role != "assistant" {
		t.Errorf("role: got %q, want assistant", msg.Role)
	}
}
