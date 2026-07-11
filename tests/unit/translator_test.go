package unit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"miroxy/core/cred"

	"miroxy/internal/irc"
	"miroxy/internal/translator"
	"miroxy/internal/types"
)

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return json.RawMessage(b)
}

func rawString(s string) json.RawMessage { return mustRaw(s) }

func geminiTranslator() *translator.GeminiTranslator {
	return translator.NewGemini("gemini-2.5-flash")
}

// decodeUpstreamBody sends a request through ToUpstream and decodes the body.
func decodeGeminiBody(t *testing.T, req *types.MessageRequest) types.GeminiRequest {
	t.Helper()
	trans := geminiTranslator()
	httpReq, err := trans.ToUpstream(context.Background(), req, &cred.QueryCredential{Param: "key", Value: "test-key"})
	if err != nil {
		t.Fatalf("ToUpstream: %v", err)
	}
	var gr types.GeminiRequest
	if err := json.NewDecoder(httpReq.Body).Decode(&gr); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return gr
}

// --- ToUpstream tests ---

func TestToUpstream_StringContent_RoleMapped(t *testing.T) {
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 100,
		Messages: []types.Message{
			{Role: "user", Content: rawString("Hello")},
			{Role: "assistant", Content: rawString("Hi there")},
			{Role: "user", Content: rawString("How are you?")},
		},
	}
	gr := decodeGeminiBody(t, req)

	if len(gr.Contents) != 3 {
		t.Fatalf("expected 3 contents, got %d", len(gr.Contents))
	}
	if gr.Contents[1].Role != "model" {
		t.Errorf("assistant role should map to model, got %q", gr.Contents[1].Role)
	}
	if gr.Contents[0].Parts[0].Text != "Hello" {
		t.Errorf("first part text: got %q, want Hello", gr.Contents[0].Parts[0].Text)
	}
}

func TestToUpstream_SystemPrompt_ExtractedToSystemInstruction(t *testing.T) {
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 50,
		System:    json.RawMessage(`"You are a helpful assistant."`),
		Messages:  []types.Message{{Role: "user", Content: rawString("Hi")}},
	}
	gr := decodeGeminiBody(t, req)

	if gr.SystemInstruction == nil {
		t.Fatal("expected systemInstruction to be set")
	}
	if len(gr.SystemInstruction.Parts) == 0 || gr.SystemInstruction.Parts[0].Text != "You are a helpful assistant." {
		t.Errorf("systemInstruction text wrong: %+v", gr.SystemInstruction)
	}
	// System prompt must NOT appear in contents
	if len(gr.Contents) != 1 {
		t.Errorf("expected 1 content (user only), got %d", len(gr.Contents))
	}
}

func TestToUpstream_BlockContent_TextExtracted(t *testing.T) {
	blocks := []types.ContentBlock{
		{Type: "text", Text: "First part."},
		{Type: "text", Text: "Second part."},
	}
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 50,
		Messages:  []types.Message{{Role: "user", Content: mustRaw(blocks)}},
	}
	gr := decodeGeminiBody(t, req)

	if len(gr.Contents[0].Parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(gr.Contents[0].Parts))
	}
}

func TestToUpstream_MaxTokensMapped(t *testing.T) {
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 512,
		Messages:  []types.Message{{Role: "user", Content: rawString("hi")}},
	}
	gr := decodeGeminiBody(t, req)

	if gr.GenerationConfig == nil || gr.GenerationConfig.MaxOutputTokens != 512 {
		t.Errorf("maxOutputTokens: got %+v", gr.GenerationConfig)
	}
}

func TestToUpstream_URLContainsKeyAndModel(t *testing.T) {
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 10,
		Messages:  []types.Message{{Role: "user", Content: rawString("hi")}},
	}
	httpReq, err := geminiTranslator().ToUpstream(context.Background(), req, &cred.QueryCredential{Param: "key", Value: "my-secret-key"})
	if err != nil {
		t.Fatal(err)
	}
	url := httpReq.URL.String()
	if !contains(url, "gemini-2.5-flash") {
		t.Errorf("URL missing model: %s", url)
	}
	if !contains(url, "my-secret-key") {
		t.Errorf("URL missing key: %s", url)
	}
}

