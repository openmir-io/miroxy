package irc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"miroxy/core/ir"
)

// --- request builder tests ---

func TestOpenAIRequestToProvider_BasicMessage(t *testing.T) {
	conv := NewOpenAIConverter("gpt-4o")
	irReq := &ir.IRRequest{
		System: "You are helpful.",
		Messages: []ir.IRMessage{
			{Role: "user", Parts: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: "Hello"}}}},
		},
		Gen: ir.IRGenerationConfig{MaxTokens: 100},
	}
	body, err := conv.RequestToProvider(irReq)
	if err != nil {
		t.Fatalf("RequestToProvider: %v", err)
	}

	var req oaiRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if req.Model != "gpt-4o" {
		t.Errorf("model: got %q, want gpt-4o", req.Model)
	}
	// messages: system + user
	if len(req.Messages) != 2 {
		t.Fatalf("messages len: got %d, want 2", len(req.Messages))
	}
	if req.Messages[0].Role != "system" {
		t.Errorf("first message role: got %q, want system", req.Messages[0].Role)
	}
	if *req.Messages[0].Content != "You are helpful." {
		t.Errorf("system content: got %q", *req.Messages[0].Content)
	}
	if req.Messages[1].Role != "user" {
		t.Errorf("second message role: got %q, want user", req.Messages[1].Role)
	}
	if *req.Messages[1].Content != "Hello" {
		t.Errorf("user content: got %q", *req.Messages[1].Content)
	}
	if req.MaxTokens != 100 {
		t.Errorf("max_tokens: got %d, want 100", req.MaxTokens)
	}
}

func TestOpenAIRequestToProvider_NoSystem(t *testing.T) {
	conv := NewOpenAIConverter("gpt-4o-mini")
	irReq := &ir.IRRequest{
		Messages: []ir.IRMessage{
			{Role: "user", Parts: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: "Hi"}}}},
		},
	}
	body, _ := conv.RequestToProvider(irReq)

	var req oaiRequest
	json.Unmarshal(body, &req)

	// No system message — only the user message.
	if len(req.Messages) != 1 {
		t.Errorf("messages len: got %d, want 1", len(req.Messages))
	}
	if req.Messages[0].Role != "user" {
		t.Errorf("role: got %q, want user", req.Messages[0].Role)
	}
}

func TestOpenAIRequestToProvider_StreamOptions(t *testing.T) {
	conv := NewOpenAIConverter("gpt-4o")
	irReq := &ir.IRRequest{
		Stream:   true,
		Messages: []ir.IRMessage{{Role: "user", Parts: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: "Hi"}}}}},
	}
	body, _ := conv.RequestToProvider(irReq)

	var req oaiRequest
	json.Unmarshal(body, &req)

	if !req.Stream {
		t.Error("stream: want true")
	}
	if req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage: want true")
	}
}

func TestOpenAIRequestToProvider_ToolChoice(t *testing.T) {
	conv := NewOpenAIConverter("gpt-4o")
	cases := []struct {
		irChoice *ir.IRToolChoice
		wantJSON string
	}{
		{&ir.IRToolChoice{Type: "auto"}, `"auto"`},
		{&ir.IRToolChoice{Type: "any"}, `"required"`},
		{&ir.IRToolChoice{Type: "none"}, `"none"`},
		{&ir.IRToolChoice{Type: "tool", Name: "myFunc"}, `{"type":"function","function":{"name":"myFunc"}}`},
	}

	for _, tc := range cases {
		irReq := &ir.IRRequest{
			Messages:   []ir.IRMessage{{Role: "user", Parts: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: "x"}}}}},
			Tools:      []ir.IRTool{{Name: "myFunc", InputSchemaJSON: []byte(`{}`)}},
			ToolChoice: tc.irChoice,
		}
		body, _ := conv.RequestToProvider(irReq)
		if !strings.Contains(string(body), tc.wantJSON) {
			t.Errorf("tool_choice %+v: want %s in %s", tc.irChoice, tc.wantJSON, string(body))
		}
	}
}

