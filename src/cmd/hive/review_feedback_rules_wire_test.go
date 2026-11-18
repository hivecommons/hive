package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/reviewfeedback"
	"github.com/hivecommons/hive/pkg/reviewrules"
)

// The maintainer's comment on the techdocs analysis PR (hivecommons/hive#19046).
const maintainersFixture = ".project/maintainers.yaml is the source of truth for ownership and maintainership; capture this in the assessment knowledge base"

const maintainersRuleText = ".project/maintainers.yaml is the source of truth for ownership and maintainership"

const fixtureCommentURL = "https://github.com/cncf/techdocs/pull/424#discussion_r1001"

type ruleHarness struct {
	rules *reviewrules.Store
	vault string
	src   *fakeFeedbackSource
	kick  *recordingKicker
	now   time.Time
}

func newRuleHarness(t *testing.T, cfg reviewrules.Config) *ruleHarness {
	t.Helper()
	vault := filepath.Join(t.TempDir(), "review-rules")
	rs, err := reviewrules.Open(t.TempDir(), vault, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &ruleHarness{rules: rs, vault: vault, src: &fakeFeedbackSource{byKey: map[string]github.PRFeedback{}}, kick: &recordingKicker{}, now: time.Now()}
}

func (h *ruleHarness) sweep(t *testing.T) reviewFeedbackSweep {
	t.Helper()
	s := feedbackSweepFor(t, h.src, h.kick, routeTo("quality"), h.now)
	s.Rules = h.rules
	s.Lane = func(owner string) string { return owner }
	s.Managed = func(repo string) bool { return repo == "hivecommons/hive" }
	return s
}

func (h *ruleHarness) feedback(items ...github.FeedbackItem) {
	fb := openDraftFeedback()
	fb.Items = items
	fb.RuleCandidates = items
	h.src.byKey[feedbackTestKey] = fb
}

func ruleItem(id, assoc, body string) github.FeedbackItem {
	return github.FeedbackItem{ID: id, ThreadID: "T-" + id, Kind: github.PRCommentInline, Author: "nate-double-u", Body: body, Path: "analysis.md", Line: 3,
		URL: "https://github.com/cncf/techdocs/pull/424#discussion_r" + strings.TrimPrefix(id, "inline:"), CreatedAt: time.Now().Add(-time.Hour), AuthorAssociation: assoc}
}

func (h *ruleHarness) retrievedFor(t *testing.T, lane string) string {
	t.Helper()
	logger := discardLogger()
	vs, err := knowledge.NewFileStore(h.vault, reviewrules.VaultName, logger)
	if err != nil {
		t.Fatal(err)
	}
	p := knowledge.NewPrimer(nil, knowledge.PrimerConfig{MaxFacts: 25}, logger)
	p.AddPinnedFileStore(reviewrules.VaultName, vs, knowledge.LayerProject)
	out, _ := p.PrimeForKick(context.Background(), nil, knowledge.KickContext{Lane: lane}, nil).Render()
	return out
}

// The concrete case: the maintainer states that the maintainers file is the
// source of truth. The sweep records it with provenance, tells the reviewer on
// the PR, hands the agent the recorded rule, and the quality agent's next run
// receives it.
func TestReviewFeedbackSweep_MaintainersFileRuleIsCapturedAndRetrievableByQuality(t *testing.T) {
	h := newRuleHarness(t, reviewrules.Config{})
	h.feedback(ruleItem("inline:1001", "MEMBER", maintainersFixture))
	res := h.sweep(t).run(context.Background(), parseReviewFeedbackKey)
	if res.Kicked != 1 || res.RulesCaptured != 1 {
		t.Fatalf("res = %+v, want one kick and one captured rule", res)
	}
	rules := h.rules.List()
	if len(rules) != 1 {
		t.Fatalf("rules = %+v", rules)
	}
	r := rules[0]
	p := r.Provenance[0]
	if r.ID != "R-1" || r.Text != maintainersRuleText || r.Status != reviewrules.StatusActive || r.AppliesTo != "quality" ||
		p.CommentURL != fixtureCommentURL || p.Reviewer != "nate-double-u" || p.Association != "MEMBER" || p.PRURL != "https://github.com/cncf/techdocs/pull/424" || p.At.IsZero() {
		t.Fatalf("rule = %+v provenance = %+v", r, p)
	}

	// The reviewer sees it was captured, where it applies, and can object.
	if len(h.src.comments) != 1 {
		t.Fatalf("comments = %q, want one acknowledgement", h.src.comments)
	}
	ack := h.src.comments[0]
	for _, want := range []string{"Recorded as rule R-1 in the hive's knowledge, applies to the `quality` agent's future work", maintainersRuleText, fixtureCommentURL, "hivectl knowledge state"} {
		if !strings.Contains(ack, want) {
			t.Errorf("acknowledgement missing %q:\n%s", want, ack)
		}
	}
	if strings.Contains(ack, "capture this") {
		t.Errorf("the capture request is not part of the rule:\n%s", ack)
	}

	// The agent is told it is recorded and not to record it again.
	brief := h.kick.kicks["quality"][0]
	for _, want := range []string{"R-1 (recorded; applies to the `quality` agent's future work)", "do NOT run hivectl knowledge create", "hive:review-rules", fixtureCommentURL} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief missing %q", want)
		}
	}

	// Retrievable for the quality agent's next run, with its provenance; not for another agent.
	got := h.retrievedFor(t, "quality")
	for _, want := range []string{maintainersRuleText, "R-1", fixtureCommentURL, "nate-double-u"} {
		if !strings.Contains(got, want) {
			t.Errorf("quality knowledge missing %q:\n%s", want, got)
		}
	}
	if other := h.retrievedFor(t, "developer"); strings.Contains(other, maintainersRuleText) {
		t.Errorf("a quality-scoped rule reached the developer agent:\n%s", other)
	}
}

