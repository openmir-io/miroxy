package cred

import "context"

// CredBroker is the interface miroxy uses to acquire and release credentials from
// an external credential management service.
//
// The interface is transport- and language-agnostic. Any service that exposes the
// three operations below — whether written in Go, Python, Node.js, or any other
// language — satisfies this contract. The canonical wire definition is in
// proto/credbroker/v1/cred_broker.proto; ConnectBrokerClient (internal/cred) is
// the production HTTP/ConnectRPC implementation.
type CredBroker interface {
	// Acquire leases one credential from the named pool.
	// callerID labels the lease for auditing; pass a stable identifier ("miroxy").
	// Returns the live credential and a lease ID to pass to Release.
	Acquire(ctx context.Context, poolID, callerID string) (leaseID string, c Credential, err error)

	// Release returns a leased credential with outcome flags.
	// Missing or already-expired leases are silently accepted.
	Release(ctx context.Context, leaseID string, rateLimited, serverOverload, callError bool) error

	// HealthyCount returns the number of healthy (non-rate-limited, non-circuit-broken)
	// entries in the named pool. Used by BrokerPoller to gate Acquire calls.
	HealthyCount(ctx context.Context, poolID string) (int, error)
}
