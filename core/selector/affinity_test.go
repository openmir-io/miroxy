package selector

import (
	"testing"
	"time"

	"miroxy/core/ir"
)

func msgReq(userTexts ...string) *ir.IRRequest {
	msgs := make([]ir.IRMessage, len(userTexts))
	for i, t := range userTexts {
		msgs[i] = ir.IRMessage{Role: "user", Parts: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: t}}}}
	}
	return &ir.IRRequest{Messages: msgs}
}

func TestSessionKeyFromRequest_EmptyForNilOrNoMessages(t *testing.T) {
	if k := SessionKeyFromRequest(nil, "m"); k != "" {
		t.Errorf("nil req: expected empty key, got %q", k)
	}
	if k := SessionKeyFromRequest(&ir.IRRequest{}, "m"); k != "" {
		t.Errorf("no messages: expected empty key, got %q", k)
	}
}

func TestSessionKeyFromRequest_StableAsConversationGrows(t *testing.T) {
	turn1 := msgReq("hello there")
	turn2 := msgReq("hello there", "some reply", "a follow-up question")

	k1 := SessionKeyFromRequest(turn1, "gemini-2.5-flash")
	k2 := SessionKeyFromRequest(turn2, "gemini-2.5-flash")
	if k1 == "" || k2 == "" {
		t.Fatal("expected non-empty keys")
	}
	if k1 != k2 {
		t.Errorf("fingerprint should stay stable as later turns are appended: %q != %q", k1, k2)
	}
}

func TestSessionKeyFromRequest_DiffersAcrossConversations(t *testing.T) {
	a := SessionKeyFromRequest(msgReq("hello there"), "gemini-2.5-flash")
	b := SessionKeyFromRequest(msgReq("totally different opener"), "gemini-2.5-flash")
	if a == b {
		t.Error("different conversations should not collide")
	}
}

func TestSessionKeyFromRequest_DiffersAcrossModels(t *testing.T) {
	a := SessionKeyFromRequest(msgReq("hello there"), "gemini-2.5-flash")
	b := SessionKeyFromRequest(msgReq("hello there"), "gemini-2.5-pro")
	if a == b {
		t.Error("same conversation prefix under a different model should not collide (shared credpool disambiguation)")
	}
}

func TestSessionKeyFromRequest_UserIDTakesPrecedence(t *testing.T) {
	req := msgReq("hello there")
	req.UserID = "user-123"
	other := msgReq("a completely different opener")
	other.UserID = "user-123"

	if SessionKeyFromRequest(req, "gemini-2.5-flash") != SessionKeyFromRequest(other, "gemini-2.5-flash") {
		t.Error("explicit user_id should override content fingerprinting")
	}
}

func TestAffinityMap_GetSetRoundTrip(t *testing.T) {
	a := newAffinityMap(time.Minute)
	if _, ok := a.Get("k"); ok {
		t.Fatal("expected miss on empty map")
	}
	a.Set("k", "cred-1")
	id, ok := a.Get("k")
	if !ok || id != "cred-1" {
		t.Fatalf("got (%q, %v), want (cred-1, true)", id, ok)
	}
}

func TestAffinityMap_ExpiresAfterTTL(t *testing.T) {
	a := newAffinityMap(10 * time.Millisecond)
	a.Set("k", "cred-1")
	time.Sleep(20 * time.Millisecond)
	if _, ok := a.Get("k"); ok {
		t.Fatal("expected binding to have expired")
	}
}
