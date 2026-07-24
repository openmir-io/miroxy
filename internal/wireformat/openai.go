package wireformat

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"miroxy/core/ir"
	"miroxy/internal/idgen"
	"miroxy/internal/types"
)

// OpenAI-compatible wire types (unexported — internal to irc package).
// Shared by OpenAIConverter, DeepSeekConverter, and GLMConverter.

type oaiRequest struct {
	Model         string         `json:"model"`
	Messages      []oaiMessage   `json:"messages"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Stop          []string       `json:"stop,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *oaiStreamOpts `json:"stream_options,omitempty"`
	Tools         []oaiTool      `json:"tools,omitempty"`
	ToolChoice    any            `json:"tool_choice,omitempty"`
}

type oaiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"` // string or array-of-parts (OpenAI spec); nil → JSON null (valid for assistant+tool_calls)
	ToolCalls  []oaiToolCall   `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// oaiContentText extracts the concatenated text from an OpenAI message
// content field. Per spec this may be a plain JSON string or an array of
// content-part objects — some SDKs (e.g. openai-python, used by smolagents)
// always normalize to array form, even for pure text. Non-text parts are
// ignored; callers that need images use oaiContentBlocks instead.
func oaiContentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// oaiContentBlocks converts an OpenAI user-message content field into
// Anthropic content blocks, preserving both text and image_url parts.
func oaiContentBlocks(raw json.RawMessage) []types.ContentBlock {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []types.ContentBlock{{Type: "text", Text: s}}
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	var blocks []types.ContentBlock
	for _, p := range parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, types.ContentBlock{Type: "text", Text: p.Text})
		case "image_url":
			blocks = append(blocks, imageBlockFromOAIURL(p.ImageURL.URL))
		}
	}
	return blocks
}

// imageBlockFromOAIURL converts an OpenAI image_url.url — either a data:
// URI with inline base64, or a real http(s) URL — into an Anthropic image
// content block.
func imageBlockFromOAIURL(url string) types.ContentBlock {
	if mediaType, payload, ok := parseDataURI(url); ok {
		return types.ContentBlock{Type: "image", Source: &types.ImageSource{Type: "base64", MediaType: mediaType, Data: payload}}
	}
	return types.ContentBlock{Type: "image", Source: &types.ImageSource{Type: "url", URL: url}}
}

// parseDataURI splits a "data:<mediaType>;base64,<payload>" URI. ok is
// false for anything else (e.g. a plain http(s) URL).
func parseDataURI(s string) (mediaType, payload string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(s, prefix) {
		return "", "", false
	}
	rest := s[len(prefix):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false
	}
	meta, payload := rest[:comma], rest[comma+1:]
	return strings.TrimSuffix(meta, ";base64"), payload, true
}

// oaiStringContent marshals a plain string into the RawMessage shape
// oaiMessage.Content expects — used on the encode direction, where miroxy
// always writes plain string content to a real upstream (never the
// array-of-parts form, which only appears on requests miroxy decodes).
func oaiStringContent(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

type oaiTool struct {
	Type     string     `json:"type"` // always "function"
	Function oaiFuncDef `json:"function"`
}

type oaiFuncDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type oaiToolCall struct {
	ID       string      `json:"id,omitempty"`
	Type     string      `json:"type,omitempty"` // "function"
	Index    int         `json:"index,omitempty"`
	Function oaiFuncCall `json:"function"`
}

type oaiFuncCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type oaiToolChoiceFunc struct {
	Type     string          `json:"type"` // "function"
	Function oaiToolChoiceFn `json:"function"`
}

type oaiToolChoiceFn struct {
	Name string `json:"name"`
}

type oaiStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

// Non-streaming response types.

type oaiResponse struct {
	ID      string      `json:"id"`
	Choices []oaiChoice `json:"choices"`
	Usage   oaiUsage    `json:"usage"`
	Error   *oaiError   `json:"error,omitempty"`
}

type oaiChoice struct {
	Index        int    `json:"index"`
	Message      oaiMsg `json:"message"`
	FinishReason string `json:"finish_reason"`
}

type oaiMsg struct {
	Role      string        `json:"role"`
	Content   *string       `json:"content"`
	ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type oaiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// Streaming chunk types.

type oaiChunk struct {
	ID      string           `json:"id"`
	Choices []oaiChunkChoice `json:"choices"`
	Usage   *oaiUsage        `json:"usage,omitempty"`
}

type oaiChunkChoice struct {
	Index        int      `json:"index"`
	Delta        oaiDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

type oaiDelta struct {
	Role      string        `json:"role,omitempty"`
	Content   *string       `json:"content,omitempty"`
	ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`
	// reasoning_content (DeepSeek R1, GLM thinking) — received but not forwarded to IR.
	// The IR has no reasoning field in v1; the final text answer is what matters.
	ReasoningContent *string `json:"reasoning_content,omitempty"`
	Reasoning        *string `json:"reasoning,omitempty"` // alias used by some providers
}

