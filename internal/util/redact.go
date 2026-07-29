package util

import (
	"regexp"
	"strconv"
	"strings"
)

// secretJSONKeys are JSON field names whose values must never reach a log.
var secretJSONKeys = []string{
	"accessToken", "refreshToken", "clientSecret", "clientId",
	"access_token", "refresh_token", "client_secret", "client_id",
	"idToken", "id_token", "secret", "password", "apiKey", "api_key",
}

// bearerPattern matches an Authorization bearer value.
var bearerPattern = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9\-._~+/=]{8,}`)

// jsonSecretPattern matches "key":"value" for the secret keys above.
var jsonSecretPattern = regexp.MustCompile(`(?i)"(` + strings.Join(secretJSONKeys, "|") + `)"\s*:\s*"[^"]*"`)

// Redact removes credential material from a string so it is safe to log.
//
// It rewrites JSON fields whose names look like secrets and any bearer token,
// replacing the value with a length-annotated placeholder. The length is kept
// because it is useful when diagnosing truncated tokens and reveals nothing.
func Redact(s string) string {
	s = jsonSecretPattern.ReplaceAllStringFunc(s, func(m string) string {
		colon := strings.Index(m, ":")
		if colon < 0 {
			return m
		}
		key := m[:colon]
		value := strings.TrimSpace(m[colon+1:])
		value = strings.Trim(value, `"`)
		return key + `:"<redacted len=` + strconv.Itoa(len(value)) + `>"`
	})
	s = bearerPattern.ReplaceAllString(s, "${1}<redacted>")
	return s
}

// RedactSecret returns a placeholder describing a secret without revealing it.
func RedactSecret(s string) string {
	if s == "" {
		return "<empty>"
	}
	return "<redacted len=" + strconv.Itoa(len(s)) + ">"
}

// Fingerprint8 returns the first 8 characters of a non-secret identifier, for
// correlating log lines. It refuses to shorten anything that could be a token
// by only ever exposing a prefix of a value the caller declared non-secret.
func Fingerprint8(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8] + "..."
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
