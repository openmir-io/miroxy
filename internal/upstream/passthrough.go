package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"miroxy/core/cred"
	"miroxy/core/ir"
	coreup "miroxy/core/upstream"
	"miroxy/internal/types"
	"miroxy/internal/wireformat"
)

// PassthroughEndpoints returns the (non-stream, stream) URLs for proto, appending
// each protocol's required path suffix to the bare api_base (mirrors the real adapters).
func PassthroughEndpoints(proto, upstreamModel, apiBase string) (endpoint, streamEndpoint string) {
	base := strings.TrimRight(apiBase, "/")
	switch proto {
	case "anthropic":
		ep := base + "/v1/messages"
		return ep, ep
	case "openai", "deepseek", "grok", "glm":
		ep := base + "/chat/completions"
		return ep, ep
	default: // "gemini" or empty
		if base == "" {
			base = defaultGeminiBase
		}
		return fmt.Sprintf("%s/v1beta/models/%s:generateContent", base, upstreamModel),
			fmt.Sprintf("%s/v1beta/models/%s:streamGenerateContent?alt=sse", base, upstreamModel)
	}
}

// PassthroughAdapter forwards a request/response verbatim, with no IR
// transform in either direction. The caller (UpstreamExecutor) only selects
// this adapter for an attempt when it has already established that the
// client's actual wire protocol matches this target's protocol — see
// ExecutionPlan.PassthroughUpstream/ForcePassthrough — so every method here
// can assume raw byte forwarding is correct without re-checking protocols.
//
// ToUpstream/ToUpstreamStream send coreup.RawBodyFromContext(ctx) verbatim.
// The response side never reaches FromUpstream — see LLMContext.SetRawResponse.
type PassthroughAdapter struct {
	endpoint       string // full URL for non-streaming POST
	streamEndpoint string // full URL for streaming POST (falls back to endpoint when empty)
	// upstreamModel, when set, replaces the outgoing body's "model" field —
	// every real transform adapter rewrites Model to its own configured
	// upstream_model (see AnthropicUpstream.build's outReq.Model = a.upstreamModel);
	// passthrough forwards everything else byte-for-byte, but the client's
	// "model" value is always miroxy's own alias (e.g. a model_routes name),
	// which the real upstream has never heard of. Empty means "forward the
	// client's model field as-is" — the provider-keyed catch-all passthrough
	// (no model_routes entry matched) intentionally has no fixed upstream
	// model to rewrite to.
	upstreamModel string
}

// NewPassthrough creates a PassthroughTranslator.
// endpoint is the full upstream URL (e.g. https://bedrock-runtime.../invoke).
// streamEndpoint is optional; if empty, endpoint is used for streaming too.
func NewPassthrough(endpoint, streamEndpoint, upstreamModel string) *PassthroughAdapter {
	return &PassthroughAdapter{endpoint: endpoint, streamEndpoint: streamEndpoint, upstreamModel: upstreamModel}
}

func (t *PassthroughAdapter) streamURL() string {
	if t.streamEndpoint != "" {
		return t.streamEndpoint
	}
	return t.endpoint
}

func (t *PassthroughAdapter) ToUpstream(ctx context.Context, req *ir.IRRequest, credential cred.Credential) (*http.Request, error) {
	return t.buildHTTPRequest(ctx, req, t.endpoint, credential)
}

func (t *PassthroughAdapter) ToUpstreamStream(ctx context.Context, req *ir.IRRequest, credential cred.Credential) (*http.Request, error) {
	return t.buildHTTPRequest(ctx, req, t.streamURL(), credential)
}

