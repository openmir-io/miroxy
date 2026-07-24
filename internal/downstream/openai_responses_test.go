package downstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"miroxy/core/ir"
)

func TestResponsesAdapter_ReasoningDecodeEncodeRoundTrip(t *testing.T) {
	body := `{
		"model": "gpt-5-codex",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"reasoning","id":"rs_123","summary":[{"type":"summary_text","text":"thinking about it"}],"encrypted_content":"cipher-xyz"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))

	a := &ResponsesAdapter{}
	irReq, model, err := a.Decode(req)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if model != "gpt-5-codex" {
		t.Errorf("model: got %q, want gpt-5-codex", model)
	}

	var reasoning *ir.IRReasoningPart
	for _, m := range irReq.Messages {
		for _, p := range m.Parts {
			if p.Reasoning != nil {
				reasoning = p.Reasoning
			}
		}
	}
	if reasoning == nil {
		t.Fatalf("reasoning item was dropped during decode; messages: %+v", irReq.Messages)
	}
	if reasoning.Text != "thinking about it" {
		t.Errorf("reasoning text: got %q, want %q", reasoning.Text, "thinking about it")
	}
	if reasoning.Signature == "" {
		t.Fatalf("want non-empty envelope signature for encrypted reasoning")
	}

	encoded, err := a.EncodeRequest(irReq, model)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	var out responsesRequest
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("unmarshal encoded request: %v", err)
	}

	var found *inputItem
	for _, raw := range out.Input {
		var item inputItem
		if json.Unmarshal(raw, &item) == nil && item.Type == "reasoning" {
			found = &item
		}
	}
	if found == nil {
		t.Fatalf("no reasoning item in re-encoded request: %s", encoded)
	}
	if found.ID != "rs_123" {
		t.Errorf("id: got %q, want rs_123", found.ID)
	}
	if found.EncryptedContent != "cipher-xyz" {
		t.Errorf("encrypted_content: got %q, want cipher-xyz", found.EncryptedContent)
	}
	if len(found.Summary) != 1 {
		t.Fatalf("summary: got %d parts, want 1", len(found.Summary))
	}
	var summaryPart inputContent
	if err := json.Unmarshal(found.Summary[0], &summaryPart); err != nil {
		t.Fatalf("unmarshal summary part: %v", err)
	}
	if summaryPart.Text != "thinking about it" {
		t.Errorf("summary text: got %q, want %q", summaryPart.Text, "thinking about it")
	}
}

// TestResponsesAdapter_PlainReasoningEncode covers reasoning that never went
// through an encrypted-envelope round trip (e.g. content originating from a
// real Anthropic upstream's thinking block) — EncodeRequest must synthesize
// a fresh reasoning item from the visible text alone.
func TestResponsesAdapter_PlainReasoningEncode(t *testing.T) {
	a := &ResponsesAdapter{}
	req := &ir.IRRequest{
		Messages: []ir.IRMessage{{
			Role: "assistant",
			Parts: []ir.IRContentPart{
				{Reasoning: &ir.IRReasoningPart{Text: "plain reasoning, no envelope"}},
				{Text: &ir.IRTextPart{Text: "answer"}},
			},
		}},
	}
	encoded, err := a.EncodeRequest(req, "gpt-5-codex")
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var out responsesRequest
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("unmarshal encoded request: %v", err)
	}
	var found *inputItem
	for _, raw := range out.Input {
		var item inputItem
		if json.Unmarshal(raw, &item) == nil && item.Type == "reasoning" {
			found = &item
		}
	}
	if found == nil {
		t.Fatalf("no reasoning item in re-encoded request: %s", encoded)
	}
	if found.EncryptedContent != "" {
		t.Errorf("encrypted_content: got %q, want empty", found.EncryptedContent)
	}
	if len(found.Summary) != 1 {
		t.Fatalf("summary: got %d parts, want 1", len(found.Summary))
	}
	var summaryPart inputContent
	if err := json.Unmarshal(found.Summary[0], &summaryPart); err != nil {
		t.Fatalf("unmarshal summary part: %v", err)
	}
	if summaryPart.Text != "plain reasoning, no envelope" {
		t.Errorf("summary text: got %q", summaryPart.Text)
	}
}
