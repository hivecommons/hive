package evidence

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// validateSchema checks v against the subset of JSON Schema that v1.json
// uses: type, const, enum, required, properties, additionalProperties,
// items, $ref into $defs, minLength, minimum, maximum, pattern and the
// date-time format.
func validateSchema(root, schema map[string]any, v any, path string) []string {
	if ref, ok := schema["$ref"].(string); ok {
		defs := root["$defs"].(map[string]any)
		return validateSchema(root, defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any), v, path)
	}
	var errs []string
	fail := func(format string, args ...any) {
		errs = append(errs, path+": "+fmt.Sprintf(format, args...))
	}
	if c, ok := schema["const"]; ok && c != v {
		fail("got %v, want const %v", v, c)
	}
	if e, ok := schema["enum"].([]any); ok {
		found := false
		for _, x := range e {
			found = found || x == v
		}
		if !found {
			fail("%v not in enum", v)
		}
	}
	switch schema["type"] {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return append(errs, path+": not an object")
		}
		props, _ := schema["properties"].(map[string]any)
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				if _, present := obj[r.(string)]; !present {
					fail("missing required %q", r)
				}
			}
		}
		for k, val := range obj {
			sub, known := props[k].(map[string]any)
			if !known {
				if schema["additionalProperties"] == false {
					fail("unexpected property %q", k)
				}
				continue
			}
			errs = append(errs, validateSchema(root, sub, val, path+"."+k)...)
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return append(errs, path+": not an array")
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, el := range arr {
				errs = append(errs, validateSchema(root, items, el, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	case "string":
		s, ok := v.(string)
		if !ok {
			return append(errs, path+": not a string")
		}
		if n, ok := schema["minLength"].(float64); ok && float64(len(s)) < n {
			fail("shorter than %v", n)
		}
		if p, ok := schema["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(s) {
			fail("%q does not match %s", s, p)
		}
		if schema["format"] == "date-time" {
			if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
				fail("bad date-time %q", s)
			}
		}
	case "integer", "number":
		n, ok := v.(float64)
		if !ok {
			return append(errs, path+": not a number")
		}
		if schema["type"] == "integer" && n != float64(int64(n)) {
			fail("%v is not an integer", n)
		}
		if m, ok := schema["minimum"].(float64); ok && n < m {
			fail("%v below minimum %v", n, m)
		}
		if m, ok := schema["maximum"].(float64); ok && n > m {
			fail("%v above maximum %v", n, m)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			fail("not a boolean")
		}
	}
	return errs
}

func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(SchemaJSON, &root); err != nil {
		t.Fatalf("schema/v1.json is not valid JSON: %v", err)
	}
	return root
}

func toGeneric(t *testing.T, b *Bundle) any {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestBundleMatchesSchema(t *testing.T) {
	root := loadSchema(t)
	_, priv := newKey(t)

	signed := sample()
	if err := Sign(signed, priv); err != nil {
		t.Fatal(err)
	}
	unsigned := sample()
	if err := Seal(unsigned, nil); err != nil {
		t.Fatal(err)
	}
	minimal := &Bundle{
		SchemaVersion: SchemaVersion,
		ID:            BundleID("o/r", 1, "h"),
		Repo:          "o/r",
		Number:        1,
		Author:        Author{Login: "a", Kind: AuthorBot},
		BaseSHA:       "b",
		HeadSHA:       "h",
		CreatedAt:     t0,
		UpdatedAt:     t0,
	}
	linked := sample()
	linked.Author = Author{Login: "hive[bot]", Kind: AuthorAgent, Agent: "coder", Backend: "copilot", Model: "gpt-5.6-luna"}
	linked.RevertOf = []string{BundleID("hivecommons/hive", 40, "a")}
	linked.FollowupOf = []string{BundleID("hivecommons/hive", 41, "b")}
	linked.RevertedBy = []string{BundleID("hivecommons/hive", 43, "c")}
	linked.FollowedUpBy = []string{BundleID("hivecommons/hive", 44, "d")}
	linked.Merge.OpenedAt = t0.Add(-26 * time.Hour)
	if err := Seal(linked, nil); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	shadowed := sample()
	shadowed.Shadow = &Shadow{
		ReviewGate: &ShadowGate{WouldHaveHeld: &yes, Reason: "r", Tier: "T2", Confidence: 0.7, EvaluatedAt: t0},
		Intent:     &ShadowGate{WouldHaveBlocked: &no, Confidence: 0, EvaluatedAt: t0},
	}
	if err := Seal(shadowed, nil); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string]*Bundle{"signed": signed, "unsigned": unsigned, "minimal": minimal, "linked": linked, "shadowed": shadowed} {
		t.Run(name, func(t *testing.T) {
			if errs := validateSchema(root, root, toGeneric(t, b), "$"); len(errs) > 0 {
				t.Fatalf("bundle does not validate against schema/v1.json:\n%s", strings.Join(errs, "\n"))
			}
		})
	}
}

