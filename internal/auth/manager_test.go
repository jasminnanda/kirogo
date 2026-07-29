package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fixedClock returns a deterministic clock function.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

var testNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

// refreshServer is a stub token endpoint that counts calls.
type refreshServer struct {
	*httptest.Server
	calls        atomic.Int64
	lastPath     atomic.Value // string
	lastBody     atomic.Value // []byte
	lastHeaders  atomic.Value // http.Header
	status       atomic.Int64
	responseBody atomic.Value // string
	delay        atomic.Int64 // nanoseconds
}

// newRefreshServer starts a stub endpoint returning a successful refresh.
func newRefreshServer(t *testing.T) *refreshServer {
	t.Helper()
	rs := &refreshServer{}
	rs.status.Store(http.StatusOK)
	rs.responseBody.Store(`{"accessToken":"fresh-access","refreshToken":"fresh-refresh","expiresIn":3600}`)
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.calls.Add(1)
		rs.lastPath.Store(r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		rs.lastBody.Store(body)
		rs.lastHeaders.Store(r.Header.Clone())
		if d := rs.delay.Load(); d > 0 {
			time.Sleep(time.Duration(d))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(rs.status.Load()))
		body2, _ := rs.responseBody.Load().(string)
		_, _ = io.WriteString(w, body2)
	}))
	t.Cleanup(rs.Close)
	return rs
}

// redirectingTransport sends every request to the stub server, so production
// endpoint URLs stay in the code under test while no real network is touched.
type redirectingTransport struct {
	target string
}

func (rt *redirectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	target := rt.target
	// Strip the scheme://host from the stub URL and graft the original path on.
	trimmed := strings.TrimPrefix(strings.TrimPrefix(target, "http://"), "https://")
	clone.URL.Scheme = "http"
	clone.URL.Host = trimmed
	return http.DefaultTransport.RoundTrip(clone)
}

// stubClient returns an http.Client whose requests all land on the stub server.
func stubClient(rs *refreshServer) *http.Client {
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &redirectingTransport{target: rs.URL},
	}
}

// captureLogs installs a logger writing into a buffer for the duration of a test
// and returns the buffer.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

