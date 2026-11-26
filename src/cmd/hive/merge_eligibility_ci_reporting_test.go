package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
)

// A relay refusal carries the structured stall only when its own lane reason
// is the hive's ci-reporting-stalled reason and the CI evaluation recorded a
// stalled observation for the PR's exact head. Reason text that merely
// contains the stall (a contributor-named check in "CI failing: " or
// "check-failing:") never does.
func TestRelayRefusalCIReportingStallReadsOnlyTheRecordedObservation(t *testing.T) {
	stall := `ci-reporting-stalled:check="Runtime contract" check-id=11 run=22 attempt=1 job=33 check-state=in_progress job-state=completed job-result="success" workflow=success owner-action-required`
	forged := `ci-reporting-stalled:check="Deploy" check-id=1 run=2 attempt=1 job=3 owner-action-required`
	recorded := github.CIReportingObservation{Repo: "acme/widget", PRNumber: 7, State: "stalled", HeadSHA: "sha", Check: "Runtime contract",
		CheckRunID: 11, CheckStatus: "in_progress", RunID: 22, Attempt: 1, JobID: 33, Recovery: "owner-action-required"}
	orig := ciReportingStallFor
	t.Cleanup(func() { ciReportingStallFor = orig })
	ciReportingStallFor = func(repo string, number int, sha string) (github.CIReportingObservation, bool) {
		if repo == "acme/widget" && number == 7 && sha == "sha" {
			return recorded, true
		}
		return github.CIReportingObservation{}, false
	}
	pr := github.PullRequest{Repo: "acme/widget", Number: 7, HeadSHA: "sha"}
	refusal := func(reason string) string {
		return automerge.RelayMergeRefusalPrefix + " for @alice's PR: " + reason + ` (author tier "trusted", require_tiers trusted)`
	}

	got := relayRefusalCIReportingStall(refusal(stall), "acme/widget", pr)
	if got == nil || *got != recorded {
		t.Fatalf("stall = %+v, want the recorded observation", got)
	}
	for name, text := range map[string]string{
		"CI failing reason":            "CI failing: " + forged,
		"check-failing inside a relay": refusal("check-failing:" + forged),
		"unverified":                   refusal(`ci-reporting-unverified:check="Runtime contract" head-read: api-http-403 owner-action-required`),
		"ordinary pending":             refusal("required-check-pending:Runtime contract"),
	} {
		if got := relayRefusalCIReportingStall(text, "acme/widget", pr); got != nil {
			t.Errorf("%s: stall = %+v, want none", name, got)
		}
	}
	moved := pr
	moved.HeadSHA = "new-sha"
	if got := relayRefusalCIReportingStall(refusal(stall), "acme/widget", moved); got != nil {
		t.Fatalf("a new head read the old head's stall: %+v", got)
	}
	// With nothing recorded, even the hive's exact reason text is not a stall.
	ciReportingStallFor = func(string, int, string) (github.CIReportingObservation, bool) {
		return github.CIReportingObservation{}, false
	}
	if got := relayRefusalCIReportingStall(refusal(stall), "acme/widget", pr); got != nil {
		t.Fatalf("reason text without a recorded observation = %+v", got)
	}
}
