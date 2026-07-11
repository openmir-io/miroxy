package irc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"miroxy/internal/idgen"
	"miroxy/core/ir"
	"miroxy/internal/types"
)

// GeminiConverter is the v1 UpstreamConverter: it converts between the neutral IR
// and Google's Gemini (generateContent / streamGenerateContent) wire format.
// It is stateless and owns no transport concerns (URL, auth, keys) — those live in
// GeminiTranslator. It owns the Gemini dialect: JSON-Schema sanitization, tool-name
// resolution from history, thought filtering, arg rectification, finishReason map.
type GeminiConverter struct{}

var _ UpstreamConverter = (*GeminiConverter)(nil)

func (*GeminiConverter) Provider() string { return "gemini" }

// RequestToProvider marshals an IR request into a Gemini request body.
func (*GeminiConverter) RequestToProvider(irReq *ir.IRRequest) ([]byte, error) {
	geminiReq, err := buildGeminiRequest(irReq)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(geminiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal gemini request: %w", err)
	}
	return body, nil
}

// ResponseToIR parses a non-streaming Gemini response body into an IR response.
func (*GeminiConverter) ResponseToIR(body []byte) (*ir.IRResponse, error) {
	var geminiResp types.GeminiResponse
	if err := json.Unmarshal(body, &geminiResp); err != nil {
		return nil, fmt.Errorf("parse gemini response: %w", err)
	}

	// G-05: surface a typed error so the server routes 429 to the rate-limit
	// cooldown path (not the circuit-break counter).
	if geminiResp.Error != nil {
		return nil, &UpstreamError{
			HTTPStatus: geminiCodeToHTTP(geminiResp.Error.Code),
			Code:       geminiResp.Error.Code,
			Message: fmt.Sprintf("gemini error %d (%s): %s",
				geminiResp.Error.Code, geminiResp.Error.Status, geminiResp.Error.Message),
		}
	}

	// G-03: safety filter block — a well-formed refusal, not an error.
	if pf := geminiResp.PromptFeedback; pf != nil && pf.BlockReason != "" {
		return &ir.IRResponse{
			Content:    []ir.IRResponseBlock{{Text: &ir.IRTextPart{Text: "Request blocked by Gemini safety filters: " + pf.BlockReason}}},
			StopReason: ir.IRStopReasonContentFilter,
			Usage:      ir.IRUsage{InputTokens: geminiResp.UsageMetadata.PromptTokenCount},
		}, nil
	}

	if len(geminiResp.Candidates) == 0 {
		return nil, fmt.Errorf("gemini response contains no candidates")
	}

	candidate := geminiResp.Candidates[0]
	var content []ir.IRResponseBlock
	hasToolUse := false
	for _, part := range candidate.Content.Parts {
		// G-02: drop extended thinking parts — not visible to the client.
		if part.Thought {
			continue
		}
		if part.Text != "" {
			content = append(content, ir.IRResponseBlock{Text: &ir.IRTextPart{Text: part.Text}})
		}
		if part.FunctionCall != nil {
			content = append(content, ir.IRResponseBlock{ToolUse: &ir.IRToolUsePart{
				ID:        idgen.NewToolID(),
				Name:      part.FunctionCall.Name,
				InputJSON: rectifyArgs(part.FunctionCall.Args), // G-10
			}})
			hasToolUse = true
		}
	}

	stopReason := mapFinishReason(candidate.FinishReason)
	if hasToolUse {
		stopReason = ir.IRStopReasonToolUse
	}

	return &ir.IRResponse{
		Content:    content,
		StopReason: stopReason,
		Usage: ir.IRUsage{
			InputTokens:  geminiResp.UsageMetadata.PromptTokenCount,
			OutputTokens: geminiResp.UsageMetadata.CandidatesTokenCount,
		},
	}, nil
}

