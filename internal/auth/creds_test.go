package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeJSONFile writes content to a file inside dir and returns its path.
func writeJSONFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestParseExpiry(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // RFC3339Nano in UTC, or "" when an error is expected
	}{
		{"rfc3339 with Z", "2026-07-29T19:33:08Z", "2026-07-29T19:33:08Z"},
		{"milliseconds", "2026-07-29T19:33:08.938Z", "2026-07-29T19:33:08.938Z"},
		{"microseconds", "2026-07-29T19:33:08.938123Z", "2026-07-29T19:33:08.938123Z"},
		{"nanoseconds", "2026-07-29T19:33:08.938123456Z", "2026-07-29T19:33:08.938123456Z"},
		{"twelve fractional digits truncated", "2026-07-29T19:33:08.938123456789Z", "2026-07-29T19:33:08.938123456Z"},
		{"offset zone", "2026-07-29T21:33:08+02:00", "2026-07-29T19:33:08Z"},
		{"negative offset", "2026-07-29T14:33:08-05:00", "2026-07-29T19:33:08Z"},
		{"no zone treated as utc", "2026-07-29T19:33:08", "2026-07-29T19:33:08Z"},
		{"no zone with fraction", "2026-07-29T19:33:08.5", "2026-07-29T19:33:08.5Z"},
		{"space separator", "2026-07-29 19:33:08", "2026-07-29T19:33:08Z"},
		{"surrounding whitespace", "  2026-07-29T19:33:08Z  ", "2026-07-29T19:33:08Z"},
		{"empty", "", ""},
		{"garbage", "not-a-date", ""},
		{"date only", "2026-07-29", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseExpiry(tc.in)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("ParseExpiry(%q) = %v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseExpiry(%q) returned error: %v", tc.in, err)
			}
			if formatted := got.Format(time.RFC3339Nano); formatted != tc.want {
				t.Errorf("ParseExpiry(%q) = %s, want %s", tc.in, formatted, tc.want)
			}
			if got.Location() != time.UTC {
				t.Errorf("ParseExpiry(%q) returned a non-UTC time in %v", tc.in, got.Location())
			}
		})
	}
}

func TestParseCredentialsRecognisesFields(t *testing.T) {
	raw := `{
	  "accessToken": "at-value",
	  "refreshToken": "rt-value",
	  "profileArn": "arn:aws:codewhisperer:eu-central-1:111122223333:profile/ABC",
	  "region": "eu-central-1",
	  "expiresAt": "2026-07-29T19:33:08.938Z",
	  "clientId": "cid",
	  "clientSecret": "csecret",
	  "authMethod": "external_idp",
	  "provider": "ExternalIdp",
	  "startUrl": "https://example.awsapps.com/start"
	}`
	creds, err := parseCredentials([]byte(raw))
	if err != nil {
		t.Fatalf("parseCredentials: %v", err)
	}

	if creds.AccessToken != "at-value" || creds.RefreshToken != "rt-value" {
		t.Errorf("tokens not parsed: %+v", creds)
	}
	if creds.Region != "eu-central-1" {
		t.Errorf("Region = %q", creds.Region)
	}
	if creds.AuthMethod != "external_idp" {
		t.Errorf("AuthMethod = %q", creds.AuthMethod)
	}
	if creds.Flow() != FlowAWSSSOOIDC {
		t.Errorf("Flow() = %v, want AWS SSO OIDC when clientId and clientSecret are present", creds.Flow())
	}
	if creds.TokenTypeHeader() != "EXTERNAL_IDP" {
		t.Errorf("TokenTypeHeader() = %q, want EXTERNAL_IDP", creds.TokenTypeHeader())
	}
	if creds.ExpiresAt.Format(time.RFC3339Nano) != "2026-07-29T19:33:08.938Z" {
		t.Errorf("ExpiresAt = %v", creds.ExpiresAt)
	}
	// Unrecognised fields must be preserved for write-back.
	for _, key := range []string{"provider", "startUrl"} {
		if _, ok := creds.extra[key]; !ok {
			t.Errorf("unknown field %q was dropped instead of preserved", key)
		}
	}
	if _, ok := creds.extra["accessToken"]; ok {
		t.Error("accessToken must not be duplicated into the preserved-extras map")
	}
}

