package agentstep

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/crux/api"
)

// MaxSchemaBytes bounds the schema a request may carry.
const MaxSchemaBytes = 256 << 10

// ErrSchema is a schema that is not usable: not an object, over the size
// bound, or using a keyword this validator does not implement (a keyword
// it cannot enforce is refused rather than ignored, so a caller never
// believes a constraint was checked when it was not).
var ErrSchema = errors.New("agentstep: invalid output_schema")

// Schema is the subset of JSON Schema (draft 2020-12 vocabulary) crux
// checks answers against: type, enum, const, properties, required,
// additionalProperties (boolean or schema), items, minItems, maxItems,
// minLength, maxLength, pattern, minimum, maximum, exclusiveMinimum,
// exclusiveMaximum, multipleOf and the annotations title, description,
// default, examples and $schema. Anything else is ErrSchema.
type Schema struct {
	types                      []string
	enum                       []json.RawMessage
	constant                   json.RawMessage
	hasConst                   bool
	properties                 map[string]*Schema
	required                   []string
	additional                 *Schema
	additionalForbidden        bool
	items                      *Schema
	minItems, maxItems         *int
	minLength, maxLength       *int
	pattern                    *regexp.Regexp
	minimum, maximum           *float64
	exclusiveMin, exclusiveMax *float64
	multipleOf                 *float64
}

// annotationKeywords are accepted and ignored: they constrain nothing.
var annotationKeywords = map[string]bool{
	"title": true, "description": true, "default": true, "examples": true, "$schema": true, "$id": true, "$comment": true,
}

// ParseSchema parses raw into a Schema or returns ErrSchema.
func ParseSchema(raw json.RawMessage) (*Schema, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: output_schema is required", ErrSchema)
	}
	if len(raw) > MaxSchemaBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds the %d-byte bound", ErrSchema, len(raw), MaxSchemaBytes)
	}
	return parseSchema(raw, "$")
}

func parseSchema(raw json.RawMessage, at string) (*Schema, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		if err == nil {
			// JSON null decodes to a nil map without error.
			return nil, fmt.Errorf("%w: %s must be an object", ErrSchema, at)
		}
		// A bare boolean schema: true accepts anything, false nothing.
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			if b {
				return &Schema{}, nil
			}
			return &Schema{enum: []json.RawMessage{}}, nil
		}
		return nil, fmt.Errorf("%w: %s must be an object", ErrSchema, at)
	}
	s := &Schema{}
	for key, val := range fields {
		var err error
		switch key {
		case "type":
			err = s.parseType(val, at)
		case "enum":
			err = json.Unmarshal(val, &s.enum)
		case "const":
			s.constant, s.hasConst = val, true
		case "properties":
			var props map[string]json.RawMessage
			if err = json.Unmarshal(val, &props); err == nil {
				s.properties = map[string]*Schema{}
				for name, sub := range props {
					if s.properties[name], err = parseSchema(sub, at+"."+name); err != nil {
						return nil, err
					}
				}
			}
		case "required":
			err = json.Unmarshal(val, &s.required)
		case "additionalProperties":
			var b bool
			if json.Unmarshal(val, &b) == nil {
				s.additionalForbidden = !b
			} else if s.additional, err = parseSchema(val, at+".*"); err != nil {
				return nil, err
			}
		case "items":
			if s.items, err = parseSchema(val, at+"[]"); err != nil {
				return nil, err
			}
		case "minItems":
			s.minItems, err = parseInt(val)
		case "maxItems":
			s.maxItems, err = parseInt(val)
		case "minLength":
			s.minLength, err = parseInt(val)
		case "maxLength":
			s.maxLength, err = parseInt(val)
		case "pattern":
			var p string
			if err = json.Unmarshal(val, &p); err == nil {
				s.pattern, err = regexp.Compile(p)
			}
		case "minimum":
			s.minimum, err = parseNumber(val)
		case "maximum":
			s.maximum, err = parseNumber(val)
		case "exclusiveMinimum":
			s.exclusiveMin, err = parseNumber(val)
		case "exclusiveMaximum":
			s.exclusiveMax, err = parseNumber(val)
		case "multipleOf":
			if s.multipleOf, err = parseNumber(val); err == nil && *s.multipleOf <= 0 {
				err = errors.New("must be positive")
			}
		default:
			if annotationKeywords[key] {
				continue
			}
			return nil, fmt.Errorf("%w: %s uses keyword %q, which this validator does not enforce", ErrSchema, at, key)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %s.%s: %v", ErrSchema, at, key, err)
		}
	}
	return s, nil
}

