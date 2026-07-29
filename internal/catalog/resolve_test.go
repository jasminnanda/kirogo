package catalog

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"kirogo/internal/kiro"
)

// staticFetcher serves fixed pages.
type staticFetcher struct {
	pages []kiro.ListModelsResponse
	calls int
	err   error
}

func (f *staticFetcher) ListAvailableModels(_ context.Context, nextToken string) (*kiro.ListModelsResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	idx := 0
	if nextToken != "" {
		// Tokens are "page-N" for N starting at 1.
		for i, p := range f.pages {
			if p.NextToken == nextToken {
				idx = i + 1
				break
			}
		}
		if strings.HasPrefix(nextToken, "page-") {
			for i := range f.pages {
				if i > 0 && f.pages[i-1].NextToken == nextToken {
					idx = i
					break
				}
			}
		}
	}
	f.calls++
	if idx >= len(f.pages) {
		return &kiro.ListModelsResponse{}, nil
	}
	page := f.pages[idx]
	return &page, nil
}

// effortSchema builds an additionalModelRequestFieldsSchema for a path.
func effortSchema(path string, levels []string, defaultLevel string) map[string]any {
	enum := make([]any, len(levels))
	for i, l := range levels {
		enum[i] = l
	}
	effort := map[string]any{"type": "string", "enum": enum}
	if defaultLevel != "" {
		effort["default"] = defaultLevel
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			path: map[string]any{
				"type":       "object",
				"properties": map[string]any{"effort": effort},
			},
		},
	}
}

// testCatalogModels is a catalog resembling a live account.
func testCatalogModels() []kiro.ModelSpec {
	return []kiro.ModelSpec{
		{ModelID: "auto", ModelName: "Auto"},
		{ModelID: "claude-sonnet-4", ModelName: "Claude Sonnet 4",
			TokenLimits: &kiro.TokenLimits{MaxInputTokens: 200000, MaxOutputTokens: 8192}},
		{ModelID: "claude-sonnet-4.5", ModelName: "Claude Sonnet 4.5", RateMultiplier: 1,
			RateUnit: "credit", TokenLimits: &kiro.TokenLimits{MaxInputTokens: 200000, MaxOutputTokens: 64000}},
		{ModelID: "claude-sonnet-4.6", ModelName: "Claude Sonnet 4.6"},
		{ModelID: "claude-haiku-4.5", ModelName: "Claude Haiku 4.5"},
		{ModelID: "claude-opus-4.5", ModelName: "Claude Opus 4.5"},
		{ModelID: "claude-opus-5", ModelName: "Claude Opus 5", RateMultiplier: 2.2, RateUnit: "credit",
			TokenLimits:                        &kiro.TokenLimits{MaxInputTokens: 1000000, MaxOutputTokens: 64000},
			AdditionalModelRequestFieldsSchema: effortSchema("reasoning", []string{"low", "medium", "high", "xhigh", "max"}, "xhigh")},
		{ModelID: "gpt-5", ModelName: "GPT 5",
			AdditionalModelRequestFieldsSchema: effortSchema("output_config", []string{"low", "high"}, "high")},
		{ModelID: "gpt-5.6", ModelName: "GPT 5.6",
			AdditionalModelRequestFieldsSchema: effortSchema("output_config", []string{"low", "high"}, "high")},
		{ModelID: "deepseek-3.2", ModelName: "DeepSeek 3.2"},
		{ModelID: "glm-5", ModelName: "GLM 5"},
		{ModelID: "minimax-m2.5", ModelName: "MiniMax M2.5"},
		{ModelID: "qwen3-coder-next", ModelName: "Qwen3 Coder Next"},
	}
}

// newTestCatalog builds a loaded catalog.
func newTestCatalog(t *testing.T, specs ...kiro.ModelSpec) *Catalog {
	t.Helper()
	if len(specs) == 0 {
		specs = testCatalogModels()
	}
	f := &staticFetcher{pages: []kiro.ListModelsResponse{{
		Models:       specs,
		DefaultModel: &kiro.DefaultModel{ModelID: "claude-sonnet-4.5"},
	}}}
	c := New(Options{Fetcher: f})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return c
}