// buildHTTPRequest always dispatches raw bytes from context (req is
// unused) — see coreup.WithRawBody / dispatchFor. Erroring when none is
// present is defensive; every real call path sets one first.
func (t *PassthroughAdapter) buildHTTPRequest(ctx context.Context, req *ir.IRRequest, url string, credential cred.Credential) (*http.Request, error) {
	body, ok := coreup.RawBodyFromContext(ctx)
	if !ok {
		return nil, errors.New("passthrough: no raw request body in context")
	}
	if t.upstreamModel != "" && !hasModelField(body, t.upstreamModel) {
		body = rewriteModelField(body, t.upstreamModel)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("passthrough: build request: %w", err)
	}
	if headers, ok := coreup.RawHeadersFromContext(ctx); ok {
		copyPassthroughHeaders(httpReq.Header, headers)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := credential.Apply(httpReq); err != nil {
		return nil, fmt.Errorf("passthrough: apply credential: %w", err)
	}
	return httpReq, nil
}

// passthroughHeaderBlocklist excludes headers this adapter owns itself
// (auth, framing) so client values never leak upstream or conflict with
// what net/http computes for the new request.
var passthroughHeaderBlocklist = map[string]bool{
	"Host":              true,
	"Authorization":     true,
	"Content-Length":    true,
	"Content-Type":      true,
	"Connection":        true,
	"Transfer-Encoding": true,
	"Accept-Encoding":   true,
}

// copyPassthroughHeaders forwards the client's original headers verbatim —
// e.g. Anthropic's required anthropic-version — onto dst, the one part of
// "byte-for-byte" passthrough that request-body forwarding alone can't cover.
func copyPassthroughHeaders(dst, src http.Header) {
	for k, v := range src {
		if passthroughHeaderBlocklist[http.CanonicalHeaderKey(k)] {
			continue
		}
		dst[http.CanonicalHeaderKey(k)] = v
	}
}

// hasModelField reports whether body's top-level "model" field already
// equals model — lets the caller skip rewriteModelField's JSON round-trip
// entirely when there's nothing to change, preserving true byte-for-byte
// forwarding (rewriteModelField's remarshal reorders keys, which is a
// needless deviation when the value was already correct).
func hasModelField(body []byte, model string) bool {
	var fields struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return false
	}
	return fields.Model == model
}

// rewriteModelField returns body with its top-level "model" key set to
// model, every other field byte-for-byte untouched in content (key order and
// whitespace are not preserved — no real JSON API parses those
// structurally, so trading them for a correctness guarantee is free).
// Falls back to the original body, unmodified, if it isn't a JSON object;
// the caller already decoded it successfully once, so this is defensive
// only, not an expected path.
func rewriteModelField(body []byte, model string) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return body
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return body
	}
	fields["model"] = encoded
	out, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return out
}

// FromUpstream is unreachable in normal operation — UpstreamExecutor
// bypasses it for passthrough attempts (see LLMContext.SetRawResponse) and
// relays the raw response body directly. Kept only for UpstreamAdapter
// conformance as a defensive fallback if this adapter is ever invoked
// outside that path.
func (t *PassthroughAdapter) FromUpstream(resp *http.Response) (*ir.IRResponse, error) {
	resp.Body.Close()
	return nil, errors.New("passthrough: FromUpstream not implemented — see doc comment")
}

// StreamFromUpstream is unreachable in normal operation — UpstreamExecutor
// bypasses it for passthrough attempts (see LLMContext.SetRawStream) and
// relays raw bytes directly. Kept only for UpstreamAdapter conformance and as
// a defensive fallback if this adapter is ever invoked outside that path.
func (t *PassthroughAdapter) StreamFromUpstream(ctx context.Context, resp *http.Response, msgID, modelAlias string) (<-chan ir.StreamEvent, error) {
	wire, err := parseAnthropicSSE(ctx, resp)
	if err != nil {
		return nil, err
	}
	return anthropicSSEToIR(ctx, wire), nil
}

// parseAnthropicSSE hand-parses a real Anthropic-wire SSE stream into the
// canonical SSEEvent channel. Shared by PassthroughAdapter's defensive
// fallback and AnthropicUpstream (a genuine Anthropic-protocol upstream
// target, whose wire format is byte-identical to canonical SSE by design).
func parseAnthropicSSE(ctx context.Context, resp *http.Response) (<-chan types.SSEEvent, error) {
	out := make(chan types.SSEEvent, 32)
	go func() {
		defer close(out)
		defer resp.Body.Close()

		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 4096)
		var eventType string

		send := func(ev types.SSEEvent) bool {
			select {
			case <-ctx.Done():
				return false
			case out <- ev:
				return true
			}
		}

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			n, err := resp.Body.Read(tmp)
			buf = append(buf, tmp[:n]...)

			for {
				nl := bytes.IndexByte(buf, '\n')
				if nl < 0 {
					break
				}
				line := string(bytes.TrimRight(buf[:nl], "\r"))
				buf = buf[nl+1:]

				if line == "" {
					eventType = ""
					continue
				}
				if after, ok := bytes.CutPrefix([]byte(line), []byte("event: ")); ok {
					eventType = string(after)
					continue
				}
				if after, ok := bytes.CutPrefix([]byte(line), []byte("data: ")); ok {
					if string(after) == "[DONE]" {
						return
					}
					ev := types.SSEEvent{Event: eventType, Data: json.RawMessage(after)}
					if !send(ev) {
						return
					}
				}
			}

			if err != nil {
				return
			}
		}
	}()
	return out, nil
}

