package rpc

// grpc.go — Connect/gRPC interceptor chain factory.
//
// TODO(rpc): implement NewConnectInterceptors(cfg Config) []connect.Interceptor
// once connectrpc.com/connect is added to go.mod (triggered by credpoold wiring).
//
// The interceptor chain will provide the same cross-cutting behaviours as the
// HTTP RoundTripper chain in http.go, but at the Connect/gRPC level:
//
//   - TimeoutInterceptor    — per-call deadline (cfg.Timeout)
//   - RetryInterceptor      — on connect.CodeUnavailable / connect.CodeDeadlineExceeded
//   - CircuitBreakerInterceptor — opens after cfg.CircuitThreshold consecutive failures
//   - ErrorNormInterceptor  — maps gRPC status codes → miroxy unified error types (errors.go)
//
// Usage pattern (per-domain client construction):
//
//   interceptors := rpc.NewConnectInterceptors(cfg)
//   client := credpoolv1connect.NewCredpoolServiceClient(
//       rpc.NewHTTPClient(cfg),
//       address,
//       connect.WithInterceptors(interceptors...),
//   )
//
// Each domain (credpoold, router sidecar, semantic sidecar) creates its own
// generated client with the shared interceptor chain injected. The proto types
// and RPC methods stay domain-specific; only the transport infrastructure is shared.
