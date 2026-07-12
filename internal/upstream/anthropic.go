package upstream

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

// AnthropicUpstream dispatches to a genuinely Anthropic-protocol upstream
// (the real Anthropic API, or an Anthropic-compatible backend) when the
// request's client protocol does NOT match — so it isn't eligible for the
// byte-for-byte PassthroughUpstream — but the target still is Anthropic
// wire-shaped. Because the canonical types.MessageRequest/MessageResponse
// were themselves modeled on Anthropic's Messages API, this transform is
// near-identity: set the upstream's own model name, marshal, send.
type AnthropicUpstream struct {
	providerModel  string
	endpoint       string
	streamEndpoint string
}

// NewAnthropicUpstream creates an AnthropicUpstream pointed at apiBase
// (expected to already include the full messages path, matching how
// api_base is used by every other adapter in this package).
func NewAnthropicUpstream(providerModel, apiBase string) *AnthropicUpstream {
	return &AnthropicUpstream{providerModel: providerModel, endpoint: apiBase, streamEndpoint: apiBase}
}

func (a *AnthropicUpstream) ToUpstream(ctx context.Context, req *types.MessageRequest, credential cred.Credential) (*http.Request, error) {
	return a.build(ctx, req, a.endpoint, credential)
}

func (a *AnthropicUpstream) ToUpstreamStream(ctx context.Context, req *types.MessageRequest, credential cred.Credential) (*http.Request, error) {
	return a.build(ctx, req, a.streamEndpoint, credential)
}

func (a *AnthropicUpstream) build(ctx context.Context, req *types.MessageRequest, url string, credential cred.Credential) (*http.Request, error) {
	// Shallow-copy before overwriting Model — req is the shared pipeline
	// request; other retry attempts (possibly against a different target)
	// must not see this target's provider model name.
	outReq := *req
	outReq.Model = a.providerModel
	body, err := json.Marshal(&outReq)
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

func (a *AnthropicUpstream) FromUpstream(resp *http.Response) (*types.MessageResponse, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("anthropic upstream: read response: %w", err)
	}
	var msgResp types.MessageResponse
	if err := json.Unmarshal(body, &msgResp); err != nil {
		return nil, fmt.Errorf("anthropic upstream: parse response: %w", err)
	}
	return &msgResp, nil
}

func (a *AnthropicUpstream) StreamFromUpstream(ctx context.Context, resp *http.Response, msgID, modelAlias string) (<-chan types.SSEEvent, error) {
	return parseAnthropicSSE(ctx, resp)
}
