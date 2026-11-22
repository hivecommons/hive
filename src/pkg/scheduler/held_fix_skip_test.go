package scheduler

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// A red PR a person holds is kept out of the shared CI repair queue; that
// skip is logged once per PR and head, so "never handed to the fixer" is not
// silent, and a new head logs again.
func TestBuildCIFailingListLogsAHeldSkipOncePerHead(t *testing.T) {
	var logs bytes.Buffer
	cfg := &config.Config{Project: config.ProjectConfig{Org: "test-org", Repos: []string{"test-org/console"}}}
	s := New(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	path := overrideCIFailingPath(t)
	write := func(head string) {
		t.Helper()
		data := `{"ci_failing":[` +
			`{"number":466,"repo":"test-org/console","title":"held","author":"hive[bot]","head_sha":"` + head + `","held":true,"failing_checks":["mutation"],"reachable_action":"push"},` +
			`{"number":467,"repo":"test-org/console","title":"open","author":"hive[bot]","head_sha":"cafe","failing_checks":["lint"],"reachable_action":"push"}]}`
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("aaaa")
	for i := 0; i < 3; i++ {
		out := s.buildCIFailingListForAgent("scanner", nil)
		if strings.Contains(out, "#466 ") {
			t.Fatalf("held PR listed in the shared repair queue:\n%s", out)
		}
	}
	if n := strings.Count(logs.String(), HeldFixSkipMessage); n != 1 {
		t.Fatalf("held skip logged %d times for one head, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "pr=466") || !strings.Contains(logs.String(), "head=aaaa") || strings.Contains(logs.String(), "pr=467") {
		t.Fatalf("log does not name the held PR and head only:\n%s", logs.String())
	}

	write("bbbb")
	_ = s.buildCIFailingListForAgent("scanner", nil)
	if n := strings.Count(logs.String(), HeldFixSkipMessage); n != 2 {
		t.Fatalf("held skip logged %d times after a new head, want twice:\n%s", n, logs.String())
	}
}

func TestLogHeldFixSkipNilSafe(t *testing.T) {
	var s *Scheduler
	s.logHeldFixSkip("ci-repair", "o/r", 1, "abc")
	(&Scheduler{}).logHeldFixSkip("ci-repair", "o/r", 1, "abc")
}
