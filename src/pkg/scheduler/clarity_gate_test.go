package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// fakeClarityForge records the clarity gate's comments and labels; the
// marker scan reads back what was posted, like GitHub would.
type fakeClarityForge struct {
	comments map[string][]string
	labels   map[string][]string
	// applied is the label history GitHub's timeline would hold: every
	// label that ever landed on the issue, whether it is still there.
	applied    map[string][]string
	labelErr   error
	scanErr    error
	historyErr error
	scanCalled int
}

func newFakeClarityForge() *fakeClarityForge {
	return &fakeClarityForge{comments: map[string][]string{}, labels: map[string][]string{}, applied: map[string][]string{}}
}

func clarityKey(repo string, number int) string {
	return repo + "#" + strconv.Itoa(number)
}

func (f *fakeClarityForge) IssueCommentsContain(ctx context.Context, repo string, number int, needle string) (bool, error) {
	f.scanCalled++
	if f.scanErr != nil {
		return false, f.scanErr
	}
	for _, c := range f.comments[clarityKey(repo, number)] {
		if strings.Contains(c, needle) {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeClarityForge) CreateIssueComment(ctx context.Context, repo string, number int, body string) error {
	k := clarityKey(repo, number)
	f.comments[k] = append(f.comments[k], body)
	return nil
}

func (f *fakeClarityForge) AddLabels(ctx context.Context, repo string, number int, labels []string) error {
	if f.labelErr != nil {
		return f.labelErr
	}
	k := clarityKey(repo, number)
	f.labels[k] = append(f.labels[k], labels...)
	f.applied[k] = append(f.applied[k], labels...)
	return nil
}

func (f *fakeClarityForge) IssueLabelEverApplied(ctx context.Context, repo string, number int, label string) (bool, error) {
	if f.historyErr != nil {
		return false, f.historyErr
	}
	return slices.Contains(f.applied[clarityKey(repo, number)], label), nil
}

func clarityGateScheduler(t *testing.T, enabled bool, forge TriageCommenter) *Scheduler {
	t.Helper()
	cfg := &config.Config{Runs: config.RunsConfig{Triage: config.TriageConfig{ClarityGate: config.ClarityGateConfig{Enabled: enabled}}}}
	s := New(cfg, slog.Default())
	s.SetRunTriageDeps(nil, forge)
	return s
}

const clarityGateClearBody = "## Goal\n" +
	"The `hive status` command should print the current queue depth so an operator can see the backlog without opening the dashboard.\n\n" +
	"## Acceptance criteria\n" +
	"- [ ] `hive status` prints a `queue: N` line, N being the actionable issue count\n" +
	"- [ ] the line is covered by a unit test next to `cmd/hive/status.go`\n"

func kickedRefs(s *Scheduler, issues []github.Issue) []string {
	msgs := s.BuildKickMessages(actionableWithIssues(issues), []string{"scanner"})
	if len(msgs) == 0 {
		return nil
	}
	return msgs[0].IssueRefs
}

func TestClarityGateDisabledIsNoOp(t *testing.T) {
	forge := newFakeClarityForge()
	s := clarityGateScheduler(t, false, forge)
	refs := kickedRefs(s, []github.Issue{{Repo: "o/r", Number: 1, Title: "vague", Body: "make it better", AuthorIsHuman: true}})
	if !slices.Contains(refs, "o/r#1") {
		t.Fatalf("disabled gate withheld the issue: refs=%v", refs)
	}
	if len(forge.comments) != 0 || len(forge.labels) != 0 || forge.scanCalled != 0 {
		t.Fatalf("disabled gate touched GitHub: comments=%v labels=%v scans=%d", forge.comments, forge.labels, forge.scanCalled)
	}
}

func TestClarityGateParksVagueIssueAndAsksOnce(t *testing.T) {
	forge := newFakeClarityForge()
	s := clarityGateScheduler(t, true, forge)
	issues := []github.Issue{
		{Repo: "o/r", Number: 1, Title: "vague", Body: "make it better", AuthorIsHuman: true},
		{Repo: "o/r", Number: 2, Title: "clear", Body: clarityGateClearBody, AuthorIsHuman: true},
	}
	for range 3 {
		refs := kickedRefs(s, issues)
		if slices.Contains(refs, "o/r#1") || !slices.Contains(refs, "o/r#2") {
			t.Fatalf("refs = %v, want only the clear issue", refs)
		}
	}
	got := forge.comments[clarityKey("o/r", 1)]
	if len(got) != 1 {
		t.Fatalf("clarity comment posted %d times, want once: %q", len(got), got)
	}
	if !strings.Contains(got[0], ClarityGateMarker) || !strings.Contains(got[0], "Acceptance criteria") || !strings.Contains(got[0], "`needs-spec`") {
		t.Fatalf("comment = %q", got[0])
	}
	if labels := forge.labels[clarityKey("o/r", 1)]; !slices.Equal(labels, []string{config.DefaultNeedsSpecLabel}) {
		t.Fatalf("park labels = %v, want [%s]", labels, config.DefaultNeedsSpecLabel)
	}
	if len(forge.comments[clarityKey("o/r", 2)]) != 0 || len(forge.labels[clarityKey("o/r", 2)]) != 0 {
		t.Fatal("clear issue was asked or parked")
	}
}

func TestClarityGateSkipsIssuesItDoesNotJudge(t *testing.T) {
	forge := newFakeClarityForge()
	s := clarityGateScheduler(t, true, forge)
	refs := kickedRefs(s, []github.Issue{
		{Repo: "o/r", Number: 1, Title: "hive-filed", Body: "short"},
		{Repo: "o/r", Number: 2, Title: "approved", Body: "short", AuthorIsHuman: true, Labels: []string{github.HumanAckLabel}},
	})
	if !slices.Contains(refs, "o/r#1") || !slices.Contains(refs, "o/r#2") {
		t.Fatalf("refs = %v, want both", refs)
	}
	if len(forge.comments) != 0 || len(forge.labels) != 0 {
		t.Fatalf("gate acted on ungated issues: %v %v", forge.comments, forge.labels)
	}
}

// An issue that already carries the marker and is back in the queue was
// un-parked after the author answered: it runs, and nobody is asked again.
func TestClarityGateResumesAnsweredIssue(t *testing.T) {
	forge := newFakeClarityForge()
	forge.comments[clarityKey("o/r", 1)] = []string{ClarityGateMarker + "\nquestions"}
	forge.applied[clarityKey("o/r", 1)] = []string{config.DefaultNeedsSpecLabel}
	s := clarityGateScheduler(t, true, forge)
	refs := kickedRefs(s, []github.Issue{{Repo: "o/r", Number: 1, Title: "vague", Body: "make it better", AuthorIsHuman: true}})
	if !slices.Contains(refs, "o/r#1") {
		t.Fatalf("answered issue withheld: refs=%v", refs)
	}
	if len(forge.comments[clarityKey("o/r", 1)]) != 1 || len(forge.labels) != 0 {
		t.Fatalf("answered issue asked or parked again: %v %v", forge.comments, forge.labels)
	}
}

func TestClarityGateRetriesFailedParkLabel(t *testing.T) {
	forge := newFakeClarityForge()
	forge.labelErr = errors.New("boom")
	s := clarityGateScheduler(t, true, forge)
	issues := []github.Issue{{Repo: "o/r", Number: 1, Title: "vague", Body: "make it better", AuthorIsHuman: true}}
	if refs := kickedRefs(s, issues); slices.Contains(refs, "o/r#1") {
		t.Fatalf("issue kicked although its park label failed: %v", refs)
	}
	forge.labelErr = nil
	if refs := kickedRefs(s, issues); slices.Contains(refs, "o/r#1") {
		t.Fatalf("issue kicked during label retry: %v", refs)
	}
	if len(forge.comments[clarityKey("o/r", 1)]) != 1 || !slices.Equal(forge.labels[clarityKey("o/r", 1)], []string{config.DefaultNeedsSpecLabel}) {
		t.Fatalf("comments=%v labels=%v", forge.comments, forge.labels)
	}
}

// The question was posted but the park label write failed, and the hive
// restarted before the in-process retry could land it: a fresh scheduler
// finds the marker, sees from the forge that the label never landed, and
// parks the issue instead of handing it to an agent.
func TestClarityGateRetriesFailedParkLabelAcrossRestart(t *testing.T) {
	forge := newFakeClarityForge()
	forge.comments[clarityKey("o/r", 1)] = []string{ClarityGateMarker + "\nquestions"}
	s := clarityGateScheduler(t, true, forge)
	issues := []github.Issue{{Repo: "o/r", Number: 1, Title: "vague", Body: "make it better", AuthorIsHuman: true}}
	if refs := kickedRefs(s, issues); slices.Contains(refs, "o/r#1") {
		t.Fatalf("issue kicked although its park label never landed: %v", refs)
	}
	if len(forge.comments[clarityKey("o/r", 1)]) != 1 || !slices.Equal(forge.labels[clarityKey("o/r", 1)], []string{config.DefaultNeedsSpecLabel}) {
		t.Fatalf("comments=%v labels=%v", forge.comments, forge.labels)
	}
}

// A park history that cannot be read withholds the issue for this cycle,
// without asking again or guessing at the label.
func TestClarityGateWithholdsWhenParkHistoryUnreadable(t *testing.T) {
	forge := newFakeClarityForge()
	forge.comments[clarityKey("o/r", 1)] = []string{ClarityGateMarker + "\nquestions"}
	forge.historyErr = errors.New("timeline down")
	s := clarityGateScheduler(t, true, forge)
	issues := []github.Issue{{Repo: "o/r", Number: 1, Title: "vague", Body: "make it better", AuthorIsHuman: true}}
	if refs := kickedRefs(s, issues); slices.Contains(refs, "o/r#1") {
		t.Fatalf("issue kicked with park history unreadable: %v", refs)
	}
	if len(forge.comments[clarityKey("o/r", 1)]) != 1 || len(forge.labels) != 0 {
		t.Fatalf("comments=%v labels=%v", forge.comments, forge.labels)
	}
}

// A labeler that keeps no park history reads the marker as un-parked, as
// the gate did before the history check existed.
func TestClarityGateWithoutParkHistoryTrustsMarker(t *testing.T) {
	forge := newFakeClarityForge()
	forge.comments[clarityKey("o/r", 1)] = []string{ClarityGateMarker + "\nquestions"}
	s := clarityGateScheduler(t, true, historylessClarityForge{forge: forge})
	refs := kickedRefs(s, []github.Issue{{Repo: "o/r", Number: 1, Title: "vague", Body: "make it better", AuthorIsHuman: true}})
	if !slices.Contains(refs, "o/r#1") || len(forge.labels) != 0 {
		t.Fatalf("refs=%v labels=%v", refs, forge.labels)
	}
}

// historylessClarityForge hides IssueLabelEverApplied; a named field, not
// an embedding, so the method is not promoted.
type historylessClarityForge struct{ forge *fakeClarityForge }

func (h historylessClarityForge) IssueCommentsContain(ctx context.Context, repo string, number int, needle string) (bool, error) {
	return h.forge.IssueCommentsContain(ctx, repo, number, needle)
}

func (h historylessClarityForge) CreateIssueComment(ctx context.Context, repo string, number int, body string) error {
	return h.forge.CreateIssueComment(ctx, repo, number, body)
}

func (h historylessClarityForge) AddLabels(ctx context.Context, repo string, number int, labels []string) error {
	return h.forge.AddLabels(ctx, repo, number, labels)
}

func TestClarityGateUsesConfiguredNeedsSpecLabel(t *testing.T) {
	forge := newFakeClarityForge()
	s := clarityGateScheduler(t, true, forge)
	s.cfg.Project.IssueFilter.HardSuppressLabels.NeedsSpec = []string{"needs-info"}
	s.cfg.Runs.Triage.ClarityGate.Threshold = 100
	kickedRefs(s, []github.Issue{{Repo: "o/r", Number: 1, Title: "almost", Body: "The dashboard should load faster on the `/runs` page.", AuthorIsHuman: true}})
	if labels := forge.labels[clarityKey("o/r", 1)]; !slices.Equal(labels, []string{"needs-info"}) {
		t.Fatalf("park labels = %v, want [needs-info]", labels)
	}
}

func TestClarityGateEffectiveThreshold(t *testing.T) {
	for in, want := range map[int]int{0: config.DefaultClarityGateThreshold, -5: config.DefaultClarityGateThreshold, 40: 40, 100: 100, 250: 100} {
		if got := (config.ClarityGateConfig{Threshold: in}).EffectiveThreshold(); got != want {
			t.Errorf("EffectiveThreshold(%d) = %d, want %d", in, got, want)
		}
	}
}
