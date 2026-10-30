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

var menuTestHiveUser = &gh.User{Login: gh.Ptr("hive[bot]")}

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
	notice := renderUnparkNotice(c.unparkMenuFor("A finding with no options in the body.", menuTestHiveUser, comments))
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
	if got := c.unparkMenuFor("no options", menuTestHiveUser, stranger); len(got.options) != 0 {
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

	m := c.unparkMenuFor("body", menuTestHiveUser, []*gh.IssueComment{ghComment("hive[bot]", step)})
	if !m.ownerStep || !strings.Contains(m.bullets(), "`/hive done`") {
		t.Fatalf("owner step must offer /hive done:\n%s", m.bullets())
	}
	// A spoofed marker from someone else does not count.
	if c.unparkMenuFor("body", menuTestHiveUser, []*gh.IssueComment{ghComment("someone", step)}).ownerStep {
		t.Fatal("only the hive's own comment marks an owner step")
	}
	// Answered: only the hive's reply to an accepted command clears it.
	// `/hive help`, the prose hint and the ai_author refusal leave the step
	// open, because the owner has done nothing yet.
	help := ghComment("hive[bot]", unparkReplyMarkerPrefix+"4 -->\n"+renderUnparkHelpFor(m))
	hint := ghComment("hive[bot]", unparkReplyMarkerPrefix+"6 -->\n"+unparkProseHintReply)
	refusal := ghComment("hive[bot]", unparkReplyMarkerPrefix+"7 -->\n"+renderUnparkAIAuthorRefusal("bot-ish"))
	if !c.unparkMenuFor("body", menuTestHiveUser, []*gh.IssueComment{ghComment("hive[bot]", step), help, hint, refusal}).ownerStep {
		t.Fatal("/hive help, the prose hint and the refusal must not answer an owner step")
	}
	answered := ghComment("hive[bot]", unparkReplyMarkerPrefix+"5 -->\n"+renderUnparkAcceptedReply("owner", unparkDoneDecision, []string{issueNeedsDecisionLabel}))
	if c.unparkMenuFor("body", menuTestHiveUser, []*gh.IssueComment{ghComment("hive[bot]", step), help, answered}).ownerStep {
		t.Fatal("an answered owner step must not offer /hive done again")
	}
	labelDrivenAnswer := ghComment("hive[bot]", unparkReplyMarkerPrefix+"8 -->\n"+renderUnparkLabelDrivenAcceptedReply("owner", unparkDoneDecision, nil, nil, "", nil, true))
	if c.unparkMenuFor("body", menuTestHiveUser, []*gh.IssueComment{ghComment("hive[bot]", step), labelDrivenAnswer}).ownerStep {
		t.Fatal("a label-driven acceptance answers the owner step too")
	}
	// A needs-decision comment that lists options is a choice, not a step.
	choice := ownerStepCommentBody("Option A: one\nOption B: two", true)
	if c.unparkMenuFor("body", menuTestHiveUser, []*gh.IssueComment{ghComment("hive[bot]", choice)}).ownerStep {
		t.Fatal("a choice between options is not an owner step")
	}
	if strings.Contains(unparkNoticeBody("Recommendation: option A.\nOption A: x"), "/hive done") {
		t.Fatal("/hive done is offered only for owner steps")
	}
	// An issue filed with `hive-open-issue issue --needs-decision` carries the
	// marker in its body and offers /hive done the same way.
	body := ownerStepCommentBody("Remaining step is yours: install the app.", true)
	if !unparkMenuFromBody(body).ownerStep || !c.unparkMenuFor(body, menuTestHiveUser, nil).ownerStep {
		t.Fatal("a body-level owner step must offer /hive done")
	}
	if c.unparkMenuFor(body, menuTestHiveUser, []*gh.IssueComment{answered}).ownerStep {
		t.Fatal("an accepted reply answers a body-level owner step")
	}
	// A body-level owner step counts only when the hive authored the issue.
	if m := c.unparkMenuFor(body, &gh.User{Login: gh.Ptr("someone")}, nil); m.ownerStep || m.releaseStep != nil {
		t.Fatal("a body-level owner step from a non-hive author must be ignored")
	}
	if unparkMenuFromBody(ownerStepCommentBody("Option A: one\nOption B: two", true)).ownerStep {
		t.Fatal("a body that lists options is a choice, not an owner step")
	}
}