// --- OpenAIConverter ---

// OpenAIConverter implements UpstreamConverter for any OpenAI-compatible upstream.
// It is the base converter for all OpenAI-compat providers (openai, deepseek, grok,
// together, groq, etc.). Provider-specific behaviour differences are handled either
// by the provider field (used for logging / error attribution) or by embedding this
// struct and overriding specific methods (GLMConverter).
//
// Providers that are wire-identical to OpenAI at the IR level (DeepSeek, Grok,
// Together AI, Groq, …) do NOT need their own converter file. They share this
// implementation and differ only in provider name, api_base, and auth credentials —
// all of which are config-level concerns, not IRC concerns.
type OpenAIConverter struct {
	model    string
	provider string // used in Provider() for logging; defaults to "openai"
}

// NewOpenAIConverter returns a converter for the standard OpenAI upstream.
func NewOpenAIConverter(model string) *OpenAIConverter {
	return &OpenAIConverter{model: model, provider: "openai"}
}

// NewOpenAICompatConverter returns a converter for any OpenAI-compatible upstream.
// Use this when the provider label should differ from "openai" (e.g. "deepseek",
// "grok", "together", "groq") but the wire format is identical to OpenAI's.
// No separate IRC file is needed for these providers.
func NewOpenAICompatConverter(model, provider string) *OpenAIConverter {
	return &OpenAIConverter{model: model, provider: provider}
}

var _ UpstreamConverter = (*OpenAIConverter)(nil)

func (c *OpenAIConverter) Provider() string { return c.provider }

func (c *OpenAIConverter) RequestToProvider(irReq *ir.IRRequest) ([]byte, error) {
	return buildOAIRequestBody(irReq, c.model, 0)
}

func (c *OpenAIConverter) ResponseToIR(body []byte) (*ir.IRResponse, error) {
	return responseToIR(body, identityReason)
}

func (c *OpenAIConverter) StreamToIR(ctx context.Context, body io.Reader) <-chan ir.StreamEvent {
	return streamToIR(ctx, body, identityReason)
}

// identityReason is a no-op finish-reason normalizer for standard OpenAI behavior.
func identityReason(r string) string { return r }

// --- Shared request builder ---

// buildOAIRequestBody converts an IR request to an OpenAI-compatible JSON body.
// maxTemp: if > 0, temperature is clamped to [0, maxTemp] (GLM uses 1.0).
func buildOAIRequestBody(irReq *ir.IRRequest, model string, maxTemp float64) ([]byte, error) {
	req := oaiRequest{
		Model:  model,
		Stream: irReq.Stream,
	}

	// System prompt → first message with role "system".
	var msgs []oaiMessage
	if irReq.System != "" {
		msgs = append(msgs, oaiMessage{Role: "system", Content: oaiStringContent(irReq.System)})
	}
	msgs = append(msgs, convertIRMessagesToOAI(irReq.Messages)...)
	req.Messages = msgs

	// Generation config.
	if irReq.Gen.Temperature != nil {
		t := *irReq.Gen.Temperature
		if maxTemp > 0 && t > maxTemp {
			t = maxTemp
		}
		req.Temperature = &t
	}
	if irReq.Gen.TopP != nil {
		req.TopP = irReq.Gen.TopP
	}
	if irReq.Gen.MaxTokens > 0 {
		req.MaxTokens = irReq.Gen.MaxTokens
	}
	if len(irReq.Gen.StopSeqs) > 0 {
		req.Stop = irReq.Gen.StopSeqs
	}

	// Tools.
	if len(irReq.Tools) > 0 {
		tools := make([]oaiTool, len(irReq.Tools))
		for i, t := range irReq.Tools {
			tools[i] = oaiTool{
				Type: "function",
				Function: oaiFuncDef{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.InputSchemaJSON,
				},
			}
		}
		req.Tools = tools
	}

	// Tool choice.
	if irReq.ToolChoice != nil {
		req.ToolChoice = mapIRToolChoice(irReq.ToolChoice)
	}

	// Streaming usage: request usage metadata on the trailing chunk.
	if irReq.Stream {
		req.StreamOptions = &oaiStreamOpts{IncludeUsage: true}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal openai request: %w", err)
	}
	return body, nil
}

