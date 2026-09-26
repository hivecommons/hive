package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// jev_mode on both agent write paths (hivecommons/hive#8939), mirroring the
// caveman_mode gates: an invalid value is a 400 naming the allowed set, never a
// persisted value that fails the next config load.

const jevModeErr = "jev_mode must be one of: off, assist (or empty to disable)"

func TestHandleAgentCreate_JevMode(t *testing.T) {
	for _, mode := range []string{"always", "ASSIST", " assist", "on"} {
		s, deps := apiServer(t)
		deps.Config.Data.AgentsDir = t.TempDir()
		rec := doPost(s, "/api/agents", map[string]interface{}{
			"name":  "jev-bad",
			"agent": map[string]interface{}{"backend": "claude", "jev_mode": mode},
		})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), jevModeErr) {
			t.Errorf("jev_mode=%q: got %d %s", mode, rec.Code, rec.Body.String())
		}
		if _, ok := deps.Config.Agents["jev-bad"]; ok {
			t.Fatalf("jev_mode=%q: invalid agent must not be persisted", mode)
		}
	}
	for _, mode := range []string{"", "off", "assist"} {
		s, deps := apiServer(t)
		deps.Config.Data.AgentsDir = t.TempDir()
		rec := doPost(s, "/api/agents", map[string]interface{}{
			"name":  "jev-ok",
			"agent": map[string]interface{}{"backend": "claude", "jev_mode": mode},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("jev_mode=%q: got %d %s", mode, rec.Code, rec.Body.String())
		}
		if got := deps.Config.Agents["jev-ok"].JevMode; got != mode {
			t.Errorf("jev_mode=%q: persisted %q", mode, got)
		}
	}
}

func TestAgentConfigGeneral_JevMode(t *testing.T) {
	s := acfgServer(t)
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"jevMode": "always"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), jevModeErr) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	for _, mode := range []string{"assist", "off", ""} {
		rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"jevMode": mode})
		if rec.Code != http.StatusOK {
			t.Fatalf("jevMode=%q: got %d %s", mode, rec.Code, rec.Body.String())
		}
		if got := s.deps.Config.Agents["scanner"].JevMode; got != mode {
			t.Errorf("jevMode=%q: persisted %q", mode, got)
		}
	}
}

// TestAgentConfigGet_JevReadiness: the general payload carries jevMode and a
// jevReady flag that flips with key availability — and never the key.
func TestAgentConfigGet_JevReadiness(t *testing.T) {
	t.Setenv("JEV_API_KEY", "")
	s := acfgServer(t)
	s.deps.Config.Governor.Gateways = nil
	read := func() map[string]any {
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
	g := read()
	if g["jevReady"] != false {
		t.Errorf("no key: jevReady = %v", g["jevReady"])
	}
	if _, has := g["jevMode"]; !has {
		t.Error("general payload must carry jevMode")
	}
	t.Setenv("JEV_API_KEY", "sk-jev-secret")
	rec := doGet(s, "/api/config/agent/scanner")
	if strings.Contains(rec.Body.String(), "sk-jev-secret") {
		t.Fatal("the Jev key must never appear in the agent config payload")
	}
	if g := read(); g["jevReady"] != true {
		t.Errorf("with key: jevReady = %v", g["jevReady"])
	}
}

// TestAgentConfigGeneral_JevModeChangeRestarts: jev_mode is applied at launch
// (skill + HIVE_JEV_MODE), so flipping it from the dialog must restart the
// agent like a model/backend change does; re-saving the same effective value
// (assist→assist, ""→off) must not.
func TestAgentConfigGeneral_JevModeChangeRestarts(t *testing.T) {
	s := acfgServer(t)
	restarts := func() int {
		st, err := s.deps.AgentMgr.GetStatus("scanner")
		if err != nil {
			t.Fatal(err)
		}
		return st.RestartCount
	}
	put := func(mode string) {
		if rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"jevMode": mode}); rec.Code != http.StatusOK {
			t.Fatalf("jevMode=%q: %d %s", mode, rec.Code, rec.Body.String())
		}
	}
	base := restarts()
	put("off") // "" → off: same effective state, no restart
	if got := restarts(); got != base {
		t.Fatalf("\"\"→off restarted (%d→%d)", base, got)
	}
	put("assist")
	if got := restarts(); got != base+1 {
		t.Fatalf("off→assist must restart once (%d→%d)", base, got)
	}
	put("assist")
	if got := restarts(); got != base+1 {
		t.Fatalf("assist→assist must not restart (%d→%d)", base+1, got)
	}
	put("")
	if got := restarts(); got != base+2 {
		t.Fatalf("assist→\"\" must restart once (%d→%d)", base+1, got)
	}
}