func TestUnparkMenuRecommendationFollowsItsOwnText(t *testing.T) {
	c := menuTestClient()
	// The body recommends B; a later hive comment lists A and B. The
	// recommendation is resolved against the body, not the first option.
	body := "Recommendation: option B, the smaller change."
	options := ghComment("hive[bot]", "Option A: rewrite\nOption B: patch")
	m := c.unparkMenuFor(body, menuTestHiveUser, []*gh.IssueComment{options})
	if m.recommended != "B" {
		t.Fatalf("recommended = %q, want B", m.recommended)
	}
	bullets := m.bullets()
	if !strings.Contains(bullets, "`/hive approve`: go with the recommendation (option B)") || !strings.Contains(bullets, "`/hive decision A`") || strings.Contains(bullets, "/hive decision B") {
		t.Fatalf("approve must name option B:\n%s", bullets)
	}

	// A recommendation that names none of the listed options is not pinned
	// onto the first one; every option stays an explicit decision.
	m = c.unparkMenuFor("I recommend the smaller change.", menuTestHiveUser, []*gh.IssueComment{options})
	bullets = m.bullets()
	if m.recommended != "" || strings.Contains(bullets, "(option") {
		t.Fatalf("no option may be attributed to a recommendation that names none:\n%s", bullets)
	}
	for _, want := range []string{"`/hive approve`: go with the recommendation", "`/hive decision A`", "`/hive decision B`"} {
		if !strings.Contains(bullets, want) {
			t.Fatalf("bullets missing %q:\n%s", want, bullets)
		}
	}

	// The newest text that states a recommendation wins.
	later := ghComment("hive[bot]", "On reflection my recommendation is A.")
	if got := c.unparkMenuFor(body, menuTestHiveUser, []*gh.IssueComment{options, later}).recommended; got != "A" {
		t.Fatalf("recommended = %q, want A from the newest recommendation", got)
	}
}

