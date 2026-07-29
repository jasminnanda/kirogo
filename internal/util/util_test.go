package util

import (
	"regexp"
	"strings"
	"testing"
)

func TestRandomHexLength(t *testing.T) {
	for _, n := range []int{1, 2, 8, 24, 31, 32, 64} {
		got := RandomHex(n)
		if len(got) != n {
			t.Errorf("RandomHex(%d) returned %d characters", n, len(got))
		}
		if !regexp.MustCompile(`^[0-9a-f]*$`).MatchString(got) {
			t.Errorf("RandomHex(%d) = %q, want lowercase hex", n, got)
		}
	}
}

func TestRandomHexIsNotConstant(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		seen[RandomHex(16)] = true
	}
	if len(seen) < 90 {
		t.Errorf("RandomHex produced only %d distinct values out of 100", len(seen))
	}
}

func TestUUID4Format(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for i := 0; i < 50; i++ {
		got := UUID4()
		if !pattern.MatchString(got) {
			t.Fatalf("UUID4() = %q, does not match RFC 4122 v4", got)
		}
	}
}

func TestIDPrefixes(t *testing.T) {
	cases := []struct {
		name   string
		fn     func() string
		prefix string
		total  int
	}{
		{"CompletionID", CompletionID, "chatcmpl-", len("chatcmpl-") + 32},
		{"MessageID", MessageID, "msg_", len("msg_") + 24},
		{"ToolUseID", ToolUseID, "toolu_", len("toolu_") + 24},
	}
	for _, c := range cases {
		got := c.fn()
		if !strings.HasPrefix(got, c.prefix) {
			t.Errorf("%s() = %q, want prefix %q", c.name, got, c.prefix)
		}
		if len(got) != c.total {
			t.Errorf("%s() = %q, length %d want %d", c.name, got, len(got), c.total)
		}
	}
}

func TestRedactJSONSecrets(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		mustNotHav string
		mustHave   string
	}{
		{
			name:       "camelCase access token",
			in:         `{"accessToken":"aya123456789secret","region":"us-east-1"}`,
			mustNotHav: "aya123456789secret",
			mustHave:   `"accessToken":"<redacted len=18>"`,
		},
		{
			name:       "snake_case refresh token",
			in:         `{"refresh_token":"rt-abcdef","x":1}`,
			mustNotHav: "rt-abcdef",
			mustHave:   `"refresh_token":"<redacted len=9>"`,
		},
		{
			name:       "client secret",
			in:         `{"clientSecret":"shhh"}`,
			mustNotHav: "shhh",
			mustHave:   "<redacted",
		},
		{
			name:       "spaces around colon",
			in:         `{"accessToken"  :  "tok"}`,
			mustNotHav: `"tok"`,
			mustHave:   "<redacted",
		},
		{
			name:       "bearer header",
			in:         `Authorization: Bearer aya-super-long-token-value`,
			mustNotHav: "aya-super-long-token-value",
			mustHave:   "Bearer <redacted>",
		},
		{
			name:       "password field",
			in:         `{"password":"hunter2"}`,
			mustNotHav: "hunter2",
			mustHave:   "<redacted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.in)
			if strings.Contains(got, tc.mustNotHav) {
				t.Errorf("Redact leaked %q: %s", tc.mustNotHav, got)
			}
			if !strings.Contains(got, tc.mustHave) {
				t.Errorf("Redact(%q) = %q, want it to contain %q", tc.in, got, tc.mustHave)
			}
		})
	}
}

func TestRedactPreservesNonSecrets(t *testing.T) {
	in := `{"region":"eu-central-1","profileArn":"arn:aws:codewhisperer:us-east-1:1:profile/X"}`
	if got := Redact(in); got != in {
		t.Errorf("Redact changed a body with no secrets:\n got %s\nwant %s", got, in)
	}
}

func TestRedactSecret(t *testing.T) {
	if got := RedactSecret(""); got != "<empty>" {
		t.Errorf("RedactSecret(\"\") = %q, want <empty>", got)
	}
	got := RedactSecret("abcdef")
	if strings.Contains(got, "abcdef") {
		t.Errorf("RedactSecret leaked the value: %q", got)
	}
	if got != "<redacted len=6>" {
		t.Errorf("RedactSecret = %q, want <redacted len=6>", got)
	}
}

func TestFingerprint8(t *testing.T) {
	cases := map[string]string{
		"":                  "",
		"short":             "short",
		"exactly8":          "exactly8",
		"longer-than-eight": "longer-t...",
	}
	for in, want := range cases {
		if got := Fingerprint8(in); got != want {
			t.Errorf("Fingerprint8(%q) = %q, want %q", in, got, want)
		}
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
