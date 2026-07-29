package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// SQLiteLoader reads credentials from a kiro-cli SQLite database. It is supplied
// by the caller so this package does not depend on the optional reader.
type SQLiteLoader func(path string) (*Credentials, error)

// Options configures a Manager.
type Options struct {
	// CredsFile is KIRO_CREDS_FILE. Highest priority source.
	CredsFile string
	// RefreshToken is REFRESH_TOKEN. Second priority.
	RefreshToken string
	// CLIDBFile is KIRO_CLI_DB_FILE. Lowest priority, and only usable when
	// SQLiteLoader is non-nil.
	CLIDBFile string
	// ProfileARN is PROFILE_ARN, used when the credentials carry none.
	ProfileARN string
	// SSORegion is KIRO_REGION, the fallback region for the refresh endpoint.
	SSORegion string
	// APIRegionOverride is KIRO_API_REGION.
	APIRegionOverride string
	// KiroVersion appears inside the User-Agent header.
	KiroVersion string
	// HTTPClient is used for refresh calls. A pooled client with proxy support
	// is created when nil.
	HTTPClient *http.Client
	// SQLiteLoader enables the kiro-cli database source when non-nil.
	SQLiteLoader SQLiteLoader
	// DisableDiscovery turns off filesystem auto-discovery. Tests use it to stay
	// isolated from the developer's real credentials.
	DisableDiscovery bool
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// Manager owns the credential set and keeps its access token valid.
//
// It is safe for concurrent use. Many goroutines asking for a token while it is
// expired collapse into a single refresh request.
type Manager struct {
	mu       sync.Mutex
	creds    *Credentials
	inflight *refreshCall

	ssoRegion   string
	apiRegion   string
	profileARN  string
	fingerprint string
	kiroVersion string
	httpClient  *http.Client
	now         func() time.Time

	// refreshes counts completed refresh requests. Tests assert on it to prove
	// concurrent callers collapse into one call.
	refreshes int
}

// refreshCall is one in-flight refresh that concurrent callers wait on.
type refreshCall struct {
	done  chan struct{}
	token string
	err   error
}

// New resolves credentials, regions and endpoints.
//
// Credential sources are tried in order: KIRO_CREDS_FILE, REFRESH_TOKEN,
// auto-discovery under ~/.aws/sso/cache, then the kiro-cli SQLite database.
func New(opts Options) (*Manager, error) {
	creds, err := loadCredentials(opts)
	if err != nil {
		return nil, err
	}

	m := &Manager{
		creds:       creds,
		fingerprint: MachineFingerprint(),
		kiroVersion: opts.KiroVersion,
		httpClient:  opts.HTTPClient,
		now:         opts.Now,
	}
	if m.kiroVersion == "" {
		m.kiroVersion = "0.7.45"
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.httpClient == nil {
		m.httpClient = &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        10,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 15 * time.Second,
			},
		}
	}

	// Profile ARN: credentials win, then PROFILE_ARN.
	m.profileARN = creds.ProfileARN
	if m.profileARN == "" {
		m.profileARN = opts.ProfileARN
	}

	// SSO region drives the refresh endpoint.
	m.ssoRegion = creds.Region
	if m.ssoRegion == "" {
		m.ssoRegion = opts.SSORegion
	}
	if m.ssoRegion == "" {
		m.ssoRegion = "us-east-1"
	}

	// API region priority: explicit override, then the profile ARN, then the
	// credential file's region, then the SSO region.
	apiRegionSource := "KIRO_API_REGION"
	m.apiRegion = opts.APIRegionOverride
	if m.apiRegion == "" {
		if r := RegionFromARN(m.profileARN); r != "" {
			m.apiRegion, apiRegionSource = r, "profile ARN"
		}
	}
	if m.apiRegion == "" && creds.Region != "" {
		m.apiRegion, apiRegionSource = creds.Region, "credentials file"
	}
	if m.apiRegion == "" {
		m.apiRegion, apiRegionSource = m.ssoRegion, "SSO region"
	}

	slog.Info("credentials loaded",
		"source", creds.Source.String(),
		"flow", creds.Flow().String(),
		"sso_region", m.ssoRegion,
		"api_region", m.apiRegion,
		"api_region_from", apiRegionSource,
		"runtime_host", m.RuntimeHost(),
		"control_plane_host", m.ControlPlaneHost(),
		"profile_arn_present", m.profileARN != "",
		"token_type_header", orNone(creds.TokenTypeHeader()))

	return m, nil
}