func TestOpenAIRequestToProvider_MultiTurnToolUse(t *testing.T) {
	// Simulates: user → assistant(tool_call) → user(tool_result).
	conv := NewOpenAIConverter("gpt-4o")
	irReq := &ir.IRRequest{
		Messages: []ir.IRMessage{
			{Role: "user", Parts: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: "What is 2+2?"}}}},
			{Role: "assistant", Parts: []ir.IRContentPart{
				{Text: &ir.IRTextPart{Text: "Let me calculate."}},
				{ToolUse: &ir.IRToolUsePart{ID: "call_1", Name: "calc", InputJSON: []byte(`{"expr":"2+2"}`)}},
			}},
			{Role: "user", Parts: []ir.IRContentPart{
				{ToolResult: &ir.IRToolResultPart{ToolUseID: "call_1", Content: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: "4"}}}}},
			}},
		},
	}
	body, _ := conv.RequestToProvider(irReq)

	var req oaiRequest
	json.Unmarshal(body, &req)

	// Expected: user("What is 2+2?"), assistant(text+tool_calls), tool(result)
	if len(req.Messages) != 3 {
		t.Fatalf("messages len: got %d, want 3 — %s", len(req.Messages), string(body))
	}

	asst := req.Messages[1]
	if asst.Role != "assistant" {
		t.Errorf("second msg role: got %q, want assistant", asst.Role)
	}
	if asst.Content == nil || *asst.Content != "Let me calculate." {
		t.Errorf("assistant content: got %v", asst.Content)
	}
	if len(asst.ToolCalls) != 1 {
		t.Fatalf("tool_calls len: got %d, want 1", len(asst.ToolCalls))
	}
	if asst.ToolCalls[0].ID != "call_1" {
		t.Errorf("tool call id: got %q, want call_1", asst.ToolCalls[0].ID)
	}
	if asst.ToolCalls[0].Function.Name != "calc" {
		t.Errorf("tool call name: got %q, want calc", asst.ToolCalls[0].Function.Name)
	}

	toolMsg := req.Messages[2]
	if toolMsg.Role != "tool" {
		t.Errorf("third msg role: got %q, want tool", toolMsg.Role)
	}
	if toolMsg.ToolCallID != "call_1" {
		t.Errorf("tool_call_id: got %q, want call_1", toolMsg.ToolCallID)
	}
	if *toolMsg.Content != "4" {
		t.Errorf("tool result content: got %q, want 4", *toolMsg.Content)
	}
}

func TestOpenAIRequestToProvider_AssistantToolCallOnly(t *testing.T) {
	// Assistant message with only tool_use (no text) → content must be null.
	conv := NewOpenAIConverter("gpt-4o")
	irReq := &ir.IRRequest{
		Messages: []ir.IRMessage{
			{Role: "user", Parts: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: "x"}}}},
			{Role: "assistant", Parts: []ir.IRContentPart{
				{ToolUse: &ir.IRToolUsePart{ID: "call_2", Name: "fn", InputJSON: []byte(`{}`)}},
			}},
		},
	}
	body, _ := conv.RequestToProvider(irReq)

	// The assistant message content must be "null", not omitted.
	if !strings.Contains(string(body), `"content":null`) {
		t.Errorf("expected content:null in body: %s", string(body))
	}
}

// --- ResponseToIR tests ---

func TestOpenAIResponseToIR_TextContent(t *testing.T) {
	conv := NewOpenAIConverter("gpt-4o")
	respBody := `{
		"id": "chatcmpl-1",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "Hello there!"}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5}
	}`
	irResp, err := conv.ResponseToIR([]byte(respBody))
	if err != nil {
		t.Fatalf("ResponseToIR: %v", err)
	}
	if irResp.StopReason != ir.IRStopReasonStop {
		t.Errorf("stop_reason: got %q, want stop", irResp.StopReason)
	}
	if len(irResp.Content) != 1 || irResp.Content[0].Text == nil {
		t.Fatalf("content: got %+v", irResp.Content)
	}
	if irResp.Content[0].Text.Text != "Hello there!" {
		t.Errorf("text: got %q", irResp.Content[0].Text.Text)
	}
	if irResp.Usage.InputTokens != 10 || irResp.Usage.OutputTokens != 5 {
		t.Errorf("usage: got %+v", irResp.Usage)
	}
}

