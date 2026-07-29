package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jasminnanda/kirogo/internal/kiro"
)

// pagedFetcher walks a scripted list of pages, keyed by the token it hands out.
type pagedFetcher struct {
	mu      sync.Mutex
	pages   []*kiro.ListModelsResponse
	served  int
	failNow error
	// tokens maps a nextToken to the page index that should be served next.
	tokens map[string]int
}

func newPagedFetcher(pages ...*kiro.ListModelsResponse) *pagedFetcher {
	f := &pagedFetcher{pages: pages, tokens: map[string]int{}}
	for i, p := range pages {
		if p.NextToken != "" {
			f.tokens[p.NextToken] = i + 1
		}
	}
	return f
}

func (f *pagedFetcher) ListAvailableModels(_ context.Context, nextToken string) (*kiro.ListModelsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNow != nil {
		return nil, f.failNow
	}
	f.served++
	idx := 0
	if nextToken != "" {
		i, ok := f.tokens[nextToken]
		if !ok {
			return nil, fmt.Errorf("unexpected nextToken %q", nextToken)
		}
		idx = i
	}
	if idx >= len(f.pages) {
		return &kiro.ListModelsResponse{}, nil
	}
	return f.pages[idx], nil
}

func (f *pagedFetcher) Served() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.served
}

func (f *pagedFetcher) SetError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNow = err
}

func TestRefreshFollowsPagination(t *testing.T) {
	f := newPagedFetcher(
		&kiro.ListModelsResponse{
			Models:       []kiro.ModelSpec{{ModelID: "m1"}, {ModelID: "m2"}},
			DefaultModel: &kiro.DefaultModel{ModelID: "m1"},
			NextToken:    "t1",
		},
		&kiro.ListModelsResponse{
			Models:    []kiro.ModelSpec{{ModelID: "m3"}},
			NextToken: "t2",
		},
		&kiro.ListModelsResponse{
			Models: []kiro.ModelSpec{{ModelID: "m4"}},
		},
	)

	c := New(Options{Fetcher: f})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := c.IDs(); strings.Join(got, ",") != "m1,m2,m3,m4" {
		t.Errorf("IDs() = %v, want all four models across three pages", got)
	}
	if f.Served() != 3 {
		t.Errorf("fetched %d pages, want 3", f.Served())
	}
	if c.DefaultModel() != "m1" {
		t.Errorf("DefaultModel() = %q", c.DefaultModel())
	}
}

func TestRefreshStopsAtThePaginationCap(t *testing.T) {
	// Every page points at another page, so only the cap can stop it.
	var pages []*kiro.ListModelsResponse
	for i := 0; i < 25; i++ {
		pages = append(pages, &kiro.ListModelsResponse{
			Models:    []kiro.ModelSpec{{ModelID: fmt.Sprintf("m%d", i)}},
			NextToken: fmt.Sprintf("t%d", i),
		})
	}
	f := newPagedFetcher(pages...)

	c := New(Options{Fetcher: f})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	if f.Served() != maxPages {
		t.Errorf("fetched %d pages, want the cap of %d", f.Served(), maxPages)
	}
	if c.Len() != maxPages {
		t.Errorf("collected %d models, want %d", c.Len(), maxPages)
	}
}

func TestRefreshSkipsEntriesWithNoModelID(t *testing.T) {
	f := newPagedFetcher(&kiro.ListModelsResponse{Models: []kiro.ModelSpec{
		{ModelID: "good-1"},
		{ModelID: ""},
		{ModelID: "   "},
		{ModelID: "good-2", ModelName: "Good Two"},
	}})

	c := New(Options{Fetcher: f})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.IDs(); strings.Join(got, ",") != "good-1,good-2" {
		t.Errorf("IDs() = %v, want the blank entries skipped", got)
	}
}

func TestRefreshOnEmptyCatalogIsAnError(t *testing.T) {
	f := newPagedFetcher(&kiro.ListModelsResponse{Models: nil})
	c := New(Options{Fetcher: f})

	err := c.Refresh(context.Background())
	if err == nil {
		t.Fatal("an empty catalog should be an error, not a silent success")
	}
	if !strings.Contains(err.Error(), "no models enabled") {
		t.Errorf("error = %q, want it to suggest the account has no models", err)
	}
}

