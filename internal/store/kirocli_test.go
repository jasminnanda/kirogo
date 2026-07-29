package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadCredentialsFromKiroCLI(t *testing.T) {
	creds, err := LoadCredentials(fixture("kirocli.sqlite3"))
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}

	if creds.AccessToken != "cli-access-token" {
		t.Errorf("AccessToken = %q", creds.AccessToken)
	}
	if creds.RefreshToken != "cli-refresh-token" {
		t.Errorf("RefreshToken = %q", creds.RefreshToken)
	}
	if creds.Region != "eu-central-1" {
		t.Errorf("Region = %q", creds.Region)
	}
	// The token carries its own ARN, so the state table is not consulted.
	if !strings.HasSuffix(creds.ProfileARN, "profile/CLIPROF12345") {
		t.Errorf("ProfileARN = %q, want the ARN from the token", creds.ProfileARN)
	}
	if creds.ClientID != "cli-client-id" || creds.ClientSecret != "cli-client-secret" {
		t.Errorf("client credentials = %q / %q", creds.ClientID, creds.ClientSecret)
	}
	if creds.SourceKey != "kirocli:social:token" {
		t.Errorf("SourceKey = %q, want the social key", creds.SourceKey)
	}

	want := time.Date(2030, 1, 2, 3, 4, 5, 123456789, time.UTC)
	if !creds.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v (nanoseconds preserved)", creds.ExpiresAt, want)
	}
}

func TestLoadCredentialsFallsBackToTheLegacyKey(t *testing.T) {
	creds, err := LoadCredentials(fixture("kirocli-legacy.sqlite3"))
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}

	if creds.SourceKey != "codewhisperer:odic:token" {
		t.Errorf("SourceKey = %q, want the legacy key with its upstream typo", creds.SourceKey)
	}
	if creds.AccessToken != "legacy-access" {
		t.Errorf("AccessToken = %q", creds.AccessToken)
	}
	if creds.ClientID != "legacy-client-id" {
		t.Errorf("ClientID = %q", creds.ClientID)
	}
	// The token has no ARN, so the state table supplies it.
	if !strings.HasSuffix(creds.ProfileARN, "profile/LEGACY123456") {
		t.Errorf("ProfileARN = %q, want the ARN from the state table", creds.ProfileARN)
	}
	// The registration's region fills in when the token has none.
	if creds.Region != "us-west-2" {
		t.Errorf("Region = %q, want the registration's region", creds.Region)
	}
}

func TestLoadCredentialsHonoursKeyPriority(t *testing.T) {
	creds, err := LoadCredentials(fixture("kirocli-multiple.sqlite3"))
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if creds.SourceKey != TokenKeys[0] {
		t.Errorf("SourceKey = %q, want the highest-priority key %q", creds.SourceKey, TokenKeys[0])
	}
	if creds.AccessToken != "first-access" {
		t.Errorf("AccessToken = %q, want the social token to win", creds.AccessToken)
	}
}

func TestLoadCredentialsWithOverflowTokens(t *testing.T) {
	// A token large enough to spill onto overflow pages must round-trip intact.
	creds, err := LoadCredentials(fixture("overflow.sqlite3"))
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if len(creds.AccessToken) != 4000 {
		t.Errorf("AccessToken length = %d, want 4000", len(creds.AccessToken))
	}
	if strings.Trim(creds.AccessToken, "X") != "" {
		t.Error("the access token was corrupted by the overflow chain")
	}
	if len(creds.RefreshToken) != 4000 || strings.Trim(creds.RefreshToken, "Y") != "" {
		t.Error("the refresh token was corrupted by the overflow chain")
	}
}

func TestLoadCredentialsFromADeepTree(t *testing.T) {
	creds, err := LoadCredentials(fixture("manyrows.sqlite3"))
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if creds.AccessToken != "deep-access" {
		t.Errorf("AccessToken = %q, want the token found through the interior pages", creds.AccessToken)
	}
}

func TestLoadCredentialsIgnoresColumnOrder(t *testing.T) {
	creds, err := LoadCredentials(fixture("column-order.sqlite3"))
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if creds.AccessToken != "reordered-access" {
		t.Errorf("AccessToken = %q, want the value column found by name", creds.AccessToken)
	}
}

func TestLoadCredentialsFromALargePageDatabase(t *testing.T) {
	creds, err := LoadCredentials(fixture("page16k.sqlite3"))
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if creds.AccessToken != "big-page-access" {
		t.Errorf("AccessToken = %q", creds.AccessToken)
	}
}

func TestLoadCredentialsNoTokenPresent(t *testing.T) {
	_, err := LoadCredentials(fixture("norows.sqlite3"))
	if err == nil {
		t.Fatal("expected an error when the table holds no token")
	}
	for _, want := range []string{"no kiro-cli token found", "auth_kv", "kiro-cli login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
	// The message should list the token keys it looked for.
	for _, key := range TokenKeys {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error should list the key %q it looked for", key)
		}
	}
}

func TestLoadCredentialsOnAnEmptyDatabase(t *testing.T) {
	_, err := LoadCredentials(fixture("empty.sqlite3"))
	if err == nil {
		t.Fatal("expected an error for a database with no tables")
	}
}

func TestLoadCredentialsRefusesWAL(t *testing.T) {
	_, err := LoadCredentials(fixture("walmode.sqlite3"))
	if !errors.Is(err, ErrWALPresent) {
		t.Errorf("error = %v, want ErrWALPresent", err)
	}
}

