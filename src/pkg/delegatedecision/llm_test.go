package delegatedecision

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRunner is the decider turn without a model: it records the prompts and
// answers with a canned reply or error.
type fakeRunner struct {
	reply   string
	err     error
	calls   int
	profile string
	system  string
	user    string
}

func (f *fakeRunner) RunDeciderTurn(_ context.Context, profile, system, user string) (string, error) {
	f.calls++
	f.profile, f.system, f.user = profile, system, user
	return f.reply, f.err
}

var (
	llmIssue = Input{Repo: "acme/console", Kind: KindIssue, Number: 7, Title: "Which default should the export use?", Body: "Option A or B?", Labels: []string{LabelNeedsDecision}}
	llmPR    = Input{Repo: "acme/console", Kind: KindPR, Number: 8, Title: "Refactor exporter", CIGreen: true, HiveAuthored: true, TrustedAuthor: true, ContributorOnline: true}
)

func TestIsReversibleDefault(t *testing.T) {
	t.Parallel()
	if !IsReversibleDefault(Decide(llmIssue)) || !IsReversibleDefault(Decide(llmPR)) {
		t.Fatalf("fixtures must hit the reversible default: %q %q", Decide(llmIssue).Rule, Decide(llmPR).Rule)
	}
	dup := llmIssue
	dup.Body = "Duplicate of #3"
	if IsReversibleDefault(Decide(dup)) {
		t.Fatal("dedupe is a rule decision, not the default")
	}
}

