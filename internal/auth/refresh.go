package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Endpoint templates. runtime.{region}.kiro.dev is universal across regions;
// codewhisperer.{region}.amazonaws.com does not exist outside us-east-1 and must
// never be used.
const (
	kiroDesktopRefreshURL = "https://prod.%s.auth.desktop.kiro.dev/refreshToken"
	awsSSOOIDCTokenURL    = "https://oidc.%s.amazonaws.com/token"
	// kiroRuntimeHost serves the streaming GenerateAssistantResponse operation.
	kiroRuntimeHost = "https://runtime.%s.kiro.dev"
	// kiroControlPlaneHost serves the control plane, including
	// ListAvailableModels. It is a separate service from the runtime host, which
	// answers 404 for control plane paths.
	kiroControlPlaneHost = "https://management.%s.kiro.dev"
)

// expiryBuffer is subtracted from the reported lifetime so the token is
// considered expired slightly before the server thinks so.
const expiryBuffer = 60 * time.Second

// refreshThreshold is how much remaining lifetime triggers a proactive refresh.
const refreshThreshold = 600 * time.Second

// defaultExpiresIn is used when the server omits expiresIn.
const defaultExpiresIn = 3600

// RuntimeHost returns the Kiro runtime host for a region.
func RuntimeHost(region string) string {
	return fmt.Sprintf(kiroRuntimeHost, region)
}

// ControlPlaneHost returns the Kiro control plane host for a region.
func ControlPlaneHost(region string) string {
	return fmt.Sprintf(kiroControlPlaneHost, region)
}

// KiroDesktopRefreshURL returns the Kiro Desktop refresh endpoint for a region.
func KiroDesktopRefreshURL(region string) string {
	return fmt.Sprintf(kiroDesktopRefreshURL, region)
}

// AWSSSOOIDCTokenURL returns the AWS SSO OIDC token endpoint for a region.
func AWSSSOOIDCTokenURL(region string) string {
	return fmt.Sprintf(awsSSOOIDCTokenURL, region)
}

// refreshResult is the normalised outcome of either refresh flow.
type refreshResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
	ProfileARN   string
}

// refreshResponseJSON covers both flows: they use the same camelCase keys.
type refreshResponseJSON struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int    `json:"expiresIn"`
	ProfileARN   string `json:"profileArn"`
}

// RefreshError describes a failed token refresh with an actionable message.
type RefreshError struct {
	// StatusCode is the HTTP status, or 0 for a transport failure.
	StatusCode int
	// Flow is the refresh protocol that failed.
	Flow Flow
	// Message is the user-facing explanation.
	Message string
	// Err is the wrapped cause, if any.
	Err error
}

// Error renders the message.
func (e *RefreshError) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

// Unwrap exposes the cause.
func (e *RefreshError) Unwrap() error { return e.Err }

// IsCredentialRejected reports whether the server refused the refresh token
// itself, which means the user has to sign in again.
func (e *RefreshError) IsCredentialRejected() bool {
	return e.StatusCode == http.StatusBadRequest ||
		e.StatusCode == http.StatusUnauthorized ||
		e.StatusCode == http.StatusForbidden
}

// refreshKiroDesktop refreshes via the Kiro Desktop auth endpoint.
func (m *Manager) refreshKiroDesktop(ctx context.Context, creds *Credentials) (*refreshResult, error) {
	if creds.RefreshToken == "" {
		return nil, &RefreshError{
			Flow:    FlowKiroDesktop,
			Message: "no refresh token is available, so the access token cannot be renewed. Sign in to Kiro IDE again, or set REFRESH_TOKEN.",
		}
	}

	body, err := json.Marshal(map[string]string{"refreshToken": creds.RefreshToken})
	if err != nil {
		return nil, &RefreshError{Flow: FlowKiroDesktop, Message: "could not encode the refresh request", Err: err}
	}

	url := KiroDesktopRefreshURL(m.ssoRegion)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, &RefreshError{Flow: FlowKiroDesktop, Message: "could not build the refresh request", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "KiroIDE-"+m.kiroVersion+"-"+m.fingerprint)

	return m.doRefreshRequest(req, FlowKiroDesktop, url)
}

