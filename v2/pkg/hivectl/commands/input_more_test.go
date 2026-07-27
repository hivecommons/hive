package commands

import (
	"bytes"
	"strings"
	"testing"
)

// TestReadInputRejectsBothFileAndStdin covers the "use only one of --file or
// --stdin" branch.
func TestReadInputRejectsBothFileAndStdin(t *testing.T) {
	_, err := readInput(strings.NewReader("x"), "somefile.yaml", true)
	if err == nil || !strings.Contains(err.Error(), "use only one of") {
		t.Fatalf("err = %v", err)
	}
}

// TestReadInputRejectsNeitherFileNorStdin covers the "one of --file or
// --stdin is required" branch.
func TestReadInputRejectsNeitherFileNorStdin(t *testing.T) {
	_, err := readInput(strings.NewReader("x"), "", false)
	if err == nil || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("err = %v", err)
	}
}

// TestReadInputFileOpenError covers the os.Open failure branch when the
// file does not exist.
func TestReadInputFileOpenError(t *testing.T) {
	_, err := readInput(strings.NewReader(""), "/nonexistent/path/does-not-exist.yaml", false)
	if err == nil || !strings.Contains(err.Error(), "read /nonexistent") {
		t.Fatalf("err = %v", err)
	}
}

// TestReadLimitedRejectsOversizedInput covers readLimited's over-limit
// branch directly.
func TestReadLimitedRejectsOversizedInput(t *testing.T) {
	data := strings.Repeat("a", 100)
	_, err := readLimited(strings.NewReader(data), 10)
	if err == nil || !strings.Contains(err.Error(), "1 MiB request limit") {
		t.Fatalf("err = %v", err)
	}
}

// TestReadLimitedWithinBounds covers readLimited's success branch directly.
func TestReadLimitedWithinBounds(t *testing.T) {
	data, err := readLimited(strings.NewReader("hello"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("data = %q", data)
	}
}

// TestDecodeStructuredJSON covers decodeStructured's JSON-success branch.
func TestDecodeStructuredJSON(t *testing.T) {
	value, err := decodeStructured([]byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := value.(map[string]any)
	if !ok || m["a"] != float64(1) {
		t.Fatalf("value = %#v", value)
	}
}

// TestDecodeStructuredYAML covers decodeStructured's YAML fallback branch,
// including the map[any]any normalization path.
func TestDecodeStructuredYAML(t *testing.T) {
	value, err := decodeStructured([]byte("a: 1\nb:\n  c: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value = %#v", value)
	}
	nested, ok := m["b"].(map[string]any)
	if !ok || nested["c"] != 2 {
		t.Fatalf("nested = %#v", m["b"])
	}
}

// TestDecodeStructuredInvalidBoth covers decodeStructured's error branch
// when input is neither valid JSON nor valid YAML.
func TestDecodeStructuredInvalidBoth(t *testing.T) {
	_, err := decodeStructured([]byte(":\n  - ["))
	if err == nil || !strings.Contains(err.Error(), "neither valid JSON nor YAML") {
		t.Fatalf("err = %v", err)
	}
}

// TestNormalizeYAMLValueHandlesSliceOfMaps covers normalizeYAMLValue's
// []any branch, recursively normalizing nested map[any]any entries
// produced by yaml.Unmarshal.
func TestNormalizeYAMLValueHandlesSliceOfMaps(t *testing.T) {
	input := []any{
		map[any]any{"key": "value"},
		"plain",
	}
	result := normalizeYAMLValue(input)
	list, ok := result.([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("result = %#v", result)
	}
	normalizedMap, ok := list[0].(map[string]any)
	if !ok || normalizedMap["key"] != "value" {
		t.Fatalf("list[0] = %#v", list[0])
	}
}

// TestReadStructuredInputEndToEnd covers readStructuredInput composing
// readInput and decodeStructured.
func TestReadStructuredInputEndToEnd(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(`{"key":"value"}`)
	value, err := readStructuredInput(&buf, "", true)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := value.(map[string]any)
	if !ok || m["key"] != "value" {
		t.Fatalf("value = %#v", value)
	}
}
