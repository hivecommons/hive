package github

import (
	"context"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
)

func hiveComment(id int64, body string) unparkWireComment {
	return humanComment(id, "hive[bot]", body)
}

func ghComment(login, body string) *gh.IssueComment {
	return &gh.IssueComment{User: &gh.User{Login: gh.Ptr(login)}, Body: gh.Ptr(body)}
}

func menuTestClient() *Client {
	c := &Client{}
	c.SetAppBotLogin("hive[bot]")
	return c
}

const dakotaStyleOptions = "### Options — please pick one (reply `A`, `B` or `C`)\n\n" +
	"**A. Pin tag + checksum in-repo (strongest).**\nAdd a lock file.\n\n" +
	"**B. Keep `latest`, verify provenance.**\nRun gh attestation verify.\n\n" +
	"**C. Accept the risk, record provenance only.**\nLog the SHA.\n\n" +
	"My recommendation is **B**; **A** is a good follow-on."

func TestUnparkOptionLetterReadsBoldLetters(t *testing.T) {
	cases := map[string]string{
		"**A. Pin tag**":         "A",
		"- **C.** document only": "C",
		"**B)** keep latest":     "B",
		"**D** — something":      "D",
		"Option E: classic":      "E",
		"**B** is a follow-on":   "",
		"**G. out of range**":    "",
		"A plain sentence.":      "",
	}
	for line, want := range cases {
		if got := unparkOptionLetter(line); got != want {
			t.Errorf("unparkOptionLetter(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestUnparkMenuReadsOptionsFromHiveComment(t *testing.T) {
	c := menuTestClient()
	comments := []*gh.IssueComment{
		ghComment("hive[bot]", dakotaStyleOptions),
		ghComment("hive[bot]", unparkNoticeMarker+"\n**What to reply**\n- `/hive decision <your instructions>`: go ahead"),
	}
	notice := renderUnparkNotice(c.unparkMenuFor("A finding with no options in the body.", comments))
	for _, want := range []string{
		"`/hive approve`: go with the recommendation (option B)",
		"`/hive decision A`: go with option A",
		"`/hive decision C`: go with option C",
	} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice missing %q:\n%s", want, notice)
		}
	}
	if strings.Contains(notice, "<your instructions>") || strings.Contains(notice, "/hive decision B") {
		t.Fatalf("options from the comment must replace the free-text form:\n%s", notice)
	}

	// A comment from someone other than the hive does not set the options.
	stranger := []*gh.IssueComment{ghComment("someone", dakotaStyleOptions)}
	if got := c.unparkMenuFor("no options", stranger); len(got.options) != 0 {
		t.Fatalf("a stranger's comment must not set options, got %v", got.options)
	}
}

func TestSweepIssueUnparkNoticeUsesCommentOptions(t *testing.T) {
	issue := parkedIssue(31, hiveComment(301, dakotaStyleOptions))
	issue.Body = "A finding; the options are in a comment."
	issue.Labels = []wireLabel{{Name: issueNeedsDecisionLabel}}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, nil)
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetAppBotLogin("hive[bot]")

	if _, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(rec.posted[31]) != 1 {
		t.Fatalf("expected one notice, got %v", rec.posted[31])
	}
	notice := rec.posted[31][0]
	for _, want := range []string{unparkNoticeMarker, "(option B)", "/hive decision A", "/hive decision C"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice missing %q:\n%s", want, notice)
		}
	}
}

func TestUnparkNoticeWithoutOptionsOrRecommendation(t *testing.T) {
	notice := unparkNoticeBody("Something is broken; please advise.")
	if strings.Contains(notice, "go with the recommendation") || strings.Contains(notice, "/hive approve") {
		t.Fatalf("no recommendation is stated, so approve must not be offered:\n%s", notice)
	}
	if !strings.Contains(notice, "/hive decision <your instructions>") {
		t.Fatalf("the free-text form is still offered:\n%s", notice)
	}
	withRec := unparkNoticeBody("Recommendation: revert the change.")
	if !strings.Contains(withRec, "`/hive approve`: go with the recommendation") {
		t.Fatalf("a stated recommendation keeps approve:\n%s", withRec)
	}
	noRecOptions := unparkNoticeBody("Option A: one\nOption B: two\n")
	if !strings.Contains(noRecOptions, "`/hive approve`: go with option A") || strings.Contains(noRecOptions, "recommendation") {
		t.Fatalf("options without a recommendation must not claim one:\n%s", noRecOptions)
	}
}

func TestUnparkMenuOffersDoneForOwnerStep(t *testing.T) {
	c := menuTestClient()
	step := ownerStepCommentBody("Remaining step is yours: enable the ruleset in Settings.", true)
	if !strings.Contains(step, unparkOwnerStepMarker) {
		t.Fatalf("a needs-decision comment must carry the owner-step marker: %q", step)
	}
	if ownerStepCommentBody("plain note", false) != "plain note" {
		t.Fatal("a comment that does not park must be unchanged")
	}
	if ownerStepCommentBody(step, true) != step {
		t.Fatal("the marker is added once")
	}

	m := c.unparkMenuFor("body", []*gh.IssueComment{ghComment("hive[bot]", step)})
	if !m.ownerStep || !strings.Contains(m.bullets(), "`/hive done`") {
		t.Fatalf("owner step must offer /hive done:\n%s", m.bullets())
	}
	// A spoofed marker from someone else does not count.
	if c.unparkMenuFor("body", []*gh.IssueComment{ghComment("someone", step)}).ownerStep {
		t.Fatal("only the hive's own comment marks an owner step")
	}
	// Answered: a later hive reply to a /hive command clears it.
	answered := ghComment("hive[bot]", unparkReplyMarkerPrefix+"5 -->\nUn-parked by @owner.")
	if c.unparkMenuFor("body", []*gh.IssueComment{ghComment("hive[bot]", step), answered}).ownerStep {
		t.Fatal("an answered owner step must not offer /hive done again")
	}
	// A needs-decision comment that lists options is a choice, not a step.
	choice := ownerStepCommentBody("Option A: one\nOption B: two", true)
	if c.unparkMenuFor("body", []*gh.IssueComment{ghComment("hive[bot]", choice)}).ownerStep {
		t.Fatal("a choice between options is not an owner step")
	}
	if strings.Contains(unparkNoticeBody("Recommendation: option A.\nOption A: x"), "/hive done") {
		t.Fatal("/hive done is offered only for owner steps")
	}
}

