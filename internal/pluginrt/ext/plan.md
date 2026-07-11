# pluginrt/ext — External Process Plugin Execution

## Purpose

Registry and invocation layer for plugins that run in an external process (sidecar).
This is the RPC tier of miroxy's three-tier execution model (native / WASM / ext).

Uses `core/rpc` for transport (retry, circuit-break, error normalisation) rather than
re-implementing it per domain.

## Current files

- `transport.go` — `Transport` interface + `TransportConfig` (protocol-agnostic)
- `registry.go` — domain → factory registration (`NewClient(domain, cfg)`)

## Planned additions

- `grpc.go` — concrete gRPC/Connect transport (once a non-connect-go consumer exists)
- `http.go` — concrete HTTP transport for REST-based sidecar services

## Relationship to core/rpc

`pluginrt/ext` knows HOW to manage plugin lifecycle (register, dial, health-check,
reconnect). `core/rpc` knows HOW to make individual calls resilient (retry, circuit).
Each external plugin client combines both:

```
domain client (credpool.go, router_sidecar.go, …)
  uses core/rpc.NewHTTPClient or core/rpc.NewConnectInterceptors
  registers with pluginrt/ext.Register(domain, factory)
  pluginrt/ext.NewClient(domain, cfg) returns a live Transport
```

## Domains that currently use or plan to use ext

| Domain | Protocol | Status |
|---|---|---|
| credpoold | gRPC/Connect | proto defined, client stubbed |
| smart router | gRPC | future |
| semantic transform | gRPC or HTTP | future |