// Re-delivery of the same comment (a fresh sweep state, a retried kick) records
// nothing twice and posts no second acknowledgement.
func TestReviewFeedbackSweep_RuleCaptureIsIdempotent(t *testing.T) {
	h := newRuleHarness(t, reviewrules.Config{})
	h.feedback(ruleItem("inline:1001", "OWNER", maintainersFixture))
	h.sweep(t).run(context.Background(), parseReviewFeedbackKey)
	res := h.sweep(t).run(context.Background(), parseReviewFeedbackKey) // new reviewfeedback state: the comment is routed again
	if res.Kicked != 1 || res.RulesCaptured != 0 || len(h.rules.List()) != 1 || len(h.src.comments) != 1 {
		t.Fatalf("res = %+v rules = %d comments = %d, want no second capture or acknowledgement", res, len(h.rules.List()), len(h.src.comments))
	}
	if !strings.Contains(h.kick.kicks["quality"][1], "R-1 (recorded") {
		t.Fatal("a retried kick still names the recorded rule")
	}
}

func TestReviewFeedbackSweep_UntrustedReviewerRuleIsQueuedNotApplied(t *testing.T) {
	h := newRuleHarness(t, reviewrules.Config{})
	h.feedback(ruleItem("inline:1001", "CONTRIBUTOR", maintainersFixture))
	res := h.sweep(t).run(context.Background(), parseReviewFeedbackKey)
	if res.Kicked != 1 || res.RulesQueued != 1 || res.RulesCaptured != 0 {
		t.Fatalf("res = %+v", res)
	}
	if len(h.src.comments) != 0 {
		t.Fatalf("a queued rule is not announced as recorded: %q", h.src.comments)
	}
	if !strings.Contains(h.kick.kicks["quality"][0], "queued for a person to confirm") {
		t.Fatal("the brief must say the rule is queued")
	}
	if got := h.retrievedFor(t, "quality"); strings.Contains(got, maintainersRuleText) {
		t.Fatalf("a queued rule must not reach an agent:\n%s", got)
	}
}

