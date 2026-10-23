package scheduler

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/priorart"
)

// fakePriorArtSearcher answers a fixed confidence per issue number.
type fakePriorArtSearcher struct {
	byNumber map[int]int
	calls    int
}

func (f *fakePriorArtSearcher) Find(ctx context.Context, issue priorart.Issue) priorart.Report {
	f.calls++
	conf := f.byNumber[issue.Number]
	if conf == 0 {
		return priorart.Report{}
	}
	return priorart.Report{
		Confidence: conf,
		Findings: []priorart.Finding{{
			Candidate:  priorart.Candidate{Kind: priorart.KindMergedPR, Title: "feat: add queue depth to status", URL: "https://example.test/pr/9", Ref: "#9"},
			Confidence: conf,
			Matched:    []string{"queue", "status"},
			Quote:      "hive status prints queue: N",
		}},
	}
}

type priorArtAdvisory struct {
	title, detail, agent string
}

func priorArtScheduler(t *testing.T, forge TriageCommenter, searcher PriorArtSearcher, mutate func(*config.Config)) (*Scheduler, *[]priorArtAdvisory) {
	t.Helper()
	cfg := &config.Config{}
	if mutate != nil {
		mutate(cfg)
	}
	s := New(cfg, slog.Default())
	s.SetRunTriageDeps(nil, forge)
	s.SetPriorArtSearcher(searcher)
	var notes []priorArtAdvisory
	s.SetAdvisoryFunc(func(title, detail, agent string) {
		notes = append(notes, priorArtAdvisory{title, detail, agent})
	})
	s.priorArt.now = func() time.Time { return time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC) }
	return s, &notes
}

func humanFeature(n int) github.Issue {
	return github.Issue{Repo: "o/r", Number: n, Title: "Show queue depth in hive status", Body: "status should print the queue", AuthorIsHuman: true, Author: "alice", Labels: []string{"kind/feature"}, URL: "https://example.test/issues/1"}
}

func TestPriorArtGateParksHumanFeatureOnceAndRecordsDiscoverability(t *testing.T) {
	forge := newFakeClarityForge()
	searcher := &fakePriorArtSearcher{byNumber: map[int]int{1: 80, 2: 30}}
	s, notes := priorArtScheduler(t, forge, searcher, nil)
	issues := []github.Issue{humanFeature(1), humanFeature(2)}
	for range 3 {
		refs := kickedRefs(s, issues)
		if slices.Contains(refs, "o/r#1") || !slices.Contains(refs, "o/r#2") {
			t.Fatalf("refs = %v, want only the unmatched issue", refs)
		}
	}
	got := forge.comments[clarityKey("o/r", 1)]
	if len(got) != 1 {
		t.Fatalf("park comment posted %d times, want once: %q", len(got), got)
	}
	for _, want := range []string{PriorArtGateMarker, "Possibly exists", "merged PR", "80% match", "/hive decision proceed", "/hive decision exists", "hive status prints queue"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("comment lacks %q:\n%s", want, got[0])
		}
	}
	if strings.Contains(got[0], "automated author") {
		t.Error("human park must not carry the clanker auto-close notice")
	}
	if labels := forge.labels[clarityKey("o/r", 1)]; !slices.Equal(labels, []string{github.PossiblyExistsStageLabel}) {
		t.Fatalf("park labels = %v", labels)
	}
	if len(forge.comments[clarityKey("o/r", 2)]) != 0 || len(forge.labels[clarityKey("o/r", 2)]) != 0 {
		t.Fatal("unmatched issue was touched")
	}
	if len(*notes) != 1 || !strings.HasPrefix((*notes)[0].title, "discoverability: ") || !strings.Contains((*notes)[0].detail, "example.test/pr/9") {
		t.Fatalf("discoverability notes = %+v", *notes)
	}
}

func TestPriorArtGateDisabledOrUnwiredIsNoOp(t *testing.T) {
	forge := newFakeClarityForge()
	searcher := &fakePriorArtSearcher{byNumber: map[int]int{1: 95}}
	off := false
	s, _ := priorArtScheduler(t, forge, searcher, func(c *config.Config) { c.Runs.Triage.PriorArt.Enabled = &off })
	if refs := kickedRefs(s, []github.Issue{humanFeature(1)}); !slices.Contains(refs, "o/r#1") {
		t.Fatalf("disabled gate withheld the issue: %v", refs)
	}
	s2, _ := priorArtScheduler(t, forge, nil, nil)
	if refs := kickedRefs(s2, []github.Issue{humanFeature(1)}); !slices.Contains(refs, "o/r#1") {
		t.Fatalf("gate without a searcher withheld the issue: %v", refs)
	}
	if searcher.calls != 0 || len(forge.comments) != 0 {
		t.Fatalf("no-op gate searched %d times, comments %v", searcher.calls, forge.comments)
	}
}

