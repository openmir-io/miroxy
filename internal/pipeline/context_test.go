package pipeline_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"miroxy/core/ir"
	"miroxy/core/router"
	"miroxy/internal/downstream"
	"miroxy/internal/pipeline"
)

func TestRefreshRawBodyIfRewritten_NoOpWhenNotRewritten(t *testing.T) {
	req := &ir.IRRequest{Gen: ir.IRGenerationConfig{MaxTokens: 100}}
	c := pipeline.NewContext(context.Background(), req, "test", router.RouteTarget{})
	c.RawRequestBody = []byte(`{"model":"test","max_tokens":1}`)

	c.RefreshRawBodyIfRewritten()

	if string(c.RawRequestBody) != `{"model":"test","max_tokens":1}` {
		t.Fatalf("RawRequestBody should be untouched, got %s", c.RawRequestBody)
	}
}

func TestRefreshRawBodyIfRewritten_ReMarshalsWhenRewritten(t *testing.T) {
	req := &ir.IRRequest{Gen: ir.IRGenerationConfig{MaxTokens: 1024}}
	c := pipeline.NewContext(context.Background(), req, "test", router.RouteTarget{})
	c.ClientProtocol = "anthropic"
	c.EncodeRequest = func(req *ir.IRRequest) ([]byte, error) {
		return (&downstream.AnthropicAdapter{}).EncodeRequest(req, "test")
	}
	c.RawRequestBody = []byte(`{"model":"test","max_tokens":1,"messages":[{"role":"user","content":"original huge conversation"}]}`)
	c.RequestRewritten = true

	c.RefreshRawBodyIfRewritten()

	var got struct {
		MaxTokens int        `json:"max_tokens"`
		Messages  []struct{} `json:"messages"`
	}
	if err := json.Unmarshal(c.RawRequestBody, &got); err != nil {
		t.Fatalf("RawRequestBody is not valid JSON: %v", err)
	}
	if got.MaxTokens != 1024 {
		t.Fatalf("expected refreshed body to reflect current Request.Gen.MaxTokens=1024, got %d", got.MaxTokens)
	}
	if len(got.Messages) != 0 {
		t.Fatalf("expected refreshed body to reflect current (compressed) Request.Messages, got %d messages", len(got.Messages))
	}
}

func TestRefreshRawBodyIfRewritten_NoOpWhenRawBodyEmpty(t *testing.T) {
	req := &ir.IRRequest{}
	c := pipeline.NewContext(context.Background(), req, "test", router.RouteTarget{})
	c.RequestRewritten = true
	// RawRequestBody left empty — e.g. this request never had a raw capture to refresh.

	c.RefreshRawBodyIfRewritten()

	if len(c.RawRequestBody) != 0 {
		t.Fatalf("expected RawRequestBody to stay empty, got %s", c.RawRequestBody)
	}
}

// TestRefreshRawBodyIfRewritten_UsesEncodeRequest reproduces a real failure:
// an OpenAI-protocol client's rewritten request must re-serialize back into
// OpenAI wire format for raw dispatch, not leak the canonical (Anthropic-
// shaped) tool_use/tool_result content blocks upstream as if they were plain
// OpenAI message content.
func TestRefreshRawBodyIfRewritten_UsesEncodeRequest(t *testing.T) {
	req := &ir.IRRequest{
		Gen: ir.IRGenerationConfig{MaxTokens: 100},
		Messages: []ir.IRMessage{
			{Role: "user", Parts: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: "read the file"}}}},
			{Role: "assistant", Parts: []ir.IRContentPart{{ToolUse: &ir.IRToolUsePart{ID: "call_1", Name: "read", InputJSON: []byte(`{"path":"x"}`)}}}},
			{Role: "user", Parts: []ir.IRContentPart{{ToolResult: &ir.IRToolResultPart{ToolUseID: "call_1", Content: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: "file contents"}}}}}}},
		},
	}
	c := pipeline.NewContext(context.Background(), req, "test", router.RouteTarget{})
	c.ClientProtocol = "openai"
	c.RawRequestBody = []byte(`{"model":"test","messages":[]}`) // the client's original (now stale) bytes
	c.RequestRewritten = true
	c.EncodeRequest = func(req *ir.IRRequest) ([]byte, error) {
		return (&downstream.OpenAIAdapter{}).EncodeRequest(req, "test")
	}

	c.RefreshRawBodyIfRewritten()

	body := string(c.RawRequestBody)
	if strings.Contains(body, `"type":"tool_use"`) || strings.Contains(body, `"type":"tool_result"`) {
		t.Fatalf("refreshed body leaked Anthropic-shaped content blocks into OpenAI wire format: %s", body)
	}

	var got struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				Function struct{ Name string } `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(c.RawRequestBody, &got); err != nil {
		t.Fatalf("refreshed body is not valid JSON: %v (body=%s)", err, body)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("expected 3 OpenAI-shaped messages, got %d: %s", len(got.Messages), body)
	}
	if len(got.Messages[1].ToolCalls) != 1 || got.Messages[1].ToolCalls[0].Function.Name != "read" {
		t.Errorf("expected assistant message[1] to carry tool_calls=[read], got %+v", got.Messages[1])
	}
	if got.Messages[2].Role != "tool" || got.Messages[2].ToolCallID != "call_1" {
		t.Errorf("expected message[2] to be role=tool with tool_call_id=call_1, got %+v", got.Messages[2])
	}
}
