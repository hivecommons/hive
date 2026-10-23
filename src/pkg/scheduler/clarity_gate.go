package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// ClarityGateMarker tags the clarity gate's question comment. Its presence
// on an issue means the author has been asked once; the gate never posts a
// second one (hivecommons/hive#13577). github.SweepClarityGateResume reads it
// back to lift the park once the author answers (hivecommons/hive#13588).
const ClarityGateMarker = github.ClarityGateMarker

// clarityGateTimeout bounds the GitHub calls the gate makes for one issue.
const clarityGateTimeout = 30 * time.Second

// clarityStaleSnapshotWindow is how long an issue the gate just parked stays
// withheld when the snapshot carries no updated_at to prove it was re-read.
const clarityStaleSnapshotWindow = 15 * time.Minute

// TriageLabeler is the label half of the clarity gate's forge surface. The
// TriageCommenter passed to SetRunTriageDeps provides it when it can; without
// it the gate cannot park and stays a no-op.
type TriageLabeler interface {
	AddLabels(ctx context.Context, repo string, number int, labels []string) error
}

// TriageParkHistory is the optional third part of the gate's forge surface:
// whether the park label ever landed on an issue, present or not. With it the
// gate tells a park label write that failed before a restart (the question
// is on the issue, the label never was: retry it) from an issue a person
// un-parked (the label was there and came off: run it). Without it the
// marker alone is read as un-parked.
type TriageParkHistory interface {
	IssueLabelEverApplied(ctx context.Context, repo string, number int, label string) (bool, error)
}

// clarityGateState is the gate's process memory, keyed owner/repo#n.
type clarityGateState struct {
	mu sync.Mutex
	// parkedAt records issues this process parked, so the snapshot the kick
	// was built from (enumerated before the park label landed) cannot hand
	// the same issue to an agent before the next enumeration drops it.
	parkedAt map[string]time.Time
	// pendingLabel holds issues whose question was posted but whose park
	// label write failed; the label is retried before anything else. Across
	// a restart the same case is recovered from the forge via
	// TriageParkHistory rather than from this map.
	pendingLabel map[string]bool
	// asked holds issues known to carry the marker and no park label: the
	// author was asked and the issue has since been un-parked, so it runs.
	asked map[string]bool
}

func (c *clarityGateState) init() {
	if c.parkedAt == nil {
		c.parkedAt = map[string]time.Time{}
		c.pendingLabel = map[string]bool{}
		c.asked = map[string]bool{}
	}
}

// applyClarityGate withholds every gated issue whose clarity score is under
// runs.triage.clarity_gate.threshold: it asks the author concise questions
// in one marker comment and parks the issue with the needs-spec
// hard-suppress label (stage/045-needs-decision, "parked on a person:
// needs-spec"), so it is neither kicked nor claimable until the label is
// removed. Off by default; with it off the issue list is returned as is.
func (s *Scheduler) applyClarityGate(issues []github.Issue) []github.Issue {
	gate := s.cfg.Runs.Triage.ClarityGate
	if !gate.Enabled || len(issues) == 0 {
		return issues
	}
	_, commenter := s.runTriageDeps()
	labeler, ok := commenter.(TriageLabeler)
	if commenter == nil || !ok {
		return issues
	}
	threshold := gate.EffectiveThreshold()
	minBody := s.cfg.Runs.Triage.EffectiveMinBodyChars()
	label := clarityParkLabel(s.cfg)
	out := make([]github.Issue, 0, len(issues))
	for _, issue := range issues {
		if !clarityGated(issue) {
			out = append(out, issue)
			continue
		}
		assessment := classify.ScoreClarity(issue.Body, minBody)
		if assessment.Score >= threshold {
			out = append(out, issue)
			continue
		}
		if !s.parkUnclearIssue(commenter, labeler, issue, assessment, threshold, label) {
			out = append(out, issue)
		}
	}
	return out
}

// clarityGated reports whether the gate judges issue at all: an unclaimed
// GitHub issue a person filed, with no `approved-direction` from a
// maintainer. Hive- and agent-filed issues have nobody to answer questions.
func clarityGated(issue github.Issue) bool {
	if issue.Repo == "" || issue.Number <= 0 || !issue.AuthorIsHuman {
		return false
	}
	if issue.SourceType != "" && !strings.EqualFold(issue.SourceType, "github") {
		return false
	}
	if strings.TrimSpace(issue.ClaimedBy) != "" {
		return false
	}
	return !issueHasLabel(issue, github.HumanAckLabel)
}

// clarityParkLabel is the needs-spec hard-suppress bucket's first label
// (project.issue_filter.hard_suppress_labels.needs_spec, default needs-spec).
func clarityParkLabel(cfg *config.Config) string {
	return cfg.Project.IssueFilter.HardSuppressLabels.EffectiveNeedsSpec()[0]
}