func mapIRToolChoice(tc *ir.IRToolChoice) any {
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return oaiToolChoiceFunc{
			Type:     "function",
			Function: oaiToolChoiceFn{Name: tc.Name},
		}
	default:
		return "auto"
	}
}

// convertIRMessagesToOAI maps IR messages to OpenAI message objects.
//
// Anthropic format:
//   - user turn containing tool_result parts → one role="tool" message per result
//   - user turn text → role="user"
//   - assistant turn with tool_use parts → role="assistant" with tool_calls array
//
// Tool result messages are output before any text from the same user turn, which
// matches the expected sequence: assistant(tool_calls) → tool(results) → user(text).
func convertIRMessagesToOAI(msgs []ir.IRMessage) []oaiMessage {
	var result []oaiMessage
	for _, msg := range msgs {
		switch msg.Role {
		case "user":
			// Tool result parts → role="tool" messages (output first).
			for _, part := range msg.Parts {
				if part.ToolResult == nil {
					continue
				}
				content := extractToolResultText(part.ToolResult.Content)
				result = append(result, oaiMessage{
					Role:       "tool",
					ToolCallID: part.ToolResult.ToolUseID,
					Content:    oaiStringContent(content),
				})
			}
			// Text parts → role="user".
			var textParts []string
			for _, part := range msg.Parts {
				if part.Text != nil {
					textParts = append(textParts, part.Text.Text)
				}
			}
			if len(textParts) > 0 {
				s := strings.Join(textParts, "")
				result = append(result, oaiMessage{Role: "user", Content: oaiStringContent(s)})
			}

		case "assistant":
			var text strings.Builder
			var toolCalls []oaiToolCall
			for _, part := range msg.Parts {
				if part.Text != nil {
					text.WriteString(part.Text.Text)
				}
				if part.ToolUse != nil {
					toolCalls = append(toolCalls, oaiToolCall{
						ID:   part.ToolUse.ID,
						Type: "function",
						Function: oaiFuncCall{
							Name:      part.ToolUse.Name,
							Arguments: string(part.ToolUse.InputJSON),
						},
					})
				}
			}
			m := oaiMessage{Role: "assistant", ToolCalls: toolCalls}
			if s := text.String(); s != "" {
				m.Content = oaiStringContent(s)
			}
			// nil Content with tool_calls marshals as "content": null — correct per OpenAI spec.
			result = append(result, m)
		}
	}
	return result
}

// extractToolResultText flattens IR tool-result content parts to a single string.
func extractToolResultText(parts []ir.IRContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Text != nil {
			b.WriteString(p.Text.Text)
		}
	}
	return b.String()
}

// --- Shared response parser ---

// responseToIR parses an OpenAI-compat non-streaming response body into IR.
// normalizeReason allows provider-specific finish_reason remapping (e.g. GLM).
func responseToIR(body []byte, normalizeReason func(string) string) (*ir.IRResponse, error) {
	var resp oaiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse openai response: %w", err)
	}
	if resp.Error != nil {
		// Body-level error (relay pattern: 200 body contains error object).
		// HTTPStatus defaults to 400; the server's retry loop handles actual HTTP status codes.
		return nil, &UpstreamError{
			HTTPStatus: 400,
			Message:    resp.Error.Message,
		}
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("openai response contains no choices")
	}

	choice := resp.Choices[0]
	msg := choice.Message

	var content []ir.IRResponseBlock
	hasToolUse := false

	if msg.Content != nil && *msg.Content != "" {
		content = append(content, ir.IRResponseBlock{
			Text: &ir.IRTextPart{Text: *msg.Content},
		})
	}
	for _, tc := range msg.ToolCalls {
		if tc.Type != "function" {
			continue
		}
		toolID := tc.ID
		if toolID == "" {
			toolID = idgen.NewToolID()
		}
		content = append(content, ir.IRResponseBlock{
			ToolUse: &ir.IRToolUsePart{
				ID:        toolID,
				Name:      tc.Function.Name,
				InputJSON: []byte(tc.Function.Arguments),
			},
		})
		hasToolUse = true
	}

	return &ir.IRResponse{
		Content:    content,
		StopReason: mapOAIFinishReason(normalizeReason(choice.FinishReason), hasToolUse),
		Usage: ir.IRUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		},
	}, nil
}

