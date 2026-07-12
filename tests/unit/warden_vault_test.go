package unit_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	corewarden "miroxy/core/warden"
	intwarden "miroxy/internal/warden"
)

func TestBuiltinVault_TokenizeIsIdempotentPerValue(t *testing.T) {
	v := intwarden.NewBuiltinVault()
	tok1 := v.Tokenize(corewarden.CategoryPII, "email", "jane@example.com")
	tok2 := v.Tokenize(corewarden.CategoryPII, "email", "jane@example.com")
	if tok1 != tok2 {
		t.Errorf("Tokenize should return the same token for the same value: %q != %q", tok1, tok2)
	}

	tok3 := v.Tokenize(corewarden.CategoryPII, "email", "other@example.com")
	if tok3 == tok1 {
		t.Errorf("Tokenize should return a distinct token for a distinct value")
	}
}

func TestBuiltinVault_ResolveRoundTrip(t *testing.T) {
	v := intwarden.NewBuiltinVault()
	tok := v.Tokenize(corewarden.CategoryPII, "email", "jane@example.com")

	text := "please email " + tok + " about the invoice"
	restored := v.Resolve(text)
	want := "please email jane@example.com about the invoice"
	if restored != want {
		t.Errorf("Resolve = %q, want %q", restored, want)
	}
}

func TestBuiltinVault_ResolveLeavesUnknownTokenAlone(t *testing.T) {
	v := intwarden.NewBuiltinVault()
	v.Tokenize(corewarden.CategoryPII, "email", "jane@example.com")

	text := "this mentions ⟦EMAIL:999⟧ which was never minted"
	restored := v.Resolve(text)
	if restored != text {
		t.Errorf("Resolve should leave an unrecognized token untouched, got %q", restored)
	}
}

func TestStreamResolver_TokenSplitAcrossChunks(t *testing.T) {
	v := intwarden.NewBuiltinVault()
	tok := v.Tokenize(corewarden.CategoryPII, "email", "jane@example.com")
	splitAt := len(tok) / 2

	r := intwarden.NewStreamResolver(v)
	var out strings.Builder
	out.WriteString(r.Feed("hello " + tok[:splitAt]))
	out.WriteString(r.Feed(tok[splitAt:] + " goodbye"))
	out.WriteString(r.Flush())

	want := "hello jane@example.com goodbye"
	if out.String() != want {
		t.Errorf("resolved stream = %q, want %q", out.String(), want)
	}
}

func TestStreamResolver_TokenSplitByteByByte(t *testing.T) {
	v := intwarden.NewBuiltinVault()
	tok := v.Tokenize(corewarden.CategoryPII, "email", "jane@example.com")

	r := intwarden.NewStreamResolver(v)
	var out strings.Builder
	full := "prefix " + tok + " suffix"
	for i := 0; i < len(full); i++ {
		out.WriteString(r.Feed(full[i : i+1]))
	}
	out.WriteString(r.Flush())

	want := "prefix jane@example.com suffix"
	if out.String() != want {
		t.Errorf("resolved stream = %q, want %q", out.String(), want)
	}
}

func TestStreamResolver_StrayDelimiterNeverStallsStream(t *testing.T) {
	v := intwarden.NewBuiltinVault()
	r := intwarden.NewStreamResolver(v)

	// A "⟦" that never closes, followed by enough filler to exceed
	// MaxTokenBytes, must eventually be flushed as literal text rather than
	// held back forever.
	var out strings.Builder
	out.WriteString(r.Feed("odd delimiter ⟦" + strings.Repeat("x", intwarden.MaxTokenBytes+10)))
	out.WriteString(r.Flush())

	if !strings.Contains(out.String(), "⟦") {
		t.Errorf("expected the stray delimiter to be emitted as literal text eventually, got %q", out.String())
	}
	if !strings.HasPrefix(out.String(), "odd delimiter ⟦") {
		t.Errorf("expected literal prefix to survive unchanged, got %q", out.String())
	}
}

func TestStreamResolver_MalformedTokenShapeTreatedAsLiteral(t *testing.T) {
	v := intwarden.NewBuiltinVault()
	r := intwarden.NewStreamResolver(v)

	// "⟦not a real token⟧" has a close delimiter but doesn't match the
	// TYPE:NNN shape -- it must pass through unchanged, not get stuck.
	text := "note: ⟦not a real token⟧ end"
	out := r.Feed(text) + r.Flush()
	if out != text {
		t.Errorf("malformed-but-closed delimiter span should pass through unchanged: got %q, want %q", out, text)
	}
}

func TestResolvingReader_ResolvesAcrossReadBoundaries(t *testing.T) {
	v := intwarden.NewBuiltinVault()
	tok := v.Tokenize(corewarden.CategoryPII, "email", "jane@example.com")

	full := "prefix " + tok + " suffix"
	splitAt := len(full) / 2
	src := io.NopCloser(io.MultiReader(
		bytes.NewReader([]byte(full[:splitAt])),
		bytes.NewReader([]byte(full[splitAt:])),
	))

	reader := intwarden.NewResolvingReader(src, v)
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	want := "prefix jane@example.com suffix"
	if string(got) != want {
		t.Errorf("resolved bytes = %q, want %q", string(got), want)
	}
}

func TestResolvingReader_SmallReadBuffer(t *testing.T) {
	v := intwarden.NewBuiltinVault()
	tok := v.Tokenize(corewarden.CategoryPII, "email", "jane@example.com")
	full := "prefix " + tok + " suffix"

	reader := intwarden.NewResolvingReader(io.NopCloser(strings.NewReader(full)), v)
	var out bytes.Buffer
	buf := make([]byte, 3) // deliberately smaller than the token itself
	for {
		n, err := reader.Read(buf)
		out.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}

	want := "prefix jane@example.com suffix"
	if out.String() != want {
		t.Errorf("resolved bytes = %q, want %q", out.String(), want)
	}
}
