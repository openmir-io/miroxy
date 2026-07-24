package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"miroxy/core/cred"
	"miroxy/core/ir"
	"miroxy/internal/types"
	"miroxy/internal/wireformat"
)

// AnthropicUpstream dispatches to a genuinely Anthropic-protocol upstream
// (the real Anthropic API, or an Anthropic-compatible backend) when the
// request's client protocol does NOT match — so it isn't eligible for the
// byte-for-byte PassthroughUpstream — but the target still is Anthropic
// wire-shaped. Converts IR↔Anthropic-wire via AnthropicConverter, same as
// every other upstream adapter converts IR↔its own wire format.
type AnthropicUpstream struct {
	upstreamModel  string
	endpoint       string
	streamEndpoint string
}

// NewAnthropicUpstream creates an AnthropicUpstream pointed at apiBase, a
// bare host (e.g. https://api.anthropic.com) — the /v1/messages path is
// appended here, matching how every other adapter owns its own path suffix.
func NewAnthropicUpstream(upstreamModel, apiBase string) *AnthropicUpstream {
	endpoint := strings.TrimRight(apiBase, "/") + "/v1/messages"
	return &AnthropicUpstream{upstreamModel: upstreamModel, endpoint: endpoint, streamEndpoint: endpoint}
}

func (a *AnthropicUpstream) ToUpstream(ctx context.Context, req *ir.IRRequest, credential cred.Credential) (*http.Request, error) {
	return a.build(ctx, req, a.endpoint, credential)
}

func (a *AnthropicUpstream) ToUpstreamStream(ctx context.Context, req *ir.IRRequest, credential cred.Credential) (*http.Request, error) {
	return a.build(ctx, req, a.streamEndpoint, credential)
}

func (a *AnthropicUpstream) build(ctx context.Context, req *ir.IRRequest, url string, credential cred.Credential) (*http.Request, error) {
	wireReq := (wireformat.AnthropicConverter{}).RequestFromIR(req, a.upstreamModel)
	body, err := json.Marshal(wireReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic upstream: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic upstream: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	if err := credential.Apply(httpReq); err != nil {
		return nil, fmt.Errorf("anthropic upstream: apply credential: %w", err)
	}
	return httpReq, nil
}

func (a *AnthropicUpstream) FromUpstream(resp *http.Response) (*ir.IRResponse, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("anthropic upstream: read response: %w", err)
	}
	var msgResp types.MessageResponse
	if err := json.Unmarshal(body, &msgResp); err != nil {
		return nil, fmt.Errorf("anthropic upstream: parse response: status=%d body=%q: %w", resp.StatusCode, truncate(body, 500), err)
	}
	return (wireformat.AnthropicConverter{}).ResponseToIR(&msgResp), nil
}

func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}

func (a *AnthropicUpstream) StreamFromUpstream(ctx context.Context, resp *http.Response, msgID, modelAlias string) (<-chan ir.StreamEvent, error) {
	wire, err := parseAnthropicSSE(ctx, resp)
	if err != nil {
		return nil, err
	}
	return anthropicSSEToIR(ctx, wire), nil
}