// mapOAIFinishReason maps OpenAI finish_reason strings to IR stop reasons.
func mapOAIFinishReason(reason string, hasToolUse bool) ir.IRStopReason {
	if hasToolUse {
		return ir.IRStopReasonToolUse
	}
	switch reason {
	case "tool_calls", "function_call":
		return ir.IRStopReasonToolUse
	case "length":
		return ir.IRStopReasonMaxTokens
	case "content_filter":
		return ir.IRStopReasonContentFilter
	default: // "stop", ""
		return ir.IRStopReasonStop
	}
}

// --- Shared streaming parser ---

// streamToIR parses an OpenAI-compatible SSE stream into IR events.
// normalizeReason allows provider-specific finish_reason remapping before
// IR stop-reason mapping (used by GLM for "network_error" / "sensitive").
//
// OpenAI SSE format: each line is "data: {json}" with no "event:" prefix.
// Stream terminates on "data: [DONE]".
func streamToIR(ctx context.Context, body io.Reader, normalizeReason func(string) string) <-chan ir.StreamEvent {
	out := make(chan ir.StreamEvent, 32)
	go func() {
		defer close(out)

		send := func(ev ir.StreamEvent) bool {
			select {
			case <-ctx.Done():
				return false
			case out <- ev:
				return true
			}
		}

		if !send(ir.StreamEvent{Kind: ir.EvStreamStart, StreamStart: &ir.StreamStart{}}) {
			return
		}

		type toolBlock struct {
			blockIndex int
		}

		blockIndex := -1
		textBlockOpen := false
		// toolBlockList maintains insertion order for deterministic ContentBlockEnd ordering.
		var toolBlockList []*toolBlock
		toolBlocks := map[int]*toolBlock{} // openai tool_call index → block
		hasToolUse := false
		var finishReason string
		var usage ir.IRUsage

		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineSize)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}

			line := scanner.Text()
			if line == "data: [DONE]" {
				break
			}
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok || data == "" {
				continue
			}

			var chunk oaiChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			// Usage-only trailing chunk (choices is empty).
			if chunk.Usage != nil && len(chunk.Choices) == 0 {
				usage.InputTokens = chunk.Usage.PromptTokens
				usage.OutputTokens = chunk.Usage.CompletionTokens
				continue
			}

			for _, choice := range chunk.Choices {
				if choice.FinishReason != nil && *choice.FinishReason != "" {
					finishReason = *choice.FinishReason
				}
				// Some providers attach usage on the finish_reason chunk.
				if chunk.Usage != nil {
					usage.InputTokens = chunk.Usage.PromptTokens
					usage.OutputTokens = chunk.Usage.CompletionTokens
				}

				delta := choice.Delta

				// Text content delta — reasoning_content is ignored (not in IR v1).
				if delta.Content != nil && *delta.Content != "" {
					if !textBlockOpen {
						blockIndex++
						if !send(ir.StreamEvent{
							Kind:              ir.EvContentBlockStart,
							ContentBlockStart: &ir.ContentBlockStart{Index: blockIndex, BlockType: "text"},
						}) {
							return
						}
						textBlockOpen = true
					}
					if !send(ir.StreamEvent{
						Kind:      ir.EvTextDelta,
						TextDelta: &ir.TextDelta{Index: blockIndex, Text: *delta.Content},
					}) {
						return
					}
				}

				// Tool call deltas.
				for _, tc := range delta.ToolCalls {
					state, exists := toolBlocks[tc.Index]
					if !exists {
						// First chunk for this tool call index.
						// Close any open text block (tool calls follow text in Anthropic protocol).
						if textBlockOpen {
							if !send(ir.StreamEvent{
								Kind:            ir.EvContentBlockEnd,
								ContentBlockEnd: &ir.ContentBlockEnd{Index: blockIndex},
							}) {
								return
							}
							textBlockOpen = false
						}
						blockIndex++
						state = &toolBlock{blockIndex: blockIndex}
						toolBlocks[tc.Index] = state
						toolBlockList = append(toolBlockList, state)
						hasToolUse = true

						if !send(ir.StreamEvent{
							Kind: ir.EvToolCallStart,
							ToolCallStart: &ir.ToolCallStart{
								Index: blockIndex,
								ID:    tc.ID,
								Name:  tc.Function.Name,
							},
						}) {
							return
						}
					}
					// Argument fragment (may be empty on first chunk).
					if tc.Function.Arguments != "" {
						if !send(ir.StreamEvent{
							Kind: ir.EvToolCallDelta,
							ToolCallDelta: &ir.ToolCallDelta{
								Index:       state.blockIndex,
								PartialJSON: tc.Function.Arguments,
							},
						}) {
							return
						}
					}
				}
			}
		}

		// Scanner errors (e.g. a line exceeding maxSSELineSize, or a network
		// read failure) fall through to the normal Finish/Usage/StreamEnd tail
		// below rather than aborting — the client still needs a terminated
		// stream. Logged so a silently truncated response is diagnosable.
		if err := scanner.Err(); err != nil {
			slog.Error("openai stream scanner error", "error", err)
		}

		// Close any open text block.
		if textBlockOpen {
			if !send(ir.StreamEvent{
				Kind:            ir.EvContentBlockEnd,
				ContentBlockEnd: &ir.ContentBlockEnd{Index: blockIndex},
			}) {
				return
			}
		}

		// Close all tool call blocks in insertion (blockIndex) order.
		for _, state := range toolBlockList {
			if !send(ir.StreamEvent{
				Kind:            ir.EvContentBlockEnd,
				ContentBlockEnd: &ir.ContentBlockEnd{Index: state.blockIndex},
			}) {
				return
			}
		}

		// Empty response fallback: emit a minimal closed text block so downstream
		// converters always see at least one content block (matches Gemini behavior).
		if blockIndex < 0 {
			send(ir.StreamEvent{
				Kind:              ir.EvContentBlockStart,
				ContentBlockStart: &ir.ContentBlockStart{Index: 0, BlockType: "text"},
			})
			send(ir.StreamEvent{
				Kind:            ir.EvContentBlockEnd,
				ContentBlockEnd: &ir.ContentBlockEnd{Index: 0},
			})
		}

		stopReason := mapOAIFinishReason(normalizeReason(finishReason), hasToolUse)
		send(ir.StreamEvent{Kind: ir.EvFinish, Finish: &ir.Finish{StopReason: stopReason}})
		send(ir.StreamEvent{Kind: ir.EvUsage, Usage: &ir.UsageEvent{
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
		}})
		send(ir.StreamEvent{Kind: ir.EvStreamEnd, StreamEnd: &ir.StreamEnd{}})
	}()
	return out
}

