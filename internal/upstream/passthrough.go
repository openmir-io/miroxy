package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"miroxy/core/cred"
	coreup "miroxy/core/upstream"
	"miroxy/internal/types"
)

// PassthroughAdapter forwards a request/response verbatim, with no IR
// transform in either direction. The caller (UpstreamExecutor) only selects
// this adapter for an attempt when it has already established that the
// client's actual wire protocol matches this target's protocol — see
// ExecutionPlan.PassthroughUpstream/ForcePassthrough — so every method here
// can assume raw byte forwarding is correct without re-checking protocols.
//
// ToUpstream/ToUpstreamStream send coreup.RawBodyFromContext(ctx) verbatim
// when the caller attached it (the normal case); falling back to
// json.Marshal(req) only covers a caller that selected this adapter without
// raw bytes available. FromUpstream always raw-captures the response body
// into types.MessageResponse's RawBody escape hatch; the delivery layer
// (internal/server) writes it directly instead of re-encoding.
type PassthroughAdapter struct {
	endpoint       string // full URL for non-streaming POST
	streamEndpoint string // full URL for streaming POST (falls back to endpoint when empty)
}

// NewPassthrough creates a PassthroughTranslator.
// endpoint is the full upstream URL (e.g. https://bedrock-runtime.../invoke).
// streamEndpoint is optional; if empty, endpoint is used for streaming too.
func NewPassthrough(endpoint, streamEndpoint string) *PassthroughAdapter {
	return &PassthroughAdapter{endpoint: endpoint, streamEndpoint: streamEndpoint}
}

func (t *PassthroughAdapter) streamURL() string {
	if t.streamEndpoint != "" {
		return t.streamEndpoint
	}
	return t.endpoint
}

func (t *PassthroughAdapter) ToUpstream(ctx context.Context, req *types.MessageRequest, credential cred.Credential) (*http.Request, error) {
	return t.buildHTTPRequest(ctx, req, t.endpoint, credential)
}

func (t *PassthroughAdapter) ToUpstreamStream(ctx context.Context, req *types.MessageRequest, credential cred.Credential) (*http.Request, error) {
	return t.buildHTTPRequest(ctx, req, t.streamURL(), credential)
}

func (t *PassthroughAdapter) buildHTTPRequest(ctx context.Context, req *types.MessageRequest, url string, credential cred.Credential) (*http.Request, error) {
	body, ok := coreup.RawBodyFromContext(ctx)
	if !ok {
		var err error
		body, err = json.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("passthrough: marshal request: %w", err)
		}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("passthrough: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := credential.Apply(httpReq); err != nil {
		return nil, fmt.Errorf("passthrough: apply credential: %w", err)
	}
	return httpReq, nil
}

// FromUpstream reads the raw response body and returns it via the
// MessageResponse.RawBody escape hatch — no parsing, since the caller has
// already established the upstream's wire shape matches what the client
// expects verbatim.
func (t *PassthroughAdapter) FromUpstream(resp *http.Response) (*types.MessageResponse, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("passthrough: read response: %w", err)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	return &types.MessageResponse{RawBody: body, RawContentType: ct, RawStatus: resp.StatusCode}, nil
}

// StreamFromUpstream is unreachable in normal operation — UpstreamExecutor
// bypasses it for passthrough attempts (see LLMContext.SetRawStream) and
// relays raw bytes directly. Kept only for UpstreamAdapter conformance and as
// a defensive fallback if this adapter is ever invoked outside that path.
func (t *PassthroughAdapter) StreamFromUpstream(ctx context.Context, resp *http.Response, msgID, modelAlias string) (<-chan types.SSEEvent, error) {
	return parseAnthropicSSE(ctx, resp)
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
