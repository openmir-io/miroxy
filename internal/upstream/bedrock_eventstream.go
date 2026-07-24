package upstream

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
)

// eventStreamFrame is one decoded AWS EventStream frame.
type eventStreamFrame struct {
	MessageType string // ":message-type" header (e.g. "event", "exception")
	Payload     []byte
}

// maxEventStreamFrameSize bounds a single frame, preventing unbounded
// allocation from a malformed or hostile upstream.
const maxEventStreamFrameSize = 10 << 20 // 10 MiB

// eventStreamDecoder reads AWS EventStream binary frames from a reader.
//
// Frame layout: [4B totalLength][4B headersLength][4B preludeCRC32][headers...][payload...][4B messageCRC32]
type eventStreamDecoder struct {
	r io.Reader
}

func newEventStreamDecoder(r io.Reader) *eventStreamDecoder {
	return &eventStreamDecoder{r: r}
}

// Next reads and returns the next frame. Returns io.EOF when the stream ends.
func (d *eventStreamDecoder) Next() (*eventStreamFrame, error) {
	var prelude [12]byte
	if _, err := io.ReadFull(d.r, prelude[:]); err != nil {
		return nil, err
	}

	totalLen := binary.BigEndian.Uint32(prelude[0:4])
	headersLen := binary.BigEndian.Uint32(prelude[4:8])
	preludeCRC := binary.BigEndian.Uint32(prelude[8:12])

	if got := crc32.ChecksumIEEE(prelude[0:8]); got != preludeCRC {
		return nil, fmt.Errorf("eventstream: prelude CRC mismatch: got %08x, want %08x", got, preludeCRC)
	}

	remaining := int(totalLen) - 12 - 4 // minus prelude, minus trailing message CRC
	if remaining < 0 || remaining > maxEventStreamFrameSize {
		return nil, fmt.Errorf("eventstream: invalid total length %d", totalLen)
	}
	if int(headersLen) > remaining {
		return nil, fmt.Errorf("eventstream: headers length %d exceeds frame payload %d", headersLen, remaining)
	}

	buf := make([]byte, remaining+4) // +4 for trailing message CRC
	if _, err := io.ReadFull(d.r, buf); err != nil {
		return nil, fmt.Errorf("eventstream: reading frame body: %w", err)
	}

	messageCRC := binary.BigEndian.Uint32(buf[remaining:])
	crcCalc := crc32.NewIEEE()
	crcCalc.Write(prelude[:])
	crcCalc.Write(buf[:remaining])
	if crcCalc.Sum32() != messageCRC {
		return nil, fmt.Errorf("eventstream: message CRC mismatch")
	}

	headers := buf[:headersLen]
	frame := &eventStreamFrame{Payload: buf[headersLen:remaining]}

	// Headers: [1B nameLen][name...][1B typeTag][value per type]. Only
	// ":message-type" is consulted (event vs exception); every other header
	// is skipped by its declared width.
	for off := 0; off < len(headers); {
		nameLen := int(headers[off])
		off++
		if off+nameLen > len(headers) {
			return nil, fmt.Errorf("eventstream: header name overflow")
		}
		name := string(headers[off : off+nameLen])
		off += nameLen

		if off >= len(headers) {
			return nil, fmt.Errorf("eventstream: missing header type tag")
		}
		typeTag := headers[off]
		off++

		switch typeTag {
		case 7: // string: 2B length prefix + data
			if off+2 > len(headers) {
				return nil, fmt.Errorf("eventstream: string header value length overflow")
			}
			valLen := int(binary.BigEndian.Uint16(headers[off : off+2]))
			off += 2
			if off+valLen > len(headers) {
				return nil, fmt.Errorf("eventstream: string header value overflow")
			}
			if name == ":message-type" {
				frame.MessageType = string(headers[off : off+valLen])
			}
			off += valLen
		case 0, 1: // bool true/false: no value bytes
		case 2: // byte
			off++
		case 3: // short
			off += 2
		case 4: // int
			off += 4
		case 5, 8: // long, timestamp
			off += 8
		case 9: // uuid
			off += 16
		case 6: // bytes: 2B length prefix + data
			if off+2 > len(headers) {
				return nil, fmt.Errorf("eventstream: bytes header length overflow")
			}
			bLen := int(binary.BigEndian.Uint16(headers[off : off+2]))
			off += 2
			if off+bLen > len(headers) {
				return nil, fmt.Errorf("eventstream: bytes header value overflow")
			}
			off += bLen
		default:
			return nil, fmt.Errorf("eventstream: unknown header type tag %d", typeTag)
		}
	}

	return frame, nil
}

// eventStreamSSEReader wraps a Bedrock InvokeModelWithResponseStream body and
// re-emits each frame's payload as a plain SSE "data: ...\n\n" line, so the
// existing parseAnthropicSSE (internal/upstream/passthrough.go) can consume
// a Bedrock stream exactly as it consumes a genuine Anthropic SSE stream.
//
// Each "event" frame's JSON payload is {"bytes": "<base64 of the original
// Anthropic SSE event JSON>"}; "exception"/"error" frames are surfaced as an
// SSE "error" event so parseAnthropicSSE's caller can classify them.
type eventStreamSSEReader struct {
	src     io.ReadCloser
	decoder *eventStreamDecoder
	pending bytes.Buffer
	err     error
}

func newEventStreamSSEReader(src io.ReadCloser) io.ReadCloser {
	return &eventStreamSSEReader{src: src, decoder: newEventStreamDecoder(src)}
}

func (r *eventStreamSSEReader) Read(p []byte) (int, error) {
	for r.pending.Len() == 0 && r.err == nil {
		frame, err := r.decoder.Next()
		if err != nil {
			r.err = err
			break
		}
		switch frame.MessageType {
		case "event":
			var ev struct {
				Bytes []byte `json:"bytes"`
			}
			if json.Unmarshal(frame.Payload, &ev) == nil && len(ev.Bytes) > 0 {
				r.pending.WriteString("data: ")
				r.pending.Write(ev.Bytes)
				r.pending.WriteString("\n\n")
			}
		case "exception", "error":
			r.pending.WriteString("event: error\ndata: ")
			r.pending.Write(frame.Payload)
			r.pending.WriteString("\n\n")
		}
	}
	if r.pending.Len() > 0 {
		return r.pending.Read(p)
	}
	return 0, r.err
}

func (r *eventStreamSSEReader) Close() error {
	return r.src.Close()
}