// StreamToIR reads the Gemini SSE stream and emits neutral IR stream events.
// It owns block-lifecycle bookkeeping (indices, open/close, empty fallback);
// the frontend converter renders these events into the client's SSE dialect.
func (*GeminiConverter) StreamToIR(ctx context.Context, body io.Reader) <-chan ir.StreamEvent {
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

		scanner := bufio.NewScanner(body)
		blockIndex := -1
		textBlockOpen := false
		hasToolUse := false
		var stopReason ir.IRStopReason
		var usage types.GeminiUsageMetadata

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}

			line, ok := strings.CutPrefix(scanner.Text(), "data: ")
			if !ok || line == "" {
				continue
			}

			var chunk types.GeminiResponse
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				continue
			}

			for _, candidate := range chunk.Candidates {
				for _, part := range candidate.Content.Parts {
					// G-02: drop extended thinking parts from the stream.
					if part.Thought {
						continue
					}

					if part.Text != "" {
						if !textBlockOpen {
							blockIndex++
							if !send(ir.StreamEvent{Kind: ir.EvContentBlockStart, ContentBlockStart: &ir.ContentBlockStart{Index: blockIndex, BlockType: "text"}}) {
								return
							}
							textBlockOpen = true
						}
						if !send(ir.StreamEvent{Kind: ir.EvTextDelta, TextDelta: &ir.TextDelta{Index: blockIndex, Text: part.Text}}) {
							return
						}
					}

					if part.FunctionCall != nil {
						// Close any open text block before starting a tool_use block.
						if textBlockOpen {
							if !send(ir.StreamEvent{Kind: ir.EvContentBlockEnd, ContentBlockEnd: &ir.ContentBlockEnd{Index: blockIndex}}) {
								return
							}
							textBlockOpen = false
						}
						blockIndex++
						toolID := idgen.NewToolID()
						// G-10: rectify args before marshaling.
						argsJSON, _ := json.Marshal(rectifyArgs(part.FunctionCall.Args))
						if !send(ir.StreamEvent{Kind: ir.EvToolCallStart, ToolCallStart: &ir.ToolCallStart{Index: blockIndex, ID: toolID, Name: part.FunctionCall.Name}}) {
							return
						}
						if !send(ir.StreamEvent{Kind: ir.EvToolCallDelta, ToolCallDelta: &ir.ToolCallDelta{Index: blockIndex, PartialJSON: string(argsJSON)}}) {
							return
						}
						if !send(ir.StreamEvent{Kind: ir.EvContentBlockEnd, ContentBlockEnd: &ir.ContentBlockEnd{Index: blockIndex}}) {
							return
						}
						hasToolUse = true
					}
				}

				if candidate.FinishReason != "" {
					stopReason = mapFinishReason(candidate.FinishReason)
				}
			}
			if chunk.UsageMetadata.CandidatesTokenCount > 0 || chunk.UsageMetadata.PromptTokenCount > 0 {
				usage = chunk.UsageMetadata
			}
		}

		// Close any open text block.
		if textBlockOpen {
			if !send(ir.StreamEvent{Kind: ir.EvContentBlockEnd, ContentBlockEnd: &ir.ContentBlockEnd{Index: blockIndex}}) {
				return
			}
		}
		// If no content blocks were emitted at all, send an empty text block so
		// clients that expect at least one block don't break.
		if blockIndex < 0 {
			if !send(ir.StreamEvent{Kind: ir.EvContentBlockStart, ContentBlockStart: &ir.ContentBlockStart{Index: 0, BlockType: "text"}}) {
				return
			}
			if !send(ir.StreamEvent{Kind: ir.EvContentBlockEnd, ContentBlockEnd: &ir.ContentBlockEnd{Index: 0}}) {
				return
			}
		}

		if hasToolUse {
			stopReason = ir.IRStopReasonToolUse
		} else if stopReason == "" {
			stopReason = ir.IRStopReasonStop
		}

		if !send(ir.StreamEvent{Kind: ir.EvFinish, Finish: &ir.Finish{StopReason: stopReason}}) {
			return
		}
		// G-07: emit real token counts from the final usage chunk.
		if !send(ir.StreamEvent{Kind: ir.EvUsage, Usage: &ir.UsageEvent{InputTokens: usage.PromptTokenCount, OutputTokens: usage.CandidatesTokenCount}}) {
			return
		}
		send(ir.StreamEvent{Kind: ir.EvStreamEnd, StreamEnd: &ir.StreamEnd{}})
	}()
	return out
}

