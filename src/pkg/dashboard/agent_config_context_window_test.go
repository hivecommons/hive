package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// agents.<key>.max_context_tokens and auto_compact_window_tokens had no
// Settings control (#19913), so an operator on a gateway that reports no
// context window had to hand-edit the dashboard-owned overlay. The General
// section now reads and writes both, saves them to the per-agent file, and
// restarts the agent on a change because the CLI reads them at launch.

func TestAgentConfigGeneral_ContextWindow(t *testing.T) {
	s := acfgServer(t)
	restarts := func() int {
		st, err := s.deps.AgentMgr.GetStatus("scanner")
		if err != nil {
			t.Fatal(err)
		}
		return st.RestartCount
	}
	put := func(body map[string]any, want int) {
		t.Helper()
		if rec := doPut(s, "/api/config/agent/scanner/general", body); rec.Code != want {
			t.Fatalf("PUT %v: %d %s, want %d", body, rec.Code, rec.Body.String(), want)
		}
	}
	readGeneral := func() map[string]any {
		t.Helper()
		rec := doGet(s, "/api/config/agent/scanner")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
		}
		var body struct {
			General map[string]any `json:"general"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.General
	}
	fileAgent := func() config.AgentConfig {
		t.Helper()
		files, err := config.LoadAgentOverrides(s.deps.Config.Data.AgentsDir)
		if err != nil {
			t.Fatalf("LoadAgentOverrides: %v", err)
		}
		return files["scanner"]
	}

	g := readGeneral()
	if g["maxContextTokens"] != float64(0) || g["autoCompactWindowTokens"] != float64(0) {
		t.Fatalf("defaults = %v / %v, want 0 / 0", g["maxContextTokens"], g["autoCompactWindowTokens"])
	}
	base := restarts()

	put(map[string]any{"maxContextTokens": 196608, "autoCompactWindowTokens": 160000}, http.StatusOK)
	a := s.deps.Config.Agents["scanner"]
	if a.MaxContextTokens != 196608 || a.AutoCompactWindowTokens != 160000 {
		t.Fatalf("config = %d / %d, want 196608 / 160000", a.MaxContextTokens, a.AutoCompactWindowTokens)
	}
	if f := fileAgent(); f.MaxContextTokens != 196608 || f.AutoCompactWindowTokens != 160000 {
		t.Errorf("per-agent file = %d / %d, want 196608 / 160000", f.MaxContextTokens, f.AutoCompactWindowTokens)
	}
	if g := readGeneral(); g["maxContextTokens"] != float64(196608) || g["autoCompactWindowTokens"] != float64(160000) {
		t.Errorf("GET after save = %v / %v", g["maxContextTokens"], g["autoCompactWindowTokens"])
	}
	if got := restarts(); got != base+1 {
		t.Fatalf("setting the windows must restart once (%d->%d)", base, got)
	}

	put(map[string]any{"maxContextTokens": 196608, "autoCompactWindowTokens": 160000}, http.StatusOK)
	if got := restarts(); got != base+1 {
		t.Fatalf("re-saving the same values must not restart (%d->%d)", base+1, got)
	}

	// A compaction window the config validator rejects is refused, not saved.
	put(map[string]any{"autoCompactWindowTokens": 50000}, http.StatusBadRequest)
	put(map[string]any{"maxContextTokens": -1}, http.StatusBadRequest)
	if a := s.deps.Config.Agents["scanner"]; a.MaxContextTokens != 196608 || a.AutoCompactWindowTokens != 160000 {
		t.Errorf("rejected values changed the config: %d / %d", a.MaxContextTokens, a.AutoCompactWindowTokens)
	}

	put(map[string]any{"maxContextTokens": 0, "autoCompactWindowTokens": 0}, http.StatusOK)
	if a := s.deps.Config.Agents["scanner"]; a.MaxContextTokens != 0 || a.AutoCompactWindowTokens != 0 {
		t.Errorf("clearing left %d / %d in the config", a.MaxContextTokens, a.AutoCompactWindowTokens)
	}
	if f := fileAgent(); f.MaxContextTokens != 0 || f.AutoCompactWindowTokens != 0 {
		t.Errorf("clearing left %d / %d in the per-agent file", f.MaxContextTokens, f.AutoCompactWindowTokens)
	}
	if got := restarts(); got != base+2 {
		t.Fatalf("clearing the windows must restart once (%d->%d)", base+1, got)
	}
}
