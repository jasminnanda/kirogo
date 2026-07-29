package catalog

import (
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Source records which resolution step produced a model id.
type Source string

// Resolution sources, in pipeline order.
const (
	// SourceAlias means a hardcoded alias matched.
	SourceAlias Source = "alias"
	// SourceExact means the normalised name was found in the catalog.
	SourceExact Source = "exact"
	// SourceCorrected means the backend's own fuzzy correction picked a model.
	SourceCorrected Source = "corrected"
	// SourcePassthrough means the name was sent upstream unchanged.
	SourcePassthrough Source = "passthrough"
)

// Resolution is the outcome of resolving a client-supplied model name.
type Resolution struct {
	// ModelID is what to send upstream.
	ModelID string
	// Requested is the untouched name the client sent, echoed back in responses.
	Requested string
	// Normalized is the name after regex normalisation.
	Normalized string
	// Source records which step decided the answer.
	Source Source
	// Model is the catalog entry, nil for a pass-through.
	Model *Model
	// EffortLevel is the reasoning effort to send, empty when none applies.
	EffortLevel string
	// EffortSchemaPath is the key to nest the effort under, empty when no effort.
	EffortSchemaPath string
	// EffortPinnedBySuffix records that the level came from the model name.
	EffortPinnedBySuffix bool
}

// AdditionalModelRequestFields builds the request document for this resolution's
// effort, or nil when no effort applies.
func (r *Resolution) AdditionalModelRequestFields() map[string]any {
	if r.EffortLevel == "" || r.EffortSchemaPath == "" {
		return nil
	}
	return map[string]any{
		r.EffortSchemaPath: map[string]any{"effort": r.EffortLevel},
	}
}

// contextSuffixPattern strips a client-side context-window marker such as [1m]
// or [200k], which is a display hint rather than part of the model id.
var contextSuffixPattern = regexp.MustCompile(`(?i)\[\d+[mk]\]$`)

// Normalisation patterns, ported verbatim from the reference gateway. The first
// match wins. They only understand claude-* names; anything else falls through
// unchanged, which is correct for newer ids such as claude-opus-5, gpt-*,
// deepseek-3.2, glm-5, minimax-m2.5 and qwen3-coder-next.
var (
	// P1: claude-haiku-4-5 and claude-haiku-4-5-20251001 become claude-haiku-4.5.
	// The minor version is 1 to 2 digits, so an 8-digit date cannot match it.
	patternStandard = regexp.MustCompile(`^(claude-(?:haiku|sonnet|opus)-\d+)-(\d{1,2})(?:-(?:\d{8}|latest|\d+))?$`)
	// P2: claude-sonnet-4-20250514 becomes claude-sonnet-4.
	patternNoMinor = regexp.MustCompile(`^(claude-(?:haiku|sonnet|opus)-\d+)(?:-\d{8})?$`)
	// P3: claude-3-7-sonnet becomes claude-3.7-sonnet.
	patternLegacy = regexp.MustCompile(`^(claude)-(\d+)-(\d+)-(haiku|sonnet|opus)(?:-(?:\d{8}|latest|\d+))?$`)
	// P4: claude-haiku-4.5-20251001 becomes claude-haiku-4.5.
	patternDotWithDate = regexp.MustCompile(`^(claude-(?:\d+\.\d+-)?(?:haiku|sonnet|opus)(?:-\d+\.\d+)?)-\d{8}$`)
	// P5: claude-4.5-opus-high becomes claude-opus-4.5. A suffix is required, so
	// an already-normalised name such as claude-3.7-sonnet cannot match.
	patternInverted = regexp.MustCompile(`^claude-(\d+)\.(\d+)-(haiku|sonnet|opus)-(.+)$`)
)

// NormalizeModelName converts a client model name into Kiro's format.
//
// A name that matches no pattern is returned unchanged, with its original case
// preserved so pass-through stays faithful.
func NormalizeModelName(name string) string {
	if name == "" {
		return name
	}

	name = contextSuffixPattern.ReplaceAllString(name, "")
	lower := strings.ToLower(name)

	if m := patternStandard.FindStringSubmatch(lower); m != nil {
		return m[1] + "." + m[2]
	}
	if m := patternNoMinor.FindStringSubmatch(lower); m != nil {
		return m[1]
	}
	if m := patternLegacy.FindStringSubmatch(lower); m != nil {
		return m[1] + "-" + m[2] + "." + m[3] + "-" + m[4]
	}
	if m := patternDotWithDate.FindStringSubmatch(lower); m != nil {
		return m[1]
	}
	if m := patternInverted.FindStringSubmatch(lower); m != nil {
		return "claude-" + m[3] + "-" + m[1] + "." + m[2]
	}
	return name
}

// SplitEffortSuffix separates a pinned effort level from a model name.
//
// It accepts "model:level" and "model-level". This must run before
// normalisation: the inverted-name pattern would otherwise swallow a trailing
// "-high" as part of the model name.
//
// The returned level is empty when no recognised level was pinned.
func SplitEffortSuffix(name string) (model, level string) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return name, ""
	}

	// Prefer the explicit colon form, splitting on the last colon so a name
	// containing colons still works.
	if i := strings.LastIndex(trimmed, ":"); i > 0 && i < len(trimmed)-1 {
		candidate := strings.ToLower(trimmed[i+1:])
		if IsValidEffortLevel(candidate) {
			return trimmed[:i], candidate
		}
	}
	if i := strings.LastIndex(trimmed, "-"); i > 0 && i < len(trimmed)-1 {
		candidate := strings.ToLower(trimmed[i+1:])
		if IsValidEffortLevel(candidate) {
			return trimmed[:i], candidate
		}
	}
	return trimmed, ""
}

