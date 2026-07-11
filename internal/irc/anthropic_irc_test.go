package irc

import (
	"encoding/json"
	"testing"

	"miroxy/core/ir"
	"miroxy/internal/types"
)

// These tests cover the Anthropic⇄IR frontend conversion. They were migrated from
// internal/ir/convert_test.go when the conversion logic moved out of the (now
// provider-neutral) ir package into AnthropicConverter.

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func ptr[T any](v T) *T { return &v }

func TestRequestToIR_StringMessage(t *testing.T) {
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 100,
		Messages:  []types.Message{{Role: "user", Content: mustJSON("Hello")}},
	}
	irReq, err := AnthropicConverter{}.RequestToIR(req)
	if err != nil {
		t.Fatalf("RequestToIR: %v", err)
	}
	if len(irReq.Messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(irReq.Messages))
	}
	msg := irReq.Messages[0]
	if msg.Role != "user" {
		t.Errorf("role: got %q, want user", msg.Role)
	}
	if len(msg.Parts) != 1 || msg.Parts[0].Text == nil {
		t.Fatalf("want 1 text part, got %+v", msg.Parts)
	}
	if msg.Parts[0].Text.Text != "Hello" {
		t.Errorf("text: got %q, want Hello", msg.Parts[0].Text.Text)
	}
}

func TestRequestToIR_SystemString(t *testing.T) {
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 50,
		System:    mustJSON("You are helpful."),
		Messages:  []types.Message{{Role: "user", Content: mustJSON("Hi")}},
	}
	irReq, err := AnthropicConverter{}.RequestToIR(req)
	if err != nil {
		t.Fatalf("RequestToIR: %v", err)
	}
	if irReq.System != "You are helpful." {
		t.Errorf("system: got %q, want %q", irReq.System, "You are helpful.")
	}
}

func TestRequestToIR_SystemBlockArray(t *testing.T) {
	blocks := []map[string]any{{"type": "text", "text": "Part A"}, {"type": "text", "text": "Part B"}}
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 50,
		System:    mustJSON(blocks),
		Messages:  []types.Message{{Role: "user", Content: mustJSON("Hi")}},
	}
	irReq, err := AnthropicConverter{}.RequestToIR(req)
	if err != nil {
		t.Fatalf("RequestToIR: %v", err)
	}
	if irReq.System != "Part A\nPart B" {
		t.Errorf("system: got %q, want %q", irReq.System, "Part A\nPart B")
	}
}

func TestRequestToIR_BlockContent(t *testing.T) {
	blocks := []map[string]any{
		{"type": "text", "text": "Consider this:"},
		{"type": "text", "text": "More text"},
	}
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 100,
		Messages:  []types.Message{{Role: "user", Content: mustJSON(blocks)}},
	}
	irReq, err := AnthropicConverter{}.RequestToIR(req)
	if err != nil {
		t.Fatalf("RequestToIR: %v", err)
	}
	parts := irReq.Messages[0].Parts
	if len(parts) != 2 {
		t.Fatalf("want 2 parts, got %d", len(parts))
	}
	if parts[0].Text.Text != "Consider this:" || parts[1].Text.Text != "More text" {
		t.Errorf("unexpected part texts: %v %v", parts[0].Text.Text, parts[1].Text.Text)
	}
}

func TestRequestToIR_CacheControlStripped(t *testing.T) {
	// cache_control is present in the Anthropic block but must not appear in the IR.
	blocks := []map[string]any{
		{"type": "text", "text": "Hello", "cache_control": map[string]any{"type": "ephemeral"}},
	}
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 100,
		Messages:  []types.Message{{Role: "user", Content: mustJSON(blocks)}},
	}
	irReq, err := AnthropicConverter{}.RequestToIR(req)
	if err != nil {
		t.Fatalf("RequestToIR: %v", err)
	}
	part := irReq.Messages[0].Parts[0]
	if part.Text == nil || part.Text.Text != "Hello" {
		t.Errorf("text part not preserved: %+v", part)
	}
}

func TestRequestToIR_ToolUse(t *testing.T) {
	blocks := []map[string]any{{
		"type":  "tool_use",
		"id":    "tu_abc",
		"name":  "calculator",
		"input": map[string]any{"expr": "1+1"},
	}}
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 100,
		Messages:  []types.Message{{Role: "assistant", Content: mustJSON(blocks)}},
	}
	irReq, err := AnthropicConverter{}.RequestToIR(req)
	if err != nil {
		t.Fatalf("RequestToIR: %v", err)
	}
	part := irReq.Messages[0].Parts[0]
	if part.ToolUse == nil {
		t.Fatal("expected ToolUse part")
	}
	if part.ToolUse.ID != "tu_abc" || part.ToolUse.Name != "calculator" {
		t.Errorf("tool_use ID/Name: %q / %q", part.ToolUse.ID, part.ToolUse.Name)
	}
	if len(part.ToolUse.InputJSON) == 0 {
		t.Error("InputJSON should not be empty")
	}
}

