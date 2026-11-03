package config

import "github.com/hivecommons/hive/pkg/intent"

// DefaultIntentConfig returns the built-in intent classifier lists that an
// empty intent.* list falls back to, as an IntentConfig so callers that
// already depend on pkg/config (the dashboard settings UI) can show them
// without importing pkg/intent directly.
func DefaultIntentConfig() IntentConfig {
	d := intent.DefaultConfig()
	return IntentConfig{
		TestPathPatterns:      d.TestPathPatterns,
		DocsPathPatterns:      d.DocsPathPatterns,
		GuardrailPathPatterns: d.GuardrailPathPatterns,
		FeatureSignals:        d.FeatureSignals,
	}
}