// refreshAWSSSOOIDC refreshes via the AWS SSO OIDC CreateToken API.
//
// The API takes a JSON body with camelCase keys, not a form-encoded one, and no
// scope parameter.
func (m *Manager) refreshAWSSSOOIDC(ctx context.Context, creds *Credentials) (*refreshResult, error) {
	switch {
	case creds.RefreshToken == "":
		return nil, &RefreshError{Flow: FlowAWSSSOOIDC, Message: "no refresh token is available. Run your SSO login again."}
	case creds.ClientID == "":
		return nil, &RefreshError{Flow: FlowAWSSSOOIDC, Message: "AWS SSO OIDC needs a clientId, which is missing from your credentials."}
	case creds.ClientSecret == "":
		return nil, &RefreshError{Flow: FlowAWSSSOOIDC, Message: "AWS SSO OIDC needs a clientSecret, which is missing from your credentials."}
	}

	body, err := json.Marshal(map[string]string{
		"grantType":    "refresh_token",
		"clientId":     creds.ClientID,
		"clientSecret": creds.ClientSecret,
		"refreshToken": creds.RefreshToken,
	})
	if err != nil {
		return nil, &RefreshError{Flow: FlowAWSSSOOIDC, Message: "could not encode the refresh request", Err: err}
	}

	url := AWSSSOOIDCTokenURL(m.ssoRegion)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, &RefreshError{Flow: FlowAWSSSOOIDC, Message: "could not build the refresh request", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")

	return m.doRefreshRequest(req, FlowAWSSSOOIDC, url)
}

// maxRefreshBodyBytes caps how much of an error body is read, so a misbehaving
// endpoint cannot exhaust memory.
const maxRefreshBodyBytes = 1 << 20

// doRefreshRequest sends a refresh request and normalises the response.
func (m *Manager) doRefreshRequest(req *http.Request, flow Flow, url string) (*refreshResult, error) {
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, &RefreshError{
			Flow:    flow,
			Message: "could not reach the token refresh endpoint " + url + ". Check your network connection, and HTTPS_PROXY if you are behind a proxy",
			Err:     err,
		}
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRefreshBodyBytes))
	if err != nil {
		return nil, &RefreshError{StatusCode: resp.StatusCode, Flow: flow, Message: "could not read the refresh response", Err: err}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &RefreshError{
			StatusCode: resp.StatusCode,
			Flow:       flow,
			Message:    refreshFailureMessage(resp.StatusCode, flow, data),
		}
	}

	var parsed refreshResponseJSON
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, &RefreshError{
			StatusCode: resp.StatusCode,
			Flow:       flow,
			Message:    "the refresh endpoint returned something that is not JSON",
			Err:        err,
		}
	}
	if parsed.AccessToken == "" {
		return nil, &RefreshError{
			StatusCode: resp.StatusCode,
			Flow:       flow,
			Message:    "the refresh endpoint returned no accessToken. Sign in again to get fresh credentials.",
		}
	}

	expiresIn := parsed.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = defaultExpiresIn
	}

	return &refreshResult{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		ExpiresIn:    expiresIn,
		ProfileARN:   parsed.ProfileARN,
	}, nil
}

// refreshFailureMessage turns an HTTP failure into advice the user can act on.
// The response body is summarised, never echoed wholesale, because it can
// contain credential material.
func refreshFailureMessage(status int, flow Flow, body []byte) string {
	detail := summariseErrorBody(body)

	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized:
		who := "Kiro IDE"
		if flow == FlowAWSSSOOIDC {
			who = "your AWS SSO session"
		}
		msg := fmt.Sprintf("the refresh token was rejected (HTTP %d). Sign in to %s again so a new token is written.", status, who)
		if detail != "" {
			msg += " Server said: " + detail
		}
		return msg
	case http.StatusForbidden:
		return fmt.Sprintf("the refresh endpoint denied the request (HTTP 403). Your Kiro account may no longer have access. %s", detail)
	case http.StatusTooManyRequests:
		return "the refresh endpoint is rate limiting kirogo (HTTP 429). Wait a moment and try again."
	default:
		if status >= 500 {
			return fmt.Sprintf("the refresh endpoint returned HTTP %d. This is an upstream outage; retry shortly. %s", status, detail)
		}
		return fmt.Sprintf("the refresh endpoint returned HTTP %d. %s", status, detail)
	}
}

// errorBodyJSON covers the error shapes both endpoints use.
type errorBodyJSON struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	Message          string `json:"message"`
	Reason           string `json:"reason"`
}

// summariseErrorBody extracts a short, non-sensitive description from an error
// body. Only known descriptive fields are used, so tokens echoed back by a
// server cannot leak into logs.
func summariseErrorBody(body []byte) string {
	var parsed errorBodyJSON
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	var parts []string
	for _, s := range []string{parsed.Error, parsed.ErrorDescription, parsed.Message, parsed.Reason} {
		if s = strings.TrimSpace(s); s != "" && !containsString(parts, s) {
			parts = append(parts, s)
		}
	}
	joined := strings.Join(parts, ": ")
	if len(joined) > 300 {
		joined = joined[:300] + "..."
	}
	return joined
}

// containsString reports whether list already holds s.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
