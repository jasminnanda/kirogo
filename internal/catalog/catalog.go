package catalog

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jasminnanda/kirogo/internal/kiro"
)

// DefaultMaxInputTokens is assumed when the backend omits tokenLimits.
const DefaultMaxInputTokens = 200000

// maxPages caps pagination, matching the Kiro IDE's own cap.
const maxPages = 10

// Aliases maps a client-facing name to a real model id.
//
// Cursor ships its own model called "auto", which collides with Kiro's automatic
// selector. Exposing Kiro's as "auto-kiro" is the only way both can coexist.
var Aliases = map[string]string{
	"auto-kiro": AutoModelID,
}

// HiddenFromList are model ids that still work when requested directly but are
// not advertised, because an alias stands in for them.
var HiddenFromList = map[string]bool{
	AutoModelID: true,
}

// Model is one entry in the catalog, normalised from the backend's schema.
type Model struct {
	// ID is the model id to send upstream.
	ID string
	// Name is the human-readable label.
	Name string
	// Description is the backend's description.
	Description string
	// Provider is the backend's modelProvider field, which is often empty.
	Provider string
	// RateMultiplier is how many rate units one request costs.
	RateMultiplier float64
	// RateUnit is the unit name, normally "credit".
	RateUnit string
	// MaxInputTokens is the context window.
	MaxInputTokens int
	// MaxOutputTokens is the response ceiling, zero when unreported.
	MaxOutputTokens int

	// effort is the reasoning effort capability, nil when unsupported.
	effort *EffortSupport
}

// Effort returns the model's reasoning effort capability, which may be nil.
func (m *Model) Effort() *EffortSupport {
	if m == nil {
		return nil
	}
	return m.effort
}

// EffortLevels returns the advertised effort levels, empty when unsupported.
func (m *Model) EffortLevels() []string {
	if !m.Effort().Supported() {
		return nil
	}
	return m.effort.Levels
}

// DefaultEffortLevel returns the model's own default effort level.
func (m *Model) DefaultEffortLevel() string {
	if !m.Effort().Supported() {
		return ""
	}
	return m.effort.DefaultLevel
}

// OwnedBy infers an OpenAI-style owner from the model id prefix. The backend does
// not report one, and clients display this field.
func (m *Model) OwnedBy() string {
	switch {
	case strings.HasPrefix(m.ID, "claude-"):
		return "anthropic"
	case strings.HasPrefix(m.ID, "gpt-"):
		return "openai"
	default:
		return "kiro"
	}
}

// Fetcher retrieves catalog pages. It is an interface so the catalog can be
// tested without a backend.
type Fetcher interface {
	ListAvailableModels(ctx context.Context, nextToken string) (*kiro.ListModelsResponse, error)
}

// Catalog holds the model list and refreshes it on a TTL.
//
// It is safe for concurrent use. A refresh that fails leaves the previous list in
// place, so a transient backend outage does not take the proxy's model list down
// with it.
type Catalog struct {
	fetcher Fetcher
	ttl     time.Duration
	now     func() time.Time

	mu           sync.RWMutex
	models       []Model
	byID         map[string]*Model
	defaultModel string
	fetchedAt    time.Time

	// refreshMu serialises refreshes so a burst of requests past the TTL causes
	// one fetch, not one per request.
	refreshMu sync.Mutex
}

