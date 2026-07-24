package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"miroxy/core/cred"
	"miroxy/core/ir"
	coreup "miroxy/core/upstream"
	"miroxy/internal/types"
	"miroxy/internal/wireformat"
)

// bedrockAnthropicVersion is the fixed value Bedrock's InvokeModel requires
// in the request body for Anthropic (Claude) models — Bedrock's own version
// tag, unrelated to (and always different from) real Anthropic's
// anthropic-version header value.
const bedrockAnthropicVersion = "bedrock-2023-05-31"

// BedrockAdapter dispatches to AWS Bedrock's InvokeModel /
// InvokeModelWithResponseStream endpoints for Anthropic (Claude) models.
// It converts IR<->wire via the same AnthropicConverter every genuine
// Anthropic upstream uses (see AnthropicUpstream) — Bedrock's InvokeModel
// body is the same Messages API shape with two differences: the model
// belongs in the URL path instead of the body, and anthropic_version is a
// body field instead of a header. Auth is AWS SigV4 (cred.SigV4Credential)
// instead of an API key, and streaming responses arrive as AWS EventStream
// binary frames instead of raw SSE — bedrock_eventstream.go translates
// those into plain SSE so the existing parseAnthropicSSE/anthropicSSEToIR
// helpers (passthrough.go) can parse them unchanged.
type BedrockAdapter struct {
	upstreamModel string
	baseURL       string
}

var _ coreup.UpstreamAdapter = (*BedrockAdapter)(nil)

// NewBedrock creates a BedrockAdapter pointed at apiBase (Bedrock's runtime
// endpoint, e.g. https://bedrock-runtime.us-east-1.amazonaws.com).
func NewBedrock(upstreamModel, apiBase string) *BedrockAdapter {
	return &BedrockAdapter{upstreamModel: upstreamModel, baseURL: strings.TrimRight(apiBase, "/")}
}

func (b *BedrockAdapter) endpointURL() string {
	return b.baseURL + "/model/" + url.PathEscape(b.upstreamModel) + "/invoke"
}

func (b *BedrockAdapter) streamEndpointURL() string {
	return b.baseURL + "/model/" + url.PathEscape(b.upstreamModel) + "/invoke-with-response-stream"
}

func (b *BedrockAdapter) ToUpstream(ctx context.Context, req *ir.IRRequest, credential cred.Credential) (*http.Request, error) {
	return b.build(ctx, req, b.endpointURL(), credential)
}

func (b *BedrockAdapter) ToUpstreamStream(ctx context.Context, req *ir.IRRequest, credential cred.Credential) (*http.Request, error) {
	return b.build(ctx, req, b.streamEndpointURL(), credential)
}

func (b *BedrockAdapter) build(ctx context.Context, req *ir.IRRequest, target string, credential cred.Credential) (*http.Request, error) {
	wireReq := (wireformat.AnthropicConverter{}).RequestFromIR(req, b.upstreamModel)
	body, err := json.Marshal(wireReq)
	if err != nil {
		return nil, fmt.Errorf("bedrock upstream: marshal request: %w", err)
	}
	body, err = bedrockTransformRequestBody(body)
	if err != nil {
		return nil, fmt.Errorf("bedrock upstream: transform request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("bedrock upstream: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := credential.Apply(httpReq); err != nil {
		return nil, fmt.Errorf("bedrock upstream: apply credential: %w", err)
	}
	return httpReq, nil
}

// bedrockTransformRequestBody rewrites AnthropicConverter's output for
// Bedrock's InvokeModel: deletes "model" (it is in the URL path; Bedrock
// rejects an unrecognized body field) and sets "anthropic_version".
func bedrockTransformRequestBody(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	delete(fields, "model")
	version, err := json.Marshal(bedrockAnthropicVersion)
	if err != nil {
		return nil, err
	}
	fields["anthropic_version"] = version
	return json.Marshal(fields)
}

func (b *BedrockAdapter) FromUpstream(resp *http.Response) (*ir.IRResponse, error) {
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("bedrock upstream: read response: %w", err)
	}
	var msgResp types.MessageResponse
	if err := json.Unmarshal(respBody, &msgResp); err != nil {
		return nil, fmt.Errorf("bedrock upstream: parse response: %w", err)
	}
	return (wireformat.AnthropicConverter{}).ResponseToIR(&msgResp), nil
}

// StreamFromUpstream wraps resp.Body — AWS EventStream binary frames — with
// a translator that re-emits it as plain SSE (bedrock_eventstream.go), then
// reuses the same Anthropic SSE parser every genuine Anthropic upstream
// target uses. resp is shallow-copied so the caller's *http.Response is
// never mutated in place.
func (b *BedrockAdapter) StreamFromUpstream(ctx context.Context, resp *http.Response, msgID, modelAlias string) (<-chan ir.StreamEvent, error) {
	wrapped := *resp
	wrapped.Body = newEventStreamSSEReader(resp.Body)
	wire, err := parseAnthropicSSE(ctx, &wrapped)
	if err != nil {
		return nil, err
	}
	return anthropicSSEToIR(ctx, wire), nil
}
