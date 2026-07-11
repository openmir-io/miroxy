package ext

import (
	"errors"
	"fmt"
	"sync"
)

// ErrUnknownDomain is returned by NewClient when no Transport factory has been
// registered for the requested domain.
var ErrUnknownDomain = errors.New("sidecar: unknown domain")

// factory dials a sidecar and returns a live Transport.
type factory func(cfg TransportConfig) (Transport, error)

var (
	mu        sync.RWMutex
	factories = map[string]factory{}
)

// Register associates a domain name with a Transport factory function.
// Concrete transport implementations call Register from their package init().
// Domain names are short lower-case strings, e.g. "cred", "router", "security".
func Register(domain string, f factory) {
	mu.Lock()
	defer mu.Unlock()
	factories[domain] = f
}

// NewClient dials the sidecar for domain using cfg and returns a ready Transport.
// Returns ErrUnknownDomain if no factory has been registered for domain.
//
// Domains that use connectrpc.com/connect may call their generated client
// directly without going through NewClient — the connect client already handles
// dial, health-check, and retry. NewClient is for non-connect transports only.
func NewClient(domain string, cfg TransportConfig) (Transport, error) {
	mu.RLock()
	f, ok := factories[domain]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownDomain, domain)
	}
	return f(cfg)
}
