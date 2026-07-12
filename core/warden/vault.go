package warden

// TokenVault implements reversible tokenization: a detected value is
// replaced with a stable placeholder token so it never reaches the upstream
// provider, and the same token maps back to the original value when the
// provider's response echoes it. Implementations are scoped to a single
// request/response cycle — miroxy keeps no cross-request state in v1.
type TokenVault interface {
	// Tokenize returns the placeholder for value, minting a new one on first
	// sight and reusing it for repeated occurrences of the same value within
	// this vault's scope.
	Tokenize(category Category, typ, value string) string

	// Resolve rewrites every known token found in text back to its original
	// value. Text with no tokens is returned unchanged.
	Resolve(text string) string
}