// buildGeminiRequest converts an IR request to a GeminiRequest.
func buildGeminiRequest(irReq *ir.IRRequest) (*types.GeminiRequest, error) {
	contents, err := convertIRMessages(irReq.Messages)
	if err != nil {
		return nil, err
	}

	// G-01: forward all generation params, not just max_tokens.
	gc := &types.GenerationConfig{MaxOutputTokens: irReq.Gen.MaxTokens}
	if irReq.Gen.Temperature != nil {
		gc.Temperature = irReq.Gen.Temperature
	}
	if irReq.Gen.TopP != nil {
		gc.TopP = irReq.Gen.TopP
	}
	if irReq.Gen.TopK != nil {
		gc.TopK = irReq.Gen.TopK
	}
	if len(irReq.Gen.StopSeqs) > 0 {
		gc.StopSequences = irReq.Gen.StopSeqs
	}

	geminiReq := &types.GeminiRequest{
		Contents:         contents,
		GenerationConfig: gc,
	}
	if irReq.System != "" {
		geminiReq.SystemInstruction = &types.GeminiSysInstruct{
			Parts: []types.GeminiPart{{Text: irReq.System}},
		}
	}
	if len(irReq.Tools) > 0 {
		var decls []types.GeminiFunctionDeclaration
		for _, tool := range irReq.Tools {
			// G-09: skip Claude Code internal tools that Gemini cannot handle.
			if tool.Name == "BatchTool" {
				continue
			}
			decls = append(decls, types.GeminiFunctionDeclaration{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  sanitizeSchemaForGemini(json.RawMessage(tool.InputSchemaJSON)),
			})
		}
		if len(decls) > 0 {
			geminiReq.Tools = []types.GeminiTools{{FunctionDeclarations: decls}}
			// G-06: buildToolConfig handles allowedFunctionNames for "tool" type.
			geminiReq.ToolConfig = &types.GeminiToolConfig{
				FunctionCallingConfig: buildToolConfig(irReq.ToolChoice),
			}
		}
	}
	return geminiReq, nil
}

// convertIRMessages maps IR messages to Gemini contents.
func convertIRMessages(msgs []ir.IRMessage) ([]types.GeminiContent, error) {
	contents := make([]types.GeminiContent, 0, len(msgs))
	for i, msg := range msgs {
		role := msg.Role
		if role == "assistant" {
			role = "model"
		}
		// Pass the full message list so tool_result parts can resolve the
		// function name from the matching tool_use elsewhere in the history.
		parts, err := convertIRParts(msg.Parts, msgs)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		contents = append(contents, types.GeminiContent{Role: role, Parts: parts})
	}
	return contents, nil
}

// convertIRParts converts IR content parts to Gemini parts.
func convertIRParts(parts []ir.IRContentPart, allMsgs []ir.IRMessage) ([]types.GeminiPart, error) {
	var geminiParts []types.GeminiPart
	for _, p := range parts {
		if p.Text != nil {
			text := p.Text.Text
			if text == "" {
				text = " " // Gemini rejects empty text parts
			}
			geminiParts = append(geminiParts, types.GeminiPart{Text: text})
		}
		if p.ToolUse != nil {
			args := json.RawMessage(p.ToolUse.InputJSON)
			if len(args) == 0 || string(args) == "null" {
				args = json.RawMessage("{}")
			}
			geminiParts = append(geminiParts, types.GeminiPart{
				FunctionCall: &types.GeminiFunctionCall{
					Name: p.ToolUse.Name,
					Args: args,
				},
			})
		}
		if p.ToolResult != nil {
			response := irResultToGeminiMap(p.ToolResult.Content)
			geminiParts = append(geminiParts, types.GeminiPart{
				FunctionResponse: &types.GeminiFunctionResponse{
					Name:     resolveFunctionNameIR(allMsgs, p.ToolResult.ToolUseID),
					Response: response,
				},
			})
		}
	}
	if len(geminiParts) == 0 {
		geminiParts = []types.GeminiPart{{Text: " "}} // Gemini rejects empty contents
	}
	return geminiParts, nil
}

// resolveFunctionNameIR scans the IR messages for the tool_use whose ID matches
// toolUseID and returns its function name. Gemini's functionResponse is matched by
// name, not ID; Anthropic tool_result carries only the ID, so the provider
// converter resolves the name here — keeping the IR provider-neutral.
func resolveFunctionNameIR(msgs []ir.IRMessage, toolUseID string) string {
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.ToolUse != nil && p.ToolUse.ID == toolUseID {
				return p.ToolUse.Name
			}
		}
	}
	return toolUseID // fallback: use ID as name
}

// irResultToGeminiMap converts IR tool result content parts to the map Gemini expects.
func irResultToGeminiMap(parts []ir.IRContentPart) map[string]any {
	var texts []string
	for _, p := range parts {
		if p.Text != nil && p.Text.Text != "" {
			texts = append(texts, p.Text.Text)
		}
	}
	return map[string]any{"content": strings.Join(texts, "\n")}
}

