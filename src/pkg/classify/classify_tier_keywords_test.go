package classify

import (
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// resetTierKeywords restores the default (unconfigured) tier keyword state so
// tests do not leak configuration into one another.
func resetTierKeywords() { SetTierKeywords(nil, nil) }

func TestSetTierKeywords_DefaultsWhenUnset(t *testing.T) {
	defer resetTierKeywords()
	resetTierKeywords()

	simple, complex := TierKeywords()
	if !reflect.DeepEqual(simple, defaultSimpleKeywords) {
		t.Errorf("simple defaults = %v, want %v", simple, defaultSimpleKeywords)
	}
	if !reflect.DeepEqual(complex, defaultComplexSignals) {
		t.Errorf("complex defaults = %v, want %v", complex, defaultComplexSignals)
	}
}

func TestClassifyTier_DefaultsUnchanged(t *testing.T) {
	defer resetTierKeywords()
	resetTierKeywords()

	cases := []struct {
		title string
		want  Tier
	}{
		{"Fix typo in README", TierSimple},           // default simple keyword
		{"rename the variable", TierSimple},          // default simple keyword
		{"race condition in scheduler", TierComplex}, // default complex signal
		{"memory leak in cache", TierComplex},        // default complex signal
		{"add a new dashboard card", TierMedium},     // neither
	}
	for _, tc := range cases {
		got := Classify(github.Issue{Title: tc.title}).Tier
		if got != tc.want {
			t.Errorf("Classify(%q).Tier = %q, want %q", tc.title, got, tc.want)
		}
	}
}

func TestSetTierKeywords_Overrides(t *testing.T) {
	defer resetTierKeywords()
	SetTierKeywords([]string{"trivial"}, []string{"quantum"})

	simple, complex := TierKeywords()
	if !reflect.DeepEqual(simple, []string{"trivial"}) {
		t.Errorf("simple override = %v", simple)
	}
	if !reflect.DeepEqual(complex, []string{"quantum"}) {
		t.Errorf("complex override = %v", complex)
	}

	// The custom keywords now drive classification...
	if got := Classify(github.Issue{Title: "a trivial change"}).Tier; got != TierSimple {
		t.Errorf("custom simple: got %q, want Simple", got)
	}
	if got := Classify(github.Issue{Title: "quantum entanglement bug"}).Tier; got != TierComplex {
		t.Errorf("custom complex: got %q, want Complex", got)
	}
	// ...and the OLD defaults no longer apply (they were replaced).
	if got := Classify(github.Issue{Title: "fix typo"}).Tier; got == TierSimple {
		t.Errorf("default 'typo' should not classify Simple after override, got %q", got)
	}
	if got := Classify(github.Issue{Title: "race condition"}).Tier; got == TierComplex {
		t.Errorf("default 'race condition' should not classify Complex after override, got %q", got)
	}
}

func TestSetTierKeywords_PartialKeepsOtherDefault(t *testing.T) {
	defer resetTierKeywords()
	// Configure only simple; complex must keep its default set.
	SetTierKeywords([]string{"tweak"}, nil)

	simple, complex := TierKeywords()
	if !reflect.DeepEqual(simple, []string{"tweak"}) {
		t.Errorf("simple = %v, want [tweak]", simple)
	}
	if !reflect.DeepEqual(complex, defaultComplexSignals) {
		t.Errorf("complex = %v, want defaults", complex)
	}
	// Default complex signal still classifies since only simple was overridden.
	if got := Classify(github.Issue{Title: "deadlock in queue"}).Tier; got != TierComplex {
		t.Errorf("default complex 'deadlock' lost: got %q", got)
	}
}

func TestTierKeywords_ReturnsCopies(t *testing.T) {
	defer resetTierKeywords()
	SetTierKeywords([]string{"a"}, []string{"b"})

	simple, complex := TierKeywords()
	simple[0] = "mutated"
	complex[0] = "mutated"

	// A fresh read must be unaffected by the caller's mutation.
	simple2, complex2 := TierKeywords()
	if simple2[0] != "a" || complex2[0] != "b" {
		t.Errorf("TierKeywords did not return copies: %v %v", simple2, complex2)
	}
}

func TestSetTierKeywords_EmptySliceFallsBackToDefault(t *testing.T) {
	defer resetTierKeywords()
	SetTierKeywords([]string{}, []string{})
	simple, complex := TierKeywords()
	if !reflect.DeepEqual(simple, defaultSimpleKeywords) || !reflect.DeepEqual(complex, defaultComplexSignals) {
		t.Errorf("empty slices should fall back to defaults, got %v / %v", simple, complex)
	}
}
