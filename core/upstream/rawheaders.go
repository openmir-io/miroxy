package upstream

import (
	"context"
	"net/http"
)

type rawHeadersKey struct{}

// WithRawHeaders attaches the client's original request headers to ctx, for
// the same reason as WithRawBody: PassthroughAdapter forwards them verbatim
// (minus a small hop-by-hop/auth blocklist it owns) instead of guessing
// protocol-specific values (e.g. Anthropic's required anthropic-version).
func WithRawHeaders(ctx context.Context, h http.Header) context.Context {
	if len(h) == 0 {
		return ctx
	}
	return context.WithValue(ctx, rawHeadersKey{}, h)
}

// RawHeadersFromContext returns the headers attached by WithRawHeaders, if any.
func RawHeadersFromContext(ctx context.Context) (http.Header, bool) {
	h, ok := ctx.Value(rawHeadersKey{}).(http.Header)
	return h, ok
}