func TestNormalizeModelNameEveryReferenceExample(t *testing.T) {
	cases := []struct{ in, want string }{
		// Pattern 1: dash to dot for the minor version.
		{"claude-haiku-4-5", "claude-haiku-4.5"},
		{"claude-haiku-4-5-20251001", "claude-haiku-4.5"},
		{"claude-haiku-4-5-latest", "claude-haiku-4.5"},
		{"claude-sonnet-4-5", "claude-sonnet-4.5"},
		{"claude-opus-4-5", "claude-opus-4.5"},
		{"claude-sonnet-4-5-1", "claude-sonnet-4.5"},
		// Pattern 2: no minor version, optional date.
		{"claude-sonnet-4", "claude-sonnet-4"},
		{"claude-sonnet-4-20250514", "claude-sonnet-4"},
		{"claude-opus-3", "claude-opus-3"},
		// Pattern 3: legacy major-minor-family.
		{"claude-3-7-sonnet", "claude-3.7-sonnet"},
		{"claude-3-7-sonnet-20250219", "claude-3.7-sonnet"},
		{"claude-3-5-haiku-latest", "claude-3.5-haiku"},
		// Pattern 4: already dotted, with a date to strip.
		{"claude-haiku-4.5-20251001", "claude-haiku-4.5"},
		{"claude-3.7-sonnet-20250219", "claude-3.7-sonnet"},
		// Pattern 5: inverted with a suffix.
		{"claude-4.5-opus-high", "claude-opus-4.5"},
		{"claude-4.5-sonnet-low", "claude-sonnet-4.5"},
		{"claude-4.5-opus-high-thinking", "claude-opus-4.5"},
		// Context-window markers are display hints, not part of the id.
		{"claude-sonnet-4-5[1m]", "claude-sonnet-4.5"},
		{"claude-sonnet-4.5[200K]", "claude-sonnet-4.5"},
		{"claude-opus-5[1M]", "claude-opus-5"},
		// Already normalised names are untouched.
		{"claude-haiku-4.5", "claude-haiku-4.5"},
		{"claude-3.7-sonnet", "claude-3.7-sonnet"},
		{"auto", "auto"},
		// Newer ids the patterns do not understand, correctly passed through.
		{"claude-opus-5", "claude-opus-5"},
		{"gpt-5", "gpt-5"},
		{"gpt-5.6-sol", "gpt-5.6-sol"},
		{"deepseek-3.2", "deepseek-3.2"},
		{"glm-5", "glm-5"},
		{"minimax-m2.5", "minimax-m2.5"},
		{"qwen3-coder-next", "qwen3-coder-next"},
		// Original case is preserved when nothing matches.
		{"GPT-5-Turbo", "GPT-5-Turbo"},
		{"", ""},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := NormalizeModelName(tc.in); got != tc.want {
				t.Errorf("NormalizeModelName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	for _, name := range []string{
		"claude-haiku-4-5-20251001", "claude-3-7-sonnet", "claude-4.5-opus-high",
		"claude-sonnet-4-20250514", "claude-opus-5", "gpt-5",
	} {
		once := NormalizeModelName(name)
		twice := NormalizeModelName(once)
		if once != twice {
			t.Errorf("normalising %q twice changed it: %q then %q", name, once, twice)
		}
	}
}

func TestEightDigitDatesCannotBeReadAsAMinorVersion(t *testing.T) {
	// Pattern 1 restricts the minor version to 1-2 digits precisely so this
	// cannot become claude-sonnet-4.20250514.
	if got := NormalizeModelName("claude-sonnet-4-20250514"); got != "claude-sonnet-4" {
		t.Errorf("got %q, want claude-sonnet-4", got)
	}
}

func TestSplitEffortSuffix(t *testing.T) {
	cases := []struct {
		in        string
		wantModel string
		wantLevel string
	}{
		{"claude-opus-5:max", "claude-opus-5", "max"},
		{"claude-opus-5:xhigh", "claude-opus-5", "xhigh"},
		{"claude-opus-5:LOW", "claude-opus-5", "low"},
		{"claude-opus-5-high", "claude-opus-5", "high"},
		{"claude-4.5-opus-high", "claude-4.5-opus", "high"},
		{"claude-opus-5", "claude-opus-5", ""},
		{"claude-opus-5:turbo", "claude-opus-5:turbo", ""},
		{"claude-opus-5-turbo", "claude-opus-5-turbo", ""},
		{"gpt-5:medium", "gpt-5", "medium"},
		{"", "", ""},
		{":max", ":max", ""},
		{"claude-opus-5:", "claude-opus-5:", ""},
		{"a:b:max", "a:b", "max"},
		{"  claude-opus-5:max  ", "claude-opus-5", "max"},
		// A hyphenated level must not be mistaken for part of an id.
		{"minimax-m2.5", "minimax-m2.5", ""},
		{"qwen3-coder-next", "qwen3-coder-next", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			model, level := SplitEffortSuffix(tc.in)
			if model != tc.wantModel || level != tc.wantLevel {
				t.Errorf("SplitEffortSuffix(%q) = (%q, %q), want (%q, %q)",
					tc.in, model, level, tc.wantModel, tc.wantLevel)
			}
		})
	}
}

func TestEffortSuffixIsSplitBeforeNormalisation(t *testing.T) {
	// If normalisation ran first, pattern 5 would swallow "-high" as noise and
	// the pinned effort would be lost.
	c := newTestCatalog(t)
	res := c.Resolve("claude-4.5-opus-high", "", "")

	if res.ModelID != "claude-opus-4.5" {
		t.Errorf("ModelID = %q, want claude-opus-4.5", res.ModelID)
	}
	if !res.EffortPinnedBySuffix {
		t.Error("the trailing -high should have been read as a pinned effort, not as name noise")
	}
}

func TestAutoCorrectBehaviour(t *testing.T) {
	available := []string{
		"auto", "claude-sonnet-4", "claude-sonnet-4.5", "claude-sonnet-4.6",
		"claude-opus-4.5", "claude-opus-5", "gpt-5", "gpt-5.6", "glm-5",
	}
	cases := []struct {
		name       string
		requested  string
		wantID     string
		wantStatus autoCorrectStatus
	}{
		{"auto is always fine", "auto", "auto", autoCorrectOK},
		{"exact match short-circuits before any fuzzy work", "claude-opus-5", "claude-opus-5", autoCorrectOK},
		{"an exact id wins even when it is also a prefix of others", "gpt-5", "gpt-5", autoCorrectOK},
		{"single substring match", "haiku", "haiku", autoCorrectUnknown},
		{"single candidate", "opus-4.5", "claude-opus-4.5", autoCorrectCorrected},
		{"several versions of one base pick the highest", "pt-5", "gpt-5.6", autoCorrectCorrected},
		{"several sonnet versions pick the highest", "sonnet-4", "claude-sonnet-4.6", autoCorrectCorrected},
		{"ambiguous across bases", "claude", "claude", autoCorrectUnknown},
		{"too short to fuzzy match", "gp", "gp", autoCorrectUnknown},
		{"no match at all", "llama-70b", "llama-70b", autoCorrectUnknown},
		{"case insensitive", "OPUS-5", "claude-opus-5", autoCorrectCorrected},
		{"empty catalog accepts anything", "whatever", "whatever", autoCorrectOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			avail := available
			if tc.name == "empty catalog accepts anything" {
				avail = nil
			}
			gotID, gotStatus := autoCorrect(tc.requested, avail)
			if gotStatus != tc.wantStatus {
				t.Errorf("status = %v, want %v", gotStatus, tc.wantStatus)
			}
			if gotID != tc.wantID {
				t.Errorf("id = %q, want %q", gotID, tc.wantID)
			}
		})
	}
}

