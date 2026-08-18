package dashboard

import (
	"reflect"
	"testing"
)

// intersectEntitled keeps only discovered models present in the entitled set,
// preserving discovery order and tolerating a missing/extra provider prefix.
func TestIntersectEntitled(t *testing.T) {
	discovered := []string{
		"Azure/gpt-4.1", "Azure/gpt-4o", "aws/us.claude-opus-4-7", "gemini/gemini-2.5-pro",
	}
	entitled := []string{"Azure/gpt-4.1", "Azure/gpt-4o", "Azure/gpt-5-2025-08-07"}
	got := intersectEntitled(discovered, entitled)
	want := []string{"Azure/gpt-4.1", "Azure/gpt-4o"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A discovered id lacking the provider prefix still matches an entitled id that
// carries it (tail-after-slash match), so the dropdown is not wrongly emptied.
func TestIntersectEntitled_PrefixInsensitive(t *testing.T) {
	got := intersectEntitled([]string{"gpt-4o", "claude-opus-4-7"}, []string{"Azure/gpt-4o"})
	want := []string{"gpt-4o"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// When nothing intersects, the caller sees an empty result and (by design)
// keeps the full advertised list rather than an empty dropdown.
func TestIntersectEntitled_NoOverlap(t *testing.T) {
	if got := intersectEntitled([]string{"aws/claude"}, []string{"Azure/gpt-4o"}); len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}
