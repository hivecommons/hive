package review

import (
	"fmt"
	"strings"
	"testing"
)

// The settings dialog renders three things this package computes: the focus
// text a hive customised (Overrides), the built-in line an override edited
// away from (DefaultFocus / DefaultFocusAll), and the effective focus the
// reviewer will actually be told (Focus). These are the display/export surface
// of #7717's configurable perspectives, and until now none of them had a
// direct unit test in this package -- the dashboard handler tests exercise
// them only through HTTP, which counts for nothing in this package's own
// coverage and pins none of the copy/fallback semantics below.

func TestOverridesReturnsCustomisedFocusOnly(t *testing.T) {
	set, err := NewPerspectiveSet(
		[]string{"security", "api-compat"},
		map[string]string{"api-compat": "public API breakage", "security": "secrets in diffs"},
	)
	if err != nil {
		t.Fatalf("NewPerspectiveSet: %v", err)
	}

	got := set.Overrides()
	if len(got) != 2 {
		t.Fatalf("Overrides() = %v, want exactly the two customised entries", got)
	}
	if got["api-compat"] != "public API breakage" {
		t.Errorf("Overrides()[api-compat] = %q", got["api-compat"])
	}
	if got["security"] != "secrets in diffs" {
		t.Errorf("Overrides()[security] = %q", got["security"])
	}
	// Built-in defaults must NOT leak into an export: the point of an export
	// is what THIS hive changed.
	if _, ok := got["correctness"]; ok {
		t.Error("Overrides() contains built-in default for correctness")
	}
}

func TestOverridesReturnsACopy(t *testing.T) {
	set, err := NewPerspectiveSet(nil, map[string]string{"style": "match CONVENTIONS.md"})
	if err != nil {
		t.Fatalf("NewPerspectiveSet: %v", err)
	}
	set.Overrides()["style"] = "mutated by caller"
	if set.Focus("style") != "match CONVENTIONS.md" {
		t.Errorf("mutating Overrides() result changed the set's focus: %q", set.Focus("style"))
	}
}

func TestOverridesEmptySet(t *testing.T) {
	var zero PerspectiveSet
	if got := zero.Overrides(); len(got) != 0 {
		t.Errorf("zero-value Overrides() = %v, want empty", got)
	}
}

func TestDefaultFocusBuiltins(t *testing.T) {
	for _, p := range DefaultPerspectives {
		if DefaultFocus(p) == "" {
			t.Errorf("DefaultFocus(%q) is empty; the settings UI would have no way back from an override", p)
		}
	}
	// A hive-defined perspective has no built-in default.
	if got := DefaultFocus("api-compat"); got != "" {
		t.Errorf("DefaultFocus(api-compat) = %q, want empty", got)
	}
}

func TestDefaultFocusAllCompleteAndCopied(t *testing.T) {
	all := DefaultFocusAll()
	if len(all) != len(DefaultPerspectives) {
		t.Fatalf("DefaultFocusAll() has %d entries, want %d", len(all), len(DefaultPerspectives))
	}
	for _, p := range DefaultPerspectives {
		if all[p] != DefaultFocus(p) {
			t.Errorf("DefaultFocusAll()[%q] = %q, want %q", p, all[p], DefaultFocus(p))
		}
	}
	// The map must be a copy: a caller (the settings UI marshals it) must not
	// be able to rewrite the package's defaults.
	all[PerspectiveSecurity] = "weakened"
	if DefaultFocus(PerspectiveSecurity) == "weakened" {
		t.Error("mutating DefaultFocusAll() result changed the package default")
	}
}

func TestFocusFallsBackToGenericLineForUnknownPerspective(t *testing.T) {
	set, err := NewPerspectiveSet([]string{"security"}, nil)
	if err != nil {
		t.Fatalf("NewPerspectiveSet: %v", err)
	}
	// No override and no built-in default: the prompt still needs a noun
	// phrase, not an empty "Focus ONLY on ." instruction.
	if got := set.Focus("api-compat"); got != "the named review perspective" {
		t.Errorf("Focus(unknown) = %q, want the generic fallback", got)
	}
}

func TestNewPerspectiveSetDefaultPathOverLimit(t *testing.T) {
	// No explicit selection, but enough hive-defined focus entries to push the
	// default set (built-ins + defined extras) past MaxPerspectives.
	focus := map[string]string{}
	for i := 0; i <= MaxPerspectives-len(DefaultPerspectives); i++ {
		focus[fmt.Sprintf("extra-%d", i)] = "something to look for"
	}
	_, err := NewPerspectiveSet(nil, focus)
	if err == nil || !strings.Contains(err.Error(), "over the limit") {
		t.Fatalf("NewPerspectiveSet(nil, %d extras) err = %v, want over-the-limit error", len(focus), err)
	}
}

func TestNewPerspectiveSetSkipsBlankAndDuplicateNames(t *testing.T) {
	set, err := NewPerspectiveSet([]string{"security", "  ", "security", ""}, nil)
	if err != nil {
		t.Fatalf("NewPerspectiveSet: %v", err)
	}
	if got := set.List(); len(got) != 1 || got[0] != PerspectiveSecurity {
		t.Errorf("List() = %v, want [security]", got)
	}
}

func TestParseFocusOverridesSkipsBlankKey(t *testing.T) {
	set, err := NewPerspectiveSet(nil, map[string]string{"   ": "orphan text", "style": "keep this"})
	if err != nil {
		t.Fatalf("NewPerspectiveSet: %v", err)
	}
	got := set.Overrides()
	if len(got) != 1 || got["style"] != "keep this" {
		t.Errorf("Overrides() = %v, want only the style entry", got)
	}
}