func TestReviewFeedbackSweep_CorrectionsAreNotRules(t *testing.T) {
	h := newRuleHarness(t, reviewrules.Config{})
	h.feedback(ruleItem("inline:1", "OWNER", "The Agent readiness score is too high."))
	res := h.sweep(t).run(context.Background(), parseReviewFeedbackKey)
	if res.Kicked != 1 || res.RulesCaptured+res.RulesQueued+res.RulesMerged != 0 || len(h.rules.List()) != 0 || len(h.src.comments) != 0 {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(h.kick.kicks["quality"][0], "hivectl knowledge create") {
		t.Fatal("with nothing captured the brief keeps the best-effort instruction")
	}
}

func TestReviewFeedbackSweep_NearDuplicateRuleMerges(t *testing.T) {
	h := newRuleHarness(t, reviewrules.Config{})
	h.feedback(ruleItem("inline:1001", "OWNER", maintainersFixture))
	h.sweep(t).run(context.Background(), parseReviewFeedbackKey)
	h.feedback(ruleItem("inline:1002", "OWNER", "Again: the .project/maintainers.yaml is the source of truth for ownership and maintainership."))
	h.now = h.now.Add(time.Hour)
	res := h.sweep(t).run(context.Background(), parseReviewFeedbackKey)
	if res.RulesMerged != 1 || len(h.rules.List()) != 1 || len(h.rules.List()[0].Provenance) != 2 {
		t.Fatalf("res = %+v rules = %+v", res, h.rules.List())
	}
}

// A rule is captured even when the burst cannot be kicked: the cap was hit, or
// no agent can take it.
func TestReviewFeedbackSweep_RuleIsCapturedWhenTheKickCannotGo(t *testing.T) {
	t.Run("cap reached", func(t *testing.T) {
		h := newRuleHarness(t, reviewrules.Config{})
		h.feedback(ruleItem("inline:1001", "OWNER", maintainersFixture))
		s := h.sweep(t)
		for i := 0; i < wireTestLimits.WindowCap; i++ {
			if err := s.Store.RecordKick(feedbackTestKey, nil, h.now.Add(-time.Duration(i+1)*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		res := s.run(context.Background(), parseReviewFeedbackKey)
		if res.CapReached != 1 || res.RulesCaptured != 1 || len(h.kick.kicks) != 0 {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("no agent", func(t *testing.T) {
		h := newRuleHarness(t, reviewrules.Config{})
		h.feedback(ruleItem("inline:1001", "OWNER", maintainersFixture))
		s := h.sweep(t)
		s.Route = func(string) reviewFeedbackRoute { return reviewFeedbackRoute{Reason: "no agent"} }
		res := s.run(context.Background(), parseReviewFeedbackKey)
		if res.NeedsHuman != 1 || res.RulesCaptured != 1 || len(h.rules.List()) != 1 {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("still debouncing", func(t *testing.T) {
		h := newRuleHarness(t, reviewrules.Config{})
		it := ruleItem("inline:1001", "OWNER", maintainersFixture)
		it.CreatedAt = h.now
		h.feedback(it)
		res := h.sweep(t).run(context.Background(), parseReviewFeedbackKey)
		if res.Debounced != 1 || len(h.rules.List()) != 0 {
			t.Fatalf("a burst still arriving is not captured yet: %+v", res)
		}
	})
}

// A rule whose home is the reviewed project's own files, in a repo the hive does
// not manage: one needs-human note carrying the exact text, once.
func TestReviewFeedbackSweep_ForeignRepoRuleGetsOneProposalNote(t *testing.T) {
	h := newRuleHarness(t, reviewrules.Config{})
	h.feedback(ruleItem("inline:1001", "OWNER", "Going forward, add the criteria to your docs: the maintainers file is the source of truth for ownership."))
	h.sweep(t).run(context.Background(), parseReviewFeedbackKey)
	h.sweep(t).run(context.Background(), parseReviewFeedbackKey)
	h.now = h.now.Add(time.Hour)
	h.sweep(t).run(context.Background(), parseReviewFeedbackKey)
	var notes int
	for _, c := range h.src.comments {
		if strings.Contains(c, reviewrules.HomeMarkerPrefix) {
			notes++
			for _, want := range []string{"Needs a human", "`cncf/techdocs`", "the maintainers file is the source of truth for ownership", fixtureCommentURL, "does not write to that repo"} {
				if !strings.Contains(c, want) {
					t.Errorf("proposal note missing %q:\n%s", want, c)
				}
			}
		}
	}
	if notes != 1 || len(h.src.comments) != 2 {
		t.Fatalf("comments = %d (proposal notes %d), want one acknowledgement and one proposal: %q", len(h.src.comments), notes, h.src.comments)
	}
}

func TestReviewFeedbackSweep_ManagedRepoRuleNeedsNoProposal(t *testing.T) {
	h := newRuleHarness(t, reviewrules.Config{})
	s := h.sweep(t)
	s.Managed = func(string) bool { return true }
	h.feedback(ruleItem("inline:1001", "OWNER", "Going forward, add the criteria to your docs: always sign commits."))
	s.run(context.Background(), parseReviewFeedbackKey)
	for _, c := range h.src.comments {
		if strings.Contains(c, reviewrules.HomeMarkerPrefix) {
			t.Fatalf("a managed repo's rule is the hive's own to keep: %s", c)
		}
	}
}

type flakyCommentSource struct {
	*fakeFeedbackSource
	fail bool
}

func (f *flakyCommentSource) CreateIssueComment(ctx context.Context, repo string, n int, body string) error {
	if f.fail {
		return errors.New("secondary rate limit")
	}
	return f.fakeFeedbackSource.CreateIssueComment(ctx, repo, n, body)
}

// A notice that could not be posted is retried on a later pass, even when the
// PR has no new feedback, and is posted once.
func TestReviewFeedbackSweep_FailedNoticesAreRetried(t *testing.T) {
	h := newRuleHarness(t, reviewrules.Config{})
	flaky := &flakyCommentSource{fakeFeedbackSource: h.src, fail: true}
	h.feedback(ruleItem("inline:1001", "OWNER", "Going forward, add the criteria to your docs: the maintainers file is the source of truth."))
	s := h.sweep(t)
	s.Source = flaky
	s.run(context.Background(), parseReviewFeedbackKey)
	if len(h.src.comments) != 0 {
		t.Fatalf("comments = %q while posting fails", h.src.comments)
	}
	flaky.fail = false
	h.feedback() // nothing new from people
	s.run(context.Background(), parseReviewFeedbackKey)
	s.run(context.Background(), parseReviewFeedbackKey)
	if len(h.src.comments) != 2 {
		t.Fatalf("comments = %q, want the acknowledgement and the proposal once each", h.src.comments)
	}
}

// The agent's structured block sharpens a rule: it is read from the agent's own
// reply even when no human comment is waiting, applied once, and must cite a
// comment the hive saw.
func TestReviewFeedbackSweep_AgentRuleBlockRefinesTheRule(t *testing.T) {
	h := newRuleHarness(t, reviewrules.Config{})
	h.feedback(ruleItem("inline:1001", "OWNER", maintainersFixture))
	h.sweep(t).run(context.Background(), parseReviewFeedbackKey)

	block := "Updated.\n<!-- hive:review-rules {\"rules\":[{\"text\":\"For every project assessment, take ownership and maintainership from .project/maintainers.yaml.\",\"scope\":\"workflow\",\"applies_to\":\"cncf-techdocs-assessment\",\"evidence_url\":\"" + fixtureCommentURL + "\"},{\"text\":\"x\",\"evidence_url\":\"https://github.com/cncf/techdocs/pull/424#discussion_r5\"}]} -->"
	fb := openDraftFeedback()
	fb.Items = nil
	fb.HiveReplies = []github.FeedbackMessage{{ID: "conversation:77", Author: "hive", Hive: true, Body: block}}
	h.src.byKey[feedbackTestKey] = fb
	h.now = h.now.Add(time.Hour)
	for i := 0; i < 2; i++ {
		h.sweep(t).run(context.Background(), parseReviewFeedbackKey)
	}
	r := h.rules.Get("R-1")
	if r.Scope != reviewrules.ScopeWorkflow || r.AppliesTo != "cncf-techdocs-assessment" || !strings.Contains(r.Text, "For every project assessment") {
		t.Fatalf("rule = %+v", r)
	}
	if len(h.rules.List()) != 1 {
		t.Fatal("the entry citing a comment the hive never saw must not create a rule")
	}
	// The workflow-scoped rule reaches that workflow's lane and no longer the agent lane.
	if got := h.retrievedFor(t, "cncf-techdocs-assessment"); !strings.Contains(got, "For every project assessment") {
		t.Fatalf("workflow lane knowledge:\n%s", got)
	}
}

func TestReviewFeedbackSweep_RulesOffChangesNothing(t *testing.T) {
	h := newRuleHarness(t, reviewrules.Config{})
	h.feedback(ruleItem("inline:1001", "OWNER", maintainersFixture))
	s := h.sweep(t)
	s.Rules = nil
	res := s.run(context.Background(), parseReviewFeedbackKey)
	if res.Kicked != 1 || len(h.src.comments) != 0 || !strings.Contains(h.kick.kicks["quality"][0], "hivectl knowledge create") {
		t.Fatalf("res = %+v comments = %q", res, h.src.comments)
	}
}

func TestReviewFeedbackSweep_RuleLimitRefusalIsLoggedNotFatal(t *testing.T) {
	h := newRuleHarness(t, reviewrules.Config{MaxRules: 1})
	h.feedback(ruleItem("inline:1001", "OWNER", maintainersFixture), ruleItem("inline:1002", "OWNER", "Going forward, cite the scorecard with a commit-pinned link."))
	res := h.sweep(t).run(context.Background(), parseReviewFeedbackKey)
	if res.Kicked != 1 || res.RulesCaptured != 1 || len(h.rules.List()) != 1 {
		t.Fatalf("res = %+v", res)
	}
}

func TestRecordedRulesBriefShape(t *testing.T) {
	brief := reviewfeedback.BuildBriefWithRules("o/r", 1, "b", "", []github.FeedbackItem{ruleItem("inline:1", "OWNER", "x")},
		[]reviewfeedback.RecordedRule{{ID: "R-2", Text: "t", Applies: "all agents' work", Queued: true, SourceURL: "u"}})
	if !strings.Contains(brief, "R-2 (queued for a person to confirm; applies to all agents' work): t [source u]") || strings.Contains(brief, "\u2014") {
		t.Fatalf("brief:\n%s", brief)
	}
}

// After the kick, the agent's reply must name the captured rule. A reply that
// does not raises one needs-human note; one that does raises nothing.
func TestReviewFeedbackSweep_ReplyMustNameCapturedRule(t *testing.T) {
	run := func(t *testing.T, replyBody string) (reviewFeedbackResult, []string) {
		h := newRuleHarness(t, reviewrules.Config{})
		h.feedback(ruleItem("inline:1001", "OWNER", maintainersFixture))
		s := h.sweep(t)
		s.run(context.Background(), parseReviewFeedbackKey)
		before := len(h.src.comments)
		fb := openDraftFeedback()
		fb.Items = nil
		fb.AgentReplies = []github.FeedbackMessage{{ID: "conversation:9", Hive: true, CreatedAt: h.now.Add(time.Minute),
			Body: replyBody + "\n" + github.FeedbackHandledMarker([]string{"inline:1001"})}}
		h.src.byKey[feedbackTestKey] = fb
		s.Now = h.now.Add(time.Hour)
		var res reviewFeedbackResult
		for i := 0; i < 2; i++ {
			res = s.run(context.Background(), parseReviewFeedbackKey)
		}
		return res, h.src.comments[before:]
	}
	t.Run("names it", func(t *testing.T) {
		if _, notes := run(t, "Recorded as R-1, applied here."); len(notes) != 0 {
			t.Fatalf("notes = %q, want none", notes)
		}
	})
	t.Run("does not name it", func(t *testing.T) {
		_, notes := run(t, "Fixed the file.")
		if len(notes) != 1 || !strings.Contains(notes[0], github.FeedbackNeedsHumanMarker) || !strings.Contains(notes[0], "R-1") {
			t.Fatalf("notes = %q, want exactly one needs-human note naming R-1", notes)
		}
	})
}

// The maintainers.yaml comment on cncf/techdocs#424 was already named in a
// handled marker, so routing no longer returns it; a plain conversation comment
// does not address the hive. Rule capture reads its own candidate set, so both
// are captured on the first pass (the backfill) without a kick, and a later
// pass does not read them again (hivecommons/hive#19136).
func TestReviewFeedbackSweep_RuleCaptureReadsHandledAndUnaddressedComments(t *testing.T) {
	h := newRuleHarness(t, reviewrules.Config{})
	plain := ruleItem("conversation:2002", "OWNER", "Going forward, cite the scorecard with a commit-pinned link.")
	plain.Kind, plain.ThreadID, plain.Path, plain.Line = github.PRCommentConversation, "", "", 0
	fb := openDraftFeedback()
	fb.Items = nil
	fb.RuleCandidates = []github.FeedbackItem{ruleItem("inline:1001", "MEMBER", maintainersFixture), plain}
	h.src.byKey[feedbackTestKey] = fb
	res := h.sweep(t).run(context.Background(), parseReviewFeedbackKey)
	if res.RulesCaptured != 2 || res.Kicked != 0 || len(h.rules.List()) != 2 {
		t.Fatalf("res = %+v rules = %d, want both captured and no kick", res, len(h.rules.List()))
	}
	if r := h.rules.Get("R-1"); r == nil || r.Text != maintainersRuleText {
		t.Fatalf("R-1 = %+v", r)
	}
	for _, id := range []string{"inline:1001", "conversation:2002"} {
		if !h.rules.Evaluated("cncf/techdocs", 424, id) {
			t.Fatalf("%s not recorded as evaluated", id)
		}
	}
	if len(h.src.comments) != 1 || !strings.Contains(h.src.comments[0], "R-1") || !strings.Contains(h.src.comments[0], "R-2") {
		t.Fatalf("comments = %q, want one acknowledgement naming both rules", h.src.comments)
	}

	// Later passes classify only new comments.
	fb.RuleCandidates = append(fb.RuleCandidates, ruleItem("inline:1003", "OWNER", "The score is too high."))
	h.src.byKey[feedbackTestKey] = fb
	h.now = h.now.Add(time.Hour)
	res = h.sweep(t).run(context.Background(), parseReviewFeedbackKey)
	if res.RulesCaptured+res.RulesMerged+res.RulesQueued != 0 || len(h.src.comments) != 1 || !h.rules.Evaluated("cncf/techdocs", 424, "inline:1003") {
		t.Fatalf("res = %+v comments = %d", res, len(h.src.comments))
	}
}

// A candidate still inside the debounce may be edited; it is read once it is
// quiet, and a kick of a comment captured on an earlier pass still names the rule.
func TestReviewFeedbackSweep_RuleCaptureWaitsForDebounceAndBriefNamesEarlierCapture(t *testing.T) {
	h := newRuleHarness(t, reviewrules.Config{})
	it := ruleItem("inline:1001", "OWNER", maintainersFixture)
	it.CreatedAt = h.now
	fb := openDraftFeedback()
	fb.Items = nil
	fb.RuleCandidates = []github.FeedbackItem{it}
	h.src.byKey[feedbackTestKey] = fb
	if res := h.sweep(t).run(context.Background(), parseReviewFeedbackKey); res.RulesCaptured != 0 || h.rules.Evaluated("cncf/techdocs", 424, "inline:1001") {
		t.Fatalf("a comment still in the debounce was read: %+v", res)
	}
	h.now = h.now.Add(time.Hour)
	if res := h.sweep(t).run(context.Background(), parseReviewFeedbackKey); res.RulesCaptured != 1 {
		t.Fatalf("res = %+v", res)
	}
	fb.Items = []github.FeedbackItem{it}
	h.src.byKey[feedbackTestKey] = fb
	h.now = h.now.Add(time.Hour)
	res := h.sweep(t).run(context.Background(), parseReviewFeedbackKey)
	if res.Kicked != 1 || res.RulesCaptured != 0 || !strings.Contains(h.kick.kicks["quality"][0], "R-1 (recorded") {
		t.Fatalf("res = %+v kicks = %q", res, h.kick.kicks)
	}
}
