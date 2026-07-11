// Package rpc provides shared transport infrastructure for all cross-process calls
// in miroxy: upstream LLM API calls, credpoold gRPC, router/semantic sidecars, etc.
//
// Every domain that needs to reach an external process uses this package's factories
// rather than implementing retry, circuit-breaking, or error normalisation itself.
//
// Three execution models (native / WASM / RPC) apply to every extensible domain.
// This package covers the RPC tier — the shared machinery that all cross-process
// callers compose, regardless of whether they use HTTP REST, gRPC/Connect, or a
// provider-native SDK.
//
// Planned factories (see plan.md for implementation roadmap):
//   - NewHTTPClient(cfg) *http.Client — retry + circuit-break via RoundTripper chain
//   - NewConnectInterceptors(cfg) []connect.Interceptor — gRPC/Connect interceptor chain
package rpc

import "time"

// Config is the shared transport configuration applied by all RPC factories.
// Zero values use safe defaults (3 retries, 30s timeout, 5-fault circuit threshold).
type Config struct {
	MaxRetries       int           // max retry attempts on transient errors (default 3)
	Timeout          time.Duration // per-call timeout (default 30s)
	CircuitThreshold int           // consecutive failures before opening circuit (default 5)
	CircuitCooldown  time.Duration // circuit open duration before half-open probe (default 60s)
}

func (c Config) withDefaults() Config {
	if c.MaxRetries == 0 {
		c.MaxRetries = 3
	}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.CircuitThreshold == 0 {
		c.CircuitThreshold = 5
	}
	if c.CircuitCooldown == 0 {
		c.CircuitCooldown = 60 * time.Second
	}
	return c
}