func TestParseLLMDecision(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		in        Input
		raw       string
		wantErr   bool
		verdict   Verdict
		kinds     []ActionKind
		keepsPark bool
	}{
		{
			name:    "direction on an issue, fenced",
			in:      llmIssue,
			raw:     "```json\n{\"verdict\":\"direction\",\"text\":\"use option B\",\"rationale\":\"B is reversible\",\"actions\":[{\"kind\":\"comment-decision\"},{\"kind\":\"labels-add\",\"labels\":[\"triage/accepted\"]}]}\n```",
			verdict: VerdictDirection, kinds: []ActionKind{ActionCommentDecision, ActionLabelsAdd},
		},
		{
			name:    "approve a PR",
			in:      llmPR,
			raw:     `{"verdict":"approve","text":"approve #8","rationale":"green and scoped","actions":[{"kind":"comment-decision"},{"kind":"approve-review"}]}`,
			verdict: VerdictApprove, kinds: []ActionKind{ActionCommentDecision, ActionApproveReview},
		},
		{
			name:    "recommend-only keeps the park",
			in:      llmIssue,
			raw:     `{"verdict":"recommend-only","text":"close as won't-fix","rationale":"scope unclear","actions":[{"kind":"recommend-only"}]}`,
			verdict: VerdictRecommendOnly, kinds: []ActionKind{ActionRecommendOnly}, keepsPark: true,
		},
		{
			name:    "close as duplicate with target",
			in:      llmIssue,
			raw:     `{"verdict":"close-duplicate","text":"Duplicate of #3","rationale":"same bug","actions":[{"kind":"close-as-duplicate","target":3}]}`,
			verdict: VerdictCloseDuplicate, kinds: []ActionKind{ActionCloseDuplicate},
		},
		{name: "no JSON", in: llmIssue, raw: "I think approve.", wantErr: true},
		{name: "malformed JSON", in: llmIssue, raw: `{"verdict": "approve",`, wantErr: true},
		{name: "unknown field", in: llmIssue, raw: `{"verdict":"approve","text":"x","rationale":"y","merge":true,"actions":[{"kind":"comment-decision"}]}`, wantErr: true},
		{name: "unknown verdict", in: llmIssue, raw: `{"verdict":"merge","text":"x","rationale":"y","actions":[{"kind":"comment-decision"}]}`, wantErr: true},
		{name: "refused is the rails' verdict", in: llmIssue, raw: `{"verdict":"refused","text":"x","rationale":"y","actions":[{"kind":"comment-decision"}]}`, wantErr: true},
		{name: "unknown action", in: llmPR, raw: `{"verdict":"approve","text":"x","rationale":"y","actions":[{"kind":"merge"}]}`, wantErr: true},
		{name: "grant permission action", in: llmIssue, raw: `{"verdict":"approve","text":"x","rationale":"y","actions":[{"kind":"grant-permission"}]}`, wantErr: true},
		{name: "no actions", in: llmIssue, raw: `{"verdict":"approve","text":"x","rationale":"y","actions":[]}`, wantErr: true},
		{name: "missing rationale", in: llmIssue, raw: `{"verdict":"approve","text":"x","actions":[{"kind":"comment-decision"}]}`, wantErr: true},
		{name: "label outside the set", in: llmIssue, raw: `{"verdict":"approve","text":"x","rationale":"y","actions":[{"kind":"labels-remove","labels":["hive/sentinel"]}]}`, wantErr: true},
		{name: "approve-review on an issue", in: llmIssue, raw: `{"verdict":"approve","text":"x","rationale":"y","actions":[{"kind":"approve-review"}]}`, wantErr: true},
		{name: "adopt-supersede on a hive PR", in: llmPR, raw: `{"verdict":"adopt-supersede","text":"x","rationale":"y","actions":[{"kind":"adopt-supersede"}]}`, wantErr: true},
		{name: "duplicate of itself", in: llmIssue, raw: `{"verdict":"close-duplicate","text":"x","rationale":"y","actions":[{"kind":"close-as-duplicate","target":7}]}`, wantErr: true},
		{name: "recommend-only that acts", in: llmPR, raw: `{"verdict":"recommend-only","text":"x","rationale":"y","actions":[{"kind":"approve-review"}]}`, wantErr: true},
		// A non-park-keeping verdict must carry its recording action: a bare
		// labels-remove of the park label would un-park with no decision
		// comment, no marker and no audit of who decided.
		{name: "un-park by labels-remove alone", in: llmIssue, raw: `{"verdict":"approve","text":"x","rationale":"y","actions":[{"kind":"labels-remove","labels":["stage/085-needs-decision"]}]}`, wantErr: true},
		{name: "park label is not removable even beside the comment", in: llmIssue, raw: `{"verdict":"approve","text":"x","rationale":"y","actions":[{"kind":"comment-decision"},{"kind":"labels-remove","labels":["stage/085-needs-decision"]}]}`, wantErr: true},
		{name: "approve by labels-add alone", in: llmIssue, raw: `{"verdict":"approve","text":"x","rationale":"y","actions":[{"kind":"labels-add","labels":["triage/accepted"]}]}`, wantErr: true},
		{name: "direction by link alone", in: llmIssue, raw: `{"verdict":"direction","text":"x","rationale":"y","actions":[{"kind":"link","target":3}]}`, wantErr: true},
		{name: "approve a PR without approve-review", in: llmPR, raw: `{"verdict":"approve","text":"x","rationale":"y","actions":[{"kind":"comment-decision"}]}`, wantErr: true},
		{
			name:    "removable labels still come off beside the comment",
			in:      llmIssue,
			raw:     `{"verdict":"direction","text":"not flaky after all","rationale":"root cause found","actions":[{"kind":"comment-decision"},{"kind":"labels-remove","labels":["flaky-test"]}]}`,
			verdict: VerdictDirection, kinds: []ActionKind{ActionCommentDecision, ActionLabelsRemove},
		},
		// close-duplicate closes the newer item in favour of the older one.
		{name: "close-duplicate in favour of a newer issue", in: llmIssue, raw: `{"verdict":"close-duplicate","text":"Duplicate of #9","rationale":"same bug","actions":[{"kind":"close-as-duplicate","target":9}]}`, wantErr: true},
		{
			name:    "close-superseded may yield a PR to the hive's newer one",
			in:      Input{Repo: "acme/console", Kind: KindPR, Number: 8, Title: "Fix exporter", CIGreen: true, ContributorOnline: true},
			raw:     `{"verdict":"close-superseded","text":"superseded by #12","rationale":"hive PR is current","actions":[{"kind":"close-as-duplicate","target":12}]}`,
			verdict: VerdictCloseSuperseded, kinds: []ActionKind{ActionCloseDuplicate},
		},
		// Verdict and actions are validated together, so the audit log never
		// records a verdict the executor did not carry out.
		{name: "close-duplicate verdict without the close", in: llmIssue, raw: `{"verdict":"close-duplicate","text":"Duplicate of #3","rationale":"y","actions":[{"kind":"comment-decision"}]}`, wantErr: true},
		{name: "close-superseded verdict without the close", in: llmPR, raw: `{"verdict":"close-superseded","text":"x","rationale":"y","actions":[{"kind":"comment-decision"}]}`, wantErr: true},
		{name: "adopt-supersede verdict without the adopt", in: Input{Repo: "acme/console", Kind: KindPR, Number: 8, ContributorOnline: false}, raw: `{"verdict":"adopt-supersede","text":"x","rationale":"y","actions":[{"kind":"comment-decision"}]}`, wantErr: true},
		{name: "close under a direction verdict", in: llmIssue, raw: `{"verdict":"direction","text":"x","rationale":"y","actions":[{"kind":"comment-decision"},{"kind":"close-as-duplicate","target":3}]}`, wantErr: true},
		{name: "approve-review under a direction verdict", in: llmPR, raw: `{"verdict":"direction","text":"x","rationale":"y","actions":[{"kind":"comment-decision"},{"kind":"approve-review"}]}`, wantErr: true},
		{
			name:    "adopt-supersede verdict with its action",
			in:      Input{Repo: "acme/console", Kind: KindPR, Number: 8, ContributorOnline: false},
			raw:     `{"verdict":"adopt-supersede","text":"adopt now in supersede mode","rationale":"author offline","actions":[{"kind":"comment-decision"},{"kind":"adopt-supersede"}]}`,
			verdict: VerdictAdoptSupersede, kinds: []ActionKind{ActionCommentDecision, ActionAdoptSupersede},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := ParseLLMDecision(tc.in, tc.raw)
			if tc.wantErr {
				if err == nil || !errors.Is(err, ErrInvalidLLMDecision) {
					t.Fatalf("want ErrInvalidLLMDecision, got %v (%+v)", err, d)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d.Verdict != tc.verdict || d.Rule != RuleLLM || d.KeepsPark != tc.keepsPark || !sameKinds(kinds(d), tc.kinds) {
				t.Fatalf("decision = %+v", d)
			}
			if tc.keepsPark && !strings.HasPrefix(d.Text, "hive recommends: ") {
				t.Fatalf("park-keeping text = %q", d.Text)
			}
		})
	}
}

