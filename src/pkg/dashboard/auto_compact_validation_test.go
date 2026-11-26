package dashboard

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandleAgentCreate_AutoCompactWindowTokens(t *testing.T) {
	for _, tc := range []struct {
		value int
		want int
	}{{-1, http.StatusBadRequest}, {50000, http.StatusBadRequest}, {0, http.StatusOK}, {100000, http.StatusOK}} {
		s, deps := apiServer(t)
		dir := t.TempDir()
		deps.Config.Data.AgentsDir = dir
		rec := doPost(s, "/api/agents", map[string]interface{}{
			"name": "compact-test",
			"agent": map[string]interface{}{"backend": "claude", "auto_compact_window_tokens": tc.value},
		})
		if rec.Code != tc.want {
			t.Fatalf("value %d: got %d %s, want %d", tc.value, rec.Code, rec.Body.String(), tc.want)
		}
		if tc.want == http.StatusBadRequest {
			if !strings.Contains(rec.Body.String(), "auto_compact_window_tokens") {
				t.Fatalf("value %d: missing validation error: %s", tc.value, rec.Body.String())
			}
			if _, ok := deps.Config.Agents["compact-test"]; ok {
				t.Fatalf("value %d: invalid agent added to configuration", tc.value)
			}
			if _, err := os.Stat(filepath.Join(dir, "compact-test")); !os.IsNotExist(err) {
				t.Fatalf("value %d: agent directory exists or cannot be checked: %v", tc.value, err)
			}
		} else if got := deps.Config.Agents["compact-test"].AutoCompactWindowTokens; got != tc.value {
			t.Fatalf("value %d: saved %d", tc.value, got)
		}
	}
}