func TestRenderUnparkAcceptedReplyNamesRemovedLabels(t *testing.T) {
	reply := renderUnparkAcceptedReply("maintainer", "go with the recommendation", []string{"owner-input"})
	if !strings.Contains(reply, unparkAcceptedMarker) || !strings.Contains(reply, "`owner-input` removed") {
		t.Fatalf("reply must carry the accepted marker and name the renamed label:\n%s", reply)
	}
	if strings.Contains(reply, "`needs-human`") {
		t.Fatalf("reply must not claim built-in labels were cleared when a renamed one was:\n%s", reply)
	}
	none := renderUnparkAcceptedReply("maintainer", "x", nil)
	if !strings.Contains(none, "No parking label was left to remove") {
		t.Fatalf("reply with nothing removed must say so:\n%s", none)
	}
	labelDriven := renderUnparkLabelDrivenReply([]string{"owner-input"})
	if !strings.Contains(labelDriven, "`owner-input`") || strings.Contains(labelDriven, "`needs-human`") {
		t.Fatalf("label-driven reply must name the issue's own parking labels:\n%s", labelDriven)
	}
	if !strings.Contains(renderUnparkLabelDrivenReply(nil), "`"+issueNeedsHumanLabel+"` and `"+NeedsDecisionStageLabel+"`") {
		t.Fatal("label-driven reply falls back to the built-in names")
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
	// The command lifts needs-human whoever added it (hivecommons/docs#419):
	// the renamed label goes like the built-in one would, and hold is never
	// touched.
	if len(rec.removed[61]) != 1 || rec.removed[61][0] != "owner-input" {
		t.Fatalf("expected only the renamed needs-human label removed (never hold), got %v", rec.removed[61])
	}
	posted := strings.Join(rec.posted[61], "\n")
	if !strings.Contains(posted, unparkNoticeMarker) {
		t.Fatalf("a renamed parking label must get the notice, got %v", rec.posted[61])
	}
	if !strings.Contains(posted, "cleared") {
		t.Fatalf("accepted reply must say the needs-human label cleared, got %v", rec.posted[61])
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

func TestUnparkOptionSummary(t *testing.T) {
	long := strings.Repeat("word ", 50)
	cases := map[string]string{
		"**A. Pin tag + checksum in-repo (strongest).**": "Pin tag + checksum in-repo (strongest).",
		"**B. Keep `latest`, verify provenance.**":       "Keep `latest`, verify provenance.",
		"- **A.** Read a daemon name":                    "Read a daemon name",
		"Option B: two":                                  "two",
		"**D** — something":                              "something",
		"**Option C** - spaced   out":                    "spaced out",
		"**C)**":                                         "",
		"Option E:":                                      "",
		"not an option":                                  "",
	}
	for line, want := range cases {
		if got := unparkOptionSummary(line); got != want {
			t.Errorf("unparkOptionSummary(%q) = %q, want %q", line, got, want)
		}
	}
	got := unparkOptionSummary("- **A.** " + long)
	if !strings.HasSuffix(got, " …") || len([]rune(got)) > 160 {
		t.Fatalf("long summary not cut: %q", got)
	}
}

// An option line in the untrusted issue body must not be able to plant a
// hive marker or an @mention in the hive-authored notice.
func TestUnparkOptionSummaryStripsMarkersAndMentions(t *testing.T) {
	cases := map[string]string{
		"- **A.** keep it " + unparkAcceptedMarker:                   "keep it",
		"- **B.** " + unparkOwnerStepMarker + " drop it":             "drop it",
		"Option C: ask <!-- hive:unpark-accepted":                    "ask",
		"- **D.** ping @octocat and @hive-bot about it":              "ping octocat and hive-bot about it",
		"- **E.** see docs/@scope or `@literal` or mail a@b.example": "see docs/@scope or `@literal` or mail a@b.example",
	}
	for line, want := range cases {
		if got := unparkOptionSummary(line); got != want {
			t.Errorf("unparkOptionSummary(%q) = %q, want %q", line, got, want)
		}
	}
	body := "Recommendation: A.\n\n- **A.** keep it " + unparkAcceptedMarker + "\n- **B.** " + unparkOwnerStepMarker + " drop it\n"
	notice := unparkNoticeBody(body)
	for _, marker := range []string{unparkAcceptedMarker, unparkOwnerStepMarker} {
		if strings.Contains(notice, marker) {
			t.Errorf("notice carries body marker %q:\n%s", marker, notice)
		}
	}
	if !strings.Contains(notice, "keep it") || !strings.Contains(notice, "drop it") {
		t.Errorf("notice lost the option text:\n%s", notice)
	}
}

func TestUnparkNoticeShowsOptionSummaries(t *testing.T) {
	notice := unparkNoticeBody("Recommendation: B.\n\n- **A.** keep it\n- **B.** drop it\n")
	for _, want := range []string{"`/hive decision A`: go with option A: keep it", "go with the recommendation (option B): drop it"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice missing %q:\n%s", want, notice)
		}
	}
	if strings.Contains(notice, "The options are in") {
		t.Fatalf("body options must not link a comment:\n%s", notice)
	}
	empty := unparkNoticeBody("- **A.**\n- **B.** two\n")
	if !strings.Contains(empty, "go with option A\n") {
		t.Fatalf("empty summary must leave no trailing colon:\n%s", empty)
	}
}

func TestUnparkNoticeLinksOptionComment(t *testing.T) {
	c := menuTestClient()
	first := ghComment("hive[bot]", "Recommendation: A.\n\n- **A.** first a\n- **B.** first b\n")
	first.HTMLURL = gh.Ptr("https://example.com/c1")
	second := ghComment("hive[bot]", "- **A.** second a\n- **B.** second b\n")
	second.HTMLURL = gh.Ptr("https://example.com/c2")

	notice := renderUnparkNotice(c.unparkMenuFor("no options here", menuTestHiveUser, []*gh.IssueComment{first}))
	for _, want := range []string{"The options are in [this comment](https://example.com/c1).\n- `/hive approve`", "(option A): first a", "option B: first b"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice missing %q:\n%s", want, notice)
		}
	}
	notice = renderUnparkNotice(c.unparkMenuFor("- **A.** body a\n", menuTestHiveUser, []*gh.IssueComment{first, second}))
	if !strings.Contains(notice, "https://example.com/c2") || strings.Contains(notice, "c1") ||
		strings.Contains(notice, "first") || strings.Contains(notice, "body a") || !strings.Contains(notice, "second a") {
		t.Fatalf("later comment must replace summaries and link:\n%s", notice)
	}
}