// OpenAI frontend conversion: OpenAI Chat Completions ↔ Anthropic MessageRequest/Response.
//
// This layer allows any OpenAI-compatible client (Codex, LiteLLM, openai-python,
// curl against /v1/chat/completions) to talk to miroxy without knowing that the
// backend speaks Anthropic/Gemini/GLM.
//
// Architecture note: we convert OpenAI ↔ Anthropic types here (not OpenAI ↔ IR) so
// the existing pipeline, retry loop, and Translator interface stay unchanged.
// When the pipeline becomes IR-native, this layer can be simplified to OpenAI ↔ IR directly.

// OAIBodyToAnthropicRequest parses an OpenAI chat/completions request body and
// converts it to an Anthropic MessageRequest for the existing translation pipeline.
func OAIBodyToAnthropicRequest(body []byte) (*types.MessageRequest, error) {
	var req oaiRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("parse openai request: %w", err)
	}
	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}

	out := &types.MessageRequest{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		Stream:    req.Stream,
	}

	// OpenAI has no min for max_tokens; Anthropic requires > 0. Default to 4096.
	if out.MaxTokens <= 0 {
		out.MaxTokens = 4096
	}

	// Generation config.
	out.Temperature = req.Temperature
	out.TopP = req.TopP
	if len(req.Stop) > 0 {
		out.StopSequences = req.Stop
	}

	// System: OpenAI embeds system prompts as messages with role="system".
	// Anthropic has a top-level System field.
	var systemParts []string
	var conversationMsgs []oaiMessage
	for _, m := range req.Messages {
		if m.Role == "system" {
			if text := oaiContentText(m.Content); text != "" {
				systemParts = append(systemParts, text)
			}
		} else {
			conversationMsgs = append(conversationMsgs, m)
		}
	}
	if len(systemParts) > 0 {
		s := strings.Join(systemParts, "\n")
		b, _ := json.Marshal(s)
		out.System = b
	}

	out.Messages = oaiMessagesToAnthropic(conversationMsgs)

	// Tools: OpenAI wraps each tool as {type:"function", function:{name,description,parameters}}.
	// Anthropic uses {name, description, input_schema}.
	if len(req.Tools) > 0 {
		out.Tools = make([]types.Tool, len(req.Tools))
		for i, t := range req.Tools {
			out.Tools[i] = types.Tool{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: t.Function.Parameters,
			}
		}
	}

	// Tool choice.
	if req.ToolChoice != nil {
		out.ToolChoice = oaiToolChoiceToAnthropic(req.ToolChoice)
	}

	return out, nil
}