// orNone renders an empty string as "(none)" for logging.
func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// loadCredentials walks the credential sources in priority order.
func loadCredentials(opts Options) (*Credentials, error) {
	var attempted []string

	if opts.CredsFile != "" {
		creds, err := LoadCredentialsFile(opts.CredsFile, SourceFile)
		if err != nil {
			// An explicitly configured file that does not work is a hard error:
			// silently falling through would hide the user's own configuration.
			return nil, fmt.Errorf("KIRO_CREDS_FILE is set but unusable: %w", err)
		}
		return creds, nil
	}
	attempted = append(attempted, "KIRO_CREDS_FILE is not set")

	if opts.RefreshToken != "" {
		return &Credentials{
			RefreshToken: opts.RefreshToken,
			ProfileARN:   opts.ProfileARN,
			Region:       "",
			Source:       SourceEnv,
			extra:        map[string]json.RawMessage{},
		}, nil
	}
	attempted = append(attempted, "REFRESH_TOKEN is not set")

	if !opts.DisableDiscovery {
		path, err := Discover()
		if err == nil {
			creds, loadErr := LoadCredentialsFile(path, SourceDiscovered)
			if loadErr == nil {
				return creds, nil
			}
			attempted = append(attempted, "found "+path+" but could not use it: "+loadErr.Error())
		} else {
			attempted = append(attempted, "auto-discovery: "+err.Error())
		}
	} else {
		attempted = append(attempted, "auto-discovery is disabled")
	}

	if opts.CLIDBFile != "" {
		if opts.SQLiteLoader == nil {
			attempted = append(attempted,
				"KIRO_CLI_DB_FILE is set but this build has no SQLite reader")
		} else if _, statErr := os.Stat(opts.CLIDBFile); statErr != nil {
			attempted = append(attempted, "KIRO_CLI_DB_FILE: "+statErr.Error())
		} else {
			creds, err := opts.SQLiteLoader(opts.CLIDBFile)
			if err == nil {
				return creds, nil
			}
			attempted = append(attempted, "KIRO_CLI_DB_FILE: "+err.Error())
		}
	} else {
		attempted = append(attempted, "KIRO_CLI_DB_FILE is not set")
	}

	return nil, &MissingCredentialsError{Attempted: attempted}
}

// Token returns a valid access token, refreshing it when it is missing, expired
// or within the refresh threshold of expiring.
//
// Concurrent callers share a single refresh request.
func (m *Manager) Token(ctx context.Context) (string, error) {
	m.mu.Lock()
	if m.creds.AccessToken != "" && !m.needsRefreshLocked() {
		token := m.creds.AccessToken
		m.mu.Unlock()
		return token, nil
	}
	call := m.joinOrStartLocked(ctx)
	m.mu.Unlock()

	return waitFor(ctx, call)
}

// ForceRefresh renews the token unconditionally. The HTTP layer calls it after
// an upstream 403, which means the current token is no longer accepted.
//
// If a refresh is already in flight, this joins it: that request is fetching a
// brand-new token from the server, which is exactly what a 403 needs.
func (m *Manager) ForceRefresh(ctx context.Context) (string, error) {
	m.mu.Lock()
	call := m.joinOrStartLocked(ctx)
	m.mu.Unlock()
	return waitFor(ctx, call)
}

// joinOrStartLocked returns the in-flight refresh, starting one if needed.
// The caller must hold m.mu.
func (m *Manager) joinOrStartLocked(ctx context.Context) *refreshCall {
	if m.inflight != nil {
		return m.inflight
	}
	call := &refreshCall{done: make(chan struct{})}
	m.inflight = call

	// The refresh runs detached from the initiating request's context. If that
	// request is cancelled mid-flight, every other waiter would otherwise be
	// left without a token.
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	go func() {
		defer cancel()
		m.runRefresh(refreshCtx, call)
	}()
	return call
}

// waitFor blocks until the refresh finishes or the caller's context is done.
func waitFor(ctx context.Context, call *refreshCall) (string, error) {
	select {
	case <-call.done:
		return call.token, call.err
	case <-ctx.Done():
		return "", fmt.Errorf("gave up waiting for a token refresh: %w", ctx.Err())
	}
}

