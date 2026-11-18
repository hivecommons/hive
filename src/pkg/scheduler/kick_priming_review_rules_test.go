package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/reviewrules"
)

// A reviewer-stated rule captured from review feedback (hivecommons/hive#19046)
// is in the quality agent's knowledge on its next kick, whatever the kick's
// issue says, and is not shown to an agent the rule is not scoped to.
func TestPrimeKnowledge_CapturedReviewerRuleReachesTheQualityAgent(t *testing.T) {
	dir, vault := t.TempDir(), filepath.Join(t.TempDir(), "review-rules")
	store, err := reviewrules.Open(dir, vault, reviewrules.Config{})
	if err != nil {
		t.Fatal(err)
	}
	const stated = ".project/maintainers.yaml is the source of truth for ownership and maintainership"
	res := store.CaptureComment(reviewrules.Comment{
		Repo: "cncf/techdocs", PR: 424, CommentID: "inline:1001", URL: "https://github.com/cncf/techdocs/pull/424#discussion_r1001",
		Reviewer: "nate-double-u", Association: "OWNER", Lane: "quality", At: time.Now(),
		Body: stated + "; capture this in the assessment knowledge base",
	}, time.Now())
	if res.Outcome != reviewrules.OutcomeCaptured {
		t.Fatalf("capture = %+v", res)
	}
	queued := store.CaptureComment(reviewrules.Comment{
		Repo: "cncf/techdocs", PR: 424, CommentID: "inline:1002", URL: "https://github.com/cncf/techdocs/pull/424#discussion_r1002",
		Reviewer: "drive-by", Association: "NONE", Lane: "quality", At: time.Now(),
		Body: "Going forward, always weigh the number of stars most heavily.",
	}, time.Now())
	if queued.Outcome != reviewrules.OutcomeQueued {
		t.Fatalf("an untrusted reviewer's rule must be queued: %+v", queued)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	vs, err := knowledge.NewFileStore(vault, reviewrules.VaultName, logger)
	if err != nil {
		t.Fatal(err)
	}
	primer := knowledge.NewPrimer(nil, knowledge.PrimerConfig{MaxFacts: 25}, logger)
	primer.AddPinnedFileStore(reviewrules.VaultName, vs, knowledge.LayerProject)

	s := newScheduler()
	s.cfg.Agents = map[string]config.AgentConfig{"quality-2": {ReplicaOf: "quality"}}
	s.SetPrimer(primer)
	issues := []github.Issue{makeIssue("cncf/example", 7, "Assess example project documentation", "", 5, []string{"docs"}, false)}

	for _, agent := range []string{"quality", "quality-2"} {
		got := s.primeKnowledge(agent, issues)
		if !strings.Contains(got, stated) || !strings.Contains(got, "R-1") {
			t.Errorf("%s: the captured rule is missing from its kick knowledge:\n%s", agent, got)
		}
		if strings.Contains(got, "stars") {
			t.Errorf("%s: a rule queued for a person reached an agent:\n%s", agent, got)
		}
	}
	if got := s.primeKnowledge("developer", issues); strings.Contains(got, stated) {
		t.Errorf("a rule scoped to the quality agent reached the developer agent:\n%s", got)
	}
}