func TestOpenAIResponseToIR_ToolCall(t *testing.T) {
	conv := NewOpenAIConverter("gpt-4o")
	respBody := `{
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [{"id": "call_xyz", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"NYC\"}"}}]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 20, "completion_tokens": 15}
	}`
	irResp, err := conv.ResponseToIR([]byte(respBody))
	if err != nil {
		t.Fatalf("ResponseToIR: %v", err)
	}
	if irResp.StopReason != ir.IRStopReasonToolUse {
		t.Errorf("stop_reason: got %q, want tool_use", irResp.StopReason)
	}
	if len(irResp.Content) != 1 || irResp.Content[0].ToolUse == nil {
		t.Fatalf("content: got %+v", irResp.Content)
	}
	tc := irResp.Content[0].ToolUse
	if tc.ID != "call_xyz" {
		t.Errorf("tool id: got %q", tc.ID)
	}
	if tc.Name != "get_weather" {
		t.Errorf("tool name: got %q", tc.Name)
	}
	if string(tc.InputJSON) != `{"city":"NYC"}` {
		t.Errorf("tool args: got %s", tc.InputJSON)
	}
}

func TestOpenAIResponseToIR_FinishReasonMapping(t *testing.T) {
	conv := NewOpenAIConverter("gpt-4o")
	cases := []struct {
		reason   string
		wantStop ir.IRStopReason
	}{
		{"stop", ir.IRStopReasonStop},
		{"length", ir.IRStopReasonMaxTokens},
		{"content_filter", ir.IRStopReasonContentFilter},
		{"function_call", ir.IRStopReasonToolUse},
		{"", ir.IRStopReasonStop},
	}
	for _, tc := range cases {
		body := `{"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"` + tc.reason + `"}],"usage":{}}`
		if tc.reason == "" {
			body = `{"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":""}],"usage":{}}`
		}
		resp, err := conv.ResponseToIR([]byte(body))
		if err != nil {
			t.Errorf("reason %q: ResponseToIR error: %v", tc.reason, err)
			continue
		}
		if resp.StopReason != tc.wantStop {
			t.Errorf("reason %q: got %q, want %q", tc.reason, resp.StopReason, tc.wantStop)
		}
	}
}

func TestOpenAIResponseToIR_BodyError(t *testing.T) {
	conv := NewOpenAIConverter("gpt-4o")
	body := `{"error":{"message":"Invalid API key","type":"invalid_request_error","code":"invalid_api_key"}}`
	_, err := conv.ResponseToIR([]byte(body))
	if err == nil {
		t.Fatal("want error for body-level error response")
	}
	var ue *UpstreamError
	if !isUpstreamError(err, &ue) {
		t.Fatalf("want *UpstreamError, got %T: %v", err, err)
	}
	if ue.HTTPStatus != 400 {
		t.Errorf("http status: got %d, want 400", ue.HTTPStatus)
	}
}

func isUpstreamError(err error, target **UpstreamError) bool {
	if ue, ok := err.(*UpstreamError); ok {
		*target = ue
		return true
	}
	return false
}

// --- StreamToIR tests ---