func TestNewPrefersExplicitCredsFile(t *testing.T) {
	dir := t.TempDir()
	path := writeJSONFile(t, dir, "explicit.json",
		`{"accessToken":"from-file","refreshToken":"r","region":"eu-west-1","expiresAt":"2030-01-01T00:00:00Z"}`)

	m, err := New(Options{
		CredsFile:        path,
		RefreshToken:     "from-env-should-lose",
		DisableDiscovery: true,
		Now:              fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Source(); got != SourceFile {
		t.Errorf("Source() = %v, want SourceFile", got)
	}
	token, err := m.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "from-file" {
		t.Errorf("token = %q, want the file's access token", token)
	}
	if m.SSORegion() != "eu-west-1" {
		t.Errorf("SSORegion() = %q, want eu-west-1 from the file", m.SSORegion())
	}
}

func TestNewFailsLoudlyWhenExplicitCredsFileIsBroken(t *testing.T) {
	dir := t.TempDir()
	path := writeJSONFile(t, dir, "broken.json", `{ not json`)

	_, err := New(Options{CredsFile: path, RefreshToken: "fallback", DisableDiscovery: true})
	if err == nil {
		t.Fatal("a configured but unusable KIRO_CREDS_FILE must be a hard error, not a silent fallback")
	}
	if !strings.Contains(err.Error(), "KIRO_CREDS_FILE is set but unusable") {
		t.Errorf("error should name the misconfiguration, got %q", err)
	}
}

func TestNewUsesRefreshTokenEnvWhenNoFile(t *testing.T) {
	m, err := New(Options{
		RefreshToken:     "env-refresh",
		ProfileARN:       "arn:aws:codewhisperer:ap-south-1:1:profile/Z",
		SSORegion:        "us-east-1",
		DisableDiscovery: true,
		Now:              fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.Source() != SourceEnv {
		t.Errorf("Source() = %v, want SourceEnv", m.Source())
	}
	if m.Source().Writable() {
		t.Error("the env source must not be writable")
	}
	if m.ProfileARN() == "" {
		t.Error("PROFILE_ARN should be adopted when the credentials carry none")
	}
	if m.APIRegion() != "ap-south-1" {
		t.Errorf("APIRegion() = %q, want ap-south-1 parsed from the profile ARN", m.APIRegion())
	}
}

func TestNewAutoDiscovery(t *testing.T) {
	// Point HOME at a fixture tree so discovery is offline and deterministic.
	home := t.TempDir()
	cacheDir := filepath.Join(home, ".aws", "sso", "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, cacheDir, PreferredCredentialFile,
		`{"accessToken":"discovered","refreshToken":"r","expiresAt":"2030-01-01T00:00:00Z","authMethod":"social"}`)
	t.Setenv("HOME", home)

	buf := captureLogs(t)
	m, err := New(Options{SSORegion: "us-east-1", Now: fixedClock(testNow)})
	if err != nil {
		t.Fatal(err)
	}
	if m.Source() != SourceDiscovered {
		t.Errorf("Source() = %v, want SourceDiscovered", m.Source())
	}
	token, err := m.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "discovered" {
		t.Errorf("token = %q, want the discovered token", token)
	}
	// The startup log must describe the setup without printing the token.
	logged := buf.String()
	for _, want := range []string{"credentials loaded", "kiro-desktop", "us-east-1", "runtime.us-east-1.kiro.dev"} {
		if !strings.Contains(logged, want) {
			t.Errorf("startup log should mention %q; got:\n%s", want, logged)
		}
	}
	if strings.Contains(logged, "discovered") && strings.Contains(logged, "access") {
		t.Errorf("startup log may be leaking the token:\n%s", logged)
	}
}

func TestDiscoveryFallsBackToAnyFileWithRefreshToken(t *testing.T) {
	home := t.TempDir()
	cacheDir := filepath.Join(home, ".aws", "sso", "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A device registration file, which must be skipped: no refreshToken.
	writeJSONFile(t, cacheDir, "0123abcdhash.json", `{"clientId":"c","clientSecret":"s"}`)
	// A real session cache entry under an opaque name.
	writeJSONFile(t, cacheDir, "deadbeef.json", `{"refreshToken":"fallback-refresh","accessToken":"fallback-access"}`)
	t.Setenv("HOME", home)

	path, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if filepath.Base(path) != "deadbeef.json" {
		t.Errorf("Discover() = %s, want the file that has a refreshToken", path)
	}
}

func TestDiscoveryPrefersKiroAuthToken(t *testing.T) {
	home := t.TempDir()
	cacheDir := filepath.Join(home, ".aws", "sso", "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, cacheDir, "aaaa.json", `{"refreshToken":"other"}`)
	writeJSONFile(t, cacheDir, PreferredCredentialFile, `{"refreshToken":"preferred"}`)
	t.Setenv("HOME", home)

	path, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != PreferredCredentialFile {
		t.Errorf("Discover() = %s, want %s to win", path, PreferredCredentialFile)
	}
}

func TestDiscoverySkipsNonJSONAndEmptyFiles(t *testing.T) {
	home := t.TempDir()
	cacheDir := filepath.Join(home, ".aws", "sso", "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, cacheDir, "notes.txt", `refreshToken lives here`)
	writeJSONFile(t, cacheDir, "empty.json", ``)
	writeJSONFile(t, cacheDir, "norefresh.json", `{"accessToken":"a"}`)
	t.Setenv("HOME", home)

	if _, err := Discover(); err == nil {
		t.Error("Discover should fail when no file carries a refreshToken")
	}
}

func TestMissingCredentialsErrorIsActionable(t *testing.T) {
	home := t.TempDir() // empty: no cache directory at all
	t.Setenv("HOME", home)

	_, err := New(Options{SSORegion: "us-east-1"})
	if err == nil {
		t.Fatal("expected an error when there are no credentials anywhere")
	}
	msg := err.Error()
	for _, want := range []string{
		"KIRO_CREDS_FILE", "REFRESH_TOKEN", "KIRO_CLI_DB_FILE",
		PreferredCredentialFile, "sign in once",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message should mention %q; got:\n%s", want, msg)
		}
	}
	var mce *MissingCredentialsError
	if !errors.As(err, &mce) {
		t.Errorf("error should be a *MissingCredentialsError, got %T", err)
	}
}

func TestSQLiteSourceIsReportedWhenNoLoaderIsBuiltIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dbPath := filepath.Join(t.TempDir(), "data.sqlite3")
	if err := os.WriteFile(dbPath, []byte("SQLite format 3\x00"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := New(Options{CLIDBFile: dbPath, SSORegion: "us-east-1"})
	if err == nil {
		t.Fatal("expected failure with no SQLite loader wired in")
	}
	if !strings.Contains(err.Error(), "no SQLite reader") {
		t.Errorf("error should explain the missing reader; got:\n%s", err)
	}
}

func TestSQLiteLoaderIsUsedWhenSupplied(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dbPath := filepath.Join(t.TempDir(), "data.sqlite3")
	if err := os.WriteFile(dbPath, []byte("SQLite format 3\x00"), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := New(Options{
		CLIDBFile: dbPath,
		SSORegion: "us-east-1",
		Now:       fixedClock(testNow),
		SQLiteLoader: func(path string) (*Credentials, error) {
			if path != dbPath {
				t.Errorf("loader got path %q, want %q", path, dbPath)
			}
			return &Credentials{
				AccessToken: "sqlite-access",
				ExpiresAt:   testNow.Add(2 * time.Hour),
				Region:      "us-west-2",
				Source:      SourceSQLite,
				Path:        path,
				extra:       map[string]json.RawMessage{},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.Source() != SourceSQLite {
		t.Errorf("Source() = %v, want SourceSQLite", m.Source())
	}
	if m.Source().Writable() {
		t.Error("the SQLite source must never be writable")
	}
	if m.APIRegion() != "us-west-2" {
		t.Errorf("APIRegion() = %q, want us-west-2", m.APIRegion())
	}
}

func TestAPIRegionPriority(t *testing.T) {
	arn := "arn:aws:codewhisperer:eu-central-1:1:profile/A"
	cases := []struct {
		name      string
		opts      Options
		credsJSON string
		wantAPI   string
		wantSSO   string
	}{
		{
			name:      "explicit override wins over everything",
			opts:      Options{APIRegionOverride: "ap-northeast-1", SSORegion: "us-east-1"},
			credsJSON: `{"refreshToken":"r","region":"us-west-2","profileArn":"` + arn + `"}`,
			wantAPI:   "ap-northeast-1",
			wantSSO:   "us-west-2",
		},
		{
			name:      "profile ARN beats the file region",
			opts:      Options{SSORegion: "us-east-1"},
			credsJSON: `{"refreshToken":"r","region":"us-west-2","profileArn":"` + arn + `"}`,
			wantAPI:   "eu-central-1",
			wantSSO:   "us-west-2",
		},
		{
			name:      "file region when the ARN has no usable region",
			opts:      Options{SSORegion: "us-east-1"},
			credsJSON: `{"refreshToken":"r","region":"us-west-2","profileArn":"arn:aws:codewhisperer::1:profile/A"}`,
			wantAPI:   "us-west-2",
			wantSSO:   "us-west-2",
		},
		{
			name:      "SSO region when the file has none",
			opts:      Options{SSORegion: "ca-central-1"},
			credsJSON: `{"refreshToken":"r"}`,
			wantAPI:   "ca-central-1",
			wantSSO:   "ca-central-1",
		},
		{
			name:      "defaults to us-east-1",
			opts:      Options{},
			credsJSON: `{"refreshToken":"r"}`,
			wantAPI:   "us-east-1",
			wantSSO:   "us-east-1",
		},
		{
			name:      "PROFILE_ARN supplies the region when the file has no ARN",
			opts:      Options{SSORegion: "us-east-1", ProfileARN: "arn:aws:codewhisperer:sa-east-1:1:profile/B"},
			credsJSON: `{"refreshToken":"r"}`,
			wantAPI:   "sa-east-1",
			wantSSO:   "us-east-1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeJSONFile(t, dir, "c.json", tc.credsJSON)
			opts := tc.opts
			opts.CredsFile = path
			opts.DisableDiscovery = true
			opts.Now = fixedClock(testNow)

			m, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			if m.APIRegion() != tc.wantAPI {
				t.Errorf("APIRegion() = %q, want %q", m.APIRegion(), tc.wantAPI)
			}
			if m.SSORegion() != tc.wantSSO {
				t.Errorf("SSORegion() = %q, want %q", m.SSORegion(), tc.wantSSO)
			}
			if want := "https://runtime." + tc.wantAPI + ".kiro.dev"; m.RuntimeHost() != want {
				t.Errorf("RuntimeHost() = %q, want %q", m.RuntimeHost(), want)
			}
		})
	}
}

func TestExpiryMathAndRefreshThreshold(t *testing.T) {
	cases := []struct {
		name        string
		expiresAt   time.Time
		accessToken string
		wantRefresh bool
	}{
		{"far in the future", testNow.Add(2 * time.Hour), "a", false},
		{"just outside the threshold", testNow.Add(601 * time.Second), "a", false},
		{"exactly at the threshold", testNow.Add(600 * time.Second), "a", true},
		{"inside the threshold", testNow.Add(599 * time.Second), "a", true},
		{"already expired", testNow.Add(-time.Second), "a", true},
		{"unknown expiry", time.Time{}, "a", true},
		{"no access token", testNow.Add(2 * time.Hour), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{
				creds: &Credentials{AccessToken: tc.accessToken, ExpiresAt: tc.expiresAt},
				now:   fixedClock(testNow),
			}
			if got := m.needsRefreshLocked(); got != tc.wantRefresh {
				t.Errorf("needsRefreshLocked() = %v, want %v", got, tc.wantRefresh)
			}
		})
	}
}

func TestRefreshSubtractsSixtySecondBuffer(t *testing.T) {
	rs := newRefreshServer(t)
	rs.responseBody.Store(`{"accessToken":"fresh","expiresIn":3600}`)

	dir := t.TempDir()
	path := writeJSONFile(t, dir, "c.json", `{"refreshToken":"r"}`)
	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true,
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := testNow.Add(3600*time.Second - 60*time.Second)
	if got := m.ExpiresAt(); !got.Equal(want) {
		t.Errorf("ExpiresAt() = %v, want %v (expiresIn minus the 60s buffer)", got, want)
	}
}

func TestRefreshDefaultsExpiresInWhenAbsent(t *testing.T) {
	rs := newRefreshServer(t)
	rs.responseBody.Store(`{"accessToken":"fresh"}`)

	dir := t.TempDir()
	path := writeJSONFile(t, dir, "c.json", `{"refreshToken":"r"}`)
	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true,
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := testNow.Add(3600*time.Second - 60*time.Second)
	if got := m.ExpiresAt(); !got.Equal(want) {
		t.Errorf("ExpiresAt() = %v, want the 3600s default minus the buffer (%v)", got, want)
	}
}

func TestKiroDesktopRefreshRequestShape(t *testing.T) {
	rs := newRefreshServer(t)
	dir := t.TempDir()
	path := writeJSONFile(t, dir, "c.json", `{"refreshToken":"my-refresh-token","region":"eu-central-1"}`)

	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true, KiroVersion: "9.9.9",
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Token(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got, _ := rs.lastPath.Load().(string); got != "/refreshToken" {
		t.Errorf("path = %q, want /refreshToken", got)
	}
	body, _ := rs.lastBody.Load().([]byte)
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if payload["refreshToken"] != "my-refresh-token" {
		t.Errorf("body = %s, want just the refresh token", body)
	}
	if len(payload) != 1 {
		t.Errorf("body has %d fields (%s), want exactly one", len(payload), body)
	}

	headers, _ := rs.lastHeaders.Load().(http.Header)
	if got := headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	wantUA := "KiroIDE-9.9.9-" + m.Fingerprint()
	if got := headers.Get("User-Agent"); got != wantUA {
		t.Errorf("User-Agent = %q, want %q", got, wantUA)
	}
}

func TestAWSSSOOIDCRefreshRequestShape(t *testing.T) {
	rs := newRefreshServer(t)
	dir := t.TempDir()
	path := writeJSONFile(t, dir, "c.json",
		`{"refreshToken":"rt","clientId":"cid","clientSecret":"csec","region":"us-west-2"}`)

	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true,
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.Flow() != FlowAWSSSOOIDC {
		t.Fatalf("Flow() = %v, want AWS SSO OIDC", m.Flow())
	}
	if _, err := m.Token(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got, _ := rs.lastPath.Load().(string); got != "/token" {
		t.Errorf("path = %q, want /token", got)
	}
	headers, _ := rs.lastHeaders.Load().(http.Header)
	if got := headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (not form-encoded)", got)
	}

	body, _ := rs.lastBody.Load().([]byte)
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	want := map[string]any{
		"grantType":    "refresh_token",
		"clientId":     "cid",
		"clientSecret": "csec",
		"refreshToken": "rt",
	}
	for k, v := range want {
		if payload[k] != v {
			t.Errorf("body[%s] = %v, want %v", k, payload[k], v)
		}
	}
	if len(payload) != len(want) {
		t.Errorf("body has %d fields (%s), want %d and no scope", len(payload), body, len(want))
	}
	if _, hasScope := payload["scope"]; hasScope {
		t.Error("the OIDC request must not send a scope parameter")
	}
	if _, snake := payload["client_id"]; snake {
		t.Error("the OIDC request must use camelCase keys, not client_id")
	}
}

func TestAWSSSOOIDCMissingClientCredentialsIsActionable(t *testing.T) {
	// Client secret present but no id: the flow stays Kiro Desktop, so force the
	// OIDC path directly to test its guard clauses.
	m := &Manager{
		ssoRegion:   "us-east-1",
		kiroVersion: "0.7.45",
		httpClient:  &http.Client{},
		now:         fixedClock(testNow),
	}
	cases := []struct {
		name  string
		creds *Credentials
		want  string
	}{
		{"no refresh token", &Credentials{ClientID: "c", ClientSecret: "s"}, "no refresh token"},
		{"no client id", &Credentials{RefreshToken: "r", ClientSecret: "s"}, "clientId"},
		{"no client secret", &Credentials{RefreshToken: "r", ClientID: "c"}, "clientSecret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.refreshAWSSSOOIDC(context.Background(), tc.creds)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

func TestKiroDesktopRefreshWithoutTokenIsActionable(t *testing.T) {
	m := &Manager{ssoRegion: "us-east-1", kiroVersion: "0.7.45", httpClient: &http.Client{}, now: fixedClock(testNow)}
	_, err := m.refreshKiroDesktop(context.Background(), &Credentials{})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"no refresh token", "Sign in to Kiro IDE", "REFRESH_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestFiftyConcurrentCallersCauseOneRefresh(t *testing.T) {
	rs := newRefreshServer(t)
	rs.delay.Store(int64(50 * time.Millisecond)) // widen the window for collisions

	dir := t.TempDir()
	path := writeJSONFile(t, dir, "c.json", `{"refreshToken":"r"}`) // no access token: refresh required
	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true,
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 50
	var wg sync.WaitGroup
	tokens := make([]string, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			tokens[idx], errs[idx] = m.Token(context.Background())
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d failed: %v", i, err)
		}
		if tokens[i] != "fresh-access" {
			t.Fatalf("goroutine %d got token %q, want fresh-access", i, tokens[i])
		}
	}
	if calls := rs.calls.Load(); calls != 1 {
		t.Errorf("the token endpoint was called %d times, want exactly 1", calls)
	}
	if got := m.RefreshCount(); got != 1 {
		t.Errorf("RefreshCount() = %d, want 1", got)
	}
}

func TestSubsequentCallsReuseTheCachedToken(t *testing.T) {
	rs := newRefreshServer(t)
	dir := t.TempDir()
	path := writeJSONFile(t, dir, "c.json", `{"refreshToken":"r"}`)
	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true,
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		if _, err := m.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls := rs.calls.Load(); calls != 1 {
		t.Errorf("endpoint called %d times, want 1: the token should be cached", calls)
	}
}

func TestForceRefreshAlwaysContactsTheEndpoint(t *testing.T) {
	rs := newRefreshServer(t)
	dir := t.TempDir()
	// A token valid for two hours: Token() must not refresh, ForceRefresh() must.
	path := writeJSONFile(t, dir, "c.json",
		`{"accessToken":"still-good","refreshToken":"r","expiresAt":"`+
			testNow.Add(2*time.Hour).Format(time.RFC3339)+`"}`)

	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true,
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}

	if tok, err := m.Token(context.Background()); err != nil || tok != "still-good" {
		t.Fatalf("Token() = %q, %v; want the cached token", tok, err)
	}
	if calls := rs.calls.Load(); calls != 0 {
		t.Fatalf("Token() triggered %d refreshes, want 0", calls)
	}

	tok, err := m.ForceRefresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "fresh-access" {
		t.Errorf("ForceRefresh() = %q, want fresh-access", tok)
	}
	if calls := rs.calls.Load(); calls != 1 {
		t.Errorf("ForceRefresh() made %d calls, want 1", calls)
	}
}

func TestRefreshedTokenIsWrittenBackToTheFile(t *testing.T) {
	rs := newRefreshServer(t)
	dir := t.TempDir()
	path := writeJSONFile(t, dir, "c.json",
		`{"refreshToken":"old-refresh","provider":"Google","startUrl":"https://x/start"}`)

	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true,
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Token(context.Background()); err != nil {
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
	if out["accessToken"] != "fresh-access" {
		t.Errorf("accessToken = %v, want fresh-access", out["accessToken"])
	}
	if out["refreshToken"] != "fresh-refresh" {
		t.Errorf("refreshToken = %v, want the rotated value", out["refreshToken"])
	}
	if out["provider"] != "Google" || out["startUrl"] != "https://x/start" {
		t.Errorf("unknown fields were not preserved: %v", out)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode after write-back = %o, want 600", perm)
	}
}

func TestRefreshKeepsTheOldRefreshTokenWhenTheServerOmitsANewOne(t *testing.T) {
	rs := newRefreshServer(t)
	rs.responseBody.Store(`{"accessToken":"fresh","expiresIn":3600}`)

	dir := t.TempDir()
	path := writeJSONFile(t, dir, "c.json", `{"refreshToken":"keep-me"}`)
	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true,
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Token(context.Background()); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out["refreshToken"] != "keep-me" {
		t.Errorf("refreshToken = %v, want the original to be kept", out["refreshToken"])
	}
}

func TestRefreshAdoptsProfileARNFromTheResponse(t *testing.T) {
	rs := newRefreshServer(t)
	rs.responseBody.Store(`{"accessToken":"a","expiresIn":3600,"profileArn":"arn:aws:codewhisperer:us-west-2:9:profile/NEW"}`)

	dir := t.TempDir()
	path := writeJSONFile(t, dir, "c.json", `{"refreshToken":"r"}`)
	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true,
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.ProfileARN(); got != "arn:aws:codewhisperer:us-west-2:9:profile/NEW" {
		t.Errorf("ProfileARN() = %q, want the ARN from the refresh response", got)
	}
}

func TestRefreshFailureWithValidTokenDegradesGracefully(t *testing.T) {
	rs := newRefreshServer(t)
	rs.status.Store(http.StatusBadRequest)
	rs.responseBody.Store(`{"error":"invalid_grant","error_description":"refresh token expired"}`)

	dir := t.TempDir()
	// Inside the 600s refresh window but not yet expired.
	path := writeJSONFile(t, dir, "c.json",
		`{"accessToken":"still-usable","refreshToken":"stale","expiresAt":"`+
			testNow.Add(300*time.Second).Format(time.RFC3339)+`"}`)

	buf := captureLogs(t)
	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true,
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, err := m.Token(context.Background())
	if err != nil {
		t.Fatalf("a failed refresh with a still-valid token should not fail: %v", err)
	}
	if tok != "still-usable" {
		t.Errorf("token = %q, want the existing access token", tok)
	}
	if !strings.Contains(buf.String(), "continuing with the existing access token") {
		t.Errorf("expected a graceful-degradation warning, got:\n%s", buf.String())
	}
}

func TestRefreshFailureWithExpiredTokenIsReported(t *testing.T) {
	rs := newRefreshServer(t)
	rs.status.Store(http.StatusBadRequest)
	rs.responseBody.Store(`{"error":"invalid_grant","error_description":"token is not valid"}`)

	dir := t.TempDir()
	path := writeJSONFile(t, dir, "c.json",
		`{"accessToken":"expired","refreshToken":"stale","expiresAt":"`+
			testNow.Add(-time.Hour).Format(time.RFC3339)+`"}`)

	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true,
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.Token(context.Background())
	if err == nil {
		t.Fatal("expected an error when both tokens are unusable")
	}
	var re *RefreshError
	if !errors.As(err, &re) {
		t.Fatalf("error should be a *RefreshError, got %T: %v", err, err)
	}
	if !re.IsCredentialRejected() {
		t.Errorf("HTTP %d should count as a rejected credential", re.StatusCode)
	}
	for _, want := range []string{"rejected", "Sign in"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q should mention %q", err, want)
		}
	}
	if !strings.Contains(err.Error(), "token is not valid") {
		t.Errorf("message should relay the server's description, got %q", err)
	}
}

func TestRefreshFailureStatusMessages(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusForbidden, "denied the request"},
		{http.StatusTooManyRequests, "rate limiting"},
		{http.StatusInternalServerError, "upstream outage"},
		{http.StatusBadGateway, "upstream outage"},
		{http.StatusNotFound, "returned HTTP 404"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			rs := newRefreshServer(t)
			rs.status.Store(int64(tc.status))
			rs.responseBody.Store(`{"message":"nope"}`)

			dir := t.TempDir()
			path := writeJSONFile(t, dir, "c.json", `{"refreshToken":"r"}`)
			m, err := New(Options{
				CredsFile: path, DisableDiscovery: true,
				HTTPClient: stubClient(rs), Now: fixedClock(testNow),
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = m.Token(context.Background())
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("message for HTTP %d = %q, should contain %q", tc.status, err, tc.want)
			}
		})
	}
}

func TestRefreshResponseWithoutAccessTokenIsRejected(t *testing.T) {
	rs := newRefreshServer(t)
	rs.responseBody.Store(`{"expiresIn":3600}`)

	dir := t.TempDir()
	path := writeJSONFile(t, dir, "c.json", `{"refreshToken":"r"}`)
	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true,
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no accessToken") {
		t.Errorf("error = %v, want a complaint about the missing accessToken", err)
	}
}

func TestRefreshResponseThatIsNotJSONIsRejected(t *testing.T) {
	rs := newRefreshServer(t)
	rs.responseBody.Store(`<html>gateway error</html>`)

	dir := t.TempDir()
	path := writeJSONFile(t, dir, "c.json", `{"refreshToken":"r"}`)
	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true,
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("error = %v, want a complaint that the body is not JSON", err)
	}
}

func TestTokenRespectsCallerCancellation(t *testing.T) {
	rs := newRefreshServer(t)
	rs.delay.Store(int64(2 * time.Second))

	dir := t.TempDir()
	path := writeJSONFile(t, dir, "c.json", `{"refreshToken":"r"}`)
	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true,
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := m.Token(ctx); err == nil {
		t.Fatal("expected the caller's deadline to be honoured")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want a wrapped DeadlineExceeded", err)
	}
}

func TestRefreshCompletesEvenIfTheInitiatingCallerGivesUp(t *testing.T) {
	rs := newRefreshServer(t)
	rs.delay.Store(int64(150 * time.Millisecond))

	dir := t.TempDir()
	path := writeJSONFile(t, dir, "c.json", `{"refreshToken":"r"}`)
	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true,
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}

	// First caller starts the refresh and then abandons it.
	shortCtx, cancelShort := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelShort()
	if _, err := m.Token(shortCtx); err == nil {
		t.Fatal("the impatient caller should have timed out")
	}

	// A patient caller must still receive the token from that same refresh.
	patientCtx, cancelPatient := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPatient()
	tok, err := m.Token(patientCtx)
	if err != nil {
		t.Fatalf("the detached refresh should still deliver a token: %v", err)
	}
	if tok != "fresh-access" {
		t.Errorf("token = %q, want fresh-access", tok)
	}
	if calls := rs.calls.Load(); calls > 2 {
		t.Errorf("endpoint called %d times, want at most 2", calls)
	}
}

func TestNoTokenValueEverReachesTheLog(t *testing.T) {
	const (
		secretAccess  = "SECRET-ACCESS-TOKEN-VALUE-0123456789"
		secretRefresh = "SECRET-REFRESH-TOKEN-VALUE-0123456789"
		secretNew     = "SECRET-NEWLY-ISSUED-TOKEN-9876543210"
		secretID      = "SECRET-CLIENT-ID-VALUE"
		secretSecret  = "SECRET-CLIENT-SECRET-VALUE"
	)

	rs := newRefreshServer(t)
	rs.responseBody.Store(`{"accessToken":"` + secretNew + `","refreshToken":"` + secretNew + `","expiresIn":3600}`)

	dir := t.TempDir()
	path := writeJSONFile(t, dir, "c.json", `{
	  "accessToken": "`+secretAccess+`",
	  "refreshToken": "`+secretRefresh+`",
	  "clientId": "`+secretID+`",
	  "clientSecret": "`+secretSecret+`",
	  "region": "us-east-1"
	}`)

	buf := captureLogs(t)
	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true,
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ForceRefresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Also exercise the failure paths, which are the likeliest place to leak.
	rs.status.Store(http.StatusBadRequest)
	rs.responseBody.Store(`{"error":"invalid_grant","error_description":"bad token"}`)
	_, _ = m.ForceRefresh(context.Background())

	logged := buf.String()
	for _, secret := range []string{secretAccess, secretRefresh, secretNew, secretID, secretSecret} {
		if strings.Contains(logged, secret) {
			t.Errorf("log leaked the secret %q:\n%s", secret, logged)
		}
	}
	if logged == "" {
		t.Error("expected some log output, so the assertion above is meaningful")
	}
}

func TestSummariseErrorBody(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"oidc style", `{"error":"invalid_grant","error_description":"expired"}`, "invalid_grant: expired"},
		{"kiro style", `{"message":"Improperly formed request.","reason":"null"}`, "Improperly formed request.: null"},
		{"deduplicates", `{"error":"same","message":"same"}`, "same"},
		{"not json", `<html>`, ""},
		{"empty object", `{}`, ""},
		{"ignores unknown fields", `{"accessToken":"leak-me"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summariseErrorBody([]byte(tc.in)); got != tc.want {
				t.Errorf("summariseErrorBody(%s) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSummariseErrorBodyIsTruncated(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := summariseErrorBody([]byte(`{"message":"` + long + `"}`))
	if len(got) > 310 {
		t.Errorf("summary length = %d, want it truncated near 300", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated summary should end in an ellipsis, got %q", got[len(got)-10:])
	}
}

func TestManagerAccessorsAreSafeUnderConcurrency(t *testing.T) {
	rs := newRefreshServer(t)
	dir := t.TempDir()
	path := writeJSONFile(t, dir, "c.json", `{"refreshToken":"r","authMethod":"external_idp"}`)
	m, err := New(Options{
		CredsFile: path, DisableDiscovery: true,
		HTTPClient: stubClient(rs), Now: fixedClock(testNow),
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = m.Token(context.Background())
			_ = m.ProfileARN()
			_ = m.Flow()
			_ = m.Source()
			_ = m.TokenTypeHeader()
			_ = m.ExpiresAt()
			_ = m.RuntimeHost()
			_ = m.Fingerprint()
			_ = m.KiroVersion()
		}()
	}
	wg.Wait()

	if got := m.TokenTypeHeader(); got != "EXTERNAL_IDP" {
		t.Errorf("TokenTypeHeader() = %q, want EXTERNAL_IDP", got)
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