func TestParseAndCompareVersions(t *testing.T) {
	if got := parseVersion("gpt-5.6"); !reflect.DeepEqual(got, []int{5, 6}) {
		t.Errorf("parseVersion(gpt-5.6) = %v, want [5 6]", got)
	}
	if got := parseVersion("claude-opus-4.5"); !reflect.DeepEqual(got, []int{4, 5}) {
		t.Errorf("parseVersion(claude-opus-4.5) = %v", got)
	}
	if got := parseVersion("qwen3-coder-next"); len(got) != 0 {
		t.Errorf("parseVersion of a name with no trailing version = %v, want empty", got)
	}

	cases := []struct {
		a, b []int
		want int
	}{
		{[]int{5, 6}, []int{5}, 1},
		{[]int{5}, []int{5, 6}, -1},
		{[]int{5, 0}, []int{5}, 0},
		{[]int{4, 5}, []int{4, 6}, -1},
		{[]int{10}, []int{9}, 1},
		{nil, nil, 0},
		{[]int{1}, nil, 1},
	}
	for _, tc := range cases {
		got := compareVersions(tc.a, tc.b)
		if (got > 0) != (tc.want > 0) || (got < 0) != (tc.want < 0) || (got == 0) != (tc.want == 0) {
			t.Errorf("compareVersions(%v, %v) = %d, want sign of %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestResolveExactMatch(t *testing.T) {
	c := newTestCatalog(t)
	res := c.Resolve("claude-opus-5", "", "")

	if res.ModelID != "claude-opus-5" {
		t.Errorf("ModelID = %q", res.ModelID)
	}
	if res.Source != SourceExact {
		t.Errorf("Source = %q, want exact", res.Source)
	}
	if res.Requested != "claude-opus-5" {
		t.Errorf("Requested = %q, want the client's original string", res.Requested)
	}
	if res.Model == nil {
		t.Fatal("Model should be populated for a catalog hit")
	}
	if res.Model.MaxInputTokens != 1000000 {
		t.Errorf("MaxInputTokens = %d, want 1000000", res.Model.MaxInputTokens)
	}
}

func TestResolveNormalisesBeforeLookup(t *testing.T) {
	c := newTestCatalog(t)
	res := c.Resolve("claude-sonnet-4-5-20250929", "", "")

	if res.Normalized != "claude-sonnet-4.5" {
		t.Errorf("Normalized = %q", res.Normalized)
	}
	if res.ModelID != "claude-sonnet-4.5" {
		t.Errorf("ModelID = %q", res.ModelID)
	}
	if res.Source != SourceExact {
		t.Errorf("Source = %q, want exact", res.Source)
	}
}

func TestResolveAliasAndHiddenModel(t *testing.T) {
	c := newTestCatalog(t)

	res := c.Resolve("auto-kiro", "", "")
	if res.ModelID != "auto" {
		t.Errorf("ModelID = %q, want auto", res.ModelID)
	}
	if res.Source != SourceAlias {
		t.Errorf("Source = %q, want alias", res.Source)
	}

	// The underlying id still works when asked for directly.
	direct := c.Resolve("auto", "", "")
	if direct.ModelID != "auto" {
		t.Errorf("direct auto ModelID = %q", direct.ModelID)
	}

	// But it must not be advertised.
	for _, e := range c.Listing(true) {
		if e.ID == "auto" {
			t.Error("auto must be hidden from the listing in favour of auto-kiro")
		}
	}
	found := false
	for _, e := range c.Listing(true) {
		if e.ID == "auto-kiro" {
			found = true
		}
	}
	if !found {
		t.Error("auto-kiro should be advertised")
	}
}

func TestResolveAutoCorrects(t *testing.T) {
	c := newTestCatalog(t)
	cases := []struct {
		requested string
		want      string
	}{
		{"sonnet-4.5", "claude-sonnet-4.5"},
		{"opus-4.5", "claude-opus-4.5"},
		{"haiku-4.5", "claude-haiku-4.5"},
	}
	for _, tc := range cases {
		t.Run(tc.requested, func(t *testing.T) {
			res := c.Resolve(tc.requested, "", "")
			if res.ModelID != tc.want {
				t.Errorf("ModelID = %q, want %q", res.ModelID, tc.want)
			}
			if res.Source != SourceCorrected {
				t.Errorf("Source = %q, want corrected", res.Source)
			}
		})
	}
}

func TestResolveAutoCorrectPicksTheNewestVersion(t *testing.T) {
	c := newTestCatalog(t)
	// gpt-5 matches both gpt-5 and gpt-5.6, but gpt-5 is an exact id so it wins
	// at step 3 without reaching the correction step.
	if res := c.Resolve("gpt-5", "", ""); res.ModelID != "gpt-5" || res.Source != SourceExact {
		t.Errorf("gpt-5 resolved to %q via %q, want an exact match", res.ModelID, res.Source)
	}
	// A name that is not itself an id does reach the correction step.
	if res := c.Resolve("pt-5", "", ""); res.ModelID != "gpt-5.6" || res.Source != SourceCorrected {
		t.Errorf("pt-5 resolved to %q via %q, want gpt-5.6 by correction", res.ModelID, res.Source)
	}
}

func TestResolveUnknownModelPassesThrough(t *testing.T) {
	c := newTestCatalog(t)
	res := c.Resolve("llama-3-70b-instruct", "", "")

	if res.ModelID != "llama-3-70b-instruct" {
		t.Errorf("ModelID = %q, want the name unchanged", res.ModelID)
	}
	if res.Source != SourcePassthrough {
		t.Errorf("Source = %q, want passthrough", res.Source)
	}
	if res.Model != nil {
		t.Error("Model should be nil for a pass-through")
	}
	if res.EffortLevel != "" {
		t.Errorf("EffortLevel = %q, want empty: no schema path is known", res.EffortLevel)
	}
}

func TestResolveAmbiguousNamePassesThroughRatherThanGuessing(t *testing.T) {
	c := newTestCatalog(t)
	res := c.Resolve("claude", "", "")
	if res.Source != SourcePassthrough {
		t.Errorf("Source = %q, want passthrough for an ambiguous name", res.Source)
	}
	if res.ModelID != "claude" {
		t.Errorf("ModelID = %q, want the name unchanged", res.ModelID)
	}
}

func TestResolveEffortFromSuffix(t *testing.T) {
	c := newTestCatalog(t)
	res := c.Resolve("claude-opus-5:max", "", "")

	if res.ModelID != "claude-opus-5" {
		t.Errorf("ModelID = %q", res.ModelID)
	}
	if res.EffortLevel != "max" {
		t.Errorf("EffortLevel = %q, want max", res.EffortLevel)
	}
	if res.EffortSchemaPath != "reasoning" {
		t.Errorf("EffortSchemaPath = %q, want reasoning", res.EffortSchemaPath)
	}

	fields := res.AdditionalModelRequestFields()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"reasoning":{"effort":"max"}}` {
		t.Errorf("additionalModelRequestFields = %s", data)
	}
}

func TestResolveEffortUsesTheModelsSchemaPath(t *testing.T) {
	c := newTestCatalog(t)
	res := c.Resolve("gpt-5:high", "", "")

	if res.EffortSchemaPath != "output_config" {
		t.Errorf("EffortSchemaPath = %q, want output_config for this model", res.EffortSchemaPath)
	}
	data, _ := json.Marshal(res.AdditionalModelRequestFields())
	if string(data) != `{"output_config":{"effort":"high"}}` {
		t.Errorf("fields = %s", data)
	}
}

func TestEffortPrecedence(t *testing.T) {
	c := newTestCatalog(t)
	cases := []struct {
		name            string
		requested       string
		requestEffort   string
		operatorDefault string
		want            string
	}{
		{"suffix beats everything", "claude-opus-5:low", "high", "max", "low"},
		{"request field beats the operator default", "claude-opus-5", "medium", "max", "medium"},
		{"operator default beats the model default", "claude-opus-5", "", "low", "low"},
		{"model default when nothing else is set", "claude-opus-5", "", "", "xhigh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := c.Resolve(tc.requested, tc.requestEffort, tc.operatorDefault)
			if res.EffortLevel != tc.want {
				t.Errorf("EffortLevel = %q, want %q", res.EffortLevel, tc.want)
			}
		})
	}
}

func TestUnsupportedEffortLevelIsClampedToTheModelDefault(t *testing.T) {
	c := newTestCatalog(t)
	// gpt-5 advertises only low and high.
	res := c.Resolve("gpt-5:max", "", "")
	if res.EffortLevel != "high" {
		t.Errorf("EffortLevel = %q, want it clamped to the model default high", res.EffortLevel)
	}
	if !contains(res.Model.EffortLevels(), res.EffortLevel) {
		t.Errorf("EffortLevel %q is not advertised by the model %v", res.EffortLevel, res.Model.EffortLevels())
	}
}

func TestClampFallsBackToTheLastLevelWhenTheDefaultIsUnusable(t *testing.T) {
	specs := []kiro.ModelSpec{{
		ModelID: "weird-model",
		// A default that is not in its own enum.
		AdditionalModelRequestFieldsSchema: effortSchema("reasoning", []string{"low", "medium"}, "max"),
	}}
	c := newTestCatalog(t, specs...)
	res := c.Resolve("weird-model:max", "", "")
	if res.EffortLevel != "medium" {
		t.Errorf("EffortLevel = %q, want the last advertised level medium", res.EffortLevel)
	}
}

func TestEffortIsNeverSentForModelsWithoutSupport(t *testing.T) {
	c := newTestCatalog(t)
	for _, model := range []string{"claude-sonnet-4.5", "deepseek-3.2", "glm-5", "qwen3-coder-next"} {
		t.Run(model, func(t *testing.T) {
			res := c.Resolve(model+":max", "max", "max")
			if res.EffortLevel != "" {
				t.Errorf("EffortLevel = %q, want empty for a model with no effort schema", res.EffortLevel)
			}
			if res.AdditionalModelRequestFields() != nil {
				t.Error("additionalModelRequestFields should be nil")
			}
		})
	}
}

func TestEffortIsNeverSentForAuto(t *testing.T) {
	specs := append(testCatalogModels(), kiro.ModelSpec{
		ModelID: "auto",
		// Even if the backend advertised effort for auto, it must not be sent.
		AdditionalModelRequestFieldsSchema: effortSchema("reasoning", []string{"low", "high"}, "high"),
	})
	c := newTestCatalog(t, specs...)

	for _, name := range []string{"auto", "auto:max", "auto-kiro", "auto-kiro:high"} {
		t.Run(name, func(t *testing.T) {
			res := c.Resolve(name, "high", "max")
			if res.ModelID != "auto" {
				t.Errorf("ModelID = %q, want auto", res.ModelID)
			}
			if res.EffortLevel != "" {
				t.Errorf("EffortLevel = %q, want empty: auto never takes an effort", res.EffortLevel)
			}
		})
	}
}

func TestInvalidEffortStringsAreIgnored(t *testing.T) {
	c := newTestCatalog(t)
	// A nonsense request-level effort falls through to the model default rather
	// than being sent verbatim.
	res := c.Resolve("claude-opus-5", "turbo", "")
	if res.EffortLevel != "xhigh" {
		t.Errorf("EffortLevel = %q, want the model default xhigh", res.EffortLevel)
	}
}

func TestResolveEffortDirectly(t *testing.T) {
	withEffort := &Model{ID: "m", effort: &EffortSupport{
		Levels: []string{"low", "high"}, SchemaPath: "reasoning", DefaultLevel: "high"}}
	noEffort := &Model{ID: "plain"}
	autoModel := &Model{ID: AutoModelID, effort: &EffortSupport{
		Levels: []string{"low"}, SchemaPath: "reasoning", DefaultLevel: "low"}}

	cases := []struct {
		name                             string
		model                            *Model
		suffix, request, operatorDefault string
		want                             string
	}{
		{"suffix wins", withEffort, "low", "high", "high", "low"},
		{"request next", withEffort, "", "low", "high", "low"},
		{"operator next", withEffort, "", "", "low", "low"},
		{"model default last", withEffort, "", "", "", "high"},
		{"unsupported clamps", withEffort, "max", "", "", "high"},
		{"no support", noEffort, "high", "high", "high", ""},
		{"nil model", nil, "high", "", "", ""},
		{"auto never", autoModel, "low", "", "", ""},
		{"case and space tolerated", withEffort, " HIGH ", "", "", "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveEffort(tc.model, tc.suffix, tc.request, tc.operatorDefault); got != tc.want {
				t.Errorf("ResolveEffort() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSuggestModelsStaysWithinTheFamily(t *testing.T) {
	c := newTestCatalog(t)

	opus := c.SuggestModels("claude-opus-9")
	for _, id := range opus {
		if !strings.Contains(id, "opus") {
			t.Errorf("an opus request was answered with %q", id)
		}
	}
	if len(opus) == 0 {
		t.Error("expected some opus suggestions")
	}

	// A name with no recognisable family falls back to the whole list.
	all := c.SuggestModels("something-else")
	if len(all) != c.Len() {
		t.Errorf("got %d suggestions, want the full catalog of %d", len(all), c.Len())
	}
}

func TestExtractFamily(t *testing.T) {
	cases := map[string]string{
		"claude-opus-5":    "opus",
		"claude-sonnet-4":  "sonnet",
		"claude-haiku-4.5": "haiku",
		"gpt-5":            "gpt",
		"deepseek-3.2":     "deepseek",
		"glm-5":            "glm",
		"minimax-m2.5":     "minimax",
		"qwen3-coder-next": "qwen",
		"llama-70b":        "",
		"":                 "",
	}
	for in, want := range cases {
		if got := extractFamily(in); got != want {
			t.Errorf("extractFamily(%q) = %q, want %q", in, got, want)
		}
	}
}

// contains reports whether list holds s.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
