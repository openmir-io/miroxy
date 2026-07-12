package warden

import (
	"io"
	"log/slog"
	"strings"

	corewarden "miroxy/core/warden"
	"miroxy/internal/types"
)

const (
	tokenOpen  = "⟦"
	tokenClose = "⟧"
)

// StreamResolver restores vault tokens that arrive split across two or more
// chunk boundaries. A token like "⟦EMAIL:001⟧" fed as "⟦EM" then "AIL:001⟧"
// must still resolve to the original value — this holds back an incomplete
// token instead of emitting it half-formed. Not safe for concurrent use;
// each stream gets its own StreamResolver.
type StreamResolver struct {
	vault corewarden.TokenVault
	buf   strings.Builder
}

func NewStreamResolver(vault corewarden.TokenVault) *StreamResolver {
	return &StreamResolver{vault: vault}
}

// Feed appends chunk to the internal buffer and returns whatever is now
// safe to emit downstream.
func (r *StreamResolver) Feed(chunk string) string {
	r.buf.WriteString(chunk)
	return r.drain()
}

// Flush returns and resolves whatever remains buffered. Call once at
// stream end — it handles a token that completed exactly at EOF with no
// further chunk to trigger drain again.
func (r *StreamResolver) Flush() string {
	out := r.vault.Resolve(r.buf.String())
	r.buf.Reset()
	return out
}

// drain repeatedly peels a safe-to-emit prefix off the buffer:
//  1. no "⟦" anywhere left -> the whole buffer is safe, resolve and emit it.
//  2. a "⟦" exists later in the buffer -> emit everything before it, keep
//     the rest for the next round.
//  3. buffer starts with "⟦" and a complete token follows -> resolve and
//     emit that one token, keep the remainder.
//  4. buffer starts with "⟦" but has a "⟧" that doesn't form a valid token
//     (e.g. stray delimiter in ordinary text) -> treat the span up to and
//     including that "⟧" as plain text and emit it.
//  5. buffer starts with "⟦", no close yet, and has grown past
//     MaxTokenBytes without completing -> give up waiting: emit just the
//     opening delimiter as literal text so a stray "⟦" in real content can
//     never stall the stream indefinitely.
//  6. otherwise the buffer is a short, still-open candidate -> wait for the
//     next Feed call.
func (r *StreamResolver) drain() string {
	var out strings.Builder
	for {
		s := r.buf.String()
		openIdx := strings.Index(s, tokenOpen)

		if openIdx < 0 {
			// A byte-level reader (ResolvingReader) can split tokenOpen's
			// own multi-byte UTF-8 encoding across two Feed calls — hold
			// back a trailing partial match of its lead bytes so the next
			// Feed can complete it, instead of flushing the broken prefix
			// as literal (which would both corrupt the delimiter and never
			// resolve the token that follows it).
			if n := partialDelimiterSuffixLen(s); n > 0 {
				safe := s[:len(s)-n]
				out.WriteString(r.vault.Resolve(safe))
				r.resetTo(s[len(safe):])
				return out.String()
			}
			out.WriteString(r.vault.Resolve(s))
			r.buf.Reset()
			return out.String()
		}
		if openIdx > 0 {
			out.WriteString(r.vault.Resolve(s[:openIdx]))
			r.resetTo(s[openIdx:])
			continue
		}

		if loc := vaultTokenPattern.FindStringIndex(s); loc != nil && loc[0] == 0 {
			out.WriteString(r.vault.Resolve(s[loc[0]:loc[1]]))
			r.resetTo(s[loc[1]:])
			continue
		}

		if closeIdx := strings.Index(s, tokenClose); closeIdx >= 0 {
			end := closeIdx + len(tokenClose)
			out.WriteString(r.vault.Resolve(s[:end]))
			r.resetTo(s[end:])
			continue
		}

		if len(s) > MaxTokenBytes {
			out.WriteString(tokenOpen)
			r.resetTo(s[len(tokenOpen):])
			continue
		}

		return out.String() // short and still open -- wait for more input
	}
}

// partialDelimiterSuffixLen returns how many trailing bytes of s could be
// the start of an as-yet-incomplete tokenOpen/tokenClose sequence — both
// delimiters share the same leading bytes, differing only in the last byte,
// so checking tokenOpen's prefixes covers either. Returns 0 when s's tail
// can't be the start of a delimiter.
func partialDelimiterSuffixLen(s string) int {
	for n := 1; n < len(tokenOpen) && n <= len(s); n++ {
		if strings.HasSuffix(s, tokenOpen[:n]) {
			return n
		}
	}
	return 0
}

func (r *StreamResolver) resetTo(s string) {
	r.buf.Reset()
	r.buf.WriteString(s)
}

// ResolvingReader wraps an upstream response body for raw-passthrough
// streaming, restoring vault tokens as bytes flow through — the same
// hold-back algorithm as StreamResolver, operating directly on the byte
// stream rather than on parsed SSEEvent text, so it works regardless of
// which wire protocol the passthrough body carries.
type ResolvingReader struct {
	src      io.ReadCloser
	resolver *StreamResolver
	pending  []byte
	readBuf  []byte
	eof      bool
}

func NewResolvingReader(src io.ReadCloser, vault corewarden.TokenVault) *ResolvingReader {
	return &ResolvingReader{
		src:      src,
		resolver: NewStreamResolver(vault),
		readBuf:  make([]byte, 32*1024),
	}
}

func (r *ResolvingReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 && !r.eof {
		n, err := r.src.Read(r.readBuf)
		if n > 0 {
			r.pending = append(r.pending, r.resolver.Feed(string(r.readBuf[:n]))...)
		}
		if err != nil {
			r.eof = true
			r.pending = append(r.pending, r.resolver.Flush()...)
			if err != io.EOF && len(r.pending) == 0 {
				return 0, err
			}
			break
		}
	}
	if len(r.pending) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *ResolvingReader) Close() error { return r.src.Close() }

// ResolveEvents relays src to a new channel, restoring vault tokens in
// each content_block_delta's text before forwarding. The returned channel
// closes once src closes; the caller's release callback (registered via
// LLMContext.SetStream) is unaffected — this only wraps the data path.
func ResolveEvents(src <-chan types.SSEEvent, vault corewarden.TokenVault) <-chan types.SSEEvent {
	out := make(chan types.SSEEvent)
	go func() {
		defer close(out)
		resolver := NewStreamResolver(vault)
		for ev := range src {
			// internal/irc constructs ContentBlockDeltaData by value (not a
			// pointer) when building these events — match that here, and
			// write the mutated copy back into ev.Data.
			if delta, ok := ev.Data.(types.ContentBlockDeltaData); ok && delta.Delta.Text != "" {
				delta.Delta.Text = resolver.Feed(delta.Delta.Text)
				ev.Data = delta
			}
			out <- ev
		}
		// drain() only ever leaves the buffer non-empty when it holds a
		// short, still-open "⟦..." candidate that never reached a closing
		// "⟧" before the source channel closed (see StreamResolver.drain).
		// Splicing that fragment in now would mean emitting a
		// content_block_delta after this stream's own content_block_stop/
		// message_stop, violating Anthropic's SSE ordering — since the
		// fragment necessarily starts with a delimiter that never appears
		// in real model output, drop it rather than risk a malformed
		// client-visible event.
		if tail := resolver.Flush(); tail != "" {
			slog.Debug("warden: dropped incomplete token fragment at stream end", "fragment", tail)
		}
	}()
	return out
}
