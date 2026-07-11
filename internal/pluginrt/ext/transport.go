// Package sidecar provides shared connection machinery for sidecar plugins.
//
// Domain-specific sidecar adapters (e.g. internal/cred/credpool.go) use this
// package for dialing, health-check, and circuit-breaking. They must NOT
// re-implement this machinery themselves — that duplication is the exact failure
// mode this package exists to prevent.
//
// Domains using connectrpc.com/connect bypass this package entirely: the
// connect-generated client already handles dial/health/retry. Use this package
// only for sidecar protocols that are not covered by a connect-go client.
package ext

import "time"

// TransportConfig holds connection parameters for a sidecar process.
type TransportConfig struct {
	// Address is a Unix socket path ("unix:///var/run/credpool.sock") or
	// a host:port string for TCP transports.
	Address string
	// TLSEnabled enables mTLS for TCP transports. Ignored for Unix sockets.
	TLSEnabled bool
	// Timeout is the per-call deadline imposed by the transport layer.
	// Zero means no transport-level deadline (rely on ctx).
	Timeout time.Duration
}

// Transport is a protocol-agnostic handle to a connected sidecar process.
// It carries only lifecycle management; the actual RPC surface is defined per
// domain in that domain's own proto file (e.g. internal/cred/credpool.proto).
//
// Concrete implementations are added only when a real, named consumer needs
// a specific protocol:
//
//	TODO(pluginrt): add grpc_transport.go when the first non-connect-go gRPC sidecar exists.
//	TODO(pluginrt): add http_transport.go when the first HTTP-only sidecar exists.
type Transport interface {
	// Close shuts down the transport and releases the underlying connection.
	Close() error
}
