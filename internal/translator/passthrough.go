package translator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"miroxy/core/cred"
	"miroxy/internal/types"
)

// PassthroughTranslator forwards the Anthropic-format request body to the upstream
// endpoint without any format conversion. It is used when the client protocol and
// the upstream protocol are identical or compatible (e.g. client → Bedrock Claude,
// which accepts the same Anthropic Messages API format).
//
// The Credential is applied to the outgoing request; the upstream response and SSE
// events are returned as-is (deserialized as Anthropic wire format).
type PassthroughTranslator struct {
	endpoint       string // full URL for non-streaming POST
	streamEndpoint string // full URL for streaming POST (falls back to endpoint when empty)
}

// NewPassthrough creates a PassthroughTranslator.
// endpoint is the full upstream URL (e.g. https://bedrock-runtime.../invoke).
// streamEndpoint is optional; if empty, endpoint is used for streaming too.
func NewPassthrough(endpoint, streamEndpoint string) *PassthroughTranslator {
	return &PassthroughTranslator{endpoint: endpoint, streamEndpoint: streamEndpoint}
}

func (t *PassthroughTranslator) streamURL() string {
	if t.streamEndpoint != "" {
		return t.streamEndpoint
	}
	return t.endpoint
}

func (t *PassthroughTranslator) ToUpstream(ctx context.Context, req *types.MessageRequest, credential cred.Credential) (*http.Request, error) {
	return t.buildHTTPRequest(ctx, req, t.endpoint, credential)
}

func (t *PassthroughTranslator) ToUpstreamStream(ctx context.Context, req *types.MessageRequest, credential cred.Credential) (*http.Request, error) {
	return t.buildHTTPRequest(ctx, req, t.streamURL(), credential)
}

func (t *PassthroughTranslator) buildHTTPRequest(ctx context.Context, req *types.MessageRequest, url string, credential cred.Credential) (*http.Request, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("passthrough: marshal request: %w", err)
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

// FromUpstream deserializes an Anthropic-format response body.
func (t *PassthroughTranslator) FromUpstream(resp *http.Response) (*types.MessageResponse, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("passthrough: read response: %w", err)
	}
	var msgResp types.MessageResponse
	if err := json.Unmarshal(body, &msgResp); err != nil {
		return nil, fmt.Errorf("passthrough: parse response: %w", err)
	}
	return &msgResp, nil
}

// StreamFromUpstream forwards upstream Anthropic SSE events directly to the channel.
func (t *PassthroughTranslator) StreamFromUpstream(ctx context.Context, resp *http.Response, msgID, modelAlias string) (<-chan types.SSEEvent, error) {
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
