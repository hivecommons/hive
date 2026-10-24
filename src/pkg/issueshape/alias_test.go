package issueshape

import "testing"

// TestAliasesAreTheCruxValidators pins the alias direction: the watcher and
// the proxy reject the same unfilled placeholders because both call the Crux
// ring's validators.
func TestAliasesAreTheCruxValidators(t *testing.T) {
	if ph, ok := UnsubstitutedTemplatePlaceholder("fix <analysis> now"); !ok || ph != "<analysis>" {
		t.Fatalf("UnsubstitutedTemplatePlaceholder = %q, %v", ph, ok)
	}
	if !BodyHasMisEscapedNewlines(`## Summary\n\nfix\n\n## Detail`) {
		t.Fatal("BodyHasMisEscapedNewlines through alias = false")
	}
	if opt, ok := UnchosenOptionList("Impact: high/medium/low"); !ok || opt == "" {
		t.Fatalf("UnchosenOptionList = %q, %v", opt, ok)
	}
}
