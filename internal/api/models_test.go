package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jasminnanda/kirogo/internal/catalog"
	"github.com/jasminnanda/kirogo/internal/config"
	"github.com/jasminnanda/kirogo/internal/kiro"
)

// fixtureFetcher serves a fixed catalog.
type fixtureFetcher struct {
	specs []kiro.ModelSpec
	err   error
}

func (f fixtureFetcher) ListAvailableModels(context.Context, string) (*kiro.ListModelsResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &kiro.ListModelsResponse{
		Models:       f.specs,
		DefaultModel: &kiro.DefaultModel{ModelID: "claude-sonnet-4.5"},
	}, nil
}

// effortSchemaFor builds an additionalModelRequestFieldsSchema fixture.
func effortSchemaFor(path string, levels []string, def string) map[string]any {
	enum := make([]any, len(levels))
	for i, l := range levels {
		enum[i] = l
	}
	return map[string]any{"properties": map[string]any{
		path: map[string]any{"properties": map[string]any{
			"effort": map[string]any{"enum": enum, "default": def},
		}},
	}}
}

// modelsServer builds a Server with a loaded catalog.
func modelsServer(t *testing.T, exposeVariants bool) *Server {
	t.Helper()
	specs := []kiro.ModelSpec{
		{ModelID: "auto", ModelName: "Auto"},
		{ModelID: "claude-sonnet-4.5", ModelName: "Claude Sonnet 4.5",
			Description:    "Balanced model.",
			RateMultiplier: 1, RateUnit: "credit",
			TokenLimits: &kiro.TokenLimits{MaxInputTokens: 200000, MaxOutputTokens: 64000}},
		{ModelID: "claude-opus-5", ModelName: "Claude Opus 5",
			RateMultiplier: 2.2, RateUnit: "credit",
			TokenLimits:                        &kiro.TokenLimits{MaxInputTokens: 1000000, MaxOutputTokens: 64000},
			AdditionalModelRequestFieldsSchema: effortSchemaFor("reasoning", []string{"low", "medium", "high", "xhigh", "max"}, "xhigh")},
		{ModelID: "gpt-5", ModelName: "GPT 5",
			AdditionalModelRequestFieldsSchema: effortSchemaFor("output_config", []string{"low", "high"}, "high")},
	}
	c := catalog.New(catalog.Options{Fetcher: fixtureFetcher{specs: specs}})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return NewServer(Deps{
		Config:  &config.Config{ProxyAPIKey: "test-key", ExposeEffortVariants: exposeVariants},
		Catalog: c,
	})
}

// getModels calls /v1/models and returns the parsed body.
func getModels(t *testing.T, s *Server, apiKey string) (*httptest.ResponseRecorder, modelListResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	var body modelListResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response is not valid JSON: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, body
}

func TestListModelsRequiresAuth(t *testing.T) {
	s := modelsServer(t, true)

	rec, _ := getModels(t, s, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("without a key, status = %d, want 401", rec.Code)
	}

	rec, _ = getModels(t, s, "wrong-key")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("with a wrong key, status = %d, want 401", rec.Code)
	}
}

