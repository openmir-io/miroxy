package warden

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	corewarden "miroxy/core/warden"
)

// vaultTokenPattern matches any token minted by BuiltinVault.Tokenize, used
// by Resolve (and by stream.go's hold-back buffer) to find candidates to
// restore. The delimiters (U+27E6/U+27E7, "mathematical white square
// bracket") are chosen — distinct from pii-redactor's guillemets — for the
// same reason: a pair of characters that essentially never occurs in real
// request/response text, so a false match is vanishingly unlikely.
var vaultTokenPattern = regexp.MustCompile(`⟦[A-Z_]+:\d{3}⟧`)

// MaxTokenBytes bounds how many bytes a single token can occupy — the
// longest type label in this package plus delimiters and the numeric
// suffix, with margin. stream.go's hold-back buffer uses this as its
// give-up threshold: a "⟦" with no matching "⟧" within this many bytes is
// treated as ordinary text, not a stalled token, so a stray delimiter
// character in real content can never stall a stream indefinitely.
const MaxTokenBytes = 64

// BuiltinVault implements reversible tokenization: the same (category, typ,
// value) triple always maps to the same token within this vault's lifetime.
// A BuiltinVault is created per request (see WardenPlugin) and discarded
// with it — no cross-request persistence, consistent with miroxy's
// "no DB in v1" constraint. Safe for concurrent use: Resolve may run
// concurrently with request-time Tokenize calls when a streaming response
// is being relayed in its own goroutine while later chunks are still being
// produced.
type BuiltinVault struct {
	mu           sync.Mutex
	valueToToken map[string]string
	tokenToValue map[string]string
	counters     map[string]int
}

func NewBuiltinVault() *BuiltinVault {
	return &BuiltinVault{
		valueToToken: make(map[string]string),
		tokenToValue: make(map[string]string),
		counters:     make(map[string]int),
	}
}

var _ corewarden.TokenVault = (*BuiltinVault)(nil)

func (v *BuiltinVault) Tokenize(category corewarden.Category, typ, value string) string {
	v.mu.Lock()
	defer v.mu.Unlock()

	valueKey := category.String() + "|" + typ + "|" + value
	if tok, ok := v.valueToToken[valueKey]; ok {
		return tok
	}

	counterKey := category.String() + "|" + typ
	v.counters[counterKey]++
	tok := fmt.Sprintf("⟦%s:%03d⟧", strings.ToUpper(typ), v.counters[counterKey])

	v.valueToToken[valueKey] = tok
	v.tokenToValue[tok] = value
	return tok
}

func (v *BuiltinVault) Resolve(text string) string {
	v.mu.Lock()
	defer v.mu.Unlock()

	if len(v.tokenToValue) == 0 || !strings.Contains(text, "⟦") {
		return text
	}
	return vaultTokenPattern.ReplaceAllStringFunc(text, func(tok string) string {
		if val, ok := v.tokenToValue[tok]; ok {
			return val
		}
		return tok
	})
}
