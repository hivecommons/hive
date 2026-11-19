package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
)

// The Ask AI vendor identifiers must never appear in a config export (#19180).
func TestRedactConfigValueHidesAskKapaIdentifiers(t *testing.T) {
	in := map[string]any{
		"ask": map[string]any{
			"enabled": true,
			"kapa": map[string]any{
				"project_id":     "proj-secret-1",
				"integration_id": "integ-secret-2",
				"api_key_file":   "/secrets/kapa-key",
			},
		},
		// An unrelated block with a key of the same name is left alone.
		"other": map[string]any{"project_id": "visible-project"},
	}
	b, err := json.Marshal(redactConfigValue(in, ""))
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, hidden := range []string{"proj-secret-1", "integ-secret-2", "/secrets/kapa-key"} {
		if strings.Contains(out, hidden) {
			t.Fatalf("export carries %q: %s", hidden, out)
		}
	}
	if !strings.Contains(out, "visible-project") || !strings.Contains(out, `"enabled":true`) {
		t.Fatalf("redaction was too broad: %s", out)
	}
	// An unset value stays unset rather than becoming a marker.
	empty := map[string]any{"kapa": map[string]any{"project_id": ""}}
	if got := redactConfigValue(empty, "ask").(map[string]any)["kapa"].(map[string]any)["project_id"]; got != "" {
		t.Fatalf("empty project_id = %v", got)
	}
}
