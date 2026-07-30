package catalog

import (
	"strings"
	"testing"

	"github.com/jasminnanda/kirogo/internal/kiro"
)

// noneSchema builds an additionalModelRequestFieldsSchema advertising levels.
func noneSchema(levels []string, def string) map[string]any {
	enum := make([]any, len(levels))
	for i, l := range levels {
		enum[i] = l
	}
	effort := map[string]any{"enum": enum}
	if def != "" {
		effort["default"] = def
	}
	return map[string]any{"properties": map[string]any{
		"reasoning": map[string]any{"properties": map[string]any{"effort": effort}},
	}}
}

// modelOffering returns a model whose schema advertises exactly these levels.
func modelOffering(t *testing.T, id string, levels []string, def string) *Model {
	t.Helper()
	c := newTestCatalog(t, kiro.ModelSpec{
		ModelID:                            id,
		ModelName:                          id,
		AdditionalModelRequestFieldsSchema: noneSchema(levels, def),
	})
	m, ok := c.Lookup(id)
	if !ok {
		t.Fatalf("model %s missing from the catalog", id)
	}
	return m
}

func TestNoneIsRecognisedButStaysOutOfTheIDEList(t *testing.T) {
	if !IsValidEffortLevel(EffortNone) {
		t.Error("none must be recognised, so a model advertising it can receive it")
	}
	for _, l := range EffortLevels {
		if l == EffortNone {
			t.Error("none must stay out of EffortLevels, which mirrors the Kiro IDE global list")
		}
	}
}

func TestLeastLevelUsesTheCanonicalRankNotTheEnumOrder(t *testing.T) {
	cases := []struct {
		name   string
		levels []string
		want   string
	}{
		{"none declared first", []string{"none", "low", "high"}, "none"},
		{"none declared last", []string{"max", "high", "low", "none"}, "none"},
		{"no none offered", []string{"low", "medium", "high", "xhigh", "max"}, "low"},
		{"only high levels", []string{"xhigh", "max"}, "xhigh"},
		{"fully reversed", []string{"max", "xhigh", "high", "medium", "low"}, "low"},
		{"single level", []string{"medium"}, "medium"},
		{"nothing recognisable", []string{"turbo", "ludicrous"}, ""},
		{"empty", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &EffortSupport{Levels: tc.levels, SchemaPath: "reasoning"}
			if got := e.LeastLevel(); got != tc.want {
				t.Errorf("LeastLevel() = %q, want %q", got, tc.want)
			}
		})
	}

	var nilSupport *EffortSupport
	if got := nilSupport.LeastLevel(); got != "" {
		t.Errorf("a nil EffortSupport must return empty, got %q", got)
	}
}

func TestResolveEffortHandlesNone(t *testing.T) {
	withNone := modelOffering(t, "gpt-5.6-sol",
		[]string{"none", "low", "medium", "high", "xhigh", "max"}, "high")
	withoutNone := modelOffering(t, "claude-opus-5",
		[]string{"low", "medium", "high", "xhigh", "max"}, "high")
	highOnly := modelOffering(t, "stubborn", []string{"high", "max"}, "high")

	cases := []struct {
		name                      string
		model                     *Model
		suffix, request, operator string
		want                      string
	}{
		{"delivered when advertised", withNone, "", "none", "", "none"},
		{"pinned by suffix", withNone, "none", "", "", "none"},
		{"as the operator default", withNone, "", "", "none", "none"},
		{"falls to the least level offered", withoutNone, "", "none", "", "low"},
		{"falls to the least of a high-only model", highOnly, "", "none", "", "high"},
		{"a suffix still outranks it", withoutNone, "xhigh", "none", "", "xhigh"},
		{"a request level outranks an operator none", withNone, "", "max", "none", "max"},
		{"model default applies when nothing is asked", withNone, "", "", "", "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveEffort(tc.model, tc.suffix, tc.request, tc.operator); got != tc.want {
				t.Errorf("ResolveEffort() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNoneNeverReachesAModelWithoutEffortSupport(t *testing.T) {
	c := newTestCatalog(t, kiro.ModelSpec{ModelID: "plain", ModelName: "Plain"})
	m, ok := c.Lookup("plain")
	if !ok {
		t.Fatal("model missing")
	}
	if got := ResolveEffort(m, "", EffortNone, ""); got != "" {
		t.Errorf("ResolveEffort() = %q, want empty for a model with no effort support", got)
	}
}

func TestAutoNeverReceivesNone(t *testing.T) {
	m := modelOffering(t, AutoModelID, []string{"none", "low", "high"}, "high")
	if got := ResolveEffort(m, EffortNone, EffortNone, EffortNone); got != "" {
		t.Errorf("ResolveEffort() = %q, want empty: auto never takes an effort field", got)
	}
}

func TestNoneIsAVariantOnlyForModelsThatAdvertiseIt(t *testing.T) {
	c := newTestCatalog(t,
		kiro.ModelSpec{ModelID: "gpt-5.6-sol", ModelName: "GPT 5.6 Sol",
			AdditionalModelRequestFieldsSchema: noneSchema([]string{"none", "low", "high"}, "high")},
		kiro.ModelSpec{ModelID: "claude-opus-5", ModelName: "Claude Opus 5",
			AdditionalModelRequestFieldsSchema: noneSchema([]string{"low", "high"}, "high")},
	)

	entries := c.Listing(true)
	ids := map[string]string{}
	for _, e := range entries {
		ids[e.ID] = e.PinnedEffort
	}

	if _, ok := ids["gpt-5.6-sol:none"]; !ok {
		t.Error("a model advertising none should expose a :none variant")
	}
	if pinned := ids["gpt-5.6-sol:none"]; pinned != EffortNone {
		t.Errorf("PinnedEffort = %q, want none: the id must carry the level through", pinned)
	}
	if _, ok := ids["claude-opus-5:none"]; ok {
		t.Error("a model that does not advertise none must not expose a :none variant")
	}

	// Turning variants off must remove it again.
	for _, e := range c.Listing(false) {
		if strings.Contains(e.ID, ":") {
			t.Errorf("variants disabled but %q was listed", e.ID)
		}
	}
}

func TestNoneSuffixParses(t *testing.T) {
	for _, name := range []string{"gpt-5.6-sol:none", "gpt-5.6-sol-none"} {
		model, level := SplitEffortSuffix(name)
		if level != EffortNone {
			t.Errorf("SplitEffortSuffix(%q) level = %q, want none", name, level)
		}
		if !strings.HasPrefix(name, model) {
			t.Errorf("SplitEffortSuffix(%q) model = %q, which is not a prefix of the input",
				name, model)
		}
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