// parkUnclearIssue asks and parks one unclear issue and reports whether it
// must be withheld from this kick. An issue that already carries the marker
// (asked before, un-parked since) runs; a GitHub error withholds it for
// this cycle only.
func (s *Scheduler) parkUnclearIssue(commenter TriageCommenter, labeler TriageLabeler, issue github.Issue, a classify.ClarityAssessment, threshold int, label string) bool {
	key := issue.Repo + "#" + strconv.Itoa(issue.Number)
	now := time.Now()
	st := &s.clarity
	st.mu.Lock()
	st.init()
	parkedAt, parked := st.parkedAt[key]
	pending := st.pendingLabel[key]
	asked := st.asked[key]
	st.mu.Unlock()

	if parked {
		fresh := issue.UpdatedAt.After(parkedAt) || (issue.UpdatedAt.IsZero() && now.Sub(parkedAt) > clarityStaleSnapshotWindow)
		if !fresh || issueHasLabel(issue, label) {
			if !pending {
				return true
			}
		} else {
			st.mu.Lock()
			delete(st.parkedAt, key)
			st.mu.Unlock()
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), clarityGateTimeout)
	defer cancel()
	if pending {
		s.addClarityParkLabel(ctx, labeler, issue, key, label, now)
		return true
	}
	if asked {
		return false
	}
	seen, err := commenter.IssueCommentsContain(ctx, issue.Repo, issue.Number, ClarityGateMarker)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("clarity gate comment scan failed", "repo", issue.Repo, "number", issue.Number, "error", err)
		}
		return true
	}
	if seen {
		if issueHasLabel(issue, label) {
			return true
		}
		landed, err := s.clarityParkLabelLanded(ctx, labeler, issue, label)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("clarity gate park history read failed", "repo", issue.Repo, "number", issue.Number, "label", label, "error", err)
			}
			return true
		}
		if !landed {
			// The question went out but the park never did, and the
			// process that would have retried it is gone: park now.
			s.addClarityParkLabel(ctx, labeler, issue, key, label, now)
			return true
		}
		st.mu.Lock()
		st.asked[key] = true
		st.mu.Unlock()
		return false
	}
	if err := commenter.CreateIssueComment(ctx, issue.Repo, issue.Number, renderClarityGateComment(a, threshold, label)); err != nil {
		if s.logger != nil {
			s.logger.Warn("clarity gate comment failed", "repo", issue.Repo, "number", issue.Number, "error", err)
		}
		return true
	}
	s.addClarityParkLabel(ctx, labeler, issue, key, label, now)
	if s.logger != nil {
		s.logger.Info("clarity gate parked issue", "repo", issue.Repo, "number", issue.Number, "score", a.Score, "threshold", threshold, "label", label)
	}
	return true
}

// clarityParkLabelLanded reports whether the park label was ever on the
// issue. A labeler without TriageParkHistory cannot tell, and is read as yes
// so that the marker keeps meaning "asked and un-parked", as before.
func (s *Scheduler) clarityParkLabelLanded(ctx context.Context, labeler TriageLabeler, issue github.Issue, label string) (bool, error) {
	history, ok := labeler.(TriageParkHistory)
	if !ok {
		return true, nil
	}
	return history.IssueLabelEverApplied(ctx, issue.Repo, issue.Number, label)
}

func (s *Scheduler) addClarityParkLabel(ctx context.Context, labeler TriageLabeler, issue github.Issue, key, label string, now time.Time) {
	err := labeler.AddLabels(ctx, issue.Repo, issue.Number, []string{label})
	st := &s.clarity
	st.mu.Lock()
	defer st.mu.Unlock()
	st.parkedAt[key] = now
	if err != nil {
		st.pendingLabel[key] = true
		if s.logger != nil {
			s.logger.Warn("clarity gate park label failed", "repo", issue.Repo, "number", issue.Number, "label", label, "error", err)
		}
		return
	}
	delete(st.pendingLabel, key)
}

// renderClarityGateComment is the one question comment the gate posts.
func renderClarityGateComment(a classify.ClarityAssessment, threshold int, label string) string {
	var b strings.Builder
	b.WriteString(ClarityGateMarker + "\n")
	b.WriteString("Thanks for filing this. Before Hive starts work, a few quick questions so the fix matches what you expect")
	fmt.Fprintf(&b, " (clarity score %d/100, needs %d):\n\n", a.Score, threshold)
	questions := a.Questions
	if len(questions) == 0 {
		questions = []string{"Could you add more detail on the expected result and how to verify it?"}
	}
	for _, q := range questions {
		b.WriteString("- " + q + "\n")
	}
	fmt.Fprintf(&b, "\nHive has parked this issue with `%s` instead of claiming it. Please reply here with the answers (updating the issue body as well helps); once you reply, Hive removes `%s` and the issue goes back into the normal queue.\n", label, label)
	return b.String()
}