func TestRefreshFailurePropagates(t *testing.T) {
	want := errors.New("network down")
	f := newPagedFetcher()
	f.SetError(want)

	c := New(Options{Fetcher: f})
	err := c.Refresh(context.Background())
	if !errors.Is(err, want) {
		t.Errorf("error = %v, want the fetch failure wrapped", err)
	}
	if c.Len() != 0 {
		t.Errorf("catalog should stay empty after a failed first fetch, got %d models", c.Len())
	}
}

func TestFailedRefreshKeepsTheStaleCatalog(t *testing.T) {
	f := newPagedFetcher(&kiro.ListModelsResponse{Models: []kiro.ModelSpec{
		{ModelID: "claude-opus-5"}, {ModelID: "gpt-5"},
	}})
	c := New(Options{Fetcher: f})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := c.IDs()

	f.SetError(errors.New("backend unavailable"))
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected the second refresh to fail")
	}

	after := c.IDs()
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Errorf("catalog changed after a failed refresh: %v then %v", before, after)
	}
	if _, ok := c.Lookup("claude-opus-5"); !ok {
		t.Error("a failed refresh must not empty the cache")
	}
}

func TestEnsureFreshRespectsTheTTL(t *testing.T) {
	f := newPagedFetcher(&kiro.ListModelsResponse{Models: []kiro.ModelSpec{{ModelID: "m1"}}})
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	clock := now
	c := New(Options{Fetcher: f, TTL: time.Hour, Now: func() time.Time { return clock }})

	// First call loads the catalog.
	c.EnsureFresh(context.Background())
	if f.Served() != 1 {
		t.Fatalf("served %d, want 1 initial load", f.Served())
	}

	// Inside the TTL: no refetch.
	clock = now.Add(59 * time.Minute)
	c.EnsureFresh(context.Background())
	if f.Served() != 1 {
		t.Errorf("served %d, want no refetch inside the TTL", f.Served())
	}

	// Past the TTL: refetch.
	clock = now.Add(61 * time.Minute)
	c.EnsureFresh(context.Background())
	if f.Served() != 2 {
		t.Errorf("served %d, want a refetch once the TTL elapsed", f.Served())
	}
}

func TestEnsureFreshSwallowsRefreshFailures(t *testing.T) {
	f := newPagedFetcher(&kiro.ListModelsResponse{Models: []kiro.ModelSpec{{ModelID: "m1"}}})
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	clock := now
	c := New(Options{Fetcher: f, TTL: time.Minute, Now: func() time.Time { return clock }})
	c.EnsureFresh(context.Background())

	f.SetError(errors.New("down"))
	clock = now.Add(2 * time.Minute)
	// Must not panic or clear the cache; the failure is only logged.
	c.EnsureFresh(context.Background())

	if _, ok := c.Lookup("m1"); !ok {
		t.Error("EnsureFresh must keep serving the cached catalog when a refresh fails")
	}
}

func TestDefaultMaxInputTokensWhenTokenLimitsAreMissing(t *testing.T) {
	f := newPagedFetcher(&kiro.ListModelsResponse{Models: []kiro.ModelSpec{
		{ModelID: "no-limits"},
		{ModelID: "zero-limits", TokenLimits: &kiro.TokenLimits{}},
		{ModelID: "with-limits", TokenLimits: &kiro.TokenLimits{MaxInputTokens: 1000000, MaxOutputTokens: 64000}},
	}})
	c := New(Options{Fetcher: f})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	cases := map[string]int{
		"no-limits":   DefaultMaxInputTokens,
		"zero-limits": DefaultMaxInputTokens,
		"with-limits": 1000000,
	}
	for id, want := range cases {
		m, ok := c.Lookup(id)
		if !ok {
			t.Fatalf("model %s missing", id)
		}
		if m.MaxInputTokens != want {
			t.Errorf("%s MaxInputTokens = %d, want %d", id, m.MaxInputTokens, want)
		}
	}
	if DefaultMaxInputTokens != 200000 {
		t.Errorf("DefaultMaxInputTokens = %d, want 200000", DefaultMaxInputTokens)
	}
}

func TestModelNameFallsBackToTheID(t *testing.T) {
	f := newPagedFetcher(&kiro.ListModelsResponse{Models: []kiro.ModelSpec{{ModelID: "bare-id"}}})
	c := New(Options{Fetcher: f})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	m, _ := c.Lookup("bare-id")
	if m.Name != "bare-id" {
		t.Errorf("Name = %q, want the id as a fallback", m.Name)
	}
}