// anthropicSSEToIR converts a genuine Anthropic-wire SSE stream (already
// split into event/data pairs by parseAnthropicSSE) into neutral IR stream
// events — the reverse of AnthropicConverter.StreamFromIR. Needed because a
// real Anthropic upstream's SSE is not automatically "the IR" just because
// it's byte-identical to what this codebase used to treat as canonical.
func anthropicSSEToIR(ctx context.Context, in <-chan types.SSEEvent) <-chan ir.StreamEvent {
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
		var inputTokens int

		for ev := range in {
			raw, _ := json.Marshal(ev.Data)
			switch ev.Event {
			case "message_start":
				var data types.MessageStartData
				if json.Unmarshal(raw, &data) != nil {
					continue
				}
				inputTokens = data.Message.Usage.InputTokens
				if !send(ir.StreamEvent{Kind: ir.EvStreamStart, StreamStart: &ir.StreamStart{
					ID: data.Message.ID, Model: data.Message.Model,
				}}) {
					return
				}

			case "content_block_start":
				var data types.ContentBlockStartData
				if json.Unmarshal(raw, &data) != nil {
					continue
				}
				var ok bool
				switch data.ContentBlock.Type {
				case "tool_use":
					ok = send(ir.StreamEvent{Kind: ir.EvToolCallStart, ToolCallStart: &ir.ToolCallStart{
						Index: data.Index, ID: data.ContentBlock.ID, Name: data.ContentBlock.Name,
					}})
				case "thinking":
					// "thinking" is Anthropic's wire term; "reasoning" is the
					// neutral IR block-type name.
					ok = send(ir.StreamEvent{Kind: ir.EvContentBlockStart, ContentBlockStart: &ir.ContentBlockStart{
						Index: data.Index, BlockType: "reasoning",
					}})
				default:
					ok = send(ir.StreamEvent{Kind: ir.EvContentBlockStart, ContentBlockStart: &ir.ContentBlockStart{
						Index: data.Index, BlockType: data.ContentBlock.Type,
					}})
				}
				if !ok {
					return
				}

			case "content_block_delta":
				var data types.ContentBlockDeltaData
				if json.Unmarshal(raw, &data) != nil {
					continue
				}
				var ok bool
				switch data.Delta.Type {
				case "input_json_delta":
					ok = send(ir.StreamEvent{Kind: ir.EvToolCallDelta, ToolCallDelta: &ir.ToolCallDelta{
						Index: data.Index, PartialJSON: data.Delta.PartialJSON,
					}})
				case "thinking_delta", "signature_delta":
					ok = send(ir.StreamEvent{Kind: ir.EvReasoningDelta, ReasoningDelta: &ir.ReasoningDelta{
						Index: data.Index, Text: data.Delta.Thinking, Signature: data.Delta.Signature,
					}})
				default:
					ok = send(ir.StreamEvent{Kind: ir.EvTextDelta, TextDelta: &ir.TextDelta{
						Index: data.Index, Text: data.Delta.Text,
					}})
				}
				if !ok {
					return
				}

			case "content_block_stop":
				var data types.ContentBlockStopData
				if json.Unmarshal(raw, &data) != nil {
					continue
				}
				if !send(ir.StreamEvent{Kind: ir.EvContentBlockEnd, ContentBlockEnd: &ir.ContentBlockEnd{Index: data.Index}}) {
					return
				}

			case "message_delta":
				var data types.MessageDeltaData
				if json.Unmarshal(raw, &data) != nil {
					continue
				}
				if !send(ir.StreamEvent{Kind: ir.EvFinish, Finish: &ir.Finish{
					StopReason: wireformat.AnthropicToIRStopReason(data.Delta.StopReason),
				}}) {
					return
				}
				if !send(ir.StreamEvent{Kind: ir.EvUsage, Usage: &ir.UsageEvent{
					InputTokens:              inputTokens,
					OutputTokens:             data.Usage.OutputTokens,
					CacheCreationInputTokens: data.Usage.CacheCreationInputTokens,
					CacheReadInputTokens:     data.Usage.CacheReadInputTokens,
				}}) {
					return
				}

			case "message_stop":
				send(ir.StreamEvent{Kind: ir.EvStreamEnd, StreamEnd: &ir.StreamEnd{}})
				return
			}
		}
	}()
	return out
}