func TestPriorArtGateSkipsBugsClaimedAndAcknowledged(t *testing.T) {
	forge := newFakeClarityForge()
	searcher := &fakePriorArtSearcher{byNumber: map[int]int{1: 95, 2: 95, 3: 95, 4: 95}}
	s, _ := priorArtScheduler(t, forge, searcher, nil)
	bug := humanFeature(1)
	bug.Labels = []string{"bug"}
	claimed := humanFeature(2)
	claimed.ClaimedBy = "coder"
	acked := humanFeature(3)
	acked.Labels = append(acked.Labels, github.HumanAckLabel)
	plain := humanFeature(4)
	plain.Labels = nil
	refs := kickedRefs(s, []github.Issue{bug, claimed, acked, plain})
	for _, want := range []string{"o/r#1", "o/r#2", "o/r#3", "o/r#4"} {
		if !slices.Contains(refs, want) {
			t.Errorf("%s withheld; refs=%v", want, refs)
		}
	}
	if searcher.calls != 0 {
		t.Fatalf("searched %d ungated issues", searcher.calls)
	}
}

func TestPriorArtGateRFCTitleTagIsGated(t *testing.T) {
	forge := newFakeClarityForge()
	searcher := &fakePriorArtSearcher{byNumber: map[int]int{1: 90}}
	s, _ := priorArtScheduler(t, forge, searcher, nil)
	rfc := humanFeature(1)
	rfc.Labels = nil
	rfc.Title = "[RFC] queue depth in hive status"
	if refs := kickedRefs(s, []github.Issue{rfc}); slices.Contains(refs, "o/r#1") {
		t.Fatalf("RFC-tagged issue not gated: %v", refs)
	}
}

func TestPriorArtGateHiveAuthoredAttachesFindingsWithoutParking(t *testing.T) {
	forge := newFakeClarityForge()
	searcher := &fakePriorArtSearcher{byNumber: map[int]int{1: 90}}
	s, notes := priorArtScheduler(t, forge, searcher, nil)
	hive := humanFeature(1)
	hive.AuthorIsHuman = false
	hive.Author = "hive-app[bot]"
	hive.Labels = []string{"kind/feature", "agent/scanner"}
	for range 2 {
		if refs := kickedRefs(s, []github.Issue{hive}); !slices.Contains(refs, "o/r#1") {
			t.Fatalf("hive-authored issue was withheld: %v", refs)
		}
	}
	got := forge.comments[clarityKey("o/r", 1)]
	if len(got) != 1 || !strings.Contains(got[0], priorart.FindingsMarker) || !strings.Contains(got[0], "Gap vs existing") {
		t.Fatalf("findings note = %q", got)
	}
	if len(forge.labels[clarityKey("o/r", 1)]) != 0 {
		t.Fatalf("hive-authored issue was parked: %v", forge.labels)
	}
	if len(*notes) != 0 {
		t.Fatalf("hive-authored issue recorded a discoverability note: %+v", *notes)
	}
}

func TestPriorArtGateClankerParkMentionsAutoClose(t *testing.T) {
	forge := newFakeClarityForge()
	searcher := &fakePriorArtSearcher{byNumber: map[int]int{1: 90}}
	s, notes := priorArtScheduler(t, forge, searcher, nil)
	bot := humanFeature(1)
	bot.AuthorIsHuman = false
	bot.Author = "other-hive[bot]"
	bot.Labels = []string{"kind/feature", "author/clanker"}
	if refs := kickedRefs(s, []github.Issue{bot}); slices.Contains(refs, "o/r#1") {
		t.Fatalf("clanker issue not parked: %v", refs)
	}
	got := forge.comments[clarityKey("o/r", 1)]
	if len(got) != 1 || !strings.Contains(got[0], "closed as `already-exists`") || !strings.Contains(got[0], "3d") {
		t.Fatalf("clanker park comment = %q", got)
	}
	if labels := forge.labels[clarityKey("o/r", 1)]; !slices.Equal(labels, []string{github.PossiblyExistsStageLabel}) {
		t.Fatalf("park labels = %v", labels)
	}
	if len(*notes) != 0 {
		t.Fatalf("clanker park recorded a discoverability note: %+v", *notes)
	}
}