func TestEffortExtractionForBothSchemaPaths(t *testing.T) {
	cases := []struct {
		name        string
		schema      map[string]any
		wantPath    string
		wantLevels  []string
		wantDefault string
	}{
		{
			name:        "output_config",
			schema:      effortSchema("output_config", []string{"low", "high"}, "high"),
			wantPath:    "output_config",
			wantLevels:  []string{"low", "high"},
			wantDefault: "high",
		},
		{
			name:        "reasoning",
			schema:      effortSchema("reasoning", []string{"low", "medium", "high", "xhigh", "max"}, "xhigh"),
			wantPath:    "reasoning",
			wantLevels:  []string{"low", "medium", "high", "xhigh", "max"},
			wantDefault: "xhigh",
		},
		{
			name:        "no default",
			schema:      effortSchema("reasoning", []string{"low"}, ""),
			wantPath:    "reasoning",
			wantLevels:  []string{"low"},
			wantDefault: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractEffortSupport(tc.schema)
			if !got.Supported() {
				t.Fatalf("extractEffortSupport returned no support for %v", tc.schema)
			}
			if got.SchemaPath != tc.wantPath {
				t.Errorf("SchemaPath = %q, want %q", got.SchemaPath, tc.wantPath)
			}
			if strings.Join(got.Levels, ",") != strings.Join(tc.wantLevels, ",") {
				t.Errorf("Levels = %v, want %v", got.Levels, tc.wantLevels)
			}
			if got.DefaultLevel != tc.wantDefault {
				t.Errorf("DefaultLevel = %q, want %q", got.DefaultLevel, tc.wantDefault)
			}
		})
	}
}

func TestOutputConfigIsProbedBeforeReasoning(t *testing.T) {
	// A schema declaring both must resolve to output_config, matching the IDE's
	// probe order.
	schema := map[string]any{"properties": map[string]any{
		"output_config": map[string]any{"properties": map[string]any{
			"effort": map[string]any{"enum": []any{"a"}, "default": "a"}}},
		"reasoning": map[string]any{"properties": map[string]any{
			"effort": map[string]any{"enum": []any{"b"}, "default": "b"}}},
	}}
	got := extractEffortSupport(schema)
	if got.SchemaPath != "output_config" {
		t.Errorf("SchemaPath = %q, want output_config to win", got.SchemaPath)
	}
}