// runRefresh performs the refresh, updates state and releases the waiters.
func (m *Manager) runRefresh(ctx context.Context, call *refreshCall) {
	defer func() {
		m.mu.Lock()
		if m.inflight == call {
			m.inflight = nil
		}
		m.mu.Unlock()
		close(call.done)
	}()

	m.mu.Lock()
	credsCopy := *m.creds
	m.mu.Unlock()

	var (
		result *refreshResult
		err    error
	)
	switch credsCopy.Flow() {
	case FlowAWSSSOOIDC:
		slog.Info("refreshing access token", "flow", "aws-sso-oidc", "sso_region", m.ssoRegion)
		result, err = m.refreshAWSSSOOIDC(ctx, &credsCopy)
	default:
		slog.Info("refreshing access token", "flow", "kiro-desktop", "sso_region", m.ssoRegion)
		result, err = m.refreshKiroDesktop(ctx, &credsCopy)
	}

	if err != nil {
		// Graceful degradation: a rejected refresh does not matter while the
		// current access token is still valid, which happens when another tool
		// (kiro-cli, Kiro IDE) rotated the refresh token behind our back.
		m.mu.Lock()
		stillValid := m.creds.AccessToken != "" && !m.expiredLocked()
		token := m.creds.AccessToken
		m.mu.Unlock()

		if stillValid {
			slog.Warn("token refresh failed, continuing with the existing access token until it expires",
				"error", err.Error())
			call.token, call.err = token, nil
			return
		}

		var refreshErr *RefreshError
		if errors.As(err, &refreshErr) && refreshErr.IsCredentialRejected() {
			slog.Error("token refresh rejected", "error", err.Error())
		} else {
			slog.Error("token refresh failed", "error", err.Error())
		}
		call.err = err
		return
	}

	m.mu.Lock()
	m.creds.AccessToken = result.AccessToken
	if result.RefreshToken != "" {
		m.creds.RefreshToken = result.RefreshToken
	}
	if result.ProfileARN != "" {
		m.creds.ProfileARN = result.ProfileARN
		if m.profileARN == "" {
			m.profileARN = result.ProfileARN
		}
	}
	m.creds.ExpiresAt = m.now().UTC().Add(time.Duration(result.ExpiresIn)*time.Second - expiryBuffer)
	m.refreshes++
	expiresAt := m.creds.ExpiresAt
	credsToSave := m.creds
	m.mu.Unlock()

	slog.Info("access token refreshed", "expires_at", expiresAt.Format(time.RFC3339))

	if err := credsToSave.Save(); err != nil {
		// A failed write-back costs us the token on the next start, but the
		// in-memory token still works, so this is a warning rather than a failure.
		slog.Warn("could not write the refreshed token back to disk", "error", err.Error())
	}

	call.token = result.AccessToken
}

// needsRefreshLocked reports whether the token should be renewed. The caller
// must hold m.mu.
func (m *Manager) needsRefreshLocked() bool {
	if m.creds.AccessToken == "" {
		return true
	}
	if m.creds.ExpiresAt.IsZero() {
		// Unknown expiry: refresh rather than gamble on a stale token.
		return true
	}
	// Inclusive comparison, matching the reference gateway: a token with exactly
	// the threshold left is already due for renewal.
	return m.creds.ExpiresAt.Sub(m.now().UTC()) <= refreshThreshold
}

// expiredLocked reports whether the token has actually expired, as opposed to
// merely being inside the refresh window. The caller must hold m.mu.
func (m *Manager) expiredLocked() bool {
	if m.creds.ExpiresAt.IsZero() {
		return true
	}
	return !m.now().UTC().Before(m.creds.ExpiresAt)
}

// RefreshCount reports how many refreshes have completed. Tests use it.
func (m *Manager) RefreshCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refreshes
}

// ProfileARN returns the resolved CodeWhisperer profile ARN, which may be empty.
func (m *Manager) ProfileARN() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.profileARN
}

// APIRegion returns the region used for Kiro API calls.
func (m *Manager) APIRegion() string { return m.apiRegion }

// SSORegion returns the region used for token refresh.
func (m *Manager) SSORegion() string { return m.ssoRegion }

// RuntimeHost returns the Kiro runtime base URL for the API region. It serves the
// streaming chat operation.
func (m *Manager) RuntimeHost() string { return RuntimeHost(m.apiRegion) }

// ControlPlaneHost returns the Kiro control plane base URL for the API region. It
// serves the model catalog, which the runtime host does not.
func (m *Manager) ControlPlaneHost() string { return ControlPlaneHost(m.apiRegion) }

// Fingerprint returns the machine fingerprint embedded in the User-Agent.
func (m *Manager) Fingerprint() string { return m.fingerprint }

// KiroVersion returns the Kiro IDE version reported in the User-Agent.
func (m *Manager) KiroVersion() string { return m.kiroVersion }

// Flow reports which refresh protocol is in use.
func (m *Manager) Flow() Flow {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.creds.Flow()
}

// Source reports where the credentials came from.
func (m *Manager) Source() Source {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.creds.Source
}

// TokenTypeHeader returns the TokenType header value for the loaded credentials,
// or an empty string when the auth method needs none.
func (m *Manager) TokenTypeHeader() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.creds.TokenTypeHeader()
}

// ExpiresAt reports the current token expiry, zero when unknown.
func (m *Manager) ExpiresAt() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.creds.ExpiresAt
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
