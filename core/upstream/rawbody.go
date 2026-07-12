package upstream

import "context"

type rawBodyKey struct{}

// WithRawBody attaches the client's original, pre-canonicalization request
// bytes to ctx. Only adapters that support byte-for-byte passthrough (see
// PassthroughAdapter in internal/upstream) read this; adapters that always
// transform (Gemini, OpenAI, …) never call RawBodyFromContext and so ignore
// it unconditionally.
func WithRawBody(ctx context.Context, body []byte) context.Context {
	if len(body) == 0 {
		return ctx
	}
	return context.WithValue(ctx, rawBodyKey{}, body)
}

// RawBodyFromContext returns the bytes attached by WithRawBody, if any.
func RawBodyFromContext(ctx context.Context) ([]byte, bool) {
	b, ok := ctx.Value(rawBodyKey{}).([]byte)
	return b, ok
}
