package github

import (
	"context"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// Danathar/goodreads-mcp#440: the issue recommends option A. "Let hive
// decide" posted a recommend-only "hive recommends: …" comment, and the
// re-posted notice read that comment as the recommendation, so `/hive
// approve` turned from "option A" into an unnamed "go with the
// recommendation" and option A dropped to `/hive decision A`.
const delegate440Body = "Option A: Move the scope to a `report` job.\n" +
	"Option B: Only take the token off disk.\n" +
	"Option C: Accept the risk and close this issue.\n\n" +
	"Recommended: A, because it takes the write scope out of the job that runs downloaded code.\n"

var delegate440Comment = "hive recommends: grant the App the missing permission (for example `workflows`) or have a human apply the change, then re-check; hive cannot clear this by deciding; owner to confirm\n\n" +
	"_Decided by the hive at @owner's request as the owner's delegate under the owner-delegate policy (rule `capability-gap`)._\n" +
	delegatedecision.Marker("h1")

func TestUnparkMenuIgnoresDelegatedRecommendation(t *testing.T) {
	c := menuTestClient()
	m := c.unparkMenuFor(delegate440Body, menuTestHiveUser, []*gh.IssueComment{ghComment("hive[bot]", delegate440Comment)})
	notice := renderUnparkNotice(m)
	for _, want := range []string{
		"`/hive approve` → go with the recommendation (option A): Move the scope",
		"`/hive decision B`",
		"`/hive decision C`",
	} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice missing %q:\n%s", want, notice)
		}
	}
	if strings.Contains(notice, "/hive decision A") {
		t.Fatalf("the recommended option is `/hive approve`, not a separate decision:\n%s", notice)
	}
}

func TestApproveAfterDelegatedRecommendationTakesTheIssuesOption(t *testing.T) {
	issue := parkedIssue(9, hiveComment(301, delegate440Comment), humanComment(302, "maintainer", "/hive approve"))
	issue.Body = delegate440Body
	issue.User = wireUser{Login: "hive[bot]"}
	issue.Labels = []wireLabel{{Name: issueNeedsDecisionLabel}}
	server, _ := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, map[string]string{"maintainer": "write"})
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetAppBotLogin("hive[bot]")
	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	want := "go with the recommendation (option A: Move the scope to a `report` job.)"
	if len(result.Unparked) != 1 || result.Unparked[0].Decision != want {
		t.Fatalf("decision = %+v, want %q", result.Unparked, want)
	}
}
