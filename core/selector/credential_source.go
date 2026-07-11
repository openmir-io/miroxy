package selector

import (
	"context"

	"miroxy/core/cred"
)

// CredentialSource returns a live Credential for each request attempt.
// Implementations handle acquisition, caching, and refresh internally —
// the CredPool calls Credential() once per Select() and never inspects
// the concrete type.
type CredentialSource interface {
	Credential(ctx context.Context) (cred.Credential, error)
}

// StaticSource is a CredentialSource that returns the same Credential on
// every call. Used for static API keys and pre-built bearer tokens.
type StaticSource struct{ c cred.Credential }

// NewStaticSource wraps a pre-built Credential in a static source.
// The caller constructs the right Credential type (HeaderCredential,
// QueryCredential, etc.) based on the provider's auth style.
func NewStaticSource(c cred.Credential) *StaticSource { return &StaticSource{c: c} }

func (s *StaticSource) Credential(_ context.Context) (cred.Credential, error) {
	return s.c, nil
}
