// Package api exposes the OpenAI- and Anthropic-compatible HTTP surfaces.
package api

import (
	"net/http"
	"time"

	"kirogo/internal/catalog"
	"kirogo/internal/config"
	"kirogo/internal/kiro"
)

// ProfileProvider supplies the CodeWhisperer profile ARN to send upstream.
type ProfileProvider interface {
	// ProfileARN returns the profile ARN, which may be empty.
	ProfileARN() string
}

// Server owns the HTTP routing table and the collaborators the handlers need.
type Server struct {
	cfg     *config.Config
	catalog *catalog.Catalog
	kiro    *kiro.Client
	auth    ProfileProvider
	mux     *http.ServeMux
}

// Deps are the collaborators a Server needs.
type Deps struct {
	// Config is the resolved configuration.
	Config *config.Config
	// Catalog supplies model metadata and name resolution.
	Catalog *catalog.Catalog
	// Kiro talks to the backend.
	Kiro *kiro.Client
	// Auth supplies the profile ARN.
	Auth ProfileProvider
}

// NewServer builds a Server.
func NewServer(deps Deps) *Server {
	s := &Server{
		cfg:     deps.Config,
		catalog: deps.Catalog,
		kiro:    deps.Kiro,
		auth:    deps.Auth,
		mux:     http.NewServeMux(),
	}
	s.routes()
	return s
}

// routes registers every endpoint. Health endpoints are unauthenticated so that
// container orchestrators can probe them without holding the API key.
func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleRoot)
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/v1/models", s.requireKey(flavorOpenAI, s.handleListModels))
	s.mux.HandleFunc("/v1/chat/completions", s.requireKey(flavorOpenAI, s.handleChatCompletions))
	s.mux.HandleFunc("/v1/messages", s.requireKey(flavorAnthropic, s.handleMessages))
	s.mux.HandleFunc("/v1/messages/count_tokens", s.requireKey(flavorAnthropic, s.handleCountTokens))
}

// Handler returns the fully wrapped HTTP handler.
func (s *Server) Handler() http.Handler {
	return withRecover(withCORS(s.mux))
}

// rootResponse is the body of GET /, kept byte-compatible with the Python
// reference gateway so existing health checks keep working.
type rootResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Version string `json:"version"`
}

// healthResponse is the body of GET /health.
type healthResponse struct {
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
	Version   string `json:"version"`
}

// handleRoot serves the liveness endpoint. Unknown paths land here too, so it
// distinguishes them and returns 404 with a helpful hint.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, flavorOpenAI, http.StatusNotFound,
			"No such endpoint: "+r.URL.Path+". kirogo serves /v1/models, /v1/chat/completions, /v1/messages and /v1/messages/count_tokens.")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, flavorOpenAI, http.StatusMethodNotAllowed, "Use GET for "+r.URL.Path+".")
		return
	}
	writeJSON(w, http.StatusOK, rootResponse{
		Status:  "ok",
		Message: "kirogo is running",
		Version: config.Version,
	})
}

// handleHealth serves the detailed health endpoint.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, flavorOpenAI, http.StatusMethodNotAllowed, "Use GET for /health.")
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{
		Status:    "healthy",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Version:   config.Version,
	})
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
