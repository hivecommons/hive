package config

import "testing"

func TestDefaultIntentConfigExposesBuiltInLists(t *testing.T) {
	d := DefaultIntentConfig()
	if len(d.TestPathPatterns) == 0 || len(d.DocsPathPatterns) == 0 ||
		len(d.GuardrailPathPatterns) == 0 || len(d.FeatureSignals) == 0 {
		t.Fatalf("DefaultIntentConfig returned an empty built-in list: %+v", d)
	}
	if d.Enforce || d.AlignmentModel != "" {
		t.Fatalf("DefaultIntentConfig should only carry pattern lists, got %+v", d)
	}
	d.TestPathPatterns[0] = "mutated"
	if DefaultIntentConfig().TestPathPatterns[0] == "mutated" {
		t.Fatal("DefaultIntentConfig must return copies, not the shared defaults")
	}
}
