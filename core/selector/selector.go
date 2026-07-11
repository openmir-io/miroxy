package selector

import (
	"context"
	"errors"

	"miroxy/core/cred"
	"miroxy/core/upstream"
	"miroxy/internal/types"
)

// ErrNoSelection is returned by Select when no credential is currently available
// (all are in rate-limit cooldown or circuit-broken).
var ErrNoSelection = errors.New("no available selection")

// ExecutionPlan carries everything needed for one upstream attempt.
// Returned by Select; passed back to Release on completion.
type ExecutionPlan struct {
	SelectionID string
	Credential  cred.Credential       // typed auth material; Apply() attaches it to the request — never log the value
	Model       string                // upstream provider model name, e.g. gemini-2.5-flash
	Upstream    upstream.UpstreamAdapter
	// ReleaseHook is set by RoutingSelector so Release() reaches the correct inner
	// selector. CredPool and TargetSelector leave this nil.
	ReleaseHook func(*ExecutionPlan, error)
}

// Selector selects a healthy credential+model combination for an upstream request.
// CredPool, ModelGroupSelector, and ProviderSelector all implement this interface.
type Selector interface {
	Select(ctx context.Context, req *types.MessageRequest) (*ExecutionPlan, error)
	Release(plan *ExecutionPlan, err error)
}
