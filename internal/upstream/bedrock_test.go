package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"miroxy/core/ir"
)

// noopCredential lets these tests inspect the request BedrockAdapter builds
// without needing real AWS signing material.
type noopCredential struct{}

func (noopCredential) Apply(*http.Request) error { return nil }
func (noopCredential) Type() string              { return "noop" }
func (noopCredential) Redacted() string          { return "noop" }

func minimalIRRequest() *ir.IRRequest {
	return &ir.IRRequest{
		Messages: []ir.IRMessage{
			{Role: "user", Parts: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: "hello"}}}},
		},
		Gen: ir.IRGenerationConfig{MaxTokens: 256},
	}
}

func TestBedrockAdapter_ToUpstream_TransformsBodyAndURL(t *testing.T) {
	a := NewBedrock("anthropic.claude-3-5-sonnet-20241022-v2:0", "https://bedrock-runtime.us-east-1.amazonaws.com")

	httpReq, err := a.ToUpstream(context.Background(), minimalIRRequest(), noopCredential{})
	if err != nil {
		t.Fatalf("ToUpstream: %v", err)
	}

	// Colon is a valid unescaped path-segment character per RFC 3986 (pchar
	// includes ":"), so Go's URL renders it unescaped here even though
	// url.PathEscape produced "%3A" — SigV4 signing (core/cred) applies its
	// own, stricter escaping to the decoded path independently, so this
	// doesn't affect signature correctness.
	wantURL := "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke"
	if got := httpReq.URL.String(); got != wantURL {
		t.Errorf("URL = %q, want %q", got, wantURL)
	}

	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if _, ok := fields["model"]; ok {
		t.Error(`body still has "model" field — Bedrock InvokeModel rejects it`)
	}
	var version string
	if err := json.Unmarshal(fields["anthropic_version"], &version); err != nil {
		t.Fatalf("anthropic_version: %v", err)
	}
	if version != bedrockAnthropicVersion {
		t.Errorf("anthropic_version = %q, want %q", version, bedrockAnthropicVersion)
	}
}

func TestBedrockAdapter_ToUpstreamStream_URLShape(t *testing.T) {
	a := NewBedrock("anthropic.claude-3-5-sonnet-20241022-v2:0", "https://bedrock-runtime.us-east-1.amazonaws.com")

	httpReq, err := a.ToUpstreamStream(context.Background(), minimalIRRequest(), noopCredential{})
	if err != nil {
		t.Fatalf("ToUpstreamStream: %v", err)
	}

	wantURL := "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke-with-response-stream"
	if got := httpReq.URL.String(); got != wantURL {
		t.Errorf("URL = %q, want %q", got, wantURL)
	}
}

func TestBedrockAdapter_FromUpstream_ParsesAnthropicShapedBody(t *testing.T) {
	a := NewBedrock("anthropic.claude-3-5-sonnet-20241022-v2:0", "https://bedrock-runtime.us-east-1.amazonaws.com")

	body := `{
		"id": "msg_123",
		"type": "message",
		"role": "assistant",
		"content": [{"type": "text", "text": "hi there"}],
		"model": "anthropic.claude-3-5-sonnet-20241022-v2:0",
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 5, "output_tokens": 3}
	}`
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}

	irResp, err := a.FromUpstream(resp)
	if err != nil {
		t.Fatalf("FromUpstream: %v", err)
	}
	if len(irResp.Content) != 1 || irResp.Content[0].Text == nil || irResp.Content[0].Text.Text != "hi there" {
		t.Errorf("Content = %+v, want a single text block \"hi there\"", irResp.Content)
	}
	if irResp.Usage.InputTokens != 5 || irResp.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v, want input=5 output=3", irResp.Usage)
	}
}