func TestEffortExtractionRejectsMalformedSchemas(t *testing.T) {
	cases := []struct {
		name   string
		schema map[string]any
	}{
		{"nil", nil},
		{"empty", map[string]any{}},
		{"no properties", map[string]any{"type": "object"}},
		{"properties is not an object", map[string]any{"properties": "nope"}},
		{"unknown path only", map[string]any{"properties": map[string]any{
			"thinking": map[string]any{"properties": map[string]any{
				"effort": map[string]any{"enum": []any{"low"}}}}}}},
		{"no effort key", map[string]any{"properties": map[string]any{
			"reasoning": map[string]any{"properties": map[string]any{
				"budget": map[string]any{"enum": []any{"low"}}}}}}},
		{"empty enum", map[string]any{"properties": map[string]any{
			"reasoning": map[string]any{"properties": map[string]any{
				"effort": map[string]any{"enum": []any{}}}}}}},
		{"missing enum", map[string]any{"properties": map[string]any{
			"reasoning": map[string]any{"properties": map[string]any{
				"effort": map[string]any{"type": "string"}}}}}},
		{"enum is not an array", map[string]any{"properties": map[string]any{
			"reasoning": map[string]any{"properties": map[string]any{
				"effort": map[string]any{"enum": "low"}}}}}},
		{"enum of non-strings", map[string]any{"properties": map[string]any{
			"reasoning": map[string]any{"properties": map[string]any{
				"effort": map[string]any{"enum": []any{1, 2, 3}}}}}}},
		{"reasoning has no properties", map[string]any{"properties": map[string]any{
			"reasoning": map[string]any{"type": "object"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractEffortSupport(tc.schema); got.Supported() {
				t.Errorf("extractEffortSupport returned %+v, want no support", got)
			}
		})
	}
}

func TestEffortSupportHelpers(t *testing.T) {
	var nilSupport *EffortSupport
	if nilSupport.Supported() {
		t.Error("a nil EffortSupport must not report support")
	}
	if nilSupport.Allows("low") {
		t.Error("a nil EffortSupport must not allow anything")
	}
	s := &EffortSupport{Levels: []string{"low", "high"}, SchemaPath: "reasoning"}
	if !s.Allows("high") || s.Allows("max") {
		t.Errorf("Allows is wrong for %+v", s)
	}
	noPath := &EffortSupport{Levels: []string{"low"}}
	if noPath.Supported() {
		t.Error("support requires a schema path")
	}
	noLevels := &EffortSupport{SchemaPath: "reasoning"}
	if noLevels.Supported() {
		t.Error("support requires at least one level")
	}
}

func TestModelHelpersOnNilAndPlainModels(t *testing.T) {
	var nilModel *Model
	if nilModel.Effort() != nil {
		t.Error("nil model should have nil effort")
	}
	if len(nilModel.EffortLevels()) != 0 {
		t.Error("nil model should have no effort levels")
	}
	if nilModel.DefaultEffortLevel() != "" {
		t.Error("nil model should have no default effort")
	}

	plain := &Model{ID: "plain"}
	if len(plain.EffortLevels()) != 0 || plain.DefaultEffortLevel() != "" {
		t.Error("a model with no effort schema should report none")
	}
}

func TestOwnedByInference(t *testing.T) {
	cases := map[string]string{
		"claude-opus-5":    "anthropic",
		"claude-sonnet-4":  "anthropic",
		"gpt-5":            "openai",
		"gpt-5.6-sol":      "openai",
		"deepseek-3.2":     "kiro",
		"glm-5":            "kiro",
		"minimax-m2.5":     "kiro",
		"qwen3-coder-next": "kiro",
		"auto":             "kiro",
	}
	for id, want := range cases {
		m := &Model{ID: id}
		if got := m.OwnedBy(); got != want {
			t.Errorf("OwnedBy() for %q = %q, want %q", id, got, want)
		}
	}
}

func TestListingGeneratesEffortVariants(t *testing.T) {
	c := newTestCatalog(t)
	entries := c.Listing(true)

	byID := map[string]Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}

	// claude-opus-5 advertises all five levels, so it gets a base entry plus five.
	if _, ok := byID["claude-opus-5"]; !ok {
		t.Error("the base model entry should be present")
	}
	for _, level := range EffortLevels {
		id := "claude-opus-5:" + level
		e, ok := byID[id]
		if !ok {
			t.Errorf("variant %s is missing", id)
			continue
		}
		if e.PinnedEffort != level {
			t.Errorf("%s PinnedEffort = %q, want %q", id, e.PinnedEffort, level)
		}
		if e.Model.ID != "claude-opus-5" {
			t.Errorf("%s points at model %q", id, e.Model.ID)
		}
	}

	// gpt-5 advertises only two levels, so only those two variants exist.
	for _, level := range []string{"low", "high"} {
		if _, ok := byID["gpt-5:"+level]; !ok {
			t.Errorf("variant gpt-5:%s is missing", level)
		}
	}
	for _, level := range []string{"medium", "xhigh", "max"} {
		if _, ok := byID["gpt-5:"+level]; ok {
			t.Errorf("gpt-5:%s should not exist: the model does not advertise it", level)
		}
	}

	// A model with no effort schema gets no variants.
	for _, level := range EffortLevels {
		if _, ok := byID["claude-sonnet-4.5:"+level]; ok {
			t.Errorf("claude-sonnet-4.5:%s should not exist", level)
		}
	}
	// auto is hidden, and never gets variants.
	for id := range byID {
		if strings.HasPrefix(id, "auto:") || strings.HasPrefix(id, "auto-kiro:") {
			t.Errorf("%s should not exist: auto never takes an effort", id)
		}
	}
}

func TestListingCanSuppressVariants(t *testing.T) {
	c := newTestCatalog(t)
	for _, e := range c.Listing(false) {
		if strings.Contains(e.ID, ":") {
			t.Errorf("variant %s emitted when variants are disabled", e.ID)
		}
		if e.PinnedEffort != "" {
			t.Errorf("%s has a pinned effort with variants disabled", e.ID)
		}
	}
	// The base models must still be listed.
	if len(c.Listing(false)) != c.Len() {
		t.Errorf("listed %d entries, want one per model (%d) minus hidden plus aliases",
			len(c.Listing(false)), c.Len())
	}
}

func TestListingIsSortedAndHidesAuto(t *testing.T) {
	c := newTestCatalog(t)
	entries := c.Listing(true)

	for i := 1; i < len(entries); i++ {
		if entries[i-1].ID > entries[i].ID {
			t.Fatalf("listing is not sorted: %q came before %q", entries[i-1].ID, entries[i].ID)
		}
	}
	for _, e := range entries {
		if e.ID == "auto" {
			t.Error("auto must be hidden in favour of the auto-kiro alias")
		}
	}
}

func TestLookupReturnsACopy(t *testing.T) {
	c := newTestCatalog(t)
	first, ok := c.Lookup("claude-opus-5")
	if !ok {
		t.Fatal("model missing")
	}
	first.MaxInputTokens = 1
	first.ID = "mutated"

	second, _ := c.Lookup("claude-opus-5")
	if second.MaxInputTokens == 1 || second.ID == "mutated" {
		t.Error("Lookup handed out a pointer into the cache: callers can corrupt it")
	}
}

func TestModelsReturnsACopy(t *testing.T) {
	c := newTestCatalog(t)
	models := c.Models()
	if len(models) == 0 {
		t.Fatal("no models")
	}
	models[0].ID = "mutated"

	if c.Models()[0].ID == "mutated" {
		t.Error("Models() handed out the backing slice")
	}
}

func TestCatalogIsSafeUnderConcurrency(t *testing.T) {
	f := newPagedFetcher(&kiro.ListModelsResponse{Models: testCatalogModels()})
	c := New(Options{Fetcher: f, TTL: time.Nanosecond})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			switch n % 6 {
			case 0:
				c.EnsureFresh(context.Background())
			case 1:
				_ = c.Models()
			case 2:
				_, _ = c.Lookup("claude-opus-5")
			case 3:
				_ = c.IDs()
			case 4:
				_ = c.Listing(true)
			case 5:
				_ = c.Resolve("claude-opus-5:max", "", "")
			}
		}(i)
	}
	wg.Wait()

	if c.Len() == 0 {
		t.Error("the catalog was emptied by concurrent access")
	}
}

