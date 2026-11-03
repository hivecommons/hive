package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// review.require_labels: the reviewer's PR list offers only labelled PRs and
// says why; other agents' lists are untouched; an empty gate changes nothing.
func TestFormatPRList_ReviewRequireLabelsGate(t *testing.T) {
	actionable := &github.ActionableResult{}
	actionable.PRs.Items = []github.PullRequest{
		{Repo: "actions", Number: 10, Title: "labelled", Author: "a", Labels: []string{"bug", "Hive-Review"}},
		{Repo: "actions", Number: 11, Title: "unlabelled", Author: "b"},
	}
	actionable.PRs.Held = []github.PullRequest{
		{Repo: "actions", Number: 12, Title: "held unlabelled", Author: "c", Labels: []string{"hold"}},
	}

	t.Run("gate off lists every PR", func(t *testing.T) {
		s := newSchedulerWithIoscan(false)
		s.cfg.Project.Org = "org"
		out, _ := s.formatPRListWithPolicyForAgent(actionable, "reviewer")
		for _, n := range []string{"#10", "#11", "#12"} {
			if !strings.Contains(out, n) {
				t.Fatalf("gate off must list %s: %q", n, out)
			}
		}
		if strings.Contains(out, "REVIEW LABEL GATE") {
			t.Fatalf("gate off must not render the notice: %q", out)
		}
	})

	t.Run("gate on withholds unlabelled PRs from the reviewer", func(t *testing.T) {
		s := newSchedulerWithIoscan(false)
		s.cfg.Project.Org = "org"
		s.cfg.Review.RequireLabels = []string{"hive-review"}
		out, _ := s.formatPRListWithPolicyForAgent(actionable, "reviewer")
		if !strings.Contains(out, "#10") {
			t.Fatalf("labelled PR must be listed: %q", out)
		}
		if strings.Contains(out, "#11") || strings.Contains(out, "#12") {
			t.Fatalf("unlabelled PRs must be withheld: %q", out)
		}
		if !strings.Contains(out, "REVIEW LABEL GATE") || !strings.Contains(out, "hive-review") {
			t.Fatalf("notice must name the gate labels: %q", out)
		}
	})

	t.Run("gate on with nothing labelled renders the notice and (none)", func(t *testing.T) {
		s := newSchedulerWithIoscan(false)
		s.cfg.Project.Org = "org"
		s.cfg.Review.RequireLabels = []string{"hive-review"}
		none := &github.ActionableResult{}
		none.PRs.Items = []github.PullRequest{{Repo: "actions", Number: 11, Title: "unlabelled", Author: "b"}}
		out, _ := s.formatPRListWithPolicyForAgent(none, "reviewer")
		if !strings.Contains(out, "REVIEW LABEL GATE") || !strings.HasSuffix(out, "(none)") {
			t.Fatalf("expected notice + (none): %q", out)
		}
	})

	t.Run("non-reviewer lists are not gated", func(t *testing.T) {
		s := newSchedulerWithIoscan(false)
		s.cfg.Project.Org = "org"
		s.cfg.Review.RequireLabels = []string{"hive-review"}
		out, _ := s.formatPRListWithPolicyForAgent(actionable, "scanner")
		if !strings.Contains(out, "#11") {
			t.Fatalf("scanner's PR list must not be review-gated: %q", out)
		}
		if strings.Contains(out, "REVIEW LABEL GATE") {
			t.Fatalf("notice is reviewer-only: %q", out)
		}
	})
}