func TestRunWithLLM(t *testing.T) {
	t.Parallel()
	held := llmIssue
	held.Labels = []string{LabelHold}
	dup := llmIssue
	dup.Body = "Duplicate of #3"
	good := `{"verdict":"direction","text":"use option B","rationale":"B is reversible","actions":[{"kind":"comment-decision"}]}`
	cases := []struct {
		name     string
		in       Input
		runner   *fakeRunner
		calls    int
		outcome  LLMOutcome
		wantErr  bool
		rule     string
		verdict  Verdict
		noKinds  []ActionKind
		refusals int
	}{
		{name: "no runner keeps the rules", in: llmIssue, outcome: LLMNotConsulted, rule: "default-approve", verdict: VerdictApprove},
		{name: "rule decision is authoritative", in: dup, runner: &fakeRunner{reply: good}, outcome: LLMNotConsulted, rule: "dedupe", verdict: VerdictCloseDuplicate},
		{name: "held item is refused before the model", in: held, runner: &fakeRunner{reply: good}, outcome: LLMNotConsulted, rule: "rail-hold", verdict: VerdictRefused, refusals: 1},
		{name: "default issue asks the model", in: llmIssue, runner: &fakeRunner{reply: good}, calls: 1, outcome: LLMAccepted, rule: RuleLLM, verdict: VerdictDirection},
		{name: "runner error falls back", in: llmIssue, runner: &fakeRunner{err: errors.New("boom")}, calls: 1, outcome: LLMFallback, wantErr: true, rule: "default-approve", verdict: VerdictApprove},
		{name: "bad reply falls back", in: llmPR, runner: &fakeRunner{reply: `{"verdict":"merge"}`}, calls: 1, outcome: LLMFallback, wantErr: true, rule: "default-release", verdict: VerdictApprove},
		{
			name: "rails still strip hold removal", in: llmIssue, calls: 1, outcome: LLMAccepted, rule: RuleLLM, verdict: VerdictDirection,
			runner:  &fakeRunner{reply: `{"verdict":"direction","text":"lift it","rationale":"r","actions":[{"kind":"comment-decision"},{"kind":"labels-remove","labels":["hold"]}]}`},
			noKinds: []ActionKind{ActionLabelsRemove}, refusals: 1,
		},
		{
			name: "un-park without a decision comment falls back", in: llmIssue, calls: 1, outcome: LLMFallback, wantErr: true, rule: "default-approve", verdict: VerdictApprove,
			runner: &fakeRunner{reply: `{"verdict":"approve","text":"lift it","rationale":"r","actions":[{"kind":"labels-remove","labels":["stage/085-needs-decision"]}]}`},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var runner TurnRunner
			if tc.runner != nil {
				runner = tc.runner
			}
			d, outcome, err := RunWithLLM(context.Background(), tc.in, LLMContext{}, runner)
			if (err != nil) != tc.wantErr || outcome != tc.outcome || d.Rule != tc.rule || d.Verdict != tc.verdict {
				t.Fatalf("got %+v outcome=%s err=%v", d, outcome, err)
			}
			if tc.runner != nil && tc.runner.calls != tc.calls {
				t.Fatalf("runner calls = %d, want %d", tc.runner.calls, tc.calls)
			}
			for _, k := range tc.noKinds {
				for _, a := range d.Actions {
					if a.Kind == k {
						t.Fatalf("rails left %s in %+v", k, d.Actions)
					}
				}
			}
			if len(d.Refusals) != tc.refusals {
				t.Fatalf("refusals = %v, want %d", d.Refusals, tc.refusals)
			}
		})
	}
}

