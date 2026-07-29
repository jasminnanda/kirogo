package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jasminnanda/kirogo/internal/catalog"
	"github.com/jasminnanda/kirogo/internal/config"
	"github.com/jasminnanda/kirogo/internal/kiro"
)

// testServer builds a Server with a known API key and an empty catalog.
func testServer(t *testing.T) *Server {
	t.Helper()
	return NewServer(Deps{
		Config:  &config.Config{ProxyAPIKey: "test-key", ExposeEffortVariants: true},
		Catalog: catalog.New(catalog.Options{Fetcher: emptyFetcher{}}),
	})
}

// emptyFetcher stands in for the backend when a test does not exercise the
// catalog.
type emptyFetcher struct{}

func (emptyFetcher) ListAvailableModels(context.Context, string) (*kiro.ListModelsResponse, error) {
	return &kiro.ListModelsResponse{}, nil
}

func TestRootResponseIsByteCompatible(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	want := `{"status":"ok","message":"kirogo is running","version":"` + config.Version + `"}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Errorf("body =\n  %s\nwant\n  %s", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestHealthResponseShapeAndTimestamp(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Field order must match the reference gateway byte for byte.
	body := strings.TrimSpace(rec.Body.String())
	if !strings.HasPrefix(body, `{"status":"healthy","timestamp":"`) {
		t.Errorf("body does not start with the expected field order: %s", body)
	}
	if !strings.HasSuffix(body, `","version":"`+config.Version+`"}`) {
		t.Errorf("body does not end with the expected version field: %s", body)
	}

	var parsed healthResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("health body is not valid JSON: %v", err)
	}
	if parsed.Status != "healthy" {
		t.Errorf("status = %q, want healthy", parsed.Status)
	}
	if _, err := time.Parse(time.RFC3339, parsed.Timestamp); err != nil {
		t.Errorf("timestamp %q is not RFC3339: %v", parsed.Timestamp, err)
	}
}

func TestHealthEndpointsAreUnauthenticated(t *testing.T) {
	s := testServer(t)
	for _, path := range []string{"/", "/health"} {
		rec := httptest.NewRecorder()
		// No Authorization header at all.
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s without a key returned %d, want 200", path, rec.Code)
		}
	}
}

func TestHealthIgnoresWrongKey(t *testing.T) {
	s := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer completely-wrong")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("health with a wrong key returned %d, want 200 (health is unauthenticated)", rec.Code)
	}
}

func TestUnknownPathReturns404WithHint(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if !strings.Contains(body.Error.Message, "/v1/chat/completions") {
		t.Errorf("404 message should list real endpoints, got %q", body.Error.Message)
	}
}

func TestWrongMethodOnHealthEndpoints(t *testing.T) {
	s := testServer(t)
	for _, path := range []string{"/", "/health"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s returned %d, want 405", path, rec.Code)
		}
	}
}

func TestCORSPreflight(t *testing.T) {
	s := testServer(t)
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://example.test")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q, want *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "authorization,content-type" {
		t.Errorf("Allow-Headers = %q, want the requested headers echoed back", got)
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Errorf("Allow-Methods = %q, should include POST", rec.Header().Get("Access-Control-Allow-Methods"))
	}
}

func TestCORSHeadersOnNormalResponses(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q, want * on ordinary responses too", got)
	}
}

