package router

import (
	"context"
	"time"

	"miroxy/core/dispatch"
	"miroxy/core/selector"
)

// ModelInfo describes a model from both the client-facing and upstream perspectives.
type ModelInfo struct {
	Name          string // client-facing alias, e.g. claude-sonnet-4-6
	UpstreamModel string // upstream model name, e.g. gemini-2.5-flash
	Provider      string // gemini / openai / kiro
}

// RouteTarget carries the resolved upstream provider for a single request.
// Set by the server (v1 config-driven lookup) or a Router plugin before pipeline.Run.
type RouteTarget struct {
	Selector   selector.Selector
	Model      ModelInfo
	Timeout    time.Duration
	Invisible  bool               // when true, upstream error bodies are passed through verbatim
	Dispatcher dispatch.Dispatcher // how to send requests to this backend (HTTP, SDK, etc.)
}

// Router maps a client model alias to a RouteTarget.
// In v1 this is a config-driven lookup; future implementations may add ML routing.
type Router interface {
	Route(ctx context.Context, model string) (*RouteTarget, error)
}