func (s *Schema) parseType(val json.RawMessage, at string) error {
	var one string
	if json.Unmarshal(val, &one) == nil {
		s.types = []string{one}
	} else if err := json.Unmarshal(val, &s.types); err != nil {
		return errors.New("must be a string or an array of strings")
	}
	for _, t := range s.types {
		switch t {
		case "object", "array", "string", "number", "integer", "boolean", "null":
		default:
			return fmt.Errorf("unknown type %q", t)
		}
	}
	return nil
}

func parseInt(val json.RawMessage) (*int, error) {
	var n int
	if err := json.Unmarshal(val, &n); err != nil || n < 0 {
		return nil, errors.New("must be a non-negative integer")
	}
	return &n, nil
}

func parseNumber(val json.RawMessage) (*float64, error) {
	var f float64
	if err := json.Unmarshal(val, &f); err != nil {
		return nil, errors.New("must be a number")
	}
	return &f, nil
}

// Validate checks answer against the schema and returns every violation
// found, in path order; none means the answer matches. answer must be one
// JSON value.
func (s *Schema) Validate(answer json.RawMessage) []api.SchemaViolation {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(answer)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return []api.SchemaViolation{{Path: "$", Message: "not valid JSON: " + err.Error()}}
	}
	if dec.More() {
		return []api.SchemaViolation{{Path: "$", Message: "must be exactly one JSON value"}}
	}
	var out []api.SchemaViolation
	s.check(v, "$", &out)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (s *Schema) check(v any, path string, out *[]api.SchemaViolation) {
	add := func(msg string) { *out = append(*out, api.SchemaViolation{Path: path, Message: msg}) }
	if len(s.types) > 0 && !typeMatches(s.types, v) {
		add(fmt.Sprintf("expected %s, got %s", strings.Join(s.types, " or "), typeName(v)))
		return
	}
	if s.hasConst && !jsonEqual(s.constant, v) {
		add("must equal the const value")
	}
	if s.enum != nil {
		found := false
		for _, e := range s.enum {
			if jsonEqual(e, v) {
				found = true
				break
			}
		}
		if !found {
			add("not one of the enum values")
		}
	}
	switch x := v.(type) {
	case map[string]any:
		s.checkObject(x, path, out)
	case []any:
		s.checkArray(x, path, out)
	case string:
		s.checkString(x, add)
	case json.Number:
		s.checkNumber(x, add)
	}
}

func (s *Schema) checkObject(x map[string]any, path string, out *[]api.SchemaViolation) {
	for _, name := range s.required {
		if _, ok := x[name]; !ok {
			*out = append(*out, api.SchemaViolation{Path: path + "." + name, Message: "required property is missing"})
		}
	}
	keys := make([]string, 0, len(x))
	for k := range x {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sub, declared := s.properties[k]
		switch {
		case declared:
			sub.check(x[k], path+"."+k, out)
		case s.additionalForbidden:
			*out = append(*out, api.SchemaViolation{Path: path + "." + k, Message: "property is not allowed"})
		case s.additional != nil:
			s.additional.check(x[k], path+"."+k, out)
		}
	}
}