func TestCORSDefaultAllowHeadersWhenNoneRequested(t *testing.T) {
	s := testServer(t)
	req := httptest.NewRequest(http.MethodOptions, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	got := rec.Header().Get("Access-Control-Allow-Headers")
	for _, want := range []string{"Authorization", "x-api-key", "anthropic-version"} {
		if !strings.Contains(got, want) {
			t.Errorf("default Allow-Headers %q should include %q", got, want)
		}
	}
}

func TestRequireKeyAcceptsCorrectCredential(t *testing.T) {
	s := testServer(t)
	handler := s.requireKey(flavorOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	cases := []struct {
		name   string
		header string
		value  string
	}{
		{"bearer scheme", "Authorization", "Bearer test-key"},
		{"lowercase scheme", "Authorization", "bearer test-key"},
		{"mixed case scheme", "Authorization", "BeArEr test-key"},
		{"raw key with no scheme", "Authorization", "test-key"},
		{"x-api-key fallback", "x-api-key", "test-key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			req.Header.Set(tc.header, tc.value)
			rec := httptest.NewRecorder()
			handler(rec, req)
			if rec.Code != http.StatusTeapot {
				t.Errorf("status = %d, want 418 (handler reached); body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRequireKeyRejects(t *testing.T) {
	s := testServer(t)
	handler := s.requireKey(flavorOpenAI, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be reached")
	})

	cases := []struct {
		name        string
		setHeader   func(*http.Request)
		wantMessage string
	}{
		{
			name:        "no header",
			setHeader:   func(*http.Request) {},
			wantMessage: "Missing API key",
		},
		{
			name:        "empty bearer",
			setHeader:   func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") },
			wantMessage: "Missing API key",
		},
		{
			name:        "wrong key",
			setHeader:   func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong-key") },
			wantMessage: "Invalid API key",
		},
		{
			name:        "key is a prefix of the real one",
			setHeader:   func(r *http.Request) { r.Header.Set("Authorization", "Bearer test-ke") },
			wantMessage: "Invalid API key",
		},
		{
			name:        "key has trailing junk",
			setHeader:   func(r *http.Request) { r.Header.Set("Authorization", "Bearer test-keyy") },
			wantMessage: "Invalid API key",
		},
		{
			name:        "case differs",
			setHeader:   func(r *http.Request) { r.Header.Set("Authorization", "Bearer TEST-KEY") },
			wantMessage: "Invalid API key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			tc.setHeader(req)
			rec := httptest.NewRecorder()
			handler(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			var body openAIError
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("not JSON: %v", err)
			}
			if !strings.Contains(body.Error.Message, tc.wantMessage) {
				t.Errorf("message %q should contain %q", body.Error.Message, tc.wantMessage)
			}
			if body.Error.Type != "authentication_error" {
				t.Errorf("type = %q, want authentication_error", body.Error.Type)
			}
		})
	}
}

func TestRequireKeyAnthropicFlavor(t *testing.T) {
	s := testServer(t)
	handler := s.requireKey(flavorAnthropic, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	t.Run("accepts x-api-key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("x-api-key", "test-key")
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusTeapot {
			t.Errorf("status = %d, want 418", rec.Code)
		}
	})

	t.Run("accepts Authorization fallback", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusTeapot {
			t.Errorf("status = %d, want 418", rec.Code)
		}
	})

	t.Run("rejects wrong x-api-key with Anthropic envelope", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("x-api-key", "nope")
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		var body anthropicError
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		if body.Type != "error" || body.Error.Type != "authentication_error" {
			t.Errorf("envelope = %+v, want type=error error.type=authentication_error", body)
		}
	})

	t.Run("missing key mentions x-api-key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)
		var body anthropicError
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		if !strings.Contains(body.Error.Message, "x-api-key") {
			t.Errorf("message %q should mention x-api-key", body.Error.Message)
		}
	})

	t.Run("x-api-key takes precedence over Authorization", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("x-api-key", "wrong")
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401: x-api-key must win when both are present", rec.Code)
		}
	})
}

func TestRecoveryMiddlewareConvertsPanicTo500(t *testing.T) {
	panicking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	rec := httptest.NewRecorder()
	withRecover(panicking).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if !strings.Contains(body.Error.Message, "LOG_LEVEL=DEBUG") {
		t.Errorf("panic message should tell the user how to get detail, got %q", body.Error.Message)
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"":                  "",
		"Bearer abc":        "abc",
		"bearer abc":        "abc",
		"BEARER   abc  ":    "abc",
		"abc":               "abc",
		"Bearer ":           "",
		"Token abc":         "Token abc",
		"Bearer a b":        "a b",
		"  Bearer   abc   ": "Bearer   abc",
		"bearerabc":         "bearerabc",
	}
	for in, want := range cases {
		if got := bearerToken(in); got != want {
			t.Errorf("bearerToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConstantTimeEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"abc", "abc", true},
		{"abc", "abd", false},
		{"abc", "ab", false},
		{"", "", true},
		{"", "a", false},
	}
	for _, c := range cases {
		if got := constantTimeEqual(c.a, c.b); got != c.want {
			t.Errorf("constantTimeEqual(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestErrorTypeFor(t *testing.T) {
	cases := []struct {
		flavor apiFlavor
		status int
		want   string
	}{
		{flavorOpenAI, http.StatusBadRequest, "invalid_request_error"},
		{flavorOpenAI, http.StatusUnauthorized, "authentication_error"},
		{flavorOpenAI, http.StatusTooManyRequests, "rate_limit_error"},
		{flavorOpenAI, http.StatusInternalServerError, "api_error"},
		{flavorAnthropic, http.StatusBadRequest, "invalid_request_error"},
		{flavorAnthropic, http.StatusUnauthorized, "authentication_error"},
		{flavorAnthropic, http.StatusRequestEntityTooLarge, "request_too_large"},
		{flavorAnthropic, http.StatusGatewayTimeout, "api_error"},
	}
	for _, c := range cases {
		if got := errorTypeFor(c.flavor, c.status); got != c.want {
			t.Errorf("errorTypeFor(%v, %d) = %q, want %q", c.flavor, c.status, got, c.want)
		}
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