func TestToUpstream_ToolsTranslatedToFunctionDeclarations(t *testing.T) {
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 10,
		Messages:  []types.Message{{Role: "user", Content: rawString("search for Go")}},
		Tools: []types.Tool{{
			Name:        "search",
			Description: "search the web",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		}},
	}
	gr := decodeGeminiBody(t, req)

	if len(gr.Tools) == 0 || len(gr.Tools[0].FunctionDeclarations) == 0 {
		t.Fatalf("expected function declarations in request, got %+v", gr.Tools)
	}
	decl := gr.Tools[0].FunctionDeclarations[0]
	if decl.Name != "search" {
		t.Errorf("function name: got %q, want search", decl.Name)
	}
	if decl.Description != "search the web" {
		t.Errorf("function description: got %q", decl.Description)
	}
	if gr.ToolConfig == nil || gr.ToolConfig.FunctionCallingConfig.Mode != "AUTO" {
		t.Errorf("tool config: got %+v", gr.ToolConfig)
	}
}

func TestToUpstream_ToolUseInContent_TranslatedToFunctionCall(t *testing.T) {
	blocks := []types.ContentBlock{{
		Type:  "tool_use",
		ID:    "toolu_abc",
		Name:  "search",
		Input: json.RawMessage(`{"q":"Go programming"}`),
	}}
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 10,
		Messages:  []types.Message{{Role: "assistant", Content: mustRaw(blocks)}},
	}
	gr := decodeGeminiBody(t, req)

	if len(gr.Contents) == 0 || len(gr.Contents[0].Parts) == 0 {
		t.Fatal("expected parts in contents")
	}
	part := gr.Contents[0].Parts[0]
	if part.FunctionCall == nil {
		t.Fatalf("expected functionCall part, got %+v", part)
	}
	if part.FunctionCall.Name != "search" {
		t.Errorf("functionCall name: got %q, want search", part.FunctionCall.Name)
	}
}

func TestToUpstream_ToolResultInContent_TranslatedToFunctionResponse(t *testing.T) {
	// Assistant calls search, user returns result.
	assistantBlocks := []types.ContentBlock{{
		Type: "tool_use", ID: "toolu_xyz", Name: "search",
		Input: json.RawMessage(`{"q":"Go"}`),
	}}
	userBlocks := []types.ContentBlock{{
		Type:      "tool_result",
		ToolUseID: "toolu_xyz",
		Content:   json.RawMessage(`"Go is a compiled language"`),
	}}
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 10,
		Messages: []types.Message{
			{Role: "user", Content: rawString("search for Go")},
			{Role: "assistant", Content: mustRaw(assistantBlocks)},
			{Role: "user", Content: mustRaw(userBlocks)},
		},
	}
	gr := decodeGeminiBody(t, req)

	// Last content (index 2) should be the user with functionResponse.
	last := gr.Contents[2]
	if len(last.Parts) == 0 || last.Parts[0].FunctionResponse == nil {
		t.Fatalf("expected functionResponse part, got %+v", last.Parts)
	}
	if last.Parts[0].FunctionResponse.Name != "search" {
		t.Errorf("functionResponse name: got %q, want search", last.Parts[0].FunctionResponse.Name)
	}
}

func TestFromUpstream_FunctionCallResponse(t *testing.T) {
	body := types.GeminiResponse{
		Candidates: []types.GeminiCandidate{{
			Content: types.GeminiContent{
				Role: "model",
				Parts: []types.GeminiPart{{
					FunctionCall: &types.GeminiFunctionCall{
						Name: "search",
						Args: json.RawMessage(`{"q":"Go"}`),
					},
				}},
			},
			FinishReason: "STOP",
		}},
		UsageMetadata: types.GeminiUsageMetadata{PromptTokenCount: 5, CandidatesTokenCount: 10},
	}
	b, _ := json.Marshal(body)
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b))}

	got, err := translator.NewGemini("gemini-2.5-flash").FromUpstream(resp)
	if err != nil {
		t.Fatal(err)
	}
	if got.StopReason != "tool_use" {
		t.Errorf("stop_reason: got %q, want tool_use", got.StopReason)
	}
	if len(got.Content) == 0 || got.Content[0].Type != "tool_use" {
		t.Fatalf("expected tool_use content block, got %+v", got.Content)
	}
	if got.Content[0].Name != "search" {
		t.Errorf("tool_use name: got %q, want search", got.Content[0].Name)
	}
	if !contains(got.Content[0].ID, "toolu_") {
		t.Errorf("tool_use ID should have toolu_ prefix, got %q", got.Content[0].ID)
	}
}

// --- FromUpstream tests ---

func makeGeminiResp(text, finishReason string, promptTokens, candidateTokens int) *http.Response {
	body := types.GeminiResponse{
		Candidates: []types.GeminiCandidate{
			{
				Content: types.GeminiContent{
					Role:  "model",
					Parts: []types.GeminiPart{{Text: text}},
				},
				FinishReason: finishReason,
			},
		},
		UsageMetadata: types.GeminiUsageMetadata{
			PromptTokenCount:     promptTokens,
			CandidatesTokenCount: candidateTokens,
		},
	}
	b, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(b)),
	}
}