// trailingVersionPattern captures a trailing version such as "5" or "4.5",
// matching the Kiro IDE's own regex.
var trailingVersionPattern = regexp.MustCompile(`(\d+(?:\.\d+)*)\s*$`)

// autoCorrectStatus is the outcome of the backend's fuzzy model matching.
type autoCorrectStatus int

const (
	// autoCorrectOK means the name is already usable.
	autoCorrectOK autoCorrectStatus = iota
	// autoCorrectCorrected means a single best match was found.
	autoCorrectCorrected
	// autoCorrectUnknown means no confident match exists.
	autoCorrectUnknown
)

// autoCorrect reproduces the Kiro IDE's model name correction.
//
// It is what makes "sonnet-4.5" resolve to claude-sonnet-4.5 and "gpt-5" resolve
// to the newest gpt-5 build. IDEs let users type arbitrary model strings, so
// without this a plausible name would be rejected by the backend.
//
// The rules: "auto" and any exact id are accepted as-is. Otherwise, for a name of
// at least three characters, every id containing it as a case-insensitive
// substring is a candidate. One candidate wins outright. Several candidates win
// only if they all share a single base once the trailing version is stripped, in
// which case the highest version is chosen. Anything else is unknown.
func autoCorrect(requested string, available []string) (string, autoCorrectStatus) {
	if requested == "" || requested == AutoModelID {
		return requested, autoCorrectOK
	}
	if len(available) == 0 {
		return requested, autoCorrectOK
	}
	for _, id := range available {
		if id == requested {
			return requested, autoCorrectOK
		}
	}

	lower := strings.ToLower(requested)
	if len(lower) < 3 {
		return requested, autoCorrectUnknown
	}

	var candidates []string
	for _, id := range available {
		if strings.Contains(strings.ToLower(id), lower) {
			candidates = append(candidates, id)
		}
	}

	switch len(candidates) {
	case 0:
		return requested, autoCorrectUnknown
	case 1:
		return candidates[0], autoCorrectCorrected
	}

	// Several matches: acceptable only when they are versions of one model.
	bases := map[string]bool{}
	for _, id := range candidates {
		bases[trailingVersionPattern.ReplaceAllString(id, "")] = true
	}
	if len(bases) != 1 {
		return requested, autoCorrectUnknown
	}

	best := candidates[0]
	for _, id := range candidates[1:] {
		if compareVersions(parseVersion(id), parseVersion(best)) > 0 {
			best = id
		}
	}
	return best, autoCorrectCorrected
}

// parseVersion extracts the trailing version of an id as numeric components.
func parseVersion(id string) []int {
	m := trailingVersionPattern.FindStringSubmatch(id)
	if m == nil {
		return nil
	}
	parts := strings.Split(m[1], ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out
		}
		out = append(out, n)
	}
	return out
}

// compareVersions compares version components element by element, treating a
// missing component as zero.
func compareVersions(a, b []int) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		var ai, bi int
		if i < len(a) {
			ai = a[i]
		}
		if i < len(b) {
			bi = b[i]
		}
		if d := ai - bi; d != 0 {
			return d
		}
	}
	return 0
}

