package dispatch

import (
	"context"
	"net/http"
)

// Dispatcher sends a prepared HTTP request to an upstream provider and returns
// the raw response. It owns physical transport only — auth (including AWS
// SigV4, via cred.SigV4Credential) is applied by the Credential before the
// request reaches the Dispatcher, so one net/http-based implementation
// (HTTPDispatcher, in internal/server) covers every upstream.
type Dispatcher interface {
	Do(ctx context.Context, req *http.Request) (*http.Response, error)
}
