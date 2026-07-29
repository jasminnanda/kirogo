package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeJSONSchema(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "nil becomes an empty object",
			in:   `null`,
			want: `{}`,
		},
		{
			name: "empty stays empty",
			in:   `{}`,
			want: `{}`,
		},
		{
			name: "empty required is dropped",
			in:   `{"type":"object","required":[]}`,
			want: `{"type":"object"}`,
		},
		{
			name: "populated required is kept",
			in:   `{"type":"object","required":["a","b"]}`,
			want: `{"required":["a","b"],"type":"object"}`,
		},
		{
			name: "additionalProperties false is dropped",
			in:   `{"type":"object","additionalProperties":false}`,
			want: `{"type":"object"}`,
		},
		{
			name: "additionalProperties true is dropped",
			in:   `{"type":"object","additionalProperties":true}`,
			want: `{"type":"object"}`,
		},
		{
			name: "additionalProperties schema is dropped",
			in:   `{"type":"object","additionalProperties":{"type":"string"}}`,
			want: `{"type":"object"}`,
		},
		{
			name: "recurses through properties",
			in:   `{"properties":{"a":{"type":"object","required":[],"additionalProperties":false}}}`,
			want: `{"properties":{"a":{"type":"object"}}}`,
		},
		{
			name: "recurses through anyOf",
			in:   `{"anyOf":[{"required":[],"type":"string"},{"additionalProperties":false,"type":"number"}]}`,
			want: `{"anyOf":[{"type":"string"},{"type":"number"}]}`,
		},
		{
			name: "recurses through oneOf",
			in:   `{"oneOf":[{"additionalProperties":true}]}`,
			want: `{"oneOf":[{}]}`,
		},
		{
			name: "recurses through allOf",
			in:   `{"allOf":[{"required":[]}]}`,
			want: `{"allOf":[{}]}`,
		},
		{
			name: "recurses through items",
			in:   `{"type":"array","items":{"additionalProperties":false,"type":"object"}}`,
			want: `{"items":{"type":"object"},"type":"array"}`,
		},
		{
			name: "recurses through $defs",
			in:   `{"$defs":{"Thing":{"additionalProperties":false,"required":[]}}}`,
			want: `{"$defs":{"Thing":{}}}`,
		},
		{
			name: "deeply nested",
			in: `{"properties":{"a":{"properties":{"b":{"anyOf":[{"items":{"additionalProperties":false,
			      "properties":{"c":{"required":[]}}}}]}}}}}`,
			want: `{"properties":{"a":{"properties":{"b":{"anyOf":[{"items":{"properties":{"c":{}}}}]}}}}}`,
		},
		{
			name: "unrelated keys survive untouched",
			in:   `{"type":"object","description":"d","enum":["x"],"default":1,"minimum":0,"format":"date"}`,
			want: `{"default":1,"description":"d","enum":["x"],"format":"date","minimum":0,"type":"object"}`,
		},
		{
			name: "required inside properties is handled independently",
			in:   `{"required":["outer"],"properties":{"inner":{"required":[]}}}`,
			want: `{"properties":{"inner":{}},"required":["outer"]}`,
		},
		{
			name: "a property literally named additionalProperties is still dropped",
			in:   `{"properties":{"additionalProperties":{"type":"string"}}}`,
			want: `{"properties":{}}`,
		},
		{
			name: "arrays of non-objects are preserved",
			in:   `{"enum":[1,"two",true,null]}`,
			want: `{"enum":[1,"two",true,null]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var in map[string]any
			if err := json.Unmarshal([]byte(tc.in), &in); err != nil {
				t.Fatalf("bad fixture: %v", err)
			}
			got := SanitizeJSONSchema(in)
			data, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.want {
				t.Errorf("SanitizeJSONSchema =\n %s\nwant\n %s", data, tc.want)
			}
		})
	}
}

func TestSanitizeHandlesStringSliceRequired(t *testing.T) {
	// A caller building a schema in Go may use []string rather than []any.
	in := map[string]any{"type": "object", "required": []string{}}
	got := SanitizeJSONSchema(in)
	if _, present := got["required"]; present {
		t.Errorf("an empty []string required should be dropped, got %v", got)
	}

	populated := map[string]any{"required": []string{"a"}}
	got = SanitizeJSONSchema(populated)
	if _, present := got["required"]; !present {
		t.Error("a populated []string required should be kept")
	}
}

func TestSanitizeDoesNotMutateTheInput(t *testing.T) {
	in := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{},
		"properties": map[string]any{
			"a": map[string]any{"additionalProperties": true},
		},
	}
	before, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}

	SanitizeJSONSchema(in)

	after, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("the input schema was mutated:\nbefore %s\nafter  %s", before, after)
	}
}

func TestSanitizeRealWorldClaudeCodeSchema(t *testing.T) {
	// A shape close to what Claude Code ships: nested objects, arrays, unions and
	// additionalProperties at several depths.
	raw := `{
	  "type": "object",
	  "additionalProperties": false,
	  "required": ["file_path"],
	  "properties": {
	    "file_path": {"type": "string", "description": "Absolute path"},
	    "edits": {
	      "type": "array",
	      "items": {
	        "type": "object",
	        "additionalProperties": false,
	        "required": ["old_string", "new_string"],
	        "properties": {
	          "old_string": {"type": "string"},
	          "new_string": {"type": "string"},
	          "replace_all": {"type": "boolean", "default": false}
	        }
	      }
	    },
	    "mode": {"anyOf": [
	      {"type": "string", "enum": ["a", "b"]},
	      {"type": "object", "additionalProperties": false, "required": []}
	    ]}
	  }
	}`
	var in map[string]any
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}

	out, err := json.Marshal(SanitizeJSONSchema(in))
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)

	if strings.Contains(body, "additionalProperties") {
		t.Errorf("additionalProperties survived at some depth: %s", body)
	}
	if strings.Contains(body, `"required":[]`) {
		t.Errorf("an empty required survived: %s", body)
	}
	// The populated required lists and all real content must survive.
	for _, want := range []string{
		`"required":["file_path"]`,
		`"required":["old_string","new_string"]`,
		`"description":"Absolute path"`,
		`"default":false`,
		`"enum":["a","b"]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q to survive, got %s", want, body)
		}
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