// oaiMessagesToAnthropic converts OpenAI messages to Anthropic format.
//
// Key differences:
//   - OpenAI role:"tool" messages → grouped into a single Anthropic user message
//     containing tool_result content blocks (one per tool message).
//   - OpenAI assistant messages with tool_calls → Anthropic assistant message
//     with tool_use content blocks.
//   - OpenAI string content → Anthropic JSON-encoded string content.
func oaiMessagesToAnthropic(msgs []oaiMessage) []types.Message {
	var result []types.Message
	i := 0
	for i < len(msgs) {
		m := msgs[i]

		if m.Role == "tool" {
			// Collect consecutive tool messages into one Anthropic user message.
			var blocks []types.ContentBlock
			for i < len(msgs) && msgs[i].Role == "tool" {
				tm := msgs[i]
				text := oaiContentText(tm.Content)
				contentJSON, _ := json.Marshal(text)
				blocks = append(blocks, types.ContentBlock{
					Type:      "tool_result",
					ToolUseID: tm.ToolCallID,
					Content:   contentJSON,
				})
				i++
			}
			blocksJSON, _ := json.Marshal(blocks)
			result = append(result, types.Message{Role: "user", Content: blocksJSON})
			continue
		}

		if m.Role == "user" {
			blocks := oaiContentBlocks(m.Content)
			var contentJSON json.RawMessage
			if len(blocks) == 0 {
				contentJSON, _ = json.Marshal("")
			} else {
				contentJSON, _ = json.Marshal(blocks)
			}
			result = append(result, types.Message{Role: "user", Content: contentJSON})
			i++
			continue
		}

		if m.Role == "assistant" {
			if len(m.ToolCalls) > 0 {
				var blocks []types.ContentBlock
				if text := oaiContentText(m.Content); text != "" {
					blocks = append(blocks, types.ContentBlock{
						Type: "text",
						Text: text,
					})
				}
				for _, tc := range m.ToolCalls {
					args := json.RawMessage(tc.Function.Arguments)
					if len(args) == 0 {
						args = json.RawMessage("{}")
					}
					blocks = append(blocks, types.ContentBlock{
						Type:  "tool_use",
						ID:    tc.ID,
						Name:  tc.Function.Name,
						Input: args,
					})
				}
				blocksJSON, _ := json.Marshal(blocks)
				result = append(result, types.Message{Role: "assistant", Content: blocksJSON})
			} else {
				text := oaiContentText(m.Content)
				contentJSON, _ := json.Marshal(text)
				result = append(result, types.Message{Role: "assistant", Content: contentJSON})
			}
			i++
			continue
		}

		i++ // skip unknown roles
	}
	return result
}

// oaiToolChoiceToAnthropic maps OpenAI tool_choice to Anthropic ToolChoice.
// OpenAI tool_choice is either a string ("auto","required","none") or an object
// {"type":"function","function":{"name":"x"}}. It arrives as `any` after JSON unmarshal.
func oaiToolChoiceToAnthropic(tc any) *types.ToolChoice {
	switch v := tc.(type) {
	case string:
		switch v {
		case "auto":
			return &types.ToolChoice{Type: "auto"}
		case "required":
			return &types.ToolChoice{Type: "any"}
			// "none" → no tool_choice (Anthropic doesn't support "none" as a value)
		}
	case map[string]any:
		if fn, ok := v["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return &types.ToolChoice{Type: "tool", Name: name}
			}
		}
	}
	return nil
}

