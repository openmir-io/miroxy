package selector

import (
	"context"
	"errors"

	"miroxy/core/cred"
	"miroxy/core/ir"
	"miroxy/core/upstream"
)

// ErrNoSelection is returned by Select when no credential is currently available
// (all are in rate-limit cooldown or circuit-broken).
var ErrNoSelection = errors.New("no available selection")

// ExecutionPlan carries everything needed for one upstream attempt.
// Returned by Select; passed back to Release on completion.
type ExecutionPlan struct {
	SelectionID string
	// PoolName is the named credpool this attempt's credential came from
	// (config's credpools.<name> key, or the model_routes entry name for an
	// inline credpool). Used to attribute stats to a credpool/provider
	// without guessing from SelectionID, which is only unique within a pool.
	PoolName   string
	Credential cred.Credential // typed auth material; Apply() attaches it to the request — never log the value
	Model      string          // upstream provider model name, e.g. gemini-2.5-flash
	Upstream   upstream.UpstreamAdapter
	// ReleaseHook is set by RoutingSelector so Release() reaches the correct inner
	// selector. CredPool and TargetSelector leave this nil.
	ReleaseHook func(*ExecutionPlan, error)

	// Protocol is this target's static upstream wire protocol (e.g. "gemini",
	// "openai", "anthropic"). UpstreamExecutor compares it against the
	// dynamically-detected client protocol (which DownstreamAdapter actually
	// decoded this request) to decide whether to dispatch via Upstream (real
	// IR transform) or PassthroughUpstream (raw bytes, no transform).
	Protocol string
	// PassthroughUpstream forwards this attempt's original request/response
	// bytes verbatim. Selected instead of Upstream when Protocol matches the
	// request's actual client protocol, or when ForcePassthrough is set.
	PassthroughUpstream upstream.UpstreamAdapter
	// ForcePassthrough mirrors the model_routes `mode: passthrough` override:
	// always use PassthroughUpstream regardless of protocol match.
	ForcePassthrough bool
}

// Selector selects a healthy credential+model combination for an upstream request.
// CredPool, ModelGroupSelector, and ProviderSelector all implement this interface.
type Selector interface {
	Select(ctx context.Context, req *ir.IRRequest, model string) (*ExecutionPlan, error)
	Release(plan *ExecutionPlan, err error)
}