func collectStream(ch <-chan ir.StreamEvent) []ir.StreamEvent {
	var events []ir.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

func TestOpenAIStreamToIR_TextOnly(t *testing.T) {
	conv := NewOpenAIConverter("gpt-4o")
	sse := `data: {"id":"1","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"1","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"1","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}

data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"1","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}

data: [DONE]

`
	events := collectStream(conv.StreamToIR(context.Background(), strings.NewReader(sse)))

	kinds := make([]ir.StreamEventKind, len(events))
	for i, e := range events {
		kinds[i] = e.Kind
	}
	want := []ir.StreamEventKind{
		ir.EvStreamStart,
		ir.EvContentBlockStart,
		ir.EvTextDelta,
		ir.EvTextDelta,
		ir.EvContentBlockEnd,
		ir.EvFinish,
		ir.EvUsage,
		ir.EvStreamEnd,
	}
	if len(kinds) != len(want) {
		t.Fatalf("event kinds: got %v, want %v", kinds, want)
	}
	for i, k := range kinds {
		if k != want[i] {
			t.Errorf("event[%d]: got %q, want %q", i, k, want[i])
		}
	}

	// Check text content.
	if events[2].TextDelta.Text != "Hello" {
		t.Errorf("first delta: got %q", events[2].TextDelta.Text)
	}
	if events[3].TextDelta.Text != " world" {
		t.Errorf("second delta: got %q", events[3].TextDelta.Text)
	}

	// Check finish reason.
	if events[5].Finish.StopReason != ir.IRStopReasonStop {
		t.Errorf("stop_reason: got %q", events[5].Finish.StopReason)
	}

	// Check usage.
	if events[6].Usage.InputTokens != 5 || events[6].Usage.OutputTokens != 2 {
		t.Errorf("usage: got %+v", events[6].Usage)
	}
}

func TestOpenAIStreamToIR_ToolCall(t *testing.T) {
	conv := NewOpenAIConverter("gpt-4o")
	sse := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"NYC\"}"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	events := collectStream(conv.StreamToIR(context.Background(), strings.NewReader(sse)))

	var kinds []ir.StreamEventKind
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}

	// Expected: StreamStart, ToolCallStart, ToolCallDelta×2, ContentBlockEnd, Finish, Usage, StreamEnd
	wantContains := map[ir.StreamEventKind]bool{
		ir.EvStreamStart:    true,
		ir.EvToolCallStart:  true,
		ir.EvToolCallDelta:  true,
		ir.EvContentBlockEnd: true,
		ir.EvFinish:         true,
		ir.EvStreamEnd:      true,
	}
	seen := map[ir.StreamEventKind]bool{}
	for _, k := range kinds {
		seen[k] = true
	}
	for want := range wantContains {
		if !seen[want] {
			t.Errorf("missing event kind %q in %v", want, kinds)
		}
	}

	// Verify tool call start has correct name and id.
	for _, ev := range events {
		if ev.Kind == ir.EvToolCallStart {
			if ev.ToolCallStart.ID != "call_1" {
				t.Errorf("tool id: got %q, want call_1", ev.ToolCallStart.ID)
			}
			if ev.ToolCallStart.Name != "get_weather" {
				t.Errorf("tool name: got %q, want get_weather", ev.ToolCallStart.Name)
			}
		}
	}

	// Finish reason should be tool_use.
	for _, ev := range events {
		if ev.Kind == ir.EvFinish {
			if ev.Finish.StopReason != ir.IRStopReasonToolUse {
				t.Errorf("stop_reason: got %q, want tool_use", ev.Finish.StopReason)
			}
		}
	}
}

