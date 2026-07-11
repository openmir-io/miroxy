package idgen

import "math/rand"

// NewMsgID returns a random Anthropic-shaped message ID: "msg_" + 24 alphanumeric chars.
func NewMsgID() string { return newID("msg_") }

// NewToolID returns a random Anthropic-shaped tool use ID: "toolu_" + 24 alphanumeric chars.
func NewToolID() string { return newID("toolu_") }

func newID(prefix string) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 24)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return prefix + string(b)
}