// TestRequestToIR_ToolResult_Neutral verifies the IR carries only the neutral
// tool_use_id and content — NOT a resolved function name. Name resolution is the
// provider converter's job (covered end-to-end in tests/unit/translator_test.go).
func TestRequestToIR_ToolResult_Neutral(t *testing.T) {
	assistantBlocks := []map[string]any{{
		"type": "tool_use", "id": "tu_xyz", "name": "get_weather", "input": map[string]any{},
	}}
	userBlocks := []map[string]any{{
		"type": "tool_result", "tool_use_id": "tu_xyz", "content": "sunny",
	}}
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 100,
		Messages: []types.Message{
			{Role: "user", Content: mustJSON("What's the weather?")},
			{Role: "assistant", Content: mustJSON(assistantBlocks)},
			{Role: "user", Content: mustJSON(userBlocks)},
		},
	}
	irReq, err := AnthropicConverter{}.RequestToIR(req)
	if err != nil {
		t.Fatalf("RequestToIR: %v", err)
	}
	userMsg := irReq.Messages[2]
	if len(userMsg.Parts) != 1 || userMsg.Parts[0].ToolResult == nil {
		t.Fatal("expected tool_result part in third message")
	}
	tr := userMsg.Parts[0].ToolResult
	if tr.ToolUseID != "tu_xyz" {
		t.Errorf("ToolUseID: got %q, want tu_xyz", tr.ToolUseID)
	}
	if len(tr.Content) != 1 || tr.Content[0].Text == nil || tr.Content[0].Text.Text != "sunny" {
		t.Errorf("Content: %+v", tr.Content)
	}
}

func TestRequestToIR_GenerationConfig(t *testing.T) {
	req := &types.MessageRequest{
		Model:         "claude-haiku",
		MaxTokens:     512,
		Temperature:   ptr(0.7),
		TopP:          ptr(0.9),
		StopSequences: []string{"STOP", "END"},
		Messages:      []types.Message{{Role: "user", Content: mustJSON("hi")}},
	}
	irReq, err := AnthropicConverter{}.RequestToIR(req)
	if err != nil {
		t.Fatalf("RequestToIR: %v", err)
	}
	gen := irReq.Gen
	if gen.MaxTokens != 512 {
		t.Errorf("MaxTokens: got %d, want 512", gen.MaxTokens)
	}
	if gen.Temperature == nil || *gen.Temperature != 0.7 {
		t.Errorf("Temperature: got %v", gen.Temperature)
	}
	if gen.TopP == nil || *gen.TopP != 0.9 {
		t.Errorf("TopP: got %v", gen.TopP)
	}
	if len(gen.StopSeqs) != 2 || gen.StopSeqs[0] != "STOP" {
		t.Errorf("StopSeqs: %v", gen.StopSeqs)
	}
}

func TestRequestToIR_Tools(t *testing.T) {
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 100,
		Tools: []types.Tool{{
			Name:        "search",
			Description: "Search the web",
			InputSchema: mustJSON(map[string]any{"type": "object"}),
		}},
		Messages: []types.Message{{Role: "user", Content: mustJSON("search for cats")}},
	}
	irReq, err := AnthropicConverter{}.RequestToIR(req)
	if err != nil {
		t.Fatalf("RequestToIR: %v", err)
	}
	if len(irReq.Tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(irReq.Tools))
	}
	tool := irReq.Tools[0]
	if tool.Name != "search" || tool.Description != "Search the web" {
		t.Errorf("tool: %+v", tool)
	}
	if len(tool.InputSchemaJSON) == 0 {
		t.Error("InputSchemaJSON should not be empty")
	}
}

func TestResponseFromIR_Basic(t *testing.T) {
	irResp := &ir.IRResponse{
		Content:    []ir.IRResponseBlock{{Text: &ir.IRTextPart{Text: "Hello!"}}},
		StopReason: "end_turn",
		Usage:      ir.IRUsage{InputTokens: 10, OutputTokens: 5},
	}
	resp := AnthropicConverter{}.ResponseFromIR(irResp, "msg_test", "claude-haiku")
	if resp.ID != "msg_test" || resp.Model != "claude-haiku" {
		t.Errorf("ID/Model: %q / %q", resp.ID, resp.Model)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason: %q", resp.StopReason)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "Hello!" {
		t.Errorf("Content: %+v", resp.Content)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Errorf("Usage: %+v", resp.Usage)
	}
}
