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
	if got := compactValue("a\x1b[31mred\x1b[0m\tb\x07\u009bc\r"); got != "a[31mred[0m bc" {
		t.Fatalf("control chars = %q", got)
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

// TestPrinterTableMapKeyWithControlCharsAndTab covers sanitisation of
// map keys containing control sequences and tabs. Keys are server-derived
// and must be escaped so they cannot break table layout or carry ANSI
// sequences into the terminal.
func TestPrinterTableMapKeyWithControlCharsAndTab(t *testing.T) {
	var buf bytes.Buffer
	p := printer{format: "table", out: &buf}
	if err := p.print(map[string]any{
		"key\x1b[31mred\x1b[0m\ttab": "value1",
		"normalkey":                  "value2",
	}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// The control chars should be stripped and tab should become space
	if strings.Contains(out, "\x1b[31m") || strings.Contains(out, "\x1b[0m") {
		t.Fatalf("control chars not stripped from output: %q", out)
	}
	if strings.Contains(out, "key\ttab") {
		t.Fatalf("tab in key not replaced: %q", out)
	}
}

// TestPrinterTableArrayOfMapsHeaderWithControlChars covers sanitisation of
// headers (map keys) in the []any-of-maps case. Headers are built from the
// union of all row keys, which come from the server, and must be escaped
// before printing.
func TestPrinterTableArrayOfMapsHeaderWithControlChars(t *testing.T) {
	var buf bytes.Buffer
	p := printer{format: "table", out: &buf}
	rows := []any{
		map[string]any{"col\u009bX": "value1", "normal": "value2"},
		map[string]any{"col\u009bX": "value3"},
	}
	if err := p.print(rows); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// The C1 control character \u009b should be stripped
	if strings.Contains(out, "\u009b") {
		t.Fatalf("C1 control char not stripped from header: %q", out)
	}
	// The original key must still be used for row lookups (values are correct)
	if !strings.Contains(out, "value1") || !strings.Contains(out, "value3") {
		t.Fatalf("values not found in output: %q", out)
	}
}

// TestPrinterTableStringValueIsSanitised covers the early string-value
// branch: a server-derived top-level string must not carry control
// sequences to the terminal.
func TestPrinterTableStringValueIsSanitised(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"ansi escape", "\x1b[31mred\x1b[0m", "[31mred[0m"},
		{"osc sequence", "a\x1b]0;title\x07b", "a]0;titleb"},
		{"newline", "one\ntwo", "one\\ntwo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			p := printer{format: "table", out: &buf}
			if err := p.print(tt.input); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSuffix(buf.String(), "\n"); got != tt.want {
				t.Fatalf("output = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPrinterTableArrayOfMapsCollidingHeadersAreDistinct covers raw keys
// that sanitise to the same label: the displayed headers must differ while
// each column keeps reading its own raw key.
func TestPrinterTableArrayOfMapsCollidingHeadersAreDistinct(t *testing.T) {
	tests := []struct {
		name        string
		rows        []any
		wantHeader  []string
		wantRowCols []string
	}{
		{
			name: "two keys collide",
			rows: []any{
				map[string]any{"a\x1bb": "esc", "ab": "plain"},
			},
			wantHeader:  []string{"ab", "ab (2)"},
			wantRowCols: []string{"esc", "plain"},
		},
		{
			name: "three keys collide",
			rows: []any{
				map[string]any{"a\x1bb": "one", "ab": "two", "a\x07b": "three"},
			},
			wantHeader:  []string{"ab", "ab (2)", "ab (3)"},
			wantRowCols: []string{"three", "one", "two"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			p := printer{format: "table", out: &buf}
			if err := p.print(tt.rows); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			if len(lines) != 2 {
				t.Fatalf("lines = %q", lines)
			}
			if got := strings.Fields(strings.ReplaceAll(lines[0], " (", "_(")); len(got) != len(tt.wantHeader) {
				t.Fatalf("header = %q", lines[0])
			}
			for i, want := range tt.wantHeader {
				if !strings.Contains(lines[0], want) {
					t.Fatalf("header %d %q missing in %q", i, want, lines[0])
				}
			}
			if got := strings.Fields(lines[1]); strings.Join(got, ",") != strings.Join(tt.wantRowCols, ",") {
				t.Fatalf("row = %q, want %q", got, tt.wantRowCols)
			}
		})
	}
}

// TestPrinterTableMapCollidingKeysAreDistinct covers the key/value branch:
// map keys that sanitise to the same label get the same " (n)" suffix the
// list headers do, and each row still reads its own raw key's value.
func TestPrinterTableMapCollidingKeysAreDistinct(t *testing.T) {
	var buf bytes.Buffer
	p := printer{format: "table", out: &buf}
	err := p.print(map[string]any{"a\x1bb": "one", "ab": "two", "a\x07b": "three"})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %q", lines)
	}
	// sorted raw keys: "a\x07b", "a\x1bb", "ab"
	want := []struct{ label, value string }{
		{"ab", "three"},
		{"ab (2)", "one"},
		{"ab (3)", "two"},
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w.label+" ") || !strings.HasSuffix(lines[i], w.value) {
			t.Fatalf("line %d = %q, want label %q value %q", i, lines[i], w.label, w.value)
		}
	}
	if strings.Contains(buf.String(), "\x1b") || strings.Contains(buf.String(), "\x07") {
		t.Fatalf("control characters leaked: %q", buf.String())
	}
}
