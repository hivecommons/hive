package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// agents.<key>.headless_turns had no Settings control (#19902), so an
// operator on an existing Kubernetes spoke had no supported way to set it.
// The General section now reads and writes it, saves the per-agent file with
// it, and restarts the agent on a change because the pane's shape is fixed
// at launch.

func TestAgentConfigGeneral_HeadlessTurns(t *testing.T) {
	s := acfgServer(t)
	restarts := func() int {
		st, err := s.deps.AgentMgr.GetStatus("scanner")
		if err != nil {
			t.Fatal(err)
		}
		return st.RestartCount
	}
	put := func(on bool) {
		t.Helper()
		if rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"headlessTurns": on}); rec.Code != http.StatusOK {
			t.Fatalf("headlessTurns=%v: %d %s", on, rec.Code, rec.Body.String())
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
	fileHeadless := func() bool {
		t.Helper()
		files, err := config.LoadAgentOverrides(s.deps.Config.Data.AgentsDir)
		if err != nil {
			t.Fatalf("LoadAgentOverrides: %v", err)
		}
		return files["scanner"].HeadlessTurnsEnabled()
	}

	if g := readGeneral(); g["headlessTurns"] != false {
		t.Fatalf("default headlessTurns = %v, want false", g["headlessTurns"])
	}
	base := restarts()

	put(true)
	if !s.deps.Config.Agents["scanner"].HeadlessTurnsEnabled() {
		t.Fatal("headlessTurns: true not applied to the config")
	}
	if !fileHeadless() {
		t.Error("headlessTurns: true not saved to the per-agent file")
	}
	if g := readGeneral(); g["headlessTurns"] != true {
		t.Errorf("GET headlessTurns = %v after enabling", g["headlessTurns"])
	}
	if got := restarts(); got != base+1 {
		t.Fatalf("enabling headless turns must restart once (%d->%d)", base, got)
	}

	put(true)
	if got := restarts(); got != base+1 {
		t.Fatalf("re-saving the same value must not restart (%d->%d)", base+1, got)
	}

	put(false)
	if s.deps.Config.Agents["scanner"].HeadlessTurnsEnabled() {
		t.Error("headlessTurns: false not applied to the config")
	}
	if fileHeadless() {
		t.Error("headlessTurns: false not saved to the per-agent file")
	}
	if got := restarts(); got != base+2 {
		t.Fatalf("disabling headless turns must restart once (%d->%d)", base+1, got)
	}
}