// geminiSchemaAllowed is the allowlist of JSON Schema fields Gemini accepts in
// function declaration parameters. Gemini rejects anything outside this set.
var geminiSchemaAllowed = map[string]bool{
	"type":          true,
	"format":        true,
	"description":   true,
	"nullable":      true,
	"enum":          true,
	"properties":    true,
	"required":      true,
	"items":         true,
	"anyOf":         true,
	"minimum":       true,
	"maximum":       true,
	"minItems":      true,
	"maxItems":      true,
	"minLength":     true,
	"maxLength":     true,
	"minProperties": true,
	"maxProperties": true,
	"pattern":       true,
	"title":         true,
	"default":       true,
	"example":       true,
}

// sanitizeSchemaForGemini keeps only the JSON Schema fields that Gemini accepts,
// dropping everything else (e.g. $schema, additionalProperties, exclusiveMinimum).
// Recursively cleans nested properties, items, and anyOf entries.
func sanitizeSchemaForGemini(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 || string(schema) == "null" {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(schema, &obj); err != nil {
		return schema // scalar — pass through
	}

	clean := make(map[string]json.RawMessage, len(obj))
	for k, v := range obj {
		if geminiSchemaAllowed[k] {
			clean[k] = v
		}
	}

	if props, ok := clean["properties"]; ok {
		var propsMap map[string]json.RawMessage
		if json.Unmarshal(props, &propsMap) == nil {
			for k, v := range propsMap {
				propsMap[k] = sanitizeSchemaForGemini(v)
			}
			if b, err := json.Marshal(propsMap); err == nil {
				clean["properties"] = b
			}
		}
	}
	if items, ok := clean["items"]; ok {
		clean["items"] = sanitizeSchemaForGemini(items)
	}
	if anyOf, ok := clean["anyOf"]; ok {
		var variants []json.RawMessage
		if json.Unmarshal(anyOf, &variants) == nil {
			for i, v := range variants {
				variants[i] = sanitizeSchemaForGemini(v)
			}
			if b, err := json.Marshal(variants); err == nil {
				clean["anyOf"] = b
			}
		}
	}

	b, err := json.Marshal(clean)
	if err != nil {
		return schema
	}
	return b
}

// buildToolConfig maps an IR tool_choice to a Gemini FunctionCallingConfig.
// G-06: "tool" type sets AllowedFunctionNames to constrain Gemini to the named function.
func buildToolConfig(tc *ir.IRToolChoice) types.GeminiFunctionCallingConfig {
	if tc == nil {
		return types.GeminiFunctionCallingConfig{Mode: "AUTO"}
	}
	switch tc.Type {
	case "any":
		return types.GeminiFunctionCallingConfig{Mode: "ANY"}
	case "tool":
		fcc := types.GeminiFunctionCallingConfig{Mode: "ANY"}
		if tc.Name != "" {
			fcc.AllowedFunctionNames = []string{tc.Name}
		}
		return fcc
	case "none":
		return types.GeminiFunctionCallingConfig{Mode: "NONE"}
	default:
		return types.GeminiFunctionCallingConfig{Mode: "AUTO"}
	}
}

// rectifyArgs defensively unwraps tool call args that relay channels occasionally
// return as a string-encoded JSON object instead of a JSON object.
// G-10: applied in ResponseToIR and StreamToIR for all FunctionCall parts.
func rectifyArgs(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	if raw[0] == '{' {
		return raw // already an object
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && len(s) > 0 && s[0] == '{' {
		return json.RawMessage(s)
	}
	return raw
}

// geminiCodeToHTTP maps a Gemini API error code to the nearest HTTP status.
// G-05: used by UpstreamError so the server can route the error to the correct selector path.
func geminiCodeToHTTP(code int) int {
	switch {
	case code == 400:
		return 400
	case code == 401 || code == 403:
		return code
	case code == 429:
		return 429
	case code >= 500:
		return 500
	default:
		return 502
	}
}

// mapFinishReason converts Gemini finishReason strings to neutral IR stop reasons.
// G-04: handles TOOL_CODE, FUNCTION_CALL, SAFETY, RECITATION, and related variants.
func mapFinishReason(r string) ir.IRStopReason {
	switch r {
	case "STOP":
		return ir.IRStopReasonStop
	case "MAX_TOKENS":
		return ir.IRStopReasonMaxTokens
	case "TOOL_CODE", "FUNCTION_CALL":
		return ir.IRStopReasonToolUse
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return ir.IRStopReasonContentFilter
	default:
		return ir.IRStopReasonStop
	}
}