// Resolve turns a client model name into a model id plus a reasoning effort.
//
// The pipeline, in this order:
//
//  0. Split a pinned effort suffix off the name.
//  1. Apply the hardcoded aliases.
//  2. Apply the normalisation patterns.
//  3. Look the result up in the catalog.
//  4. Fall back to the backend's own fuzzy correction.
//  5. Pass the name upstream unchanged and let the backend decide.
//
// It never fails. Step 5 exists because kirogo is a gateway, not a gatekeeper:
// the backend is the authority on which models exist, and refusing a name kirogo
// has not heard of would break the day a new model ships.
func (c *Catalog) Resolve(requested, requestEffort, operatorDefaultEffort string) Resolution {
	res := Resolution{Requested: requested}

	// Step 0: pinned effort suffix, before any pattern can eat it.
	nameWithoutEffort, suffixLevel := SplitEffortSuffix(requested)

	// Step 1: aliases, applied to both forms of the name.
	withoutEffort, aliasedA := applyAlias(nameWithoutEffort)
	fullName, aliasedB := applyAlias(requested)

	// Candidates are tried in order. Splitting the effort suffix first is
	// required, or pattern P5 would read a trailing "-high" as part of the model
	// name. But P5 only fires when a suffix is present, so the full name has to
	// stay in the running: "claude-4.5-opus-high" needs P5 to reach
	// claude-opus-4.5 while still pinning the effort to high.
	type candidate struct {
		name string
		// effort is the level to treat as pinned when this candidate wins.
		effort string
		alias  bool
	}
	candidates := []candidate{{name: withoutEffort, effort: suffixLevel, alias: aliasedA}}
	if suffixLevel != "" {
		// A model whose id genuinely ends in something like "-low" must win over
		// the effort reading, so the unsplit name is tried first for an exact
		// catalog hit, then again after normalisation.
		if _, ok := c.Lookup(fullName); ok {
			candidates = []candidate{{name: fullName, effort: "", alias: aliasedB}}
		} else {
			candidates = append(candidates, candidate{name: fullName, effort: suffixLevel, alias: aliasedB})
		}
	}

	res.Normalized = NormalizeModelName(candidates[0].name)

	// Step 2 and 3: normalise, then look the result up exactly.
	for _, cand := range candidates {
		normalized := NormalizeModelName(cand.name)
		model, ok := c.Lookup(normalized)
		if !ok {
			continue
		}
		res.ModelID = model.ID
		res.Model = model
		res.Normalized = normalized
		res.Source = SourceExact
		if cand.alias {
			res.Source = SourceAlias
		}
		res.EffortPinnedBySuffix = cand.effort != ""
		res.applyEffort(model, cand.effort, requestEffort, operatorDefaultEffort)
		return res
	}

	// Step 4: the backend's own fuzzy correction.
	available := c.IDs()
	for _, cand := range candidates {
		normalized := NormalizeModelName(cand.name)
		corrected, status := autoCorrect(normalized, available)
		if status != autoCorrectCorrected {
			continue
		}
		model, ok := c.Lookup(corrected)
		if !ok {
			continue
		}
		slog.Info("model name auto-corrected", "requested", requested, "resolved", corrected)
		res.ModelID = model.ID
		res.Model = model
		res.Normalized = normalized
		res.Source = SourceCorrected
		res.EffortPinnedBySuffix = cand.effort != ""
		res.applyEffort(model, cand.effort, requestEffort, operatorDefaultEffort)
		return res
	}

	// Step 5: pass-through. The backend decides, and its INVALID_MODEL_ID error
	// is more authoritative than any guess kirogo could make.
	slog.Info("model not in the catalog, passing it upstream unchanged",
		"requested", requested, "normalized", res.Normalized, "available", strings.Join(available, ", "))
	res.ModelID = res.Normalized
	res.Source = SourcePassthrough
	res.EffortPinnedBySuffix = suffixLevel != ""

	// Without catalog metadata there is no schema path, so no effort can be sent.
	if suffixLevel != "" {
		slog.Debug("effort suffix ignored for an unknown model: no schema path is known",
			"model", res.Normalized, "effort", suffixLevel)
	}
	return res
}

// applyAlias resolves a hardcoded alias, reporting whether one matched.
func applyAlias(name string) (string, bool) {
	if target, ok := Aliases[strings.ToLower(strings.TrimSpace(name))]; ok {
		slog.Debug("model alias applied", "from", name, "to", target)
		return target, true
	}
	return name, false
}

// applyEffort fills in the effort fields for a resolved model.
func (r *Resolution) applyEffort(model *Model, suffixLevel, requestLevel, operatorDefault string) {
	level := ResolveEffort(model, suffixLevel, requestLevel, operatorDefault)
	if level == "" {
		return
	}
	r.EffortLevel = level
	r.EffortSchemaPath = model.Effort().SchemaPath
}

// SuggestModels returns catalog ids related to a requested name, for an error
// message. It prefers ids sharing the same family word so an Opus request is not
// answered with Sonnet suggestions.
func (c *Catalog) SuggestModels(requested string) []string {
	ids := c.IDs()
	family := extractFamily(requested)
	if family == "" {
		return ids
	}
	var matches []string
	for _, id := range ids {
		if strings.Contains(strings.ToLower(id), family) {
			matches = append(matches, id)
		}
	}
	if len(matches) == 0 {
		return ids
	}
	sort.Strings(matches)
	return matches
}

// familyWords are the model family names worth matching on.
var familyWords = []string{"haiku", "sonnet", "opus", "gpt", "deepseek", "glm", "minimax", "qwen"}

// extractFamily finds a known family word inside a model name.
func extractFamily(name string) string {
	lower := strings.ToLower(name)
	for _, w := range familyWords {
		if strings.Contains(lower, w) {
			return w
		}
	}
	return ""
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