func TestFetchedAtIsRecorded(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	f := newPagedFetcher(&kiro.ListModelsResponse{Models: []kiro.ModelSpec{{ModelID: "m"}}})
	c := New(Options{Fetcher: f, Now: func() time.Time { return now }})

	if !c.FetchedAt().IsZero() {
		t.Error("FetchedAt should be zero before the first load")
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !c.FetchedAt().Equal(now) {
		t.Errorf("FetchedAt() = %v, want %v", c.FetchedAt(), now)
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	c := New(Options{Fetcher: newPagedFetcher()})
	if c.ttl != time.Hour {
		t.Errorf("default TTL = %v, want 1h", c.ttl)
	}
	if c.now == nil {
		t.Error("clock should default to time.Now")
	}
}

func TestModelIDEndingInAnEffortWordIsNotMisread(t *testing.T) {
	// A model genuinely named "special-low" must win over reading "-low" as a
	// pinned effort level.
	f := newPagedFetcher(&kiro.ListModelsResponse{Models: []kiro.ModelSpec{
		{ModelID: "special-low"},
		{ModelID: "claude-opus-5",
			AdditionalModelRequestFieldsSchema: effortSchema("reasoning", []string{"low", "high"}, "high")},
	}})
	c := New(Options{Fetcher: f})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	res := c.Resolve("special-low", "", "")
	if res.ModelID != "special-low" {
		t.Errorf("ModelID = %q, want the real model id special-low", res.ModelID)
	}
	if res.Source != SourceExact {
		t.Errorf("Source = %q, want exact", res.Source)
	}
	if res.EffortPinnedBySuffix {
		t.Error("the -low in a real model id must not be read as a pinned effort")
	}
}

func TestInvertedNameWithEffortResolvesBothPartsCorrectly(t *testing.T) {
	f := newPagedFetcher(&kiro.ListModelsResponse{Models: []kiro.ModelSpec{
		{ModelID: "claude-opus-4.5",
			AdditionalModelRequestFieldsSchema: effortSchema("reasoning", []string{"low", "high", "max"}, "high")},
	}})
	c := New(Options{Fetcher: f})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The inverted form needs its trailing token for pattern P5 to fire, while
	// that same token is the pinned effort.
	res := c.Resolve("claude-4.5-opus-high", "", "")
	if res.ModelID != "claude-opus-4.5" {
		t.Errorf("ModelID = %q, want claude-opus-4.5", res.ModelID)
	}
	if res.EffortLevel != "high" {
		t.Errorf("EffortLevel = %q, want high", res.EffortLevel)
	}
	if !res.EffortPinnedBySuffix {
		t.Error("EffortPinnedBySuffix should be true")
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