func TestFromUpstream_TextResponse(t *testing.T) {
	resp := makeGeminiResp("Hello, world!", "STOP", 10, 5)
	got, err := geminiTranslator().FromUpstream(resp)
	if err != nil {
		t.Fatalf("FromUpstream: %v", err)
	}
	if got.Role != "assistant" {
		t.Errorf("role: got %q, want assistant", got.Role)
	}
	if got.StopReason != "end_turn" {
		t.Errorf("stop_reason: got %q, want end_turn", got.StopReason)
	}
	if len(got.Content) == 0 || got.Content[0].Text != "Hello, world!" {
		t.Errorf("content: %+v", got.Content)
	}
	if got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 5 {
		t.Errorf("usage: %+v", got.Usage)
	}
}

func TestFromUpstream_MaxTokensFinishReason(t *testing.T) {
	resp := makeGeminiResp("truncated", "MAX_TOKENS", 5, 3)
	got, err := geminiTranslator().FromUpstream(resp)
	if err != nil {
		t.Fatal(err)
	}
	if got.StopReason != "max_tokens" {
		t.Errorf("stop_reason: got %q, want max_tokens", got.StopReason)
	}
}

func TestFromUpstream_GeminiErrorPropagated(t *testing.T) {
	body := types.GeminiResponse{
		Error: &types.GeminiError{Code: 429, Status: "RESOURCE_EXHAUSTED", Message: "quota exceeded"},
	}
	b, _ := json.Marshal(body)
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(bytes.NewReader(b)),
	}
	_, err := geminiTranslator().FromUpstream(resp)
	if err == nil {
		t.Fatal("expected error from Gemini error response, got nil")
	}
}

func TestFromUpstream_IDFormat(t *testing.T) {
	resp := makeGeminiResp("hi", "STOP", 1, 1)
	got, err := geminiTranslator().FromUpstream(resp)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ID) < 5 || got.ID[:4] != "msg_" {
		t.Errorf("ID format wrong: %q", got.ID)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// ── G-01: GenerationConfig sampling params ────────────────────────────────

func TestToUpstream_Temperature_ForwardedToGenerationConfig(t *testing.T) {
	temp := 0.2
	req := &types.MessageRequest{
		Model:       "claude-haiku",
		MaxTokens:   50,
		Temperature: &temp,
		Messages:    []types.Message{{Role: "user", Content: rawString("hi")}},
	}
	gr := decodeGeminiBody(t, req)
	if gr.GenerationConfig == nil || gr.GenerationConfig.Temperature == nil {
		t.Fatal("expected generationConfig.temperature to be set")
	}
	if *gr.GenerationConfig.Temperature != 0.2 {
		t.Errorf("temperature: got %v, want 0.2", *gr.GenerationConfig.Temperature)
	}
}

func TestToUpstream_NilTemperature_AbsentFromGenerationConfig(t *testing.T) {
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 50,
		Messages:  []types.Message{{Role: "user", Content: rawString("hi")}},
	}
	gr := decodeGeminiBody(t, req)
	if gr.GenerationConfig != nil && gr.GenerationConfig.Temperature != nil {
		t.Errorf("temperature should be nil when not set, got %v", gr.GenerationConfig.Temperature)
	}
}

func TestToUpstream_StopSequences_Forwarded(t *testing.T) {
	req := &types.MessageRequest{
		Model:         "claude-haiku",
		MaxTokens:     50,
		StopSequences: []string{"END", "STOP"},
		Messages:      []types.Message{{Role: "user", Content: rawString("hi")}},
	}
	gr := decodeGeminiBody(t, req)
	if gr.GenerationConfig == nil || len(gr.GenerationConfig.StopSequences) != 2 {
		t.Fatalf("expected 2 stop sequences, got %+v", gr.GenerationConfig)
	}
	if gr.GenerationConfig.StopSequences[0] != "END" {
		t.Errorf("first stop sequence: got %q, want END", gr.GenerationConfig.StopSequences[0])
	}
}

// ── G-02: Thought parts filtered ─────────────────────────────────────────

