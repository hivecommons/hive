package agentstep

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func mustSchema(t *testing.T, raw string) *Schema {
	t.Helper()
	s, err := ParseSchema(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("ParseSchema(%s): %v", raw, err)
	}
	return s
}

func paths(s *Schema, answer string) string {
	var out []string
	for _, v := range s.Validate(json.RawMessage(answer)) {
		out = append(out, v.Path)
	}
	return strings.Join(out, " ")
}

func TestSchemaValidatesTheSupportedKeywords(t *testing.T) {
	s := mustSchema(t, `{
		"type": "object",
		"required": ["verdict", "items"],
		"additionalProperties": false,
		"properties": {
			"verdict": {"enum": ["pass", "fail"]},
			"score": {"type": "number", "minimum": 0, "maximum": 1, "exclusiveMaximum": 1},
			"count": {"type": "integer", "multipleOf": 2},
			"name": {"type": "string", "minLength": 2, "maxLength": 4, "pattern": "^[a-z]+$"},
			"items": {"type": "array", "minItems": 1, "maxItems": 2, "items": {"type": "string"}},
			"flag": {"const": true},
			"any": true,
			"never": false,
			"nullable": {"type": ["string", "null"]},
			"nested": {"type": "object", "additionalProperties": {"type": "integer"}}
		}
	}`)
	for _, tc := range []struct{ answer, want string }{
		{`{"verdict":"pass","items":["a"]}`, ""},
		{`{"verdict":"pass","items":["a"],"score":0.5,"count":4,"name":"ab","flag":true,"any":[1],"nullable":null,"nested":{"x":1}}`, ""},
		{`{"verdict":"maybe","items":[]}`, "$.items $.verdict"},
		{`{"items":["a"]}`, "$.verdict"},
		{`{"verdict":"pass","items":["a"],"extra":1}`, "$.extra"},
		{`{"verdict":"pass","items":["a"],"score":1}`, "$.score"},
		{`{"verdict":"pass","items":["a"],"score":-1}`, "$.score"},
		{`{"verdict":"pass","items":["a"],"count":3}`, "$.count"},
		{`{"verdict":"pass","items":["a"],"count":2.5}`, "$.count"},
		{`{"verdict":"pass","items":["a"],"name":"a"}`, "$.name"},
		{`{"verdict":"pass","items":["a"],"name":"abcde"}`, "$.name"},
		{`{"verdict":"pass","items":["a"],"name":"AB"}`, "$.name"},
		{`{"verdict":"pass","items":["a","b","c"]}`, "$.items"},
		{`{"verdict":"pass","items":[1]}`, "$.items[0]"},
		{`{"verdict":"pass","items":["a"],"flag":false}`, "$.flag"},
		{`{"verdict":"pass","items":["a"],"never":1}`, "$.never"},
		{`{"verdict":"pass","items":["a"],"nullable":1}`, "$.nullable"},
		{`{"verdict":"pass","items":["a"],"nested":{"x":"y"}}`, "$.nested.x"},
		{`[]`, "$"},
		{`not json`, "$"},
	} {
		if got := paths(s, tc.answer); got != tc.want {
			t.Errorf("Validate(%s) violated %q, want %q", tc.answer, got, tc.want)
		}
	}
}

func TestSchemaParseRefusesWhatItCannotEnforce(t *testing.T) {
	for _, raw := range []string{
		``, `null`, `[]`, `{"type": "thing"}`, `{"type": 3}`, `{"$ref": "#/x"}`, `{"oneOf": []}`,
		`{"minimum": "1"}`, `{"minItems": -1}`, `{"pattern": "("}`, `{"properties": {"a": 1}}`,
		`{"required": "a"}`, `{"enum": 1}`, `{"additionalProperties": 1}`, `{"items": [1]}`,
		`{"multipleOf": 0}`,
	} {
		if _, err := ParseSchema(json.RawMessage(raw)); !errors.Is(err, ErrSchema) {
			t.Errorf("ParseSchema(%s) = %v, want ErrSchema", raw, err)
		}
	}
	if _, err := ParseSchema(json.RawMessage(`{"type":"string","` + strings.Repeat("x", MaxSchemaBytes) + `":1}`)); !errors.Is(err, ErrSchema) {
		t.Errorf("oversized schema: %v", err)
	}
	// Annotations are allowed and ignored.
	s := mustSchema(t, `{"$schema":"https://json-schema.org/draft/2020-12/schema","title":"t","description":"d","examples":[1],"default":1,"$id":"x","$comment":"c","type":"integer"}`)
	if got := paths(s, `1`); got != "" {
		t.Errorf("annotated schema violated %q", got)
	}
}

func TestCorrectivePromptNamesTheViolations(t *testing.T) {
	s := mustSchema(t, `{"type":"object","required":["a"]}`)
	v := s.Validate(json.RawMessage(`{}`))
	got := CorrectivePrompt(json.RawMessage(`{"type":"object","required":["a"]}`), v, 2)
	for _, want := range []string{`"required":["a"]`, "- $.a: ", "remaining after this one: 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("CorrectivePrompt lacks %q:\n%s", want, got)
		}
	}
}
