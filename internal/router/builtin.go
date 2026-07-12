// Package router provides the builtin (in-process) implementation of
// core/router.Router. A future sidecar-backed Router (e.g. an external
// smart-routing service) would live alongside BuiltinRouter here, exactly
// how internal/cred holds both OAuthSource (builtin) and CredSource
// (sidecar) behind core/cred.CredentialSource — no shared transport
// abstraction, each implementation picks HTTP/gRPC/whatever internally.
package router

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"miroxy/core/dispatch"
	coreouter "miroxy/core/router"
	"miroxy/core/selector"
	"miroxy/internal/config"
)

// ErrModelNotFound is returned by BuiltinRouter.Route when the requested
// model matches no model_routes entry and no provider-tagged passthrough
// fallback exists either.
var ErrModelNotFound = errors.New("model not found")

// RoutingTable is the per-request-lookup data BuiltinRouter needs: which
// Selector serves each model_routes entry, its timeout, and the
// provider-tagged passthrough fallback selectors for models not explicitly
// listed in model_routes. Building this table (from config.yaml, credpools,
// etc.) is server wiring and stays in internal/server — the same split as
// CredSource's construction staying in internal/server while CredSource
// itself only owns runtime behavior.
type RoutingTable struct {
	Selectors            map[string]selector.Selector
	Timeouts             map[string]time.Duration
	PassthroughSelectors map[string]selector.Selector
}

// BuiltinRouter is the in-process Router: a config-driven model-name lookup.
// It holds its own atomically-swapped config/table so Route() never blocks
// on a mutex and is safe under concurrent Reload. UpdateConfig/UpdateRouting
// must be called once before the first real Route() (New()) and again on
// every config reload.
type BuiltinRouter struct {
	cfg        atomic.Pointer[config.Config]
	table      atomic.Pointer[RoutingTable]
	dispatcher dispatch.Dispatcher
}

var _ coreouter.Router = (*BuiltinRouter)(nil)

// NewBuiltinRouter creates a BuiltinRouter that dispatches through d.
func NewBuiltinRouter(d dispatch.Dispatcher) *BuiltinRouter {
	return &BuiltinRouter{dispatcher: d}
}

// UpdateConfig swaps in a new config snapshot (LookupModel's model_routes).
func (b *BuiltinRouter) UpdateConfig(cfg *config.Config) { b.cfg.Store(cfg) }

// UpdateRouting swaps in a new routing table (built by internal/server from
// the same config generation passed to UpdateConfig).
func (b *BuiltinRouter) UpdateRouting(t *RoutingTable) { b.table.Store(t) }

// Route resolves a client-requested model name to a RouteTarget.
func (b *BuiltinRouter) Route(ctx context.Context, model string) (*coreouter.RouteTarget, error) {
	cfg := b.cfg.Load()
	entry, ok := cfg.LookupModel(model)
	if !ok {
		return nil, ErrModelNotFound
	}
	if entry.ModelName != model {
		slog.Debug("model routed to default", "requested", model, "routed", entry.ModelName)
	}

	table := b.table.Load()
	sel := table.Selectors[entry.ModelName]
	providerModel := entry.ProviderModel

	// Passthrough: entry came from LookupModel's inferred-provider step. No
	// pre-built selector exists; use the passthrough selector for the
	// provider and forward the original model name to the upstream as-is.
	if sel == nil && entry.Provider != "" {
		sel = table.PassthroughSelectors[entry.Provider]
		if providerModel == "" {
			providerModel = model
		}
	}

	return &coreouter.RouteTarget{
		Invisible: entry.Invisible,
		Model: coreouter.ModelInfo{
			Name:          entry.ModelName,
			ProviderModel: providerModel,
			Provider:      entry.Provider,
		},
		Selector:   sel,
		Timeout:    table.Timeouts[entry.ModelName],
		Dispatcher: b.dispatcher,
	}, nil
}