func TestParseCredentialsFlowDetection(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want Flow
	}{
		{"no client credentials", `{"refreshToken":"r"}`, FlowKiroDesktop},
		{"client id only", `{"refreshToken":"r","clientId":"c"}`, FlowKiroDesktop},
		{"client secret only", `{"refreshToken":"r","clientSecret":"s"}`, FlowKiroDesktop},
		{"both present", `{"refreshToken":"r","clientId":"c","clientSecret":"s"}`, FlowAWSSSOOIDC},
		{"empty strings", `{"refreshToken":"r","clientId":"","clientSecret":""}`, FlowKiroDesktop},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			creds, err := parseCredentials([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if got := creds.Flow(); got != tc.want {
				t.Errorf("Flow() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseCredentialsRejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"not json", `not json at all`},
		{"json array", `["a","b"]`},
		{"json string", `"just a string"`},
		{"empty object", `{}`},
		{"no tokens", `{"region":"us-east-1"}`},
		{"wrong type for token", `{"refreshToken": 12345}`},
		{"truncated", `{"refreshToken":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseCredentials([]byte(tc.raw)); err == nil {
				t.Errorf("parseCredentials(%q) should fail", tc.raw)
			}
		})
	}
}

func TestParseCredentialsUnparseableExpiryForcesRefresh(t *testing.T) {
	creds, err := parseCredentials([]byte(`{"accessToken":"a","refreshToken":"r","expiresAt":"whenever"}`))
	if err != nil {
		t.Fatalf("a bad expiry must not fail the load: %v", err)
	}
	if !creds.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt = %v, want the zero time so a refresh is forced", creds.ExpiresAt)
	}
}

func TestLoadCredentialsFileMissing(t *testing.T) {
	_, err := LoadCredentialsFile(filepath.Join(t.TempDir(), "absent.json"), SourceFile)
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if !strings.Contains(err.Error(), "could not read credentials file") {
		t.Errorf("error should name the problem, got %q", err)
	}
}

func TestLoadCredentialsFileEnterpriseDeviceRegistration(t *testing.T) {
	dir := t.TempDir()
	writeJSONFile(t, dir, "abc123hash.json", `{"clientId":"enterprise-cid","clientSecret":"enterprise-secret","region":"us-west-2"}`)
	path := writeJSONFile(t, dir, "kiro-auth-token.json", `{"refreshToken":"r","clientIdHash":"abc123hash"}`)

	creds, err := LoadCredentialsFile(path, SourceFile)
	if err != nil {
		t.Fatal(err)
	}
	if creds.ClientID != "enterprise-cid" || creds.ClientSecret != "enterprise-secret" {
		t.Errorf("client credentials not chained from the device registration: %+v", creds)
	}
	if creds.Region != "us-west-2" {
		t.Errorf("Region = %q, want the device registration region as a fallback", creds.Region)
	}
	if creds.Flow() != FlowAWSSSOOIDC {
		t.Errorf("Flow() = %v, want AWS SSO OIDC after chaining", creds.Flow())
	}
}

func TestLoadCredentialsFileMissingDeviceRegistrationIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	path := writeJSONFile(t, dir, "kiro-auth-token.json", `{"refreshToken":"r","clientIdHash":"nope"}`)

	creds, err := LoadCredentialsFile(path, SourceFile)
	if err != nil {
		t.Fatalf("a missing device registration should not fail the load: %v", err)
	}
	if creds.Flow() != FlowKiroDesktop {
		t.Errorf("Flow() = %v, want Kiro Desktop when chaining failed", creds.Flow())
	}
}

func TestLoadCredentialsFileDeviceRegistrationDoesNotOverrideExplicitValues(t *testing.T) {
	dir := t.TempDir()
	writeJSONFile(t, dir, "h.json", `{"clientId":"from-registration","clientSecret":"from-registration"}`)
	path := writeJSONFile(t, dir, "creds.json",
		`{"refreshToken":"r","clientIdHash":"h","clientId":"explicit","clientSecret":"explicit"}`)

	creds, err := LoadCredentialsFile(path, SourceFile)
	if err != nil {
		t.Fatal(err)
	}
	if creds.ClientID != "explicit" || creds.ClientSecret != "explicit" {
		t.Errorf("explicit client credentials were overwritten: %+v", creds)
	}
}

func TestSavePreservesUnknownFieldsAndSetsMode0600(t *testing.T) {
	dir := t.TempDir()
	original := `{
	  "accessToken": "old-access",
	  "refreshToken": "old-refresh",
	  "expiresAt": "2020-01-01T00:00:00Z",
	  "profileArn": "arn:aws:codewhisperer:us-east-1:1:profile/A",
	  "region": "us-east-1",
	  "authMethod": "social",
	  "provider": "Google",
	  "startUrl": "https://example/start",
	  "registrationExpiresAt": "2030-01-01T00:00:00Z",
	  "nested": {"keep": [1, 2, 3]}
	}`
	path := writeJSONFile(t, dir, "creds.json", original)

	creds, err := LoadCredentialsFile(path, SourceFile)
	if err != nil {
		t.Fatal(err)
	}
	creds.AccessToken = "new-access"
	creds.RefreshToken = "new-refresh"
	creds.ExpiresAt = time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)

	if err := creds.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600", perm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("saved file is not valid JSON: %v", err)
	}

	wantStrings := map[string]string{
		"accessToken":           "new-access",
		"refreshToken":          "new-refresh",
		"expiresAt":             "2027-01-02T03:04:05Z",
		"profileArn":            "arn:aws:codewhisperer:us-east-1:1:profile/A",
		"region":                "us-east-1",
		"authMethod":            "social",
		"provider":              "Google",
		"startUrl":              "https://example/start",
		"registrationExpiresAt": "2030-01-01T00:00:00Z",
	}
	for k, want := range wantStrings {
		if got, _ := out[k].(string); got != want {
			t.Errorf("saved %s = %v, want %q", k, out[k], want)
		}
	}
	nested, ok := out["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested object was lost: %v", out["nested"])
	}
	if arr, ok := nested["keep"].([]any); !ok || len(arr) != 3 {
		t.Errorf("nested.keep = %v, want a 3-element array", nested["keep"])
	}
}

func TestSaveIsNoOpForNonWritableSources(t *testing.T) {
	for _, source := range []Source{SourceEnv, SourceSQLite} {
		t.Run(source.String(), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "should-not-appear.json")
			creds := &Credentials{
				AccessToken: "a",
				Source:      source,
				Path:        path,
				extra:       map[string]json.RawMessage{},
			}
			if err := creds.Save(); err != nil {
				t.Fatalf("Save should be a silent no-op, got %v", err)
			}
			if _, err := os.Stat(path); err == nil {
				t.Errorf("Save wrote %s for source %v, which must never be written", path, source)
			}
		})
	}
}

func TestSaveWithEmptyPathIsNoOp(t *testing.T) {
	creds := &Credentials{AccessToken: "a", Source: SourceFile, Path: "", extra: map[string]json.RawMessage{}}
	if err := creds.Save(); err != nil {
		t.Errorf("Save with no path should be a no-op, got %v", err)
	}
}

func TestSaveOmitsEmptyOptionalFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	creds := &Credentials{
		AccessToken:  "a",
		RefreshToken: "r",
		Source:       SourceFile,
		Path:         path,
		extra:        map[string]json.RawMessage{},
	}
	if err := creds.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"profileArn", "region", "clientId", "clientSecret", "clientIdHash", "authMethod", "expiresAt"} {
		if _, present := out[key]; present {
			t.Errorf("empty field %q should be omitted, got %v", key, out[key])
		}
	}
}

func TestSaveLeavesNoTemporaryFilesBehind(t *testing.T) {
	dir := t.TempDir()
	path := writeJSONFile(t, dir, "creds.json", `{"refreshToken":"r"}`)
	creds, err := LoadCredentialsFile(path, SourceFile)
	if err != nil {
		t.Fatal(err)
	}
	creds.AccessToken = "a"
	if err := creds.Save(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "creds.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory contains %v, want only creds.json", names)
	}
}

func TestRegionFromARN(t *testing.T) {
	cases := map[string]string{
		"arn:aws:codewhisperer:us-east-1:123456789012:profile/ABC":    "us-east-1",
		"arn:aws:codewhisperer:eu-central-1:123456789012:profile/ABC": "eu-central-1",
		"arn:aws:codewhisperer:ap-southeast-2:1:profile/X":            "ap-southeast-2",
		"arn:aws:codewhisperer::123456789012:profile/ABC":             "",
		"arn:aws:codewhisperer:NOT-A-REGION:1:profile/X":              "",
		"arn:aws:codewhisperer:useast1:1:profile/X":                   "",
		"arn:aws:codewhisperer:us-east:1:profile/X":                   "",
		"arn:aws:s3": "",
		"":           "",
		"not-an-arn": "",
	}
	for arn, want := range cases {
		if got := RegionFromARN(arn); got != want {
			t.Errorf("RegionFromARN(%q) = %q, want %q", arn, got, want)
		}
	}
}

func TestTokenTypeHeaderMapping(t *testing.T) {
	cases := map[string]string{
		"external_idp":  "EXTERNAL_IDP",
		"machine_token": "KIRO_MACHINE_TOKEN",
		"api_key":       "API_KEY",
		"IdC":           "SSO_OIDC",
		"social":        "",
		"":              "",
		"unknown":       "",
	}
	for method, want := range cases {
		creds := &Credentials{AuthMethod: method}
		if got := creds.TokenTypeHeader(); got != want {
			t.Errorf("authMethod %q gave TokenType %q, want %q", method, got, want)
		}
	}
}

func TestSourceWritable(t *testing.T) {
	cases := map[Source]bool{
		SourceFile:       true,
		SourceDiscovered: true,
		SourceEnv:        false,
		SourceSQLite:     false,
	}
	for source, want := range cases {
		if got := source.Writable(); got != want {
			t.Errorf("%v.Writable() = %v, want %v", source, got, want)
		}
	}
}

func TestFlowString(t *testing.T) {
	if FlowKiroDesktop.String() != "kiro-desktop" {
		t.Errorf("FlowKiroDesktop.String() = %q", FlowKiroDesktop.String())
	}
	if FlowAWSSSOOIDC.String() != "aws-sso-oidc" {
		t.Errorf("FlowAWSSSOOIDC.String() = %q", FlowAWSSSOOIDC.String())
	}
}

func TestEndpointTemplates(t *testing.T) {
	if got := RuntimeHost("eu-central-1"); got != "https://runtime.eu-central-1.kiro.dev" {
		t.Errorf("RuntimeHost = %q", got)
	}
	if got := ControlPlaneHost("eu-central-1"); got != "https://management.eu-central-1.kiro.dev" {
		t.Errorf("ControlPlaneHost = %q", got)
	}
	if RuntimeHost("us-east-1") == ControlPlaneHost("us-east-1") {
		t.Error("the runtime and control plane hosts are different services and must not be the same URL")
	}
	if got := KiroDesktopRefreshURL("us-east-1"); got != "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken" {
		t.Errorf("KiroDesktopRefreshURL = %q", got)
	}
	if got := AWSSSOOIDCTokenURL("us-west-2"); got != "https://oidc.us-west-2.amazonaws.com/token" {
		t.Errorf("AWSSSOOIDCTokenURL = %q", got)
	}
	// The endpoint that does not exist outside us-east-1 must never appear.
	for _, url := range []string{RuntimeHost("eu-central-1"), KiroDesktopRefreshURL("eu-central-1")} {
		if strings.Contains(url, "codewhisperer.") && strings.Contains(url, "amazonaws.com") {
			t.Errorf("built a codewhisperer.{region}.amazonaws.com URL: %s", url)
		}
	}
}

func TestMachineFingerprintIsStableAndOpaque(t *testing.T) {
	first := MachineFingerprint()
	second := MachineFingerprint()
	if first != second {
		t.Errorf("fingerprint is not stable: %q vs %q", first, second)
	}
	if len(first) != 64 {
		t.Errorf("fingerprint length = %d, want 64 hex characters", len(first))
	}
	for _, c := range first {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("fingerprint contains a non-hex character %q: %s", c, first)
		}
	}
	// It must not embed the raw hostname or username.
	host, _ := os.Hostname()
	if host != "" && strings.Contains(first, host) {
		t.Error("fingerprint leaks the hostname")
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