// --- Response conversion ---

// oaiFrontendResponse is the complete OpenAI chat.completion response sent to the client.
type oaiFrontendResponse struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"` // "chat.completion"
	Created int64               `json:"created"`
	Model   string              `json:"model"`
	Choices []oaiFrontendChoice `json:"choices"`
	Usage   oaiUsage            `json:"usage"`
}

type oaiFrontendChoice struct {
	Index        int            `json:"index"`
	Message      oaiFrontendMsg `json:"message"`
	FinishReason string         `json:"finish_reason"`
}

type oaiFrontendMsg struct {
	Role      string          `json:"role"`
	Content   *string         `json:"content"`
	ToolCalls []oaiFrontendTC `json:"tool_calls,omitempty"`
}

// oaiFrontendTC is the tool call structure in the response (index always present).
type oaiFrontendTC struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function oaiFuncCall `json:"function"`
}

// AnthropicToOAIResponseBody converts an Anthropic MessageResponse to an OpenAI
// chat.completion JSON body. model is the alias the client originally requested.
// IRResponseToOAIBody converts a canonical IR response directly into an
// OpenAI chat.completion wire body — no Anthropic-shaped intermediate (see
// docs/dev/DESIGNLOG.md, 2026-07-19). msgID/model are supplied by the
// caller — the IR carries neither.
func IRResponseToOAIBody(irResp *ir.IRResponse, msgID, model string) []byte {
	var textParts []string
	var toolCalls []oaiFrontendTC

	for _, block := range irResp.Content {
		switch {
		case block.Text != nil:
			textParts = append(textParts, block.Text.Text)
		case block.ToolUse != nil:
			args := string(block.ToolUse.InputJSON)
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, oaiFrontendTC{
				ID:   block.ToolUse.ID,
				Type: "function",
				Function: oaiFuncCall{
					Name:      block.ToolUse.Name,
					Arguments: args,
				},
			})
		}
	}

	var content *string
	if s := strings.Join(textParts, ""); s != "" {
		content = &s
	}

	out := oaiFrontendResponse{
		ID:      toChatCmplID(msgID),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []oaiFrontendChoice{{
			Index: 0,
			Message: oaiFrontendMsg{
				Role:      "assistant",
				Content:   content,
				ToolCalls: toolCalls,
			},
			FinishReason: irStopReasonToOAIFinishReason(irResp.StopReason),
		}},
		Usage: oaiUsage{
			PromptTokens:     irResp.Usage.InputTokens,
			CompletionTokens: irResp.Usage.OutputTokens,
			TotalTokens:      irResp.Usage.InputTokens + irResp.Usage.OutputTokens,
		},
	}
	b, _ := json.Marshal(out)
	return b
}

// toChatCmplID adapts an Anthropic message ID (msg_xxx) to OpenAI format (chatcmpl-xxx).
func toChatCmplID(id string) string {
	if strings.HasPrefix(id, "msg_") {
		return "chatcmpl-" + id[4:]
	}
	return "chatcmpl-" + id
}

// --- Streaming conversion ---

// oaiFrontendChunk is a single OpenAI chat.completion.chunk SSE payload.
type oaiFrontendChunk struct {
	ID      string                   `json:"id"`
	Object  string                   `json:"object"` // "chat.completion.chunk"
	Created int64                    `json:"created"`
	Model   string                   `json:"model"`
	Choices []oaiFrontendChunkChoice `json:"choices"`
	Usage   *oaiUsage                `json:"usage,omitempty"`
}

type oaiFrontendChunkChoice struct {
	Index        int              `json:"index"`
	Delta        oaiFrontendDelta `json:"delta"`
	FinishReason *string          `json:"finish_reason"`
}

type oaiFrontendDelta struct {
	Role      string               `json:"role,omitempty"`
	Content   *string              `json:"content,omitempty"`
	ToolCalls []oaiFrontendTCDelta `json:"tool_calls,omitempty"`
}