func TestSchemaRejectsInvalidBundles(t *testing.T) {
	root := loadSchema(t)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing head_sha", func(m map[string]any) { delete(m, "head_sha") }},
		{"unknown field", func(m map[string]any) { m["extra"] = 1 }},
		{"wrong version", func(m map[string]any) { m["schema_version"] = "v9" }},
		{"bad author kind", func(m map[string]any) { m["author"].(map[string]any)["kind"] = "robot" }},
		{"bad repo", func(m map[string]any) { m["repo"] = "nope" }},
		{"number type", func(m map[string]any) { m["number"] = "1" }},
		{"fractional number", func(m map[string]any) { m["number"] = 1.5 }},
		{"confidence range", func(m map[string]any) { m["verdicts"].([]any)[0].(map[string]any)["confidence"] = 2.0 }},
		{"bad timestamp", func(m map[string]any) { m["created_at"] = "yesterday" }},
		{"bad hash", func(m map[string]any) { m["hash"] = "xyz" }},
		{"signed type", func(m map[string]any) { m["signed"] = "yes" }},
		{"checks not array", func(m map[string]any) { m["ci"].(map[string]any)["checks"] = "none" }},
		{"empty string", func(m map[string]any) { m["id"] = "" }},
		{"policy not object", func(m map[string]any) { m["policy"] = 3 }},
		{"author model type", func(m map[string]any) { m["author"].(map[string]any)["model"] = 5 }},
		{"revert_of not array", func(m map[string]any) { m["revert_of"] = "o/r#1@h" }},
		{"shadow unknown field", func(m map[string]any) { m["shadow"] = map[string]any{"extra": 1} }},
		{"shadow gate missing verdict", func(m map[string]any) {
			m["shadow"] = map[string]any{"intent": map[string]any{"confidence": 0.5, "evaluated_at": "2026-10-08T12:00:00Z"}}
		}},
		{"shadow gate wrong flag", func(m map[string]any) {
			m["shadow"] = map[string]any{"review_gate": map[string]any{"would_have_blocked": true, "confidence": 0.5, "evaluated_at": "2026-10-08T12:00:00Z"}}
		}},
		{"reverted_by empty id", func(m map[string]any) { m["reverted_by"] = []any{""} }},
		{"merge opened_at not a timestamp", func(m map[string]any) { m["merge"].(map[string]any)["opened_at"] = "last week" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := sample()
			if err := Seal(b, nil); err != nil {
				t.Fatal(err)
			}
			m := toGeneric(t, b).(map[string]any)
			tc.mutate(m)
			if errs := validateSchema(root, root, m, "$"); len(errs) == 0 {
				t.Fatal("schema accepted an invalid bundle")
			}
		})
	}
}

func TestCalibrationConfidenceSchemaRoundTrip(t *testing.T) {
	b := sample()
	raw, derived := .82, .75
	b.Verdicts[0].Confidence, b.Verdicts[0].ConfidenceRaw, b.Verdicts[0].ConfidenceDerived = .85, &raw, &derived
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	schema := loadSchema(t)
	if errs := validateSchema(schema, schema, toGeneric(t, b), "bundle"); len(errs) > 0 {
		t.Fatal(errs)
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Bundle
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if *decoded.Verdicts[0].ConfidenceRaw != raw || *decoded.Verdicts[0].ConfidenceDerived != derived {
		t.Fatal("confidence fields lost")
	}
	derived = 1.1
	if b.Validate() == nil {
		t.Fatal("accepted derived score above 1")
	}
	if errs := validateSchema(schema, schema, toGeneric(t, b), "bundle"); len(errs) == 0 {
		t.Fatal("schema accepted derived score above 1")
	}
	// Optional fields must not change the canonical JSON of legacy records.
	b = sample()
	data, _ = json.Marshal(b)
	if strings.Contains(string(data), "confidence_derived") || strings.Contains(string(data), "confidence_raw") {
		t.Fatal("legacy bundle gained fields")
	}
}
