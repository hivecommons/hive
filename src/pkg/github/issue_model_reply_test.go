package github

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

// techdocsReplyProfile compiles the techdocs test model with a label mapped
// to draft-under-way, so `approve analysis` at analysis-review (045) both
// moves the item and applies a label while `revise` only applies the label
// its event observes.
func techdocsReplyProfile(t *testing.T) *workflowmodel.Profile {
	t.Helper()
	b, err := os.ReadFile("../workflowmodel/testdata/models/techdocs.json")
	if err != nil {
		t.Fatal(err)
	}
	var m workflowmodel.Model
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m.Spec.Labels = workflowmodel.LabelSet{Prefix: "docs", Entries: []workflowmodel.LabelEntry{{Name: "docs/040-draft-under-way", State: "draft-under-way"}}}
	p, _, err := workflowmodel.Compile(&m)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func modelParkedIssue(number int, comments ...unparkWireComment) unparkWireIssue {
	issue := parkedIssue(number, comments...)
	issue.Labels = []wireLabel{{Name: stagelabels.IssueNeedsDecision.Label()}, {Name: "needs-human"}}
	return issue
}

func TestParseUnparkCommandForModelVerbs(t *testing.T) {
	verbs := []string{"approve analysis", "revise", "approve", ""}
	cases := []struct {
		body       string
		kind       unparkCommandKind
		verb, text string
		ok         bool
	}{
		{"/hive Approve_Analysis", unparkCommandModelReply, "approve analysis", "", true},
		{"/hive approve-analysis looks right\n\n1. keep the table", unparkCommandModelReply, "approve analysis", "looks right\n\n1. keep the table", true},
		{"  /HIVE revise: tighten section 2", unparkCommandModelReply, "revise", "tighten section 2", true},
		{"/hive approve", unparkCommandModelReply, "approve", "", true},
		{"/hive decision B", unparkCommandDecision, "", "B", true},
		{"/hive revised", "", "", "", false},
		{"> /hive revise", "", "", "", false},
		{"```\n/hive revise\n```", "", "", "", false},
		{"/hive", "", "", "", false},
		{"please /hive revise", "", "", "", false},
	}
	for _, c := range cases {
		cmd, ok := parseUnparkCommandFor(c.body, verbs)
		if ok != c.ok || cmd.Kind != c.kind || cmd.Verb != c.verb || cmd.DecisionText() != c.text {
			t.Errorf("parseUnparkCommandFor(%q) = %+v %v, want kind=%q verb=%q text=%q ok=%v", c.body, cmd, ok, c.kind, c.verb, c.text, c.ok)
		}
	}
	// No verbs: a model verb is the unknown command it always was.
	if cmd, ok := parseUnparkCommandFor("/hive revise", nil); ok || cmd.Kind != unparkCommandNone {
		t.Fatalf("unbound: %+v %v", cmd, ok)
	}
	if !isUnparkWatermarkCommandFor("/hive revise", verbs) || isUnparkWatermarkCommand("/hive revise") {
		t.Fatal("an answered model reply is a watermark only on a bound repository")
	}
}

func TestModelReplyVerbsFollowStateAndSkipHiveDefault(t *testing.T) {
	p := techdocsReplyProfile(t)
	if got := modelReplyVerbs(p, []string{stagelabels.IssueNeedsDecision.Label(), "needs-human"}); strings.Join(got, ",") != "approve analysis,revise" {
		t.Fatalf("045 verbs = %v", got)
	}
	if got := modelReplyVerbs(p, []string{"needs-human"}); got != nil {
		t.Fatalf("no stage label: %v", got)
	}
	if got := modelReplyVerbs(hivedefault.Profile(), []string{stagelabels.IssueNeedsDecision.Label()}); got != nil {
		t.Fatalf("hive-default must offer no comment verbs, got %v", got)
	}
	if got := modelReplyVerbs(nil, []string{stagelabels.IssueNeedsDecision.Label()}); got != nil {
		t.Fatalf("nil profile: %v", got)
	}
}

func TestSweepIssueUnparkCommandsModelReplyAppliesMappedLabel(t *testing.T) {
	issues := []unparkWireIssue{modelParkedIssue(7, humanComment(11, "maintainer", "/hive Approve analysis — go with the shorter outline"))}
	server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{"maintainer": "write"})
	c := newTestClient(t, server, "org", []string{"repo"})
	p := techdocsReplyProfile(t)
	var audited []IssueUnparkEvent

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{
		WorkflowFor: func(owner, repo string) *workflowmodel.Profile {
			if owner != "org" || repo != "repo" {
				t.Fatalf("WorkflowFor(%q,%q)", owner, repo)
			}
			return p
		},
		Audit: func(e IssueUnparkEvent) { audited = append(audited, e) },
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 1 || result.Unparked[0].Number != 7 {
		t.Fatalf("expected issue 7 un-parked, got %+v", result.Unparked)
	}
	if got := result.Unparked[0].Decision; got != "approve analysis: — go with the shorter outline" {
		t.Fatalf("decision = %q", got)
	}
	if len(audited) != 1 || audited[0].CommentURL == "" {
		t.Fatalf("audit = %+v", audited)
	}
	added := strings.Join(rec.added[7], ",")
	if !strings.Contains(added, HumanAckLabel) || !strings.Contains(added, "docs/040-draft-under-way") {
		t.Fatalf("expected %s and the mapped model label added, got %v", HumanAckLabel, rec.added[7])
	}
	if !strings.Contains(strings.Join(rec.removed[7], ","), "needs-human") {
		t.Fatalf("needs-human is answered by the reply, got %v", rec.removed[7])
	}
	joined := strings.Join(rec.posted[7], "\n")
	if !strings.Contains(joined, "Un-parked by @maintainer. Decision: approve analysis") || !strings.Contains(joined, "Workflow reply `approve analysis` moves the item from `analysis-review` to `draft-under-way`; `docs/040-draft-under-way` applied.") {
		t.Fatalf("reply = %v", rec.posted[7])
	}
	if !strings.Contains(joined, unparkReplyMarkerPrefix+"11 -->") {
		t.Fatalf("reply must answer comment 11, got %v", rec.posted[7])
	}
}

func TestSweepIssueUnparkCommandsModelVerbUnknownWhenUnbound(t *testing.T) {
	issues := []unparkWireIssue{modelParkedIssue(7, humanComment(11, "maintainer", "/hive revise"))}
	server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{"maintainer": "write"})
	c := newTestClient(t, server, "org", []string{"repo"})

	for name, opts := range map[string]IssueUnparkSweepOptions{
		"no lookup":    {},
		"nil profile":  {WorkflowFor: func(string, string) *workflowmodel.Profile { return nil }},
		"hive-default": {WorkflowFor: func(string, string) *workflowmodel.Profile { return hivedefault.Profile() }},
	} {
		result, err := c.SweepIssueUnparkCommands(context.Background(), opts)
		if err != nil {
			t.Fatalf("%s: sweep: %v", name, err)
		}
		if len(result.Unparked) != 0 || len(rec.added[7]) != 0 || len(rec.removed[7]) != 0 {
			t.Fatalf("%s: a model verb on an unbound repo must not act: %+v added=%v removed=%v", name, result.Unparked, rec.added[7], rec.removed[7])
		}
		for _, body := range rec.posted[7] {
			if strings.Contains(body, "Un-parked") || strings.Contains(body, "Workflow reply") {
				t.Fatalf("%s: unexpected reply %q", name, body)
			}
		}
	}
}

