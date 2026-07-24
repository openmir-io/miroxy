package types

import (
	"encoding/json"
	"testing"
)

func TestNormalizeSystem_PlainStringSystemMessage(t *testing.T) {
	r := &MessageRequest{Messages: []Message{
		{Role: "system", Content: []byte(`"be concise"`)},
		{Role: "user", Content: []byte(`"hi"`)},
	}}
	r.NormalizeSystem()

	if len(r.Messages) != 1 || r.Messages[0].Role != "user" {
		t.Fatalf("expected system message stripped, got %+v", r.Messages)
	}
	var sys string
	if err := decodeSystem(r.System, &sys); err != nil || sys != "be concise" {
		t.Fatalf("expected System %q, got %q (err=%v)", "be concise", sys, err)
	}
}

func TestNormalizeSystem_BlockArraySystemMessage(t *testing.T) {
	r := &MessageRequest{Messages: []Message{
		{Role: "user", Content: []byte(`"hi"`)},
		{Role: "system", Content: []byte(`[{"type":"text","text":"be concise"}]`)},
	}}
	r.NormalizeSystem()

	if err := r.Validate(); err != nil {
		t.Fatalf("Validate() after normalize: %v", err)
	}
	for _, m := range r.Messages {
		if m.Role == "system" {
			t.Fatalf("system-role message survived normalization: %+v", r.Messages)
		}
	}
	var sys string
	if err := decodeSystem(r.System, &sys); err != nil || sys != "be concise" {
		t.Fatalf("expected System %q, got %q (err=%v)", "be concise", sys, err)
	}
}

func TestNormalizeSystem_UnparseableSystemMessageStillStripped(t *testing.T) {
	r := &MessageRequest{Messages: []Message{
		{Role: "user", Content: []byte(`"hi"`)},
		{Role: "system", Content: []byte(`null`)},
		{Role: "assistant", Content: []byte(`"ok"`)},
	}}
	r.NormalizeSystem()

	if err := r.Validate(); err != nil {
		t.Fatalf("Validate() after normalize: %v", err)
	}
	if len(r.Messages) != 2 {
		t.Fatalf("expected system message stripped even with no extractable text, got %+v", r.Messages)
	}
}

func TestNormalizeSystem_NoOpWithoutSystemMessages(t *testing.T) {
	r := &MessageRequest{Messages: []Message{
		{Role: "user", Content: []byte(`"hi"`)},
		{Role: "assistant", Content: []byte(`"ok"`)},
	}}
	r.NormalizeSystem()

	if len(r.Messages) != 2 {
		t.Fatalf("expected no change, got %+v", r.Messages)
	}
	if len(r.System) != 0 {
		t.Fatalf("expected System untouched, got %q", r.System)
	}
}

func decodeSystem(raw []byte, out *string) error {
	if len(raw) == 0 {
		*out = ""
		return nil
	}
	return json.Unmarshal(raw, out)
}