func TestFromUpstream_ThoughtPart_FilteredFromResponse(t *testing.T) {
	body := types.GeminiResponse{
		Candidates: []types.GeminiCandidate{{
			Content: types.GeminiContent{
				Role: "model",
				Parts: []types.GeminiPart{
					{Text: "thinking...", Thought: true}, // must be dropped
					{Text: "actual answer"},
				},
			},
			FinishReason: "STOP",
		}},
		UsageMetadata: types.GeminiUsageMetadata{PromptTokenCount: 5, CandidatesTokenCount: 3},
	}
	b, _ := json.Marshal(body)
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b))}

	got, err := geminiTranslator().FromUpstream(resp)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Content) != 1 {
		t.Fatalf("expected 1 content block (thought filtered), got %d: %+v", len(got.Content), got.Content)
	}
	if got.Content[0].Text != "actual answer" {
		t.Errorf("text: got %q, want 'actual answer'", got.Content[0].Text)
	}
}

// ── G-03: Safety filter refusal ──────────────────────────────────────────

func TestFromUpstream_SafetyFilter_ReturnsRefusalResponse(t *testing.T) {
	body := types.GeminiResponse{
		PromptFeedback: &types.GeminiPromptFeedback{BlockReason: "SAFETY"},
		UsageMetadata:  types.GeminiUsageMetadata{PromptTokenCount: 10},
	}
	b, _ := json.Marshal(body)
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b))}

	got, err := geminiTranslator().FromUpstream(resp)
	if err != nil {
		t.Fatalf("expected no error for safety refusal, got: %v", err)
	}
	if got.StopReason != "end_turn" {
		t.Errorf("stop_reason: got %q, want end_turn (content_filter maps to end_turn in Anthropic format)", got.StopReason)
	}
	if len(got.Content) == 0 || got.Content[0].Type != "text" {
		t.Errorf("expected text content block in safety-blocked response, got %+v", got.Content)
	}
	if !contains(got.Content[0].Text, "SAFETY") {
		t.Errorf("safety block text should mention block reason, got %q", got.Content[0].Text)
	}
}

// ── G-04: finishReason completeness ──────────────────────────────────────

func TestFromUpstream_FinishReasonToolCode_MapsToToolUse(t *testing.T) {
	body := types.GeminiResponse{
		Candidates: []types.GeminiCandidate{{
			Content:      types.GeminiContent{Role: "model", Parts: []types.GeminiPart{{Text: "calling"}}},
			FinishReason: "TOOL_CODE",
		}},
		UsageMetadata: types.GeminiUsageMetadata{},
	}
	b, _ := json.Marshal(body)
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b))}

	got, err := geminiTranslator().FromUpstream(resp)
	if err != nil {
		t.Fatal(err)
	}
	if got.StopReason != "tool_use" {
		t.Errorf("stop_reason for TOOL_CODE: got %q, want tool_use", got.StopReason)
	}
}

func TestFromUpstream_FinishReasonSafety_MapsToEndTurn(t *testing.T) {
	body := types.GeminiResponse{
		Candidates: []types.GeminiCandidate{{
			Content:      types.GeminiContent{Role: "model", Parts: []types.GeminiPart{{Text: "..."}}},
			FinishReason: "SAFETY",
		}},
		UsageMetadata: types.GeminiUsageMetadata{},
	}
	b, _ := json.Marshal(body)
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b))}

	got, err := geminiTranslator().FromUpstream(resp)
	if err != nil {
		t.Fatal(err)
	}
	if got.StopReason != "end_turn" {
		t.Errorf("stop_reason for SAFETY: got %q, want end_turn", got.StopReason)
	}
}

// ── G-05: UpstreamError type ─────────────────────────────────────────────

func TestFromUpstream_GeminiError429_ReturnsUpstreamError429(t *testing.T) {
	body := types.GeminiResponse{
		Error: &types.GeminiError{Code: 429, Status: "RESOURCE_EXHAUSTED", Message: "quota exceeded"},
	}
	b, _ := json.Marshal(body)
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b))}

	_, err := geminiTranslator().FromUpstream(resp)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var upErr *irc.UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("expected *irc.UpstreamError, got %T: %v", err, err)
	}
	if upErr.HTTPStatus != 429 {
		t.Errorf("HTTPStatus: got %d, want 429", upErr.HTTPStatus)
	}
}

func TestFromUpstream_GeminiError400_ReturnsUpstreamError400(t *testing.T) {
	body := types.GeminiResponse{
		Error: &types.GeminiError{Code: 400, Status: "INVALID_ARGUMENT", Message: "bad request"},
	}
	b, _ := json.Marshal(body)
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b))}

	_, err := geminiTranslator().FromUpstream(resp)
	var upErr *irc.UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("expected *irc.UpstreamError, got %T", err)
	}
	if upErr.HTTPStatus != 400 {
		t.Errorf("HTTPStatus: got %d, want 400", upErr.HTTPStatus)
	}
}

// ── G-06: tool_choice "tool" → allowedFunctionNames ──────────────────────

