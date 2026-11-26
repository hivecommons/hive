package dashboard

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/hivecommons/hive/pkg/github"
)

// A required check GitHub still reports unfinished after its Actions job
// finished (hivecommons/hive#15941) keeps the PR CI-pending: the stage, the
// gates and the merge verdict all still wait on it, and nothing in the hive
// re-runs the job. #15941 named the next step as an owner's (inspect the
// exact attempt, then one affected-workflow rerun) but showed it only in the
// reason tooltip, so the PR sat under "Agents are working it" until a person
// happened to open it. This rule puts that ask in the your-turn inbox, and
// from there on the Overview card and in the Activity monitor's Waiting on
// you list. It is display only: no stage, gate, verdict or agent pass reads
// it, and it writes nothing.

// OwnershipReasonCIReportingStalled: the PR's merge verdict carries a
// confirmed CI reporting stall (MergeVerdict.CIReportingStall: past the
// propagation grace, on the head the verdict judged), whose recovery is
// owner-action-required.
const OwnershipReasonCIReportingStalled = "ci_reporting_stalled"

func init() {
	ownershipReasonVerbs[OwnershipReasonCIReportingStalled] = OwnershipVerbReview
}

// prCIReportingStalledOwnership is the your-turn decision for a PR whose
// merge verdict carries a confirmed CI reporting stall. It reads only the
// verdict's structured observation, which the merge classifier attaches from
// the CI evaluation itself; reason text is never parsed, because a check name
// a contributor chose can appear in it. Today that is a contributor-lane PR
// whose relay refusal is the stall: hive-authored, trusted-bot and
// trusted-author PRs do not reach it. The verdict is recomputed for the PR's
// current head on every governor pass, so the row goes away by itself once a
// pass no longer sees the stall: a rerun whose new attempt reports, a new
// head, or the check finishing. ci-reporting-unverified (an API refusal) and
// a mismatch still inside the grace never count: neither is a stalled
// observation. A draft, a verdict GitHub itself blocks (a conflict), an
// eligible verdict, a hand merge and a head the hive's review returned for
// changes are left alone.
func prCIReportingStalledOwnership(pr github.PullRequest, verdict *github.MergeVerdict) (Ownership, bool) {
	if verdict == nil || verdict.CIReportingStall == nil || pr.Draft || verdict.ReviewChangesRequested || verdict.HandMerge {
		return Ownership{}, false
	}
	switch verdict.State {
	case github.MergeVerdictBlocked, github.MergeVerdictEligible:
		return Ownership{}, false
	}
	stall := *verdict.CIReportingStall
	head := strings.TrimSpace(pr.HeadSHA)
	if stall.State != "stalled" || stall.Recovery != "owner-action-required" || head == "" || !strings.EqualFold(strings.TrimSpace(stall.HeadSHA), head) ||
		(stall.PRNumber > 0 && stall.PRNumber != pr.Number) || strings.TrimSpace(stall.Check) == "" || stall.RunID <= 0 || stall.Attempt <= 0 {
		return Ownership{}, false
	}
	return yourTurn(OwnershipReasonCIReportingStalled, ciReportingStalledReason(nonEmpty(stall.Repo, pr.Repo), pr.URL, stall)), true
}

// ciReportingStalledReason is the decision's reason: what GitHub shows, the
// run and attempt, and the one rerun #15941 allows, as a command. With no
// job id it re-runs the run (that workflow only). The link is built from the
// PR's own URL, so it points at the same GitHub host.
func ciReportingStalledReason(repo, prURL string, stall github.CIReportingObservation) string {
	state := "unfinished"
	switch stall.CheckStatus {
	case "in_progress":
		state = "running"
	case "queued", "waiting", "pending", "requested":
		state = "queued"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "GitHub still shows check '%s' %s though its job finished (run %d, attempt %d).", ciReportingCheckLabel(stall.Check), state, stall.RunID, stall.Attempt)
	look := "Look at the run"
	if link := ciReportingRunURL(prURL, stall.RunID, stall.Attempt); link != "" {
		look += " (" + link + ")"
	}
	command := "gh run rerun " + strconv.FormatInt(stall.RunID, 10)
	what := "re-run that workflow"
	if stall.JobID > 0 {
		command += " --job " + strconv.FormatInt(stall.JobID, 10)
		what = "re-run that job"
	}
	if repo = strings.TrimSpace(repo); strings.Contains(repo, "/") {
		command += " -R " + repo
	}
	b.WriteString(" " + look + ", then " + what + ": " + command + ".")
	return b.String()
}

// ciReportingRunURL is the run attempt's page beside the PR's own
// .../pull/N URL, or "" when the PR URL is not one.
func ciReportingRunURL(prURL string, runID int64, attempt int) string {
	prURL = strings.TrimSpace(prURL)
	i := strings.LastIndex(prURL, "/pull/")
	if i <= 0 || runID <= 0 {
		return ""
	}
	base := prURL[:i]
	link := base + "/actions/runs/" + strconv.FormatInt(runID, 10)
	if attempt > 0 {
		link += "/attempts/" + strconv.Itoa(attempt)
	}
	return link
}

// ciReportingCheckLabel is the check name as the reason quotes it. A
// workflow's author names the check, so quotes, backticks, "@" and control
// characters are dropped and the name is cut to 100 characters: it cannot
// close the quote and pass prose (a second command) off as the hive's.
func ciReportingCheckLabel(name string) string {
	var b strings.Builder
	n := 0
	for _, r := range name {
		if n >= 100 {
			b.WriteString("...")
			break
		}
		switch {
		case r == '\'' || r == '"' || r == '`' || r == '@' || unicode.IsControl(r):
			continue
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}
