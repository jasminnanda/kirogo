package translate

// SanitizeJSONSchema removes the JSON Schema constructs the Kiro backend rejects.
//
// Two rules, both learned from the backend answering "Improperly formed request."
// with no further detail:
//
//   - required is dropped when it is an empty array. A present-but-empty required
//     list is a validation failure, while omitting it is fine.
//   - additionalProperties is dropped entirely. The backend does not accept it in
//     any form, true, false or a nested schema.
//
// The walk is recursive through every nested object and array, so the rules apply
// inside properties, items, anyOf, oneOf, allOf, $defs and anything else a client
// nests. A nil or empty schema becomes an empty object, because the backend wants
// an object rather than null.
func SanitizeJSONSchema(schema map[string]any) map[string]any {
	if len(schema) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(schema))
	for key, value := range schema {
		switch key {
		case "required":
			// Drop only when it is an empty array; a populated one is required.
			if arr, ok := value.([]any); ok && len(arr) == 0 {
				continue
			}
			if arr, ok := value.([]string); ok && len(arr) == 0 {
				continue
			}
			out[key] = sanitizeValue(value)
		case "additionalProperties":
			// Never forwarded, in any form.
			continue
		default:
			out[key] = sanitizeValue(value)
		}
	}
	return out
}

// sanitizeValue applies the schema rules to any JSON value.
func sanitizeValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return SanitizeJSONSchema(v)
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, sanitizeValue(item))
		}
		return out
	default:
		return value
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