func TestPriorArtGateResumesAskedIssue(t *testing.T) {
	forge := newFakeClarityForge()
	k := clarityKey("o/r", 1)
	forge.comments[k] = []string{PriorArtGateMarker + "\nasked"}
	forge.applied[k] = []string{github.PossiblyExistsStageLabel}
	searcher := &fakePriorArtSearcher{byNumber: map[int]int{1: 90}}
	s, _ := priorArtScheduler(t, forge, searcher, nil)
	if refs := kickedRefs(s, []github.Issue{humanFeature(1)}); !slices.Contains(refs, "o/r#1") {
		t.Fatalf("un-parked issue withheld: %v", refs)
	}
	if len(forge.comments[k]) != 1 || len(forge.labels[k]) != 0 {
		t.Fatalf("asked issue was asked or parked again: %v %v", forge.comments[k], forge.labels[k])
	}
}

func TestPriorArtGateRetriesParkLabelAcrossRestart(t *testing.T) {
	forge := newFakeClarityForge()
	k := clarityKey("o/r", 1)
	forge.comments[k] = []string{PriorArtGateMarker + "\nasked"}
	searcher := &fakePriorArtSearcher{byNumber: map[int]int{1: 90}}
	s, _ := priorArtScheduler(t, forge, searcher, nil)
	if refs := kickedRefs(s, []github.Issue{humanFeature(1)}); slices.Contains(refs, "o/r#1") {
		t.Fatalf("issue with an unlanded park ran: %v", refs)
	}
	if labels := forge.labels[k]; !slices.Equal(labels, []string{github.PossiblyExistsStageLabel}) {
		t.Fatalf("park label not retried: %v", labels)
	}
	if len(forge.comments[k]) != 1 {
		t.Fatalf("asked twice: %v", forge.comments[k])
	}
}

func TestPriorArtGateHonoursParkBudget(t *testing.T) {
	forge := newFakeClarityForge()
	searcher := &fakePriorArtSearcher{byNumber: map[int]int{1: 90, 2: 90, 3: 90}}
	s, _ := priorArtScheduler(t, forge, searcher, func(c *config.Config) { c.Runs.Triage.PriorArt.MaxParksPerCycle = 1 })
	refs := kickedRefs(s, []github.Issue{humanFeature(1), humanFeature(2), humanFeature(3)})
	if len(refs) != 0 {
		t.Fatalf("matched issues ran: %v", refs)
	}
	if len(forge.comments) != 1 {
		t.Fatalf("parked %d issues in one cycle, budget 1: %v", len(forge.comments), forge.comments)
	}
}

func TestPriorArtGateAlreadyParkedIsWithheld(t *testing.T) {
	forge := newFakeClarityForge()
	searcher := &fakePriorArtSearcher{byNumber: map[int]int{1: 90}}
	s, _ := priorArtScheduler(t, forge, searcher, nil)
	parked := humanFeature(1)
	parked.Labels = append(parked.Labels, github.PossiblyExistsStageLabel)
	if refs := kickedRefs(s, []github.Issue{parked}); slices.Contains(refs, "o/r#1") {
		t.Fatalf("parked issue ran: %v", refs)
	}
	if searcher.calls != 0 || len(forge.comments) != 0 {
		t.Fatal("parked issue was searched or asked again")
	}
}

func TestPriorArtGateConfigDefaults(t *testing.T) {
	var g config.PriorArtGateConfig
	if !g.IsEnabled() || g.EffectiveThreshold() != config.DefaultPriorArtGateThreshold || g.EffectiveMaxFindings() != config.DefaultPriorArtGateMaxFindings || g.EffectiveBotCloseTTL() != 72*time.Hour {
		t.Fatalf("defaults: %v %d %d %v", g.IsEnabled(), g.EffectiveThreshold(), g.EffectiveMaxFindings(), g.EffectiveBotCloseTTL())
	}
	g.Threshold, g.BotCloseTTL = 150, -1
	if g.EffectiveThreshold() != 100 || g.EffectiveBotCloseTTL() != 0 {
		t.Fatalf("caps: %d %v", g.EffectiveThreshold(), g.EffectiveBotCloseTTL())
	}
	if kinds := g.EffectiveKinds(); !slices.Equal(kinds, config.DefaultPriorArtGateKinds) {
		t.Fatalf("kinds = %v", kinds)
	}
}
