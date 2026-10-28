package dashboard

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// TestStageTrackerDecisionGlossaryMirrorsGo: the SPA's plain-words table
// (STAGE_TRACKER_DECISION_GLOSSARY) is byte-for-byte the Go glossary
// (pkg/delegatedecision.GlossaryJSON), so toast, popover, PR comment and
// tooltip all say the same thing about a rule (hivecommons/hive#14822).
// Regenerate the JS block from GlossaryJSON() when this fails.
func TestStageTrackerDecisionGlossaryMirrorsGo(t *testing.T) {
	html := indexHTML(t)
	block := jsConstBlock(t, html, "STAGE_TRACKER_DECISION_GLOSSARY")
	raw := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(block, "const STAGE_TRACKER_DECISION_GLOSSARY = ")), ";")
	// Trailing commas are JS, not JSON.
	raw = strings.ReplaceAll(raw, ",\n    }", "\n    }")
	var js, want map[string]map[string]string
	if err := json.Unmarshal([]byte(raw), &js); err != nil {
		t.Fatalf("STAGE_TRACKER_DECISION_GLOSSARY is not JSON: %v\n%s", err, raw)
	}
	if err := json.Unmarshal([]byte(delegatedecision.GlossaryJSON()), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(js, want) {
		for k, v := range want {
			if !reflect.DeepEqual(js[k], v) {
				t.Errorf("%q: js=%v go=%v", k, js[k], v)
			}
		}
		for k := range js {
			if _, ok := want[k]; !ok {
				t.Errorf("%q: in JS only", k)
			}
		}
	}
	for _, rule := range []string{"contributor-offline", "sentinel-trusted", "default-release"} {
		if _, ok := js[rule]; !ok {
			t.Errorf("rule %q missing from the JS mirror", rule)
		}
	}
}
