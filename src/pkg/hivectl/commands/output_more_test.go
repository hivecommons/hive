package commands

import (
	"bytes"
	"strings"
	"testing"
)

// TestPrinterJSONLArraySplitsEncodesEachItem covers printJSONL's branch
// where the value is a []any: each item is encoded on its own line.
func TestPrinterJSONLArraySplitsEncodesEachItem(t *testing.T) {
	var buf bytes.Buffer
	p := printer{format: "jsonl", out: &buf}
	err := p.print([]any{map[string]any{"a": 1}, map[string]any{"b": 2}})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %v", lines)
	}
	if !strings.Contains(lines[0], `"a":1`) || !strings.Contains(lines[1], `"b":2`) {
		t.Fatalf("lines = %v", lines)
	}
}

// TestPrinterJSONLNonArrayEncodesWholeValue covers printJSONL's fallback
// branch when value is not a []any.
func TestPrinterJSONLNonArrayEncodesWholeValue(t *testing.T) {
	var buf bytes.Buffer
	p := printer{format: "jsonl", out: &buf}
	if err := p.print(map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(buf.String()); got != `{"ok":true}` {
		t.Fatalf("output = %q", got)
	}
}

// TestPrinterTableStringValuePrintsVerbatim covers printTable's early
// string-value branch.
func TestPrinterTableStringValuePrintsVerbatim(t *testing.T) {
	var buf bytes.Buffer
	p := printer{format: "table", out: &buf}
	if err := p.print("hello world"); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(buf.String()); got != "hello world" {
		t.Fatalf("output = %q", got)
	}
}

// TestPrinterTableMapValuePrintsKeyValueRows covers printTable's
// map[string]any branch, including sorted keys and compactValue formatting.
func TestPrinterTableMapValuePrintsKeyValueRows(t *testing.T) {
	var buf bytes.Buffer
	p := printer{format: "table", out: &buf}
	if err := p.print(map[string]any{"zeta": "z", "alpha": "a"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	alphaIdx := strings.Index(out, "alpha")
	zetaIdx := strings.Index(out, "zeta")
	if alphaIdx == -1 || zetaIdx == -1 || alphaIdx > zetaIdx {
		t.Fatalf("expected sorted keys, got %q", out)
	}
}

// TestPrinterTableArrayOfMapsPrintsHeaderAndRows covers printTable's
// []any-of-maps branch with header union and per-row value lookups.
func TestPrinterTableArrayOfMapsPrintsHeaderAndRows(t *testing.T) {
	var buf bytes.Buffer
	p := printer{format: "table", out: &buf}
	rows := []any{
		map[string]any{"name": "a", "count": float64(1)},
		map[string]any{"name": "b"},
	}
	if err := p.print(rows); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "count") || !strings.Contains(out, "name") {
		t.Fatalf("output = %q", out)
	}
	if !strings.Contains(out, "a") || !strings.Contains(out, "b") {
		t.Fatalf("output = %q", out)
	}
}

// TestPrinterTableArrayOfNonMapsFallsBackToJSONL covers the branch in
// printTable's []any case where an item is not a map[string]any, so it
// falls back to printJSONL.
func TestPrinterTableArrayOfNonMapsFallsBackToJSONL(t *testing.T) {
	var buf bytes.Buffer
	p := printer{format: "table", out: &buf}
	if err := p.print([]any{"plain-string-item"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(buf.String()); got != `"plain-string-item"` {
		t.Fatalf("output = %q", got)
	}
}

// TestPrinterTableDefaultCaseMarshalsJSON covers printTable's default
// branch for scalar/other types (e.g. a plain number or bool at top level).
func TestPrinterTableDefaultCaseMarshalsJSON(t *testing.T) {
	var buf bytes.Buffer
	p := printer{format: "table", out: &buf}
	if err := p.print(float64(42)); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(buf.String()); got != "42" {
		t.Fatalf("output = %q", got)
	}
}

// TestPrinterUnsupportedFormatReturnsError covers print()'s default branch.
func TestPrinterUnsupportedFormatReturnsError(t *testing.T) {
	var buf bytes.Buffer
	p := printer{format: "xml", out: &buf}
	err := p.print("x")
	if err == nil || !strings.Contains(err.Error(), "unsupported output format") {
		t.Fatalf("err = %v", err)
	}
}

// TestPrinterYAMLFormat covers print()'s "yaml" branch directly.
func TestPrinterYAMLFormat(t *testing.T) {
	var buf bytes.Buffer
	p := printer{format: "yaml", out: &buf}
	if err := p.print(map[string]any{"key": "value"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "key: value") {
		t.Fatalf("output = %q", buf.String())
	}
}

// TestCompactValueNilAndTypes covers compactValue's nil, string (newline
// escaping), float64/bool, and default (JSON marshal) branches.
func TestCompactValueNilAndTypes(t *testing.T) {
	if got := compactValue(nil); got != "" {
		t.Fatalf("nil = %q", got)
	}
	if got := compactValue("line1\nline2"); got != "line1\\nline2" {
		t.Fatalf("string = %q", got)
	}
	if got := compactValue(float64(3.5)); got != "3.5" {
		t.Fatalf("float64 = %q", got)
	}
	if got := compactValue(true); got != "true" {
		t.Fatalf("bool = %q", got)
	}
	if got := compactValue([]any{"a", "b"}); got != `["a","b"]` {
		t.Fatalf("default = %q", got)
	}
}

// TestCompactValueUnmarshalableFallsBackToSprint covers compactValue's
// error branch when json.Marshal fails (e.g. a channel value).
func TestCompactValueUnmarshalableFallsBackToSprint(t *testing.T) {
	ch := make(chan int)
	got := compactValue(ch)
	if !strings.Contains(got, "0x") && got == "" {
		t.Fatalf("got = %q, want non-empty fmt.Sprint fallback", got)
	}
}