func TestDeciderPrompts(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{reply: `{"verdict":"approve","text":"approve #8","rationale":"ok","actions":[{"kind":"comment-decision"},{"kind":"approve-review"}]}`}
	in := llmPR
	in.SentinelReason = "touches workflows"
	c := LLMContext{
		Thread:            []string{"@alice: should this land?"},
		SentinelFindings:  []string{"workflow permission widened"},
		Linked:            []string{"#5 Exporter rewrite (open)"},
		AutoMerge:         true,
		AuthorAssociation: "MEMBER",
	}
	if _, outcome, err := RunWithLLM(context.Background(), in, c, r); err != nil || outcome != LLMAccepted {
		t.Fatalf("outcome=%s err=%v", outcome, err)
	}
	if r.profile != DeciderProfile || !strings.Contains(r.system, PolicyText) {
		t.Fatalf("profile/system prompt missing the policy: %q", r.profile)
	}
	for _, a := range AllowedActions {
		if !strings.Contains(r.system, string(a)) {
			t.Fatalf("system prompt does not list %s", a)
		}
	}
	for _, want := range []string{"acme/console", "pr #8", "@alice: should this land?", "workflow permission widened", "#5 Exporter rewrite", "auto-merge enabled: true", "MEMBER", "Labels:"} {
		if !strings.Contains(r.user, want) {
			t.Fatalf("user prompt missing %q:\n%s", want, r.user)
		}
	}
	long := LLMContext{}
	for i := 0; i < llmMaxThreadEntries+5; i++ {
		long.Thread = append(long.Thread, "entry")
	}
	if got := strings.Count(DeciderUserPrompt(llmIssue, long), "---\nentry"); got != llmMaxThreadEntries {
		t.Fatalf("thread entries = %d, want %d", got, llmMaxThreadEntries)
	}
}