func TestListModelsShape(t *testing.T) {
	s := modelsServer(t, true)
	rec, body := getModels(t, s, "test-key")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if body.Object != "list" {
		t.Errorf("object = %q, want list", body.Object)
	}
	if len(body.Data) == 0 {
		t.Fatal("no models returned")
	}

	byID := map[string]modelEntry{}
	for _, e := range body.Data {
		byID[e.ID] = e
		if e.Object != "model" {
			t.Errorf("%s object = %q, want model", e.ID, e.Object)
		}
		if e.Created == 0 {
			t.Errorf("%s has no created timestamp", e.ID)
		}
		if e.OwnedBy == "" {
			t.Errorf("%s has no owned_by", e.ID)
		}
	}

	opus, ok := byID["claude-opus-5"]
	if !ok {
		t.Fatal("claude-opus-5 is missing")
	}
	if opus.OwnedBy != "anthropic" {
		t.Errorf("owned_by = %q, want anthropic", opus.OwnedBy)
	}
	if opus.ContextLength != 1000000 || opus.MaxInputTokens != 1000000 {
		t.Errorf("context = %d / %d, want 1000000", opus.ContextLength, opus.MaxInputTokens)
	}
	if opus.MaxOutputTokens != 64000 {
		t.Errorf("max_output_tokens = %d, want 64000", opus.MaxOutputTokens)
	}
	if opus.RateMultiplier != 2.2 || opus.RateUnit != "credit" {
		t.Errorf("rate = %v %q", opus.RateMultiplier, opus.RateUnit)
	}
	if strings.Join(opus.SupportedEffortLevels, ",") != "low,medium,high,xhigh,max" {
		t.Errorf("supported_effort_levels = %v", opus.SupportedEffortLevels)
	}
	if opus.DefaultEffortLevel != "xhigh" {
		t.Errorf("default_effort_level = %q", opus.DefaultEffortLevel)
	}
	if opus.EffortLevel != "" {
		t.Errorf("effort_level = %q, want empty on the base entry", opus.EffortLevel)
	}

	gpt := byID["gpt-5"]
	if gpt.OwnedBy != "openai" {
		t.Errorf("gpt-5 owned_by = %q, want openai", gpt.OwnedBy)
	}
}

func TestListModelsIncludesEffortVariants(t *testing.T) {
	s := modelsServer(t, true)
	_, body := getModels(t, s, "test-key")

	byID := map[string]modelEntry{}
	for _, e := range body.Data {
		byID[e.ID] = e
	}

	for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
		id := "claude-opus-5:" + level
		e, ok := byID[id]
		if !ok {
			t.Errorf("variant %s is missing", id)
			continue
		}
		if e.EffortLevel != level {
			t.Errorf("%s effort_level = %q, want %q", id, e.EffortLevel, level)
		}
		// A variant carries the same metadata as its base model.
		if e.ContextLength != 1000000 || e.RateMultiplier != 2.2 {
			t.Errorf("%s lost its base metadata: %+v", id, e)
		}
	}

	// gpt-5 advertises two levels only.
	if _, ok := byID["gpt-5:low"]; !ok {
		t.Error("gpt-5:low is missing")
	}
	if _, ok := byID["gpt-5:max"]; ok {
		t.Error("gpt-5:max should not exist: the model does not advertise it")
	}

	// A model with no effort schema gets no variants.
	for _, level := range []string{"low", "high", "max"} {
		if _, ok := byID["claude-sonnet-4.5:"+level]; ok {
			t.Errorf("claude-sonnet-4.5:%s should not exist", level)
		}
	}
}

func TestListModelsCanSuppressVariants(t *testing.T) {
	s := modelsServer(t, false)
	_, body := getModels(t, s, "test-key")

	for _, e := range body.Data {
		if strings.Contains(e.ID, ":") {
			t.Errorf("variant %s returned with KIRO_EXPOSE_EFFORT_VARIANTS=false", e.ID)
		}
	}
	// The base metadata is still there, so a client that can send
	// reasoning_effort keeps working.
	for _, e := range body.Data {
		if e.ID == "claude-opus-5" && len(e.SupportedEffortLevels) == 0 {
			t.Error("supported_effort_levels should still be advertised on the base entry")
		}
	}
}

func TestListModelsHidesAutoAndExposesTheAlias(t *testing.T) {
	s := modelsServer(t, true)
	_, body := getModels(t, s, "test-key")

	var sawAuto, sawAlias bool
	for _, e := range body.Data {
		if e.ID == "auto" {
			sawAuto = true
		}
		if e.ID == "auto-kiro" {
			sawAlias = true
		}
		if strings.HasPrefix(e.ID, "auto-kiro:") || strings.HasPrefix(e.ID, "auto:") {
			t.Errorf("%s should not exist: auto never takes an effort", e.ID)
		}
	}
	if sawAuto {
		t.Error("auto must be hidden, because Cursor ships its own model with that name")
	}
	if !sawAlias {
		t.Error("auto-kiro should be advertised in its place")
	}
}

