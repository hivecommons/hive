package tracing

import (
	"testing"

	"github.com/hivecommons/hive/pkg/timeline"
	"go.opentelemetry.io/otel/attribute"
)

// TestTimelineSpanAttributes_PRNumberAndURL covers the eventAttrPRNumber /
// eventAttrPRURL cases in TimelineSpanAttributes, which no existing test
// exercised: TestTimelineSpanAttributes_GenAIAndHiveAttrs uses a "pr" attr
// value ("42") indirectly via TestStartTimelineSpan_RecordsMappedSpan but
// never asserts the resulting pr.number/pr.url attribute values, and no test
// sets pr_url at all.
func TestTimelineSpanAttributes_PRNumberAndURL(t *testing.T) {
	e := timeline.Event{
		Kind: timeline.KindPROpened,
		Attrs: map[string]string{
			"pr":     "42",
			"pr_url": "https://github.com/acme/widget/pull/42",
		},
	}

	attrs := TimelineSpanAttributes(e)
	got := map[attribute.Key]string{}
	for _, kv := range attrs {
		got[kv.Key] = kv.Value.AsString()
	}
	if got[attribute.Key(attrPRNumber)] != "42" {
		t.Errorf("pr.number = %q, want 42", got[attribute.Key(attrPRNumber)])
	}
	if got[attribute.Key(attrPRURL)] != "https://github.com/acme/widget/pull/42" {
		t.Errorf("pr.url = %q", got[attribute.Key(attrPRURL)])
	}
}

// TestTimelineSpanAttributes_MalformedNumericAttrsOmitted covers the ok=false
// branch of parseIntAttr for each of its three call sites (input_tokens,
// output_tokens, acmm_level): a non-numeric value must be silently dropped
// from the attribute set rather than panicking or emitting a zero value that
// would misrepresent a real "0" reading.
func TestTimelineSpanAttributes_MalformedNumericAttrsOmitted(t *testing.T) {
	e := timeline.Event{
		Kind: timeline.KindKicked,
		Attrs: map[string]string{
			"input_tokens":  "not-a-number",
			"output_tokens": "",
			"acmm_level":    "NaN",
		},
	}

	attrs := TimelineSpanAttributes(e)
	for _, kv := range attrs {
		switch string(kv.Key) {
		case AttrGenAIUsageInputTokens, AttrGenAIUsageOutputTokens, AttrHiveACMMLevel:
			t.Errorf("malformed numeric attr %s must be omitted, got %v", kv.Key, kv.Value)
		}
	}
}

// TestParseIntAttr covers parseIntAttr directly: the trimming of surrounding
// whitespace on the success path and the ok=false result for non-numeric
// input, neither of which any existing test called out by name.
func TestParseIntAttr(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		wantN  int
		wantOK bool
	}{
		{"plain", "7", 7, true},
		{"whitespace_trimmed", "  12 ", 12, true},
		{"negative", "-3", -3, true},
		{"empty", "", 0, false},
		{"non_numeric", "abc", 0, false},
		{"float_rejected", "1.5", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, ok := parseIntAttr(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("parseIntAttr(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if ok && n != tc.wantN {
				t.Fatalf("parseIntAttr(%q) = %d, want %d", tc.in, n, tc.wantN)
			}
		})
	}
}