// Options configures a Catalog.
type Options struct {
	// Fetcher retrieves catalog pages.
	Fetcher Fetcher
	// TTL is how long a fetched catalog stays fresh.
	TTL time.Duration
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// New builds an empty Catalog. Call Refresh before serving requests.
func New(opts Options) *Catalog {
	c := &Catalog{
		fetcher: opts.Fetcher,
		ttl:     opts.TTL,
		now:     opts.Now,
		byID:    map[string]*Model{},
	}
	if c.ttl <= 0 {
		c.ttl = time.Hour
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// Refresh fetches the whole catalog, following pagination.
//
// On success the new list replaces the old one atomically. On failure the
// previous list is kept and the error is returned, so the caller can decide
// whether a stale catalog is acceptable.
func (c *Catalog) Refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	var (
		collected    []Model
		defaultModel string
		nextToken    string
	)

	for page := 0; page < maxPages; page++ {
		resp, err := c.fetcher.ListAvailableModels(ctx, nextToken)
		if err != nil {
			return fmt.Errorf("could not fetch the model catalog: %w", err)
		}

		for _, spec := range resp.Models {
			// The backend can return placeholder entries with no id.
			if strings.TrimSpace(spec.ModelID) == "" {
				slog.Debug("skipping a catalog entry with no modelId")
				continue
			}
			collected = append(collected, modelFromSpec(spec))
		}
		if defaultModel == "" && resp.DefaultModel != nil {
			defaultModel = resp.DefaultModel.ModelID
		}

		nextToken = resp.NextToken
		if nextToken == "" {
			break
		}
		if page == maxPages-1 {
			slog.Warn("model catalog pagination cap reached, ignoring further pages",
				"pages", maxPages)
		}
	}

	if len(collected) == 0 {
		return fmt.Errorf("the model catalog came back empty. Your Kiro account may have no models enabled")
	}

	byID := make(map[string]*Model, len(collected))
	for i := range collected {
		byID[collected[i].ID] = &collected[i]
	}

	c.mu.Lock()
	c.models = collected
	c.byID = byID
	c.defaultModel = defaultModel
	c.fetchedAt = c.now()
	c.mu.Unlock()

	slog.Info("model catalog loaded", "models", len(collected), "default_model", orNone(defaultModel))
	return nil
}

// orNone renders an empty string as "(none)" for logging.
func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// modelFromSpec normalises a backend entry.
func modelFromSpec(spec kiro.ModelSpec) Model {
	m := Model{
		ID:             spec.ModelID,
		Name:           spec.ModelName,
		Description:    spec.Description,
		Provider:       spec.ModelProvider,
		RateMultiplier: spec.RateMultiplier,
		RateUnit:       spec.RateUnit,
		MaxInputTokens: DefaultMaxInputTokens,
		effort:         extractEffortSupport(spec.AdditionalModelRequestFieldsSchema),
	}
	if spec.TokenLimits != nil {
		if spec.TokenLimits.MaxInputTokens > 0 {
			m.MaxInputTokens = spec.TokenLimits.MaxInputTokens
		}
		m.MaxOutputTokens = spec.TokenLimits.MaxOutputTokens
	}
	if m.Name == "" {
		m.Name = m.ID
	}
	return m
}

// EnsureFresh refreshes the catalog when the TTL has elapsed.
//
// A failed refresh is logged and swallowed: continuing to serve the previous
// catalog is better than failing requests over a stale model list.
func (c *Catalog) EnsureFresh(ctx context.Context) {
	c.mu.RLock()
	age := c.now().Sub(c.fetchedAt)
	loaded := !c.fetchedAt.IsZero()
	c.mu.RUnlock()

	if loaded && age < c.ttl {
		return
	}
	if err := c.Refresh(ctx); err != nil {
		slog.Warn("model catalog refresh failed, continuing with the cached list",
			"age", age.Truncate(time.Second), "error", err.Error())
	}
}

// Models returns a copy of the catalog.
func (c *Catalog) Models() []Model {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Model, len(c.models))
	copy(out, c.models)
	return out
}

// Lookup returns the model with exactly this id.
func (c *Catalog) Lookup(id string) (*Model, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.byID[id]
	if !ok {
		return nil, false
	}
	// Return a copy so callers cannot mutate the cache.
	clone := *m
	return &clone, true
}

// IDs returns every model id, sorted.
func (c *Catalog) IDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.models))
	for _, m := range c.models {
		out = append(out, m.ID)
	}
	sort.Strings(out)
	return out
}

// DefaultModel returns the backend's default model id, possibly empty.
func (c *Catalog) DefaultModel() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.defaultModel
}

// Len returns how many models are cached.
func (c *Catalog) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.models)
}

// FetchedAt reports when the catalog was last loaded successfully.
func (c *Catalog) FetchedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fetchedAt
}

// Entry is one advertised model in the /v1/models listing.
type Entry struct {
	// ID is what the client should send as "model".
	ID string
	// Model is the underlying catalog entry.
	Model Model
	// PinnedEffort is non-empty for a "model:level" variant.
	PinnedEffort string
}

// Listing returns the models to advertise.
//
// Aliases replace the ids they stand in for. When variants are enabled, each
// model that supports reasoning effort also gets one "model:level" entry per
// advertised level, because clients such as Cursor, Cline and Continue send only
// a model name and have no other way to pick an effort.
func (c *Catalog) Listing(exposeEffortVariants bool) []Entry {
	models := c.Models()

	// Reverse the alias map so a hidden id can be advertised under its alias.
	aliasFor := make(map[string]string, len(Aliases))
	for alias, target := range Aliases {
		aliasFor[target] = alias
	}

	var entries []Entry
	for _, m := range models {
		advertisedID := m.ID
		if alias, ok := aliasFor[m.ID]; ok {
			advertisedID = alias
		} else if HiddenFromList[m.ID] {
			continue
		}

		entries = append(entries, Entry{ID: advertisedID, Model: m})

		if !exposeEffortVariants || !m.Effort().Supported() || m.ID == AutoModelID {
			continue
		}
		for _, level := range m.EffortLevels() {
			if !IsValidEffortLevel(level) {
				continue
			}
			entries = append(entries, Entry{
				ID:           advertisedID + ":" + level,
				Model:        m,
				PinnedEffort: level,
			})
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