func TestListModelsIsSorted(t *testing.T) {
	s := modelsServer(t, true)
	_, body := getModels(t, s, "test-key")

	for i := 1; i < len(body.Data); i++ {
		if body.Data[i-1].ID > body.Data[i].ID {
			t.Fatalf("not sorted: %q before %q", body.Data[i-1].ID, body.Data[i].ID)
		}
	}
}

func TestListModelsRejectsWrongMethod(t *testing.T) {
	s := modelsServer(t, true)
	req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/models = %d, want 405", rec.Code)
	}
}

func TestListModelsOmitsEmptyOptionalFields(t *testing.T) {
	specs := []kiro.ModelSpec{{ModelID: "bare-model"}}
	c := catalog.New(catalog.Options{Fetcher: fixtureFetcher{specs: specs}})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := NewServer(Deps{
		Config:  &config.Config{ProxyAPIKey: "k", ExposeEffortVariants: true},
		Catalog: c,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	raw := rec.Body.String()
	for _, absent := range []string{"description", "max_output_tokens", "rate_multiplier",
		"rate_unit", "supported_effort_levels", "default_effort_level", "effort_level"} {
		if strings.Contains(raw, `"`+absent+`"`) {
			t.Errorf("%q should be omitted when empty, got %s", absent, raw)
		}
	}
	// The required OpenAI fields must always be present.
	for _, present := range []string{"id", "object", "created", "owned_by"} {
		if !strings.Contains(raw, `"`+present+`"`) {
			t.Errorf("%q must always be present, got %s", present, raw)
		}
	}
	// A model with no reported limits still advertises the assumed default.
	if !strings.Contains(raw, `"context_length":200000`) {
		t.Errorf("expected the default context length, got %s", raw)
	}
}

func TestListModelsServesStaleCatalogWhenRefreshFails(t *testing.T) {
	fetcher := &togglingFetcher{specs: []kiro.ModelSpec{{ModelID: "claude-opus-5"}}}
	c := catalog.New(catalog.Options{Fetcher: fetcher, TTL: 1})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := NewServer(Deps{
		Config:  &config.Config{ProxyAPIKey: "k", ExposeEffortVariants: false},
		Catalog: c,
	})

	fetcher.fail = true
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a failed refresh must not fail the request", rec.Code)
	}
	var body modelListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 1 || body.Data[0].ID != "claude-opus-5" {
		t.Errorf("data = %+v, want the cached model still served", body.Data)
	}
}

// togglingFetcher can be switched into a failing state mid-test.
type togglingFetcher struct {
	specs []kiro.ModelSpec
	fail  bool
}

func (f *togglingFetcher) ListAvailableModels(context.Context, string) (*kiro.ListModelsResponse, error) {
	if f.fail {
		return nil, context.DeadlineExceeded
	}
	return &kiro.ListModelsResponse{Models: f.specs}, nil
}

func TestBuildModelEntry(t *testing.T) {
	c := catalog.New(catalog.Options{Fetcher: fixtureFetcher{specs: []kiro.ModelSpec{
		{ModelID: "claude-opus-5", ModelName: "Claude Opus 5", Description: "Deep model.",
			RateMultiplier: 2.2, RateUnit: "credit",
			TokenLimits:                        &kiro.TokenLimits{MaxInputTokens: 1000000, MaxOutputTokens: 64000},
			AdditionalModelRequestFieldsSchema: effortSchemaFor("reasoning", []string{"low", "max"}, "max")},
	}}})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	entries := c.Listing(true)
	var base, variant catalog.Entry
	for _, e := range entries {
		switch e.ID {
		case "claude-opus-5":
			base = e
		case "claude-opus-5:max":
			variant = e
		}
	}

	b := buildModelEntry(base)
	if b.EffortLevel != "" {
		t.Errorf("base effort_level = %q, want empty", b.EffortLevel)
	}
	if b.Description != "Deep model." {
		t.Errorf("description = %q", b.Description)
	}

	v := buildModelEntry(variant)
	if v.ID != "claude-opus-5:max" {
		t.Errorf("variant id = %q", v.ID)
	}
	if v.EffortLevel != "max" {
		t.Errorf("variant effort_level = %q, want max", v.EffortLevel)
	}
	if v.ContextLength != b.ContextLength {
		t.Error("a variant must carry the same context length as its base model")
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