func TestRecordOperatorIssueModelReply(t *testing.T) {
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{modelParkedIssue(7), modelParkedIssue(8)}, nil)
	c := newTestClient(t, server, "org", []string{"repo"})
	p := techdocsReplyProfile(t)

	event, err := c.RecordOperatorIssueModelReply(context.Background(), "org/repo", 7, "maintainer", "Revise", " section 2 is thin ", p)
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if event.Decision != "revise: section 2 is thin" || event.Actor != "maintainer" {
		t.Fatalf("event = %+v", event)
	}
	added := strings.Join(rec.added[7], ",")
	if !strings.Contains(added, HumanAckLabel) || !strings.Contains(added, "techdocs/analysis") {
		t.Fatalf("expected %s and the event label added, got %v", HumanAckLabel, rec.added[7])
	}
	joined := strings.Join(rec.posted[7], "\n")
	if !strings.Contains(joined, "Workflow reply `revise` moves the item from `analysis-review` to `analysing`; `techdocs/analysis` applied.") || !strings.Contains(joined, "hive dashboard") {
		t.Fatalf("reply = %v", rec.posted[7])
	}

	for name, c2 := range map[string]struct {
		verb    string
		profile *workflowmodel.Profile
	}{"unknown verb": {"ship it", p}, "wrong state verb": {"approve", p}, "nil profile": {"revise", nil}, "hive-default": {"revise", hivedefault.Profile()}} {
		if _, err := c.RecordOperatorIssueModelReply(context.Background(), "org/repo", 8, "maintainer", c2.verb, "", c2.profile); !errors.Is(err, ErrUnknownModelReply) {
			t.Errorf("%s: err = %v, want ErrUnknownModelReply", name, err)
		}
	}
	if _, err := c.RecordOperatorIssueModelReply(context.Background(), "org/repo", 8, "", "revise", "", p); err == nil {
		t.Error("blank actor must be refused")
	}
	if len(rec.added[8])+len(rec.posted[8]) != 0 {
		t.Fatalf("refused replies must write nothing, got added=%v posted=%v", rec.added[8], rec.posted[8])
	}
}

