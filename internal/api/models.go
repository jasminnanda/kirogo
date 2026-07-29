package api

import (
	"net/http"

	"kirogo/internal/catalog"
)

// modelListResponse is the OpenAI-compatible /v1/models body.
type modelListResponse struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

// modelEntry is one advertised model.
//
// The first five fields are the OpenAI shape. The rest are additive: strict
// clients ignore unknown fields, and they let a client that does look show the
// context window, the credit cost and the reasoning effort levels without a
// second request.
type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`

	Description string `json:"description,omitempty"`

	ContextLength   int `json:"context_length,omitempty"`
	MaxInputTokens  int `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`

	RateMultiplier float64 `json:"rate_multiplier,omitempty"`
	RateUnit       string  `json:"rate_unit,omitempty"`

	SupportedEffortLevels []string `json:"supported_effort_levels,omitempty"`
	DefaultEffortLevel    string   `json:"default_effort_level,omitempty"`
	// EffortLevel is set only on a pinned "model:level" variant.
	EffortLevel string `json:"effort_level,omitempty"`
}

// modelsCreatedAt is a fixed timestamp for the created field. The backend does
// not report model creation times, and inventing a moving value would make
// responses non-deterministic for no benefit.
const modelsCreatedAt int64 = 1690000000

// handleListModels serves GET /v1/models.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, flavorOpenAI, http.StatusMethodNotAllowed, "Use GET for /v1/models.")
		return
	}

	// Refresh on the TTL. A failure here leaves the cached list in place.
	s.catalog.EnsureFresh(r.Context())

	entries := s.catalog.Listing(s.cfg.ExposeEffortVariants)
	out := modelListResponse{Object: "list", Data: make([]modelEntry, 0, len(entries))}
	for _, e := range entries {
		out.Data = append(out.Data, buildModelEntry(e))
	}
	writeJSON(w, http.StatusOK, out)
}

// buildModelEntry converts a catalog entry into the response shape.
func buildModelEntry(e catalog.Entry) modelEntry {
	m := e.Model
	entry := modelEntry{
		ID:                    e.ID,
		Object:                "model",
		Created:               modelsCreatedAt,
		OwnedBy:               m.OwnedBy(),
		Description:           m.Description,
		ContextLength:         m.MaxInputTokens,
		MaxInputTokens:        m.MaxInputTokens,
		MaxOutputTokens:       m.MaxOutputTokens,
		RateMultiplier:        m.RateMultiplier,
		RateUnit:              m.RateUnit,
		SupportedEffortLevels: m.EffortLevels(),
		DefaultEffortLevel:    m.DefaultEffortLevel(),
		EffortLevel:           e.PinnedEffort,
	}
	return entry
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
