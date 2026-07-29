package api

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
)

// apiFlavor selects the error envelope shape used for a route.
type apiFlavor int

const (
	flavorOpenAI apiFlavor = iota
	flavorAnthropic
)

// writeJSON writes v as compact JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		slog.Debug("failed to write JSON response", "error", err)
	}
}

// openAIError is the OpenAI-compatible error envelope.
type openAIError struct {
	Error openAIErrorBody `json:"error"`
}

type openAIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

// anthropicError is the Anthropic-compatible error envelope.
type anthropicError struct {
	Type  string             `json:"type"`
	Error anthropicErrorBody `json:"error"`
}

type anthropicErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// errorTypeFor maps an HTTP status to the error type string each API uses.
func errorTypeFor(flavor apiFlavor, status int) string {
	if flavor == flavorAnthropic {
		switch status {
		case http.StatusBadRequest:
			return "invalid_request_error"
		case http.StatusUnauthorized:
			return "authentication_error"
		case http.StatusForbidden:
			return "permission_error"
		case http.StatusNotFound:
			return "not_found_error"
		case http.StatusRequestEntityTooLarge:
			return "request_too_large"
		case http.StatusTooManyRequests:
			return "rate_limit_error"
		case http.StatusGatewayTimeout, http.StatusBadGateway, http.StatusServiceUnavailable:
			return "api_error"
		default:
			return "api_error"
		}
	}
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "invalid_request_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

// writeError emits an error body in the shape the given API flavor expects.
func writeError(w http.ResponseWriter, flavor apiFlavor, status int, message string) {
	kind := errorTypeFor(flavor, status)
	if flavor == flavorAnthropic {
		writeJSON(w, status, anthropicError{
			Type:  "error",
			Error: anthropicErrorBody{Type: kind, Message: message},
		})
		return
	}
	writeJSON(w, status, openAIError{
		Error: openAIErrorBody{Message: message, Type: kind},
	})
}

// withCORS allows any origin, method and header, and answers preflight
// requests. Browser-based clients such as web IDE front ends need this.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
			h.Set("Access-Control-Allow-Headers", req)
		} else {
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-api-key, anthropic-version, anthropic-beta")
		}
		h.Set("Access-Control-Max-Age", "86400")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withRecover converts a panic in a handler into a 500 instead of tearing down
// the process. Handlers are not expected to panic; this is a backstop.
func withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("recovered from panic in handler",
					"path", r.URL.Path,
					"panic", rec,
					"stack", string(debug.Stack()))
				writeError(w, flavorOpenAI, http.StatusInternalServerError,
					"kirogo hit an internal error while handling this request. Run with LOG_LEVEL=DEBUG and check the log for a stack trace.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// constantTimeEqual compares two secrets without leaking their contents through
// timing. Lengths are compared first because subtle.ConstantTimeCompare returns
// 0 for unequal lengths anyway.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// bearerToken extracts the credential from an Authorization header.
func bearerToken(h string) string {
	if h == "" {
		return ""
	}
	const prefix = "bearer "
	if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	// Some clients send the raw key with no scheme.
	return strings.TrimSpace(h)
}

// requireKey wraps a handler with client authentication.
//
// OpenAI routes read Authorization: Bearer <key>. Anthropic routes read
// x-api-key and fall back to Authorization: Bearer, because several Anthropic
// clients send the key in whichever header they were configured with.
func (s *Server) requireKey(flavor apiFlavor, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var presented string
		var where string

		if flavor == flavorAnthropic {
			if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
				presented, where = v, "x-api-key"
			} else if v := bearerToken(r.Header.Get("Authorization")); v != "" {
				presented, where = v, "Authorization"
			}
		} else if v := bearerToken(r.Header.Get("Authorization")); v != "" {
			presented, where = v, "Authorization"
		} else if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
			presented, where = v, "x-api-key"
		}

		if presented == "" {
			header := "Authorization: Bearer <PROXY_API_KEY>"
			if flavor == flavorAnthropic {
				header = "x-api-key: <PROXY_API_KEY>"
			}
			writeError(w, flavor, http.StatusUnauthorized,
				"Missing API key. Send your PROXY_API_KEY as \""+header+"\".")
			return
		}

		if !constantTimeEqual(presented, s.cfg.ProxyAPIKey) {
			slog.Warn("rejected request with wrong API key", "path", r.URL.Path, "header", where)
			writeError(w, flavor, http.StatusUnauthorized,
				"Invalid API key. It must match the PROXY_API_KEY that kirogo was started with.")
			return
		}

		next(w, r)
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