func TestRecordOperatorIssueModelReplyRefusesUnparkedIssue(t *testing.T) {
	// A verb-less state: the issue is at 045 only by label, so the model
	// offers verbs, but without a parking label there is nothing to answer.
	issue := modelParkedIssue(9)
	issue.Labels = []wireLabel{{Name: "bug"}}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, nil)
	c := newTestClient(t, server, "org", []string{"repo"})
	if _, err := c.RecordOperatorIssueModelReply(context.Background(), "org/repo", 9, "maintainer", "revise", "", techdocsReplyProfile(t)); !errors.Is(err, ErrUnknownModelReply) {
		t.Fatalf("err = %v", err)
	}
	if len(rec.added[9])+len(rec.posted[9]) != 0 {
		t.Fatal("must write nothing")
	}
}

func TestRecordOperatorIssueModelReplyArgumentErrors(t *testing.T) {
	server, _ := newUnparkServer(t, "org", "repo", []unparkWireIssue{modelParkedIssue(7)}, nil)
	c := newTestClient(t, server, "org", []string{"repo"})
	p := techdocsReplyProfile(t)
	var nilClient *Client
	if _, err := nilClient.RecordOperatorIssueModelReply(context.Background(), "org/repo", 7, "m", "revise", "", p); !errors.Is(err, ErrNoGitHubClient) {
		t.Errorf("nil client: %v", err)
	}
	if _, err := c.RecordOperatorIssueModelReply(context.Background(), "org/repo", 0, "m", "revise", "", p); err == nil {
		t.Error("issue 0 must be refused")
	}
	if _, err := c.RecordOperatorIssueModelReply(context.Background(), "org/../x", 7, "m", "revise", "", p); err == nil {
		t.Error("invalid repo must be refused")
	}
	if _, err := c.RecordOperatorIssueModelReply(context.Background(), "org/repo", 404, "m", "revise", "", p); err == nil {
		t.Error("missing issue must surface the read error")
	}
}

func TestRenderModelReplyAcceptedBranches(t *testing.T) {
	outcome := unparkNeedsHumanOutcome{}
	got := renderModelReplyAccepted("m", "revise", workflowmodel.ReplyEffect{Verb: "revise", From: "a", To: "b"}, "", nil, false, outcome)
	if !strings.Contains(got, "moves the item from `a` to `b`; the model maps no label to that state, so the reply is recorded here only.") {
		t.Fatalf("no-label branch: %s", got)
	}
	got = renderModelReplyAccepted("m", "hold", workflowmodel.ReplyEffect{Verb: "hold"}, "", nil, false, outcome)
	if !strings.Contains(got, "Workflow reply `hold` is recorded; the model declares no transition for it.") {
		t.Fatalf("no-transition branch: %s", got)
	}
}
