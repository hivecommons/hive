package github

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Owner decision: issues park at stage/045-needs-decision; 085 is PR-only.
// The one-shot sweep moves every open ISSUE still wearing 085 to 045 in one
// write, leaves PRs wearing 085 alone, never deletes the 085 definition,
// and runs once per data dir.
func TestMigrateIssueNeedsDecisionStage_IssuesOnlyOnce(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	from, to := stagelabels.NeedsDecision.Label(), stagelabels.IssueNeedsDecision.Label()

	fake.mu.Lock()
	fake.truth[1] = []string{"kind/bug", from, "needs-human"}
	fake.truth[2] = []string{from, "agent/scanner"}
	fake.truth[3] = []string{to}
	fake.truth[200] = []string{from, "author/clanker"}
	fake.prs[200] = true
	fake.mu.Unlock()

	dir := t.TempDir()
	report, err := c.MigrateIssueNeedsDecisionStage(context.Background(), IssueNeedsDecisionMigrationOptions{DataDir: dir, Logger: testLogger()})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if report == nil || report.From != from || report.To != to || report.SkippedPRs != 1 || len(report.Errors) != 0 {
		t.Fatalf("report = %+v", report)
	}
	if len(report.Restaged) != 2 || report.Restaged[0].Number != 1 || report.Restaged[1].Number != 2 {
		t.Fatalf("restaged = %+v, want issues 1 and 2", report.Restaged)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, n := range []int{1, 2} {
		if got := strings.Join(fake.added[n], ","); got != to {
			t.Errorf("issue %d added %v, want %s", n, fake.added[n], to)
		}
		if got := strings.Join(fake.removed[n], ","); got != from {
			t.Errorf("issue %d removed %v, want %s", n, fake.removed[n], from)
		}
	}
	if !strings.Contains(strings.Join(fake.truth[1], ","), "needs-human") {
		t.Errorf("needs-human must survive the re-stage: %v", fake.truth[1])
	}
	if len(fake.added[3]) != 0 || len(fake.removed[3]) != 0 {
		t.Errorf("issue already at 045 must not be written: added=%v removed=%v", fake.added[3], fake.removed[3])
	}
	if len(fake.added[200]) != 0 || len(fake.removed[200]) != 0 || fake.gets[200] != 0 {
		t.Errorf("PR wearing 085 must be left alone: added=%v removed=%v gets=%d", fake.added[200], fake.removed[200], fake.gets[200])
	}
	if len(fake.deletedDefs) != 0 {
		t.Errorf("the 085 definition is PR-only, never deleted: %v", fake.deletedDefs)
	}

	if _, err := os.Stat(filepath.Join(dir, issueNeedsDecisionMigrationFileStem+".done")); err != nil {
		t.Fatalf("marker: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, issueNeedsDecisionMigrationFileStem+".json"))
	if err != nil {
		t.Fatalf("report file: %v", err)
	}
	var onDisk IssueNeedsDecisionMigrationReport
	if err := json.Unmarshal(raw, &onDisk); err != nil || onDisk.MigrationID != IssueNeedsDecisionMigrationID || len(onDisk.Restaged) != 2 {
		t.Fatalf("report on disk = %+v (%v)", onDisk, err)
	}

	// Second run: marker present, nothing listed, nothing written.
	fake.mu.Unlock()
	again, err := c.MigrateIssueNeedsDecisionStage(context.Background(), IssueNeedsDecisionMigrationOptions{DataDir: dir, Logger: testLogger()})
	fake.mu.Lock()
	if err != nil || again != nil {
		t.Fatalf("second run = %+v, %v; want nil, nil", again, err)
	}
	if len(fake.added[1]) != 1 || len(fake.added[2]) != 1 {
		t.Fatalf("second run wrote labels: %v %v", fake.added[1], fake.added[2])
	}
}