func TestOpenAIStreamToIR_EmptyResponse(t *testing.T) {
	// Provider sends finish_reason with no content — should emit the empty-block fallback.
	conv := NewOpenAIConverter("gpt-4o")
	sse := `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	events := collectStream(conv.StreamToIR(context.Background(), strings.NewReader(sse)))

	hasStart := false
	hasEnd := false
	for _, ev := range events {
		if ev.Kind == ir.EvContentBlockStart {
			hasStart = true
		}
		if ev.Kind == ir.EvContentBlockEnd {
			hasEnd = true
		}
	}
	if !hasStart || !hasEnd {
		t.Errorf("empty response fallback: want ContentBlockStart+End, got %v", events)
	}
}

// --- GLM-specific tests ---

func TestGLMConverter_TemperatureClamp(t *testing.T) {
	conv := NewGLMConverter("glm-4")
	temp := 1.5 // GLM max is 1.0
	irReq := &ir.IRRequest{
		Messages: []ir.IRMessage{{Role: "user", Parts: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: "hi"}}}}},
		Gen:      ir.IRGenerationConfig{Temperature: &temp},
	}
	body, _ := conv.RequestToProvider(irReq)

	var req oaiRequest
	json.Unmarshal(body, &req)

	if req.Temperature == nil {
		t.Fatal("temperature: got nil")
	}
	if *req.Temperature != 1.0 {
		t.Errorf("temperature: got %.2f, want 1.0 (clamped)", *req.Temperature)
	}
}

func TestGLMConverter_FinishReasonNormalization(t *testing.T) {
	conv := NewGLMConverter("glm-4")
	cases := []struct {
		reason   string
		wantStop ir.IRStopReason
	}{
		{"network_error", ir.IRStopReasonStop},
		{"sensitive", ir.IRStopReasonContentFilter},
		{"stop", ir.IRStopReasonStop},
		{"length", ir.IRStopReasonMaxTokens},
	}
	for _, tc := range cases {
		body := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"` + tc.reason + `"}],"usage":{}}`)
		resp, err := conv.ResponseToIR(body)
		if err != nil {
			t.Errorf("reason %q: error %v", tc.reason, err)
			continue
		}
		if resp.StopReason != tc.wantStop {
			t.Errorf("reason %q: got %q, want %q", tc.reason, resp.StopReason, tc.wantStop)
		}
	}
}

func TestGLMConverter_TemperatureWithinRange(t *testing.T) {
	// Temperature within [0, 1] must not be altered.
	conv := NewGLMConverter("glm-4")
	temp := 0.7
	irReq := &ir.IRRequest{
		Messages: []ir.IRMessage{{Role: "user", Parts: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: "hi"}}}}},
		Gen:      ir.IRGenerationConfig{Temperature: &temp},
	}
	body, _ := conv.RequestToProvider(irReq)

	var req oaiRequest
	json.Unmarshal(body, &req)

	if req.Temperature == nil || *req.Temperature != 0.7 {
		t.Errorf("temperature: got %v, want 0.7", req.Temperature)
	}
}

// --- NewOpenAICompatConverter: verify provider label and identical wire format ---

func TestOpenAICompatConverter_ProviderLabel(t *testing.T) {
	// OpenAI-compat providers (deepseek, grok, together…) share OpenAIConverter
	// and are distinguished only by their provider label — no separate IRC file needed.
	cases := []struct{ provider string }{
		{"deepseek"}, {"grok"}, {"together"}, {"groq"},
	}
	for _, tc := range cases {
		conv := NewOpenAICompatConverter("some-model", tc.provider)
		if conv.Provider() != tc.provider {
			t.Errorf("provider %q: Provider() = %q", tc.provider, conv.Provider())
		}
	}
}

func TestOpenAICompatConverter_WireFormatIdentical(t *testing.T) {
	// The wire format produced by any OpenAI-compat converter must be identical
	// to the base OpenAI converter — the provider label has no effect on the body.
	irReq := &ir.IRRequest{
		System:   "sys",
		Messages: []ir.IRMessage{{Role: "user", Parts: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: "hi"}}}}},
		Gen:      ir.IRGenerationConfig{MaxTokens: 50},
	}
	baseBody, _ := NewOpenAIConverter("model-a").RequestToProvider(irReq)
	dsBody, _ := NewOpenAICompatConverter("model-a", "deepseek").RequestToProvider(irReq)

	var base, ds oaiRequest
	json.Unmarshal(baseBody, &base)
	json.Unmarshal(dsBody, &ds)

	if base.MaxTokens != ds.MaxTokens || len(base.Messages) != len(ds.Messages) {
		t.Errorf("wire format differs: base=%+v compat=%+v", base, ds)
	}
}