func TestToUpstream_ToolChoiceTool_SetsAllowedFunctionNames(t *testing.T) {
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 10,
		Messages:  []types.Message{{Role: "user", Content: rawString("call search")}},
		Tools: []types.Tool{{
			Name:        "search",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		}},
		ToolChoice: &types.ToolChoice{Type: "tool", Name: "search"},
	}
	gr := decodeGeminiBody(t, req)

	if gr.ToolConfig == nil {
		t.Fatal("expected toolConfig to be set")
	}
	fcc := gr.ToolConfig.FunctionCallingConfig
	if fcc.Mode != "ANY" {
		t.Errorf("mode: got %q, want ANY", fcc.Mode)
	}
	if len(fcc.AllowedFunctionNames) != 1 || fcc.AllowedFunctionNames[0] != "search" {
		t.Errorf("allowedFunctionNames: got %v, want [search]", fcc.AllowedFunctionNames)
	}
}

func TestToUpstream_ToolChoiceAny_NoAllowedFunctionNames(t *testing.T) {
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 10,
		Messages:  []types.Message{{Role: "user", Content: rawString("hi")}},
		Tools: []types.Tool{{
			Name:        "search",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		}},
		ToolChoice: &types.ToolChoice{Type: "any"},
	}
	gr := decodeGeminiBody(t, req)

	if gr.ToolConfig == nil {
		t.Fatal("expected toolConfig to be set")
	}
	if len(gr.ToolConfig.FunctionCallingConfig.AllowedFunctionNames) != 0 {
		t.Errorf("allowedFunctionNames should be empty for 'any', got %v",
			gr.ToolConfig.FunctionCallingConfig.AllowedFunctionNames)
	}
}

// ── G-09: BatchTool filtering ─────────────────────────────────────────────

func TestToUpstream_BatchTool_FilteredFromDeclarations(t *testing.T) {
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 10,
		Messages:  []types.Message{{Role: "user", Content: rawString("hi")}},
		Tools: []types.Tool{
			{Name: "BatchTool", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
			{Name: "search", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
		},
	}
	gr := decodeGeminiBody(t, req)

	if len(gr.Tools) == 0 {
		t.Fatal("expected tools to be set after filtering BatchTool")
	}
	for _, decl := range gr.Tools[0].FunctionDeclarations {
		if decl.Name == "BatchTool" {
			t.Errorf("BatchTool should be filtered from functionDeclarations")
		}
	}
	if len(gr.Tools[0].FunctionDeclarations) != 1 || gr.Tools[0].FunctionDeclarations[0].Name != "search" {
		t.Errorf("expected only 'search' declaration, got %+v", gr.Tools[0].FunctionDeclarations)
	}
}

func TestToUpstream_OnlyBatchTool_NoToolsInRequest(t *testing.T) {
	req := &types.MessageRequest{
		Model:     "claude-haiku",
		MaxTokens: 10,
		Messages:  []types.Message{{Role: "user", Content: rawString("hi")}},
		Tools: []types.Tool{
			{Name: "BatchTool", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
		},
	}
	gr := decodeGeminiBody(t, req)

	if len(gr.Tools) != 0 {
		t.Errorf("expected no tools when only BatchTool present, got %+v", gr.Tools)
	}
}

// ── G-10: Malformed tool call arg rectification ──────────────────────────

func TestFromUpstream_StringWrappedArgs_Rectified(t *testing.T) {
	// Relay channels occasionally return args as a string-encoded JSON object.
	body := types.GeminiResponse{
		Candidates: []types.GeminiCandidate{{
			Content: types.GeminiContent{
				Role: "model",
				Parts: []types.GeminiPart{{
					FunctionCall: &types.GeminiFunctionCall{
						Name: "search",
						Args: json.RawMessage(`"{\"q\":\"Go\"}"`), // string-wrapped object
					},
				}},
			},
			FinishReason: "STOP",
		}},
		UsageMetadata: types.GeminiUsageMetadata{},
	}
	b, _ := json.Marshal(body)
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b))}

	got, err := geminiTranslator().FromUpstream(resp)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Content) == 0 || got.Content[0].Type != "tool_use" {
		t.Fatalf("expected tool_use block, got %+v", got.Content)
	}
	// Input must be a JSON object after rectification.
	var input map[string]any
	if err := json.Unmarshal(got.Content[0].Input, &input); err != nil {
		t.Errorf("tool input should be a JSON object after rectification, got %q: %v",
			got.Content[0].Input, err)
	}
	if input["q"] != "Go" {
		t.Errorf("input[q]: got %v, want Go", input["q"])
	}
}