func TestParseUnparkCommandDone(t *testing.T) {
	cmd, ok := parseUnparkCommand("/hive done")
	if !ok || cmd.Kind != unparkCommandDone {
		t.Fatalf("parseUnparkCommand(/hive done) = %+v, %v", cmd, ok)
	}
	if got := unparkCommandDecisionText(cmd); got != unparkDoneDecision {
		t.Fatalf("decision = %q", got)
	}
	if _, ok := parseUnparkCommand("> /hive done"); ok {
		t.Fatal("a quoted /hive done is not a command")
	}
}

func TestSweepIssueUnparkDoneClearsParkLabel(t *testing.T) {
	step := hiveComment(401, ownerStepCommentBody("Remaining step is yours: add the secret.", true))
	issue := parkedIssue(41, step, humanComment(402, "maintainer", "/hive done"))
	issue.Labels = []wireLabel{{Name: issueNeedsDecisionLabel}, {Name: "hold"}}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, map[string]string{"maintainer": "write"})
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetAppBotLogin("hive[bot]")

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 1 || result.Unparked[0].Decision != unparkDoneDecision {
		t.Fatalf("expected /hive done to un-park with the done decision, got %+v", result.Unparked)
	}
	if got := strings.Join(rec.removed[41], ","); got != issueNeedsDecisionLabel {
		t.Fatalf("expected only %s removed, got %v", issueNeedsDecisionLabel, rec.removed[41])
	}
	if len(rec.added[41]) != 1 || rec.added[41][0] != HumanAckLabel {
		t.Fatalf("expected %s added, got %v", HumanAckLabel, rec.added[41])
	}
	joined := strings.Join(rec.posted[41], "\n")
	if !strings.Contains(joined, "`/hive done`") || !strings.Contains(joined, unparkDoneDecision) {
		t.Fatalf("expected the notice to offer /hive done and the reply to record it, got %v", rec.posted[41])
	}
}

func TestSweepIssueUnparkDoneIgnoresIneligibleCommenter(t *testing.T) {
	step := hiveComment(501, ownerStepCommentBody("Remaining step is yours: add the secret.", true))
	issue := parkedIssue(51, step, humanComment(502, "drive-by", "/hive done"))
	issue.Labels = []wireLabel{{Name: issueNeedsDecisionLabel}}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, map[string]string{"drive-by": "read"})
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetAppBotLogin("hive[bot]")

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 0 || result.Replies != 0 {
		t.Fatalf("an ineligible /hive done must be ignored, got %+v", result)
	}
	if len(rec.removed[51]) != 0 || len(rec.added[51]) != 0 {
		t.Fatalf("labels changed: removed=%v added=%v", rec.removed[51], rec.added[51])
	}
	for _, body := range rec.posted[51] {
		if !strings.Contains(body, unparkNoticeMarker) {
			t.Fatalf("only the notice may be posted, got %q", body)
		}
	}
}

func TestSweepIssueUnparkHonoursRenamedParkingLabels(t *testing.T) {
	renamed := config.IssueFilterConfig{HardSuppressLabels: config.HardSuppressLabelsConfig{
		NeedsHuman: []string{"owner-input"},
		NeedsSpec:  []string{"spec-please"},
	}}
	parked := parkedIssue(61, humanComment(601, "maintainer", "/hive approve"))
	parked.Labels = []wireLabel{{Name: "owner-input"}, {Name: "hold"}}
	spec := parkedIssue(62, humanComment(602, "maintainer", "/hive approve"))
	spec.Labels = []wireLabel{{Name: "spec-please"}}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{parked, spec}, map[string]string{"maintainer": "write"})
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetIssueFilter(renamed)

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 1 || result.Unparked[0].Number != 61 {
		t.Fatalf("expected the renamed needs-human issue un-parked, got %+v", result.Unparked)
	}
	if got := strings.Join(rec.removed[61], ","); got != "owner-input" {
		t.Fatalf("expected the renamed label removed (and never hold), got %v", rec.removed[61])
	}
	if !strings.Contains(strings.Join(rec.posted[61], "\n"), unparkNoticeMarker) {
		t.Fatalf("a renamed parking label must get the notice, got %v", rec.posted[61])
	}
	if len(rec.posted[62]) != 0 || len(rec.removed[62]) != 0 {
		t.Fatalf("needs-spec stays outside the un-park commands, got posted=%v removed=%v", rec.posted[62], rec.removed[62])
	}

	if !c.hasNeedsHumanLabel([]string{"Owner-Input"}) || c.hasNeedsHumanLabel([]string{"spec-please"}) {
		t.Fatal("hasNeedsHumanLabel must follow the configured needs_human names only")
	}
	if !c.isParkingLabel(issueNeedsHumanLabel) {
		t.Fatal("the built-in name Hive applies itself still parks")
	}
}
