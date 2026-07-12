package irc

import (
	"encoding/json"
	"testing"

	"miroxy/core/ir"
	"miroxy/internal/types"
)

// TestGeminiResponseToIR_CapturesThoughtSignature covers the write side of
// the thought-signature correlation cache (see gemini_thoughtsig.go): when
// a Gemini response's functionCall part carries a thoughtSignature, it must
// be stashed under the freshly generated tool-call ID so a later request
// replaying this tool_use can re-attach it.
func TestGeminiResponseToIR_CapturesThoughtSignature(t *testing.T) {
	conv := &GeminiConverter{}
	body := mustJSON(types.GeminiResponse{
		Candidates: []types.GeminiCandidate{{
			Content: types.GeminiContent{
				Role: "model",
				Parts: []types.GeminiPart{{
					FunctionCall:     &types.GeminiFunctionCall{Name: "Bash", Args: mustJSON(map[string]string{"cmd": "ls"})},
					ThoughtSignature: "sig-abc123",
				}},
			},
			FinishReason: "STOP",
		}},
	})

	resp, err := conv.ResponseToIR(body)
	if err != nil {
		t.Fatalf("ResponseToIR: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].ToolUse == nil {
		t.Fatalf("expected one tool_use block, got %+v", resp.Content)
	}

	id := resp.Content[0].ToolUse.ID
	if got := geminiThoughtSigs.lookup(id); got != "sig-abc123" {
		t.Errorf("geminiThoughtSigs.lookup(%q) = %q, want %q", id, got, "sig-abc123")
	}
}

// TestGeminiRequestToProvider_ReattachesThoughtSignature covers the read
// side: replaying an assistant tool_use whose ID is already in the cache
// (as it would be, having been populated by ResponseToIR on an earlier
// turn) must re-attach the signature to the outgoing functionCall part.
func TestGeminiRequestToProvider_ReattachesThoughtSignature(t *testing.T) {
	const toolID = "test-tool-id-reattach"
	geminiThoughtSigs.store(toolID, "sig-xyz789")

	conv := &GeminiConverter{}
	irReq := &ir.IRRequest{
		Messages: []ir.IRMessage{{
			Role: "assistant",
			Parts: []ir.IRContentPart{{
				ToolUse: &ir.IRToolUsePart{ID: toolID, Name: "Bash", InputJSON: []byte(`{"cmd":"ls"}`)},
			}},
		}},
	}

	body, err := conv.RequestToProvider(irReq)
	if err != nil {
		t.Fatalf("RequestToProvider: %v", err)
	}
	var geminiReq types.GeminiRequest
	if err := json.Unmarshal(body, &geminiReq); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if len(geminiReq.Contents) != 1 || len(geminiReq.Contents[0].Parts) != 1 {
		t.Fatalf("unexpected contents shape: %+v", geminiReq.Contents)
	}
	if got := geminiReq.Contents[0].Parts[0].ThoughtSignature; got != "sig-xyz789" {
		t.Errorf("ThoughtSignature = %q, want %q", got, "sig-xyz789")
	}
}

// TestGeminiRequestToProvider_NoSignatureWhenNotCached covers a tool_use
// whose ID was never captured (e.g. a non-thinking model, or an expired
// cache entry) — the outgoing part must have no thoughtSignature rather
// than a stale or fabricated one.
func TestGeminiRequestToProvider_NoSignatureWhenNotCached(t *testing.T) {
	conv := &GeminiConverter{}
	irReq := &ir.IRRequest{
		Messages: []ir.IRMessage{{
			Role: "assistant",
			Parts: []ir.IRContentPart{{
				ToolUse: &ir.IRToolUsePart{ID: "never-cached-tool-id", Name: "Bash", InputJSON: []byte(`{"cmd":"ls"}`)},
			}},
		}},
	}

	body, err := conv.RequestToProvider(irReq)
	if err != nil {
		t.Fatalf("RequestToProvider: %v", err)
	}
	var geminiReq types.GeminiRequest
	if err := json.Unmarshal(body, &geminiReq); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if got := geminiReq.Contents[0].Parts[0].ThoughtSignature; got != "" {
		t.Errorf("ThoughtSignature = %q, want empty (never cached)", got)
	}
}