func TestLoadCredentialsOnMalformedFiles(t *testing.T) {
	for _, name := range []string{"notsqlite.bin", "zerolength.sqlite3", "truncated.sqlite3", "badpagesize.sqlite3"} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadCredentials(fixture(name)); err == nil {
				t.Errorf("LoadCredentials(%s) should fail", name)
			}
		})
	}
}

func TestLoadCredentialsOnMissingFile(t *testing.T) {
	_, err := LoadCredentials(filepath.Join(t.TempDir(), "absent.sqlite3"))
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestLoadCredentialsNeverWrites(t *testing.T) {
	// Copy a fixture, note its bytes and modification time, then read it.
	dir := t.TempDir()
	path := filepath.Join(dir, "db.sqlite3")
	original, err := os.ReadFile(fixture("kirocli.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := LoadCredentials(path); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Error("the database file was modified: this reader must never write")
	}
	afterStat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !afterStat.ModTime().Equal(before.ModTime()) {
		t.Error("the database modification time changed")
	}

	// No journal or WAL sidecar may have been created either.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory contains %v, want only the database", names)
	}
}

func TestParseTimestamp(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // RFC3339Nano in UTC, or "" for an expected error
	}{
		{"seconds", "2030-01-02T03:04:05Z", "2030-01-02T03:04:05Z"},
		{"milliseconds", "2030-01-02T03:04:05.123Z", "2030-01-02T03:04:05.123Z"},
		{"nanoseconds", "2030-01-02T03:04:05.123456789Z", "2030-01-02T03:04:05.123456789Z"},
		{"twelve fractional digits truncated", "2030-01-02T03:04:05.123456789999Z", "2030-01-02T03:04:05.123456789Z"},
		{"offset zone", "2030-01-02T05:04:05+02:00", "2030-01-02T03:04:05Z"},
		{"negative offset", "2030-01-01T22:04:05-05:00", "2030-01-02T03:04:05Z"},
		{"no zone treated as utc", "2030-01-02T03:04:05", "2030-01-02T03:04:05Z"},
		{"surrounding whitespace", "  2030-01-02T03:04:05Z  ", "2030-01-02T03:04:05Z"},
		{"empty", "", ""},
		{"garbage", "not a date", ""},
		{"date only", "2030-01-02", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTimestamp(tc.in)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("ParseTimestamp(%q) = %v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTimestamp(%q): %v", tc.in, err)
			}
			if formatted := got.Format(time.RFC3339Nano); formatted != tc.want {
				t.Errorf("ParseTimestamp(%q) = %s, want %s", tc.in, formatted, tc.want)
			}
		})
	}
}

func TestTokenKeyOrderMatchesUpstream(t *testing.T) {
	// The order matters, and the "odic" spelling is kiro-cli's own typo. Both are
	// asserted so a well-meaning correction cannot silently break lookups.
	want := []string{
		"kirocli:social:token",
		"kirocli:odic:token",
		"codewhisperer:odic:token",
	}
	if len(TokenKeys) != len(want) {
		t.Fatalf("TokenKeys = %v, want %v", TokenKeys, want)
	}
	for i := range want {
		if TokenKeys[i] != want[i] {
			t.Errorf("TokenKeys[%d] = %q, want %q", i, TokenKeys[i], want[i])
		}
	}

	wantRegistration := []string{
		"kirocli:odic:device-registration",
		"codewhisperer:odic:device-registration",
	}
	for i := range wantRegistration {
		if RegistrationKeys[i] != wantRegistration[i] {
			t.Errorf("RegistrationKeys[%d] = %q, want %q", i, RegistrationKeys[i], wantRegistration[i])
		}
	}

	if ProfileStateKey != "api.codewhisperer.profile" {
		t.Errorf("ProfileStateKey = %q", ProfileStateKey)
	}
}

func TestUnparseableExpiryIsTreatedAsUnknown(t *testing.T) {
	// An expiry that cannot be read must not fail the load: an unknown expiry
	// simply forces a refresh.
	creds, err := LoadCredentials(fixture("bad-expiry.sqlite3"))
	if err != nil {
		t.Fatalf("a bad expiry must not fail the load: %v", err)
	}
	if !creds.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt = %v, want the zero time", creds.ExpiresAt)
	}
	if creds.AccessToken != "a" {
		t.Errorf("AccessToken = %q", creds.AccessToken)
	}
}

func TestMalformedTokenJSONFallsThroughToTheNextKey(t *testing.T) {
	// The social key holds junk, the OIDC key holds a usable token.
	creds, err := LoadCredentials(fixture("bad-json.sqlite3"))
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if creds.AccessToken != "second-choice" {
		t.Errorf("AccessToken = %q, want the next usable key to be tried", creds.AccessToken)
	}
	if creds.SourceKey != TokenKeys[1] {
		t.Errorf("SourceKey = %q, want %q", creds.SourceKey, TokenKeys[1])
	}
}

func TestTokenRowWithNoTokensIsSkipped(t *testing.T) {
	creds, err := LoadCredentials(fixture("no-tokens.sqlite3"))
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if creds.AccessToken != "real" {
		t.Errorf("AccessToken = %q, want a row with no tokens to be skipped", creds.AccessToken)
	}
	if creds.SourceKey != TokenKeys[1] {
		t.Errorf("SourceKey = %q, want %q", creds.SourceKey, TokenKeys[1])
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