func (s *Schema) checkArray(x []any, path string, out *[]api.SchemaViolation) {
	if s.minItems != nil && len(x) < *s.minItems {
		*out = append(*out, api.SchemaViolation{Path: path, Message: fmt.Sprintf("needs at least %d items, has %d", *s.minItems, len(x))})
	}
	if s.maxItems != nil && len(x) > *s.maxItems {
		*out = append(*out, api.SchemaViolation{Path: path, Message: fmt.Sprintf("allows at most %d items, has %d", *s.maxItems, len(x))})
	}
	if s.items != nil {
		for i, item := range x {
			s.items.check(item, path+"["+strconv.Itoa(i)+"]", out)
		}
	}
}

func (s *Schema) checkString(x string, add func(string)) {
	n := len([]rune(x))
	if s.minLength != nil && n < *s.minLength {
		add(fmt.Sprintf("shorter than %d characters", *s.minLength))
	}
	if s.maxLength != nil && n > *s.maxLength {
		add(fmt.Sprintf("longer than %d characters", *s.maxLength))
	}
	if s.pattern != nil && !s.pattern.MatchString(x) {
		add("does not match pattern " + s.pattern.String())
	}
}

func (s *Schema) checkNumber(x json.Number, add func(string)) {
	f, err := x.Float64()
	if err != nil {
		add("not a representable number")
		return
	}
	if s.minimum != nil && f < *s.minimum {
		add(fmt.Sprintf("below minimum %v", *s.minimum))
	}
	if s.maximum != nil && f > *s.maximum {
		add(fmt.Sprintf("above maximum %v", *s.maximum))
	}
	if s.exclusiveMin != nil && f <= *s.exclusiveMin {
		add(fmt.Sprintf("not above %v", *s.exclusiveMin))
	}
	if s.exclusiveMax != nil && f >= *s.exclusiveMax {
		add(fmt.Sprintf("not below %v", *s.exclusiveMax))
	}
	if s.multipleOf != nil {
		q := f / *s.multipleOf
		if math.Abs(q-math.Round(q)) > 1e-9 {
			add(fmt.Sprintf("not a multiple of %v", *s.multipleOf))
		}
	}
}

func typeMatches(types []string, v any) bool {
	for _, t := range types {
		switch t {
		case "null":
			if v == nil {
				return true
			}
		case "boolean":
			if _, ok := v.(bool); ok {
				return true
			}
		case "string":
			if _, ok := v.(string); ok {
				return true
			}
		case "object":
			if _, ok := v.(map[string]any); ok {
				return true
			}
		case "array":
			if _, ok := v.([]any); ok {
				return true
			}
		case "number":
			if _, ok := v.(json.Number); ok {
				return true
			}
		case "integer":
			if n, ok := v.(json.Number); ok {
				if _, err := n.Int64(); err == nil {
					return true
				}
				if f, err := n.Float64(); err == nil && f == math.Trunc(f) {
					return true
				}
			}
		}
	}
	return false
}

func typeName(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		if _, err := x.Int64(); err == nil {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// jsonEqual compares a raw schema value with a decoded answer value by
// re-encoding both canonically.
func jsonEqual(raw json.RawMessage, v any) bool {
	var want any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&want); err != nil {
		return false
	}
	a, err1 := json.Marshal(want)
	b, err2 := json.Marshal(v)
	return err1 == nil && err2 == nil && string(a) == string(b)
}

// CorrectivePrompt is the repair instruction appended to the prompt of the
// attempt after a schema mismatch: the violations, verbatim, and the shape
// asked for. The pattern is outputschema.CorrectivePrompt, generalised to
// a caller-supplied schema.
func CorrectivePrompt(schema json.RawMessage, violations []api.SchemaViolation, remaining int) string {
	var b strings.Builder
	b.WriteString("Your previous answer did not match the required JSON Schema and could not be used. ")
	b.WriteString("Reply with exactly one JSON value matching this schema and nothing else:\n")
	b.Write(schema)
	b.WriteString("\nFix these validation errors:\n")
	for _, v := range violations {
		fmt.Fprintf(&b, "- %s: %s\n", v.Path, v.Message)
	}
	fmt.Fprintf(&b, "Attempts remaining after this one: %d.", remaining)
	return b.String()
}
