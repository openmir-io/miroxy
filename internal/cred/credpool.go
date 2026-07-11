package cred

import (
	"context"
	"log/slog"

	corecred "miroxy/core/cred"
)

// CredpoolClient is the interface satisfied by the connect-go generated client
// for CredpoolService (see credpool.proto).
//
// TODO(cred): after running `buf generate` from credpool.proto, replace this
// hand-rolled interface with the generated credpoolv1connect.CredpoolServiceClient
// and wrap its connect.Request/connect.Response envelope in a thin adapter that
// implements CredpoolClient — or change CredpoolSource to hold the generated
// client directly if the added verbosity is acceptable.
type CredpoolClient interface {
	// Acquire returns a typed Credential for the given model+provider.
	// leaseID must be passed back to Release when the upstream call completes.
	Acquire(ctx context.Context, model, provider string) (credential corecred.Credential, leaseID string, err error)
	// Release reports the outcome of the upstream call and returns the lease.
	Release(ctx context.Context, leaseID string, rateLimited, callError bool) error
}

// localChain is the subset of OAuthSource / StaticSource used for fallback.
// Declared as a local interface so CredpoolSource does not need to import
// core/selector directly (Go structural typing handles the match).
type localChain interface {
	Credential(ctx context.Context) (corecred.Credential, error)
}

// CredpoolSource implements selector.CredentialSource.
//
// It attempts Credpool first (shared rate-limit state across all miroxy
// instances). On any transport error or Credpool unavailability it falls back
// to the local credential chain (fallback), which is typically an *OAuthSource
// or core/selector.StaticSource.
//
// The fallback decision lives entirely inside this type — pipeline/loader.go
// sees exactly one credential plugin and has no knowledge of the internal
// Credpool → local chain logic.
type CredpoolSource struct {
	client   CredpoolClient // nil = always use fallback (pre-connect-go wiring)
	fallback localChain
	model    string
	provider string
}

// NewCredpoolSource constructs a CredpoolSource.
//
// client may be nil before the connect-go generated client is wired in; in that
// case every call falls through to fallback immediately.
// fallback is required — it is the existing local credential chain.
func NewCredpoolSource(client CredpoolClient, fallback localChain, model, provider string) *CredpoolSource {
	return &CredpoolSource{
		client:   client,
		fallback: fallback,
		model:    model,
		provider: provider,
	}
}

// Credential implements selector.CredentialSource.
//
// Tries Credpool first. On any failure it logs a warning and falls back to the
// local chain. A Credpool transport failure never surfaces to the caller — the
// proxy continues working with local credentials.
func (c *CredpoolSource) Credential(ctx context.Context) (corecred.Credential, error) {
	if c.client != nil {
		credential, _, err := c.client.Acquire(ctx, c.model, c.provider)
		if err == nil {
			return credential, nil
		}
		slog.Warn("credpool: acquire failed, using local credential chain",
			"model", c.model, "provider", c.provider, "error", err)
	}
	return c.fallback.Credential(ctx)
}

// TODO(cred): add Release(ctx, leaseID, rateLimited, callError) and call it
// from the server executor after each upstream attempt. Currently leases expire
// by TTL on the Credpool side — correct but suboptimal (Credpool cannot react
// to 429s in real time). Requires threading leaseID through ExecutionPlan or a
// separate context value; defer until the executor refactor for Epic 3.
