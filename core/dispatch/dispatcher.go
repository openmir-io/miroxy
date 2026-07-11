package dispatch

import (
	"context"
	"net/http"
)

// Dispatcher sends a prepared HTTP request to an upstream provider and returns
// the raw response. It owns physical transport only — auth is applied by the
// Credential before the request reaches the Dispatcher.
//
// Two implementations are planned:
//   - HTTPDispatcher (internal/server): net/http client; covers all REST APIs.
//   - SDKDispatcher (future): provider-native SDK (e.g. AWS Bedrock SDK) that
//     handles transport and auth internally; needed for SigV4 providers.
type Dispatcher interface {
	Do(ctx context.Context, req *http.Request) (*http.Response, error)
}