// oaiFrontendTCDelta is a streaming tool_call delta. Index has no omitempty so
// that index 0 is always written (OpenAI clients use it for correlation).
type oaiFrontendTCDelta struct {
	Index    int         `json:"index"`
	ID       string      `json:"id,omitempty"`
	Type     string      `json:"type,omitempty"`
	Function oaiFuncCall `json:"function"`
}

// irStopReasonToOAIFinishReason maps the neutral IR stop reason to OpenAI's
// finish_reason vocabulary.
func irStopReasonToOAIFinishReason(r ir.IRStopReason) string {
	switch r {
	case ir.IRStopReasonMaxTokens:
		return "length"
	case ir.IRStopReasonToolUse:
		return "tool_calls"
	default: // stop, content_filter, error
		return "stop"
	}
}

// StreamIRToOAI reads neutral IR stream events from in and writes OpenAI SSE
// chunks to w, flushing after each chunk — a direct IR→OpenAI-wire converter,
// with no Anthopic-shaped intermediate (see docs/dev/DESIGNLOG.md, 2026-07-19).
//
// The function owns reading from `in` until it receives EvStreamEnd or ctx is
// cancelled. The caller is responsible for releasing the upstream connection
// after this function returns.
func StreamIRToOAI(
	ctx context.Context,
	in <-chan ir.StreamEvent,
	w io.Writer,
	flusher http.Flusher,
	msgID, model string,
) {
	created := time.Now().Unix()

	writeChunk := func(choice oaiFrontendChunkChoice, usage *oaiUsage) {
		chunk := oaiFrontendChunk{
			ID:      msgID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []oaiFrontendChunkChoice{choice},
			Usage:   usage,
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	// First chunk: role announcement (OpenAI convention).
	writeChunk(oaiFrontendChunkChoice{
		Delta: oaiFrontendDelta{Role: "assistant"},
	}, nil)

	// State: track IR content-part index → tool call output index.
	toolCallByBlock := map[int]int{}
	nextToolIdx := 0

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-in:
			if !ok {
				return
			}

			switch ev.Kind {
			case ir.EvToolCallStart:
				s := ev.ToolCallStart
				toolCallByBlock[s.Index] = nextToolIdx
				writeChunk(oaiFrontendChunkChoice{
					Delta: oaiFrontendDelta{
						ToolCalls: []oaiFrontendTCDelta{{
							Index:    nextToolIdx,
							ID:       s.ID,
							Type:     "function",
							Function: oaiFuncCall{Name: s.Name},
						}},
					},
				}, nil)
				nextToolIdx++
				// EvContentBlockStart (plain text): no chunk (OpenAI has no block-start event).

			case ir.EvTextDelta:
				s := ev.TextDelta.Text
				writeChunk(oaiFrontendChunkChoice{
					Delta: oaiFrontendDelta{Content: &s},
				}, nil)

			case ir.EvToolCallDelta:
				d := ev.ToolCallDelta
				tcIdx, ok := toolCallByBlock[d.Index]
				if !ok {
					continue
				}
				writeChunk(oaiFrontendChunkChoice{
					Delta: oaiFrontendDelta{
						ToolCalls: []oaiFrontendTCDelta{{
							Index:    tcIdx,
							Function: oaiFuncCall{Arguments: d.PartialJSON},
						}},
					},
				}, nil)

			case ir.EvFinish:
				reason := irStopReasonToOAIFinishReason(ev.Finish.StopReason)
				writeChunk(oaiFrontendChunkChoice{
					Delta:        oaiFrontendDelta{},
					FinishReason: &reason,
				}, nil)

			case ir.EvUsage:
				u := ev.Usage
				if u.OutputTokens > 0 || u.InputTokens > 0 {
					usage := &oaiUsage{
						PromptTokens:     u.InputTokens,
						CompletionTokens: u.OutputTokens,
						TotalTokens:      u.InputTokens + u.OutputTokens,
					}
					chunk := oaiFrontendChunk{
						ID:      msgID,
						Object:  "chat.completion.chunk",
						Created: created,
						Model:   model,
						Choices: []oaiFrontendChunkChoice{},
						Usage:   usage,
					}
					b, _ := json.Marshal(chunk)
					fmt.Fprintf(w, "data: %s\n\n", b)
					flusher.Flush()
				}

			case ir.EvStreamEnd:
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
		}
	}
}
