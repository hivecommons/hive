package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// An issue the hive re-authored (its earlier PR closed as superseded after
// the fixer gave up) tells the kick to open a fresh PR declaring the
// supersession; an ordinary issue gets no such line.
func TestFormatIssueList_ReauthoredIssueHint(t *testing.T) {
	s := newSchedulerWithIoscan(false)
	issues := []github.Issue{
		{Repo: "test-org/console", Number: 7, Title: "fix the widget", AgeMinutes: 1, Labels: []string{"bug", stagelabels.LabelReauthored}},
		{Repo: "test-org/console", Number: 8, Title: "tidy the gadget", AgeMinutes: 1, Labels: []string{"bug"}},
	}
	out, _ := s.formatIssueListWithPolicy(issues)
	if strings.Count(out, "↳ re-authoring:") != 1 || !strings.Contains(out, "Supersedes #<old PR>") {
		t.Fatalf("re-author hint missing or repeated:\n%s", out)
	}
	if idx := strings.Index(out, "↳ re-authoring:"); idx < strings.Index(out, "#7") || idx > strings.Index(out, "#8") {
		t.Fatalf("hint not under issue #7:\n%s", out)
	}
}
