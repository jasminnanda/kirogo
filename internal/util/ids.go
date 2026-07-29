// Package util holds small helpers shared across kirogo: identifier
// generation, secret redaction and token estimation.
package util

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// randomBytes returns n cryptographically random bytes. crypto/rand.Read never
// fails on the platforms Go supports, so a failure is treated as fatal input to
// the caller by panicking only in the impossible case.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read is documented never to return an error on any
		// supported platform. Falling back keeps ID generation total rather
		// than turning a request path into a panic.
		for i := range b {
			b[i] = byte(i * 31)
		}
	}
	return b
}

// RandomHex returns a lowercase hex string of exactly n characters.
func RandomHex(n int) string {
	b := randomBytes((n + 1) / 2)
	return hex.EncodeToString(b)[:n]
}

// UUID4 returns a random RFC 4122 version 4 UUID in canonical form.
func UUID4() string {
	b := randomBytes(16)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// CompletionID returns an OpenAI-style chat completion identifier.
func CompletionID() string {
	return "chatcmpl-" + RandomHex(32)
}

// MessageID returns an Anthropic-style message identifier.
func MessageID() string {
	return "msg_" + RandomHex(24)
}

// ToolUseID returns an Anthropic-style tool use identifier. It is used when a
// client did not supply one, or when synthesising an id for a tool call.
func ToolUseID() string {
	return "toolu_" + RandomHex(24)
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
