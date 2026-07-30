// Package catalog discovers the live Kiro model catalog, resolves the model
// names clients send into real model ids, and works out the reasoning effort to
// request.
package catalog

import "strings"

// EffortLevels is the allowlist of reasoning effort levels the backend accepts,
// verified against the Kiro IDE bundle.
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// EffortNone turns reasoning off.
//
// It is deliberately absent from EffortLevels, because the Kiro IDE's own global
// list does not contain it. Several models advertise it in their individual
// schema regardless, and the per-model schema is the authority here: it is also
// what decides whether effort nests under output_config or reasoning. So "none"
// is accepted for a model that advertises it and refused for one that does not.
const EffortNone = "none"

// GlobalDefaultEffort matches the Kiro IDE default.
const GlobalDefaultEffort = "xhigh"

// AutoModelID is the backend's automatic model selector. It never receives a
// reasoning effort field.
const AutoModelID = "auto"

// effortRank orders every level kirogo knows from least reasoning to most, so a
// request it cannot satisfy exactly can be answered with the nearest level a
// model does offer.
var effortRank = []string{EffortNone, "low", "medium", "high", "xhigh", "max"}

// IsValidEffortLevel reports whether level is one kirogo recognises.
func IsValidEffortLevel(level string) bool {
	if level == EffortNone {
		return true
	}
	for _, l := range EffortLevels {
		if l == level {
			return true
		}
	}
	return false
}

// effortSchemaPaths are probed in order. The first location with a non-empty
// enum wins, matching the Kiro IDE's own probe order.
var effortSchemaPaths = []string{"output_config", "reasoning"}

// EffortSupport describes a model's reasoning effort capability.
type EffortSupport struct {
	// Levels are the levels this model advertises, in the order given.
	Levels []string
	// SchemaPath is the key to nest the effort field under, either
	// "output_config" or "reasoning".
	SchemaPath string
	// DefaultLevel is the model's own default, which may be empty.
	DefaultLevel string
}

// Supported reports whether the model accepts a reasoning effort at all.
func (e *EffortSupport) Supported() bool {
	return e != nil && e.SchemaPath != "" && len(e.Levels) > 0
}

// Allows reports whether level is one of the model's advertised levels.
func (e *EffortSupport) Allows(level string) bool {
	if e == nil {
		return false
	}
	for _, l := range e.Levels {
		if l == level {
			return true
		}
	}
	return false
}

// LeastLevel returns the lowest effort this model advertises, or an empty string
// when it advertises nothing kirogo recognises.
//
// The model's own enum order is not trusted for this; the canonical ranking is,
// so a model listing its levels in an unexpected order still gets the right answer.
func (e *EffortSupport) LeastLevel() string {
	if e == nil {
		return ""
	}
	for _, level := range effortRank {
		if e.Allows(level) {
			return level
		}
	}
	return ""
}

// extractEffortSupport probes a model's additionalModelRequestFieldsSchema for a
// reasoning effort enum.
//
// The schema is walked as schema.properties.<path>.properties.effort, trying
// "output_config" then "reasoning". A location counts only when it carries a
// non-empty enum. A nil result means the model has no effort support.
func extractEffortSupport(schema map[string]any) *EffortSupport {
	if len(schema) == 0 {
		return nil
	}
	topProperties, ok := objectAt(schema, "properties")
	if !ok {
		return nil
	}

	for _, path := range effortSchemaPaths {
		container, ok := objectAt(topProperties, path)
		if !ok {
			continue
		}
		containerProperties, ok := objectAt(container, "properties")
		if !ok {
			continue
		}
		effort, ok := objectAt(containerProperties, "effort")
		if !ok {
			continue
		}

		levels := stringsAt(effort, "enum")
		if len(levels) == 0 {
			continue
		}

		support := &EffortSupport{Levels: levels, SchemaPath: path}
		if def, ok := effort["default"].(string); ok {
			support.DefaultLevel = def
		}
		return support
	}
	return nil
}

// objectAt returns a nested JSON object by key.
func objectAt(m map[string]any, key string) (map[string]any, bool) {
	v, ok := m[key]
	if !ok {
		return nil, false
	}
	obj, ok := v.(map[string]any)
	return obj, ok
}

// stringsAt returns a nested array of strings by key, skipping non-string
// entries rather than failing: a malformed enum should degrade, not crash.
func stringsAt(m map[string]any, key string) []string {
	v, ok := m[key]
	if !ok {
		return nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ResolveEffort decides which effort level to send for a model.
//
// Precedence, highest first: a level pinned in the model name, the level the
// client asked for in the request body, the operator's KIRO_EFFORT_LEVEL, then
// the model's own default. A level the model does not advertise is clamped to
// that model's default rather than being sent and rejected.
//
// It returns an empty string when no effort should be sent at all.
func ResolveEffort(model *Model, suffixLevel, requestLevel, operatorDefault string) string {
	// The automatic selector never takes an effort field.
	if model != nil && model.ID == AutoModelID {
		return ""
	}
	support := model.Effort()
	if !support.Supported() {
		return ""
	}

	for _, candidate := range []string{suffixLevel, requestLevel, operatorDefault, support.DefaultLevel} {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if candidate == "" {
			continue
		}
		if support.Allows(candidate) {
			return candidate
		}
		// A request to turn reasoning off that this model cannot express. Letting
		// it fall through to the model's default would answer with the opposite of
		// what was asked, because every default the backend advertises is "high" or
		// above, so the least effort this model does offer is the honest reply.
		if candidate == EffortNone {
			return support.LeastLevel()
		}
		// Asked for something real but unsupported by this model: fall back to
		// the model's own default so the request still carries an effort.
		if IsValidEffortLevel(candidate) {
			if support.Allows(support.DefaultLevel) {
				return support.DefaultLevel
			}
			return support.Levels[len(support.Levels)-1]
		}
	}
	return ""
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
