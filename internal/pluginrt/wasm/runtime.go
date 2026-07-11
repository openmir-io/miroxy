// Package wasm provides the wazero-embedded sandbox runtime for WASM plugins.
//
// One Runtime is created per WASM plugin binary at load time; each incoming
// request gets its own Module instance (stateless guest model). The runtime
// enforces the core redlines: SSE stream handling, KeyPool locking,
// 429-retry timing, protocol translation, and secret decryption are
// NEVER callable from guest WASM — see hostfuncs.go for the allow-list.
//
// TODO(pluginrt): wire wazero when the first WASM plugin (e.g. security or compress)
// is ready to ship. Add github.com/tetratelabs/wazero to go.mod at that point.
package wasm

import "context"

// Runtime manages the lifecycle of a wazero runtime instance.
type Runtime struct {
	// TODO(pluginrt): wazero.Runtime — add after `go get github.com/tetratelabs/wazero`.
}

// New creates a wazero runtime configured for miroxy WASM plugins.
//
// TODO(pluginrt): replace stub with:
//
//	wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true))
func New() (*Runtime, error) {
	return &Runtime{}, nil
}

// Close tears down the runtime and all loaded WASM module instances.
func (r *Runtime) Close(_ context.Context) error {
	return nil
}
