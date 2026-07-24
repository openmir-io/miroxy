package upstream

import (
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"io"
	"strings"
	"testing"
)

// encodeEventStreamFrame builds a valid AWS EventStream binary frame for a
// single ":message-type" string header plus payload — enough to exercise
// eventStreamDecoder without needing the other header types Bedrock never
// actually sends for this purpose.
func encodeEventStreamFrame(t *testing.T, messageType string, payload []byte) []byte {
	t.Helper()

	var headers []byte
	name := ":message-type"
	headers = append(headers, byte(len(name)))
	headers = append(headers, name...)
	headers = append(headers, 7) // string type tag
	var valLen [2]byte
	binary.BigEndian.PutUint16(valLen[:], uint16(len(messageType)))
	headers = append(headers, valLen[:]...)
	headers = append(headers, messageType...)

	totalLen := 12 + len(headers) + len(payload) + 4
	prelude := make([]byte, 12)
	binary.BigEndian.PutUint32(prelude[0:4], uint32(totalLen))
	binary.BigEndian.PutUint32(prelude[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[0:8]))

	msg := append(append([]byte{}, prelude...), headers...)
	msg = append(msg, payload...)
	crcCalc := crc32.NewIEEE()
	crcCalc.Write(msg)
	var messageCRC [4]byte
	binary.BigEndian.PutUint32(messageCRC[:], crcCalc.Sum32())
	return append(msg, messageCRC[:]...)
}

func TestEventStreamDecoder_DecodesValidFrame(t *testing.T) {
	payload := []byte(`{"bytes":"aGVsbG8="}`) // base64("hello")
	encoded := encodeEventStreamFrame(t, "event", payload)

	dec := newEventStreamDecoder(strings.NewReader(string(encoded)))
	frame, err := dec.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if frame.MessageType != "event" {
		t.Errorf("MessageType = %q, want %q", frame.MessageType, "event")
	}
	if string(frame.Payload) != string(payload) {
		t.Errorf("Payload = %q, want %q", frame.Payload, payload)
	}

	if _, err := dec.Next(); err != io.EOF {
		t.Errorf("second Next() err = %v, want io.EOF", err)
	}
}

func TestEventStreamDecoder_RejectsCorruptedCRC(t *testing.T) {
	encoded := encodeEventStreamFrame(t, "event", []byte(`{}`))
	encoded[len(encoded)-1] ^= 0xFF // flip a bit in the trailing message CRC

	dec := newEventStreamDecoder(strings.NewReader(string(encoded)))
	if _, err := dec.Next(); err == nil {
		t.Fatal("expected a CRC mismatch error, got nil")
	}
}

func TestEventStreamSSEReader_TranslatesEventFrame(t *testing.T) {
	inner := []byte(`{"type":"content_block_delta"}`)
	payload := []byte(`{"bytes":"` + base64.StdEncoding.EncodeToString(inner) + `"}`)
	encoded := encodeEventStreamFrame(t, "event", payload)

	r := newEventStreamSSEReader(io.NopCloser(strings.NewReader(string(encoded))))
	defer r.Close()

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := "data: " + string(inner) + "\n\n"
	if string(out) != want {
		t.Errorf("SSE output = %q, want %q", out, want)
	}
}

func TestEventStreamSSEReader_TranslatesExceptionFrame(t *testing.T) {
	payload := []byte(`{"message":"model timed out"}`)
	encoded := encodeEventStreamFrame(t, "exception", payload)

	r := newEventStreamSSEReader(io.NopCloser(strings.NewReader(string(encoded))))
	defer r.Close()

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := "event: error\ndata: " + string(payload) + "\n\n"
	if string(out) != want {
		t.Errorf("SSE output = %q, want %q", out, want)
	}
}
