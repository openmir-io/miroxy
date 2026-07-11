# core/rpc — Shared RPC Transport Infrastructure

## Purpose

Generic, reusable transport infrastructure for all cross-process calls in miroxy.
Any domain that communicates with an external process (upstream LLM API, credpoold,
smart router sidecar, semantic transform sidecar) composes from this package rather
than implementing retry/circuit-break/error-normalisation itself.

## What belongs here

- `http.go` — `NewHTTPClient(cfg Config) *http.Client`
  Wraps a bare `*http.Client` with a `RoundTripper` chain:
    1. `RateLimitRoundTripper` — detects HTTP 429, applies backoff
    2. `CircuitBreakerRoundTripper` — opens on N consecutive failures
    3. `RetryRoundTripper` — retries on transient errors (5xx, network)
    4. `net/http.DefaultTransport` — actual TCP connection

- `grpc.go` — `NewConnectInterceptors(cfg Config) []connect.Interceptor`
  Connect-go interceptor chain providing the same behaviours as the HTTP chain
  but at the gRPC/Connect level:
    - Timeout interceptor
    - Retry interceptor (on connect.CodeUnavailable, connect.CodeDeadlineExceeded)
    - Circuit-breaker interceptor
    - Error-normalisation interceptor (gRPC status → miroxy RPC error type)

- `retry.go` — shared retry logic (backoff schedule, jitter) used by both HTTP and gRPC
- `circuit.go` — shared circuit-breaker state machine used by both
- `errors.go` — unified `RPCError` type that downstream callers handle regardless of protocol

## What does NOT belong here

- Provider-specific SDK calls (AWS Bedrock SDK, Azure SDK) — those live in each upstream
  adapter's package and don't use a generic Dispatcher
- Per-domain proto types — each domain owns its own .proto and generated client code
- Plugin lifecycle (load/unload/health) — that's `pluginrt/ext/`

## Who uses this

| Caller | Protocol | Factory |
|---|---|---|
| upstream LLM calls (gemini, openai, …) | HTTP REST | `NewHTTPClient` |
| credpoold client | gRPC/Connect | `NewConnectInterceptors` |
| router sidecar (future) | gRPC/Connect | `NewConnectInterceptors` |
| semantic sidecar (future) | gRPC or HTTP | either factory |

## Implementation order

1. `http.go` — implement `RetryRoundTripper` + `CircuitBreakerRoundTripper`; wire into
   `internal/server/http_dispatcher.go` replacing the bare `*http.Client`
2. `grpc.go` — implement Connect interceptors; wire into `internal/cred/credpool.go`
   replacing the TODO stub once `buf generate` runs on `credpool.proto`
3. `errors.go` — unified error type once gRPC and HTTP paths both exist
4. `circuit.go` / `retry.go` — extract shared state machine once both transports are live
