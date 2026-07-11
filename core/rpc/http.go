package rpc

import (
	"context"
	"net/http"

	"miroxy/core/dispatch"
)

// NewHTTPDispatcher wraps an *http.Client as a dispatch.Dispatcher.
// This is the default transport for all upstream LLM API calls and any
// HTTP-protocol sidecar services.
//
// Future: replace the bare *http.Client with NewHTTPClient(cfg) which adds
// retry, circuit-breaking, and rate-limit detection via a RoundTripper chain
// (see plan.md). Callers stay the same — only the client passed in changes.
func NewHTTPDispatcher(client *http.Client) *HTTPDispatcher {
	return &HTTPDispatcher{client: client}
}

// HTTPDispatcher implements dispatch.Dispatcher using net/http.
// Physical transport only — auth is applied by Credential.Apply() before the
// request reaches the dispatcher, and retry logic lives in UpstreamExecutor.
type HTTPDispatcher struct {
	client *http.Client
}

var _ dispatch.Dispatcher = (*HTTPDispatcher)(nil)

func (d *HTTPDispatcher) Do(_ context.Context, req *http.Request) (*http.Response, error) {
	return d.client.Do(req)
}

// NewHTTPClient builds an *http.Client with a shared RoundTripper chain for
// retry, circuit-breaking, and rate-limit handling.
//
// TODO(rpc): implement RoundTripper chain — see plan.md for the full spec.
// Until then, callers that need retry/circuit-break logic implement it themselves
// (e.g. UpstreamExecutor's retry loop in internal/server/upstream.go).
func NewHTTPClient(cfg Config) *http.Client {
	cfg = cfg.withDefaults()
	return &http.Client{
		Timeout: cfg.Timeout, // basic timeout; full retry chain is TODO
	}
}
