package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Pins the contributors-dir resolution introduced for #6074/#6077:
// explicit SetContributorsDir > HIVE_CONTRIBUTORS_DIR > config-derived data
// root > compiled default, with nil-receiver safety throughout.
func TestContributorsDirOrDefaultPrecedence(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", "/env/contributors")

	s := NewServer(0, nil)
	s.deps = &Dependencies{Config: &config.Config{
		Data: config.DataConfig{AgentsDir: "/root/agents"},
	}}

	if got := s.contributorsDirOrDefault(); got != "/env/contributors" {
		t.Fatalf("env should win over config: got %q", got)
	}

	s.SetContributorsDir("/explicit/contributors")
	if got := s.contributorsDirOrDefault(); got != "/explicit/contributors" {
		t.Fatalf("explicit dir should win over env: got %q", got)
	}

	s.SetContributorsDir("")
	t.Setenv("HIVE_CONTRIBUTORS_DIR", "")
	if got := s.contributorsDirOrDefault(); got != "/root/contributors" {
		t.Fatalf("config-derived root should apply when env is empty: got %q", got)
	}

	s.deps = nil
	if got := s.contributorsDirOrDefault(); got != defaultContributorsDir {
		t.Fatalf("nil deps should fall back to default: got %q", got)
	}
}

func TestContributorsDirNilReceiverSafety(t *testing.T) {
	var s *Server
	s.SetContributorsDir("/ignored") // must not panic

	t.Setenv("HIVE_CONTRIBUTORS_DIR", "/env/contributors")
	if got := s.contributorsDirOrDefault(); got != "/env/contributors" {
		t.Fatalf("nil server should still honor env: got %q", got)
	}

	t.Setenv("HIVE_CONTRIBUTORS_DIR", "")
	if got := s.contributorsDirOrDefault(); got != defaultContributorsDir {
		t.Fatalf("nil server without env should default: got %q", got)
	}
}

func TestDataRootFromDir(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   ", ""},
		{"solo", ""},
		{sep + "x", ""},
		{filepath.Join(sep+"data", "contributors"), sep + "data"},
		{filepath.Join(sep+"data", "contributors") + sep, sep + "data"},
		{filepath.Join(sep+"data", "agents"), sep + "data"},
		{filepath.Join(sep+"srv", "hive", "metrics"), filepath.Join(sep+"srv", "hive")},
		{filepath.Join("rel", "agents"), "rel"},
	}
	for _, c := range cases {
		if got := dataRootFromDir(c.in); got != c.want {
			t.Errorf("dataRootFromDir(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The run-stats endpoint must resolve the task-run log through the server's
// contributors dir when no contribute hub exists, and surface unreadable logs
// as HTTP 500 rather than silently reporting an empty window.
func TestHandleContributeRunStatsServerFallback(t *testing.T) {
	dir := t.TempDir()
	line, err := json.Marshal(TaskRunRecord{
		TS:       time.Now().UTC().Format(time.RFC3339),
		Username: "alice",
		Backend:  "claude",
		Outcome:  "completed",
		Scenario: scenarioVerdictComplete,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, taskRunLogFileName), append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewServer(0, nil) // no contributeHub: exercises the server fallback
	s.SetContributorsDir(dir)

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/run-stats?days=1", nil)
	w := httptest.NewRecorder()
	s.handleContributeRunStats(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		WindowDays int                   `json:"window_days"`
		Total      int                   `json:"total"`
		Backends   []taskRunBackendStats `json:"backends"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response: %v", err)
	}
	if resp.WindowDays != 1 || resp.Total != 1 || len(resp.Backends) != 1 ||
		resp.Backends[0].Backend != "claude" {
		t.Fatalf("aggregate wrong: %+v", resp)
	}
}

func TestHandleContributeRunStatsUnreadableLog(t *testing.T) {
	dir := t.TempDir()
	// A directory where the log file should be makes ReadFile fail with a
	// non-IsNotExist error, which must map to HTTP 500.
	if err := os.MkdirAll(filepath.Join(dir, taskRunLogFileName), 0o755); err != nil {
		t.Fatal(err)
	}

	s := NewServer(0, nil)
	s.SetContributorsDir(dir)

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/run-stats", nil)
	w := httptest.NewRecorder()
	s.handleContributeRunStats(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}
