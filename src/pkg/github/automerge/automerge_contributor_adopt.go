package automerge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Stage-074 adoption (hivecommons/hive#12436).
//
// A contributor PR in stage/072-returned-to-contributor whose author has not
// pushed a new head within the grace after the nudge is adopted: the hive
// re-lanes it through the takeover path so the scanner can finish it. The
// grace is contributor_grace_online (default 2h) when the author's relay
// was reachable at the nudge, contributor_grace_offline (1h) otherwise — an
// external author or an offline relay is the offline case, and a relay
// contributor who was over their fix budget ("queued") counts as online
// because adoption is the pressure-relief valve for THEIR overflow too.
//
// Adopting means: push `hive/adopt-<n>` in the base repo at the contributor
// head (the fork's commits, authorship and sign-off preserved byte for byte),
// open an App PR from it that supersedes the original and carries its
// Closes lines, comment once on the original, and move it to
// stage/074-adopted-by-hive. The App PR is red with the same failures, so it
// lands in ci-failing.json and the scanner's FIX-BEFORE-NEW kick (the
// existing red-PR repair route for hive-authored PRs) rebases it on the base
// and fixes CI on that branch — no second kick mechanism. Once the App PR
// merges the original is closed with a note; if the App PR is closed
// unmerged the adoption is forgotten and the original falls back to 072.
//
// One attempt per contributor head, whatever the outcome; a new head from
// the author before adoption re-arms the nudge instead.

const (
	// adoptBranchPrefix names the App branch that adopts contributor PR <n>.
	adoptBranchPrefix = "hive/adopt-"
	// AdoptionMarkerPrefix opens the one adoption comment on the original PR.
	AdoptionMarkerPrefix = "<!-- hive:adopted"
	// adoptionCloseMarker is stamped on the close note after the App PR merged.
	adoptionCloseMarker = "<!-- hive:adopted-merged -->"
	// adoptCheckInterval bounds how often an adopted PR's superseder is polled.
	adoptCheckInterval = 10 * time.Minute
)

// adoptClosesPattern lifts the original PR's closing keywords so the
// superseding PR closes the same issues. A non-closing `Refs #N` line
// travels too (hivecommons/hive#14471): it is the original's link to its
// issue, and the claim ledger reads it as the superseder covering that issue.
var adoptClosesPattern = regexp.MustCompile(`(?im)^\s*(?:fixes|closes|resolves|refs)\s+(?:[\w.-]+/[\w.-]+)?#\d+\s*$`)

var adoptHiveRunPattern = regexp.MustCompile(`(?im)^\s*Hive-Run\s*:\s*(?:[\w.-]+/[\w.-]+)?#(\d+)\s*$`)

// adoptionGrace returns the grace that applies to the record's first nudge.
func adoptionGrace(rec stagelabels.Record, policy StageLabelPolicy) time.Duration {
	if rec.NudgeOnline {
		if policy.GraceOnline > 0 {
			return policy.GraceOnline
		}
		return 2 * time.Hour
	}
	if policy.GraceOffline > 0 {
		return policy.GraceOffline
	}
	return 15 * time.Minute
}

// ContributorPresence is what the hub knows about a contributor's relay:
// whether they have a contributor profile on this hive at all, whether a
// relay heartbeat is within the presence window right now, and the last
// heartbeat seen (zero when none this process).
type ContributorPresence struct {
	Registered bool
	Online     bool
	LastSeen   time.Time
}

// ContributorPresenceSource is the optional second face of the nudge sink:
// live presence, re-read every sweep, so adoption reacts to a relay that went
// away AFTER the nudge instead of trusting the snapshot taken at nudge time.
type ContributorPresenceSource interface {
	Presence(ctx context.Context, login string) ContributorPresence
}

// Adoption reasons recorded on the store and in the adoption marker.
const (
	AdoptReasonOffline      = "contributor-offline"
	AdoptReasonUnregistered = "contributor-unregistered"
	AdoptReasonGrace        = "grace-elapsed"
	// AdoptReasonDCORehome marks a second takeover: an in-place adoption
	// whose head stayed DCO-red with unsigned commits the hive cannot
	// rewrite on the contributor branch, re-homed onto hive/adopt-<n>.
	AdoptReasonDCORehome = "dco-rehome"
	// AdoptReasonSignoffRehome marks a second takeover of an in-place
	// adoption whose head carries a hive commit the squash merge cannot
	// attribute (its Signed-off-by names the App, the squash lands as the
	// contributor): the hive cannot deliver a signed commit to the
	// contributor branch, so the work re-homes onto hive/adopt-<n>
	// (hivecommons/hive#12858).
	AdoptReasonSignoffRehome = "signoff-rehome"
	// AdoptReasonInPlaceStalled marks a second takeover of an in-place
	// adoption whose head sat past the 074 dwell with a blocking review
	// finding the in-place lane (which repairs CI) never acted on.
	AdoptReasonInPlaceStalled = "in-place-stalled"
	// AdoptReasonRebaseRehome gives idle rebase work a hive-owned fix branch.
	AdoptReasonRebaseRehome = "rebase-rehome"
	// AdoptReasonPushRehome recovers an adoption that lost fork push access.
	AdoptReasonPushRehome = "push-rehome"
)

// AdoptionStalledMarker opens the one note posted when an adopted PR is
// handed to a person (stage 085) for sitting past the 074 dwell.
const AdoptionStalledMarker = "<!-- hive:adoption-stalled"

// AdoptionWorkflowRefusedMarker opens the one note posted when the hive
// refuses to publish a head carrying workflow / action edits upstream.
const AdoptionWorkflowRefusedMarker = "<!-- hive:adoption-workflow-refused"

// adoptionWorkflowRefusedReason is the stage-085 reason for that refusal. It
// is terminal for the head: the escalation holds until the head moves.
const adoptionWorkflowRefusedReason = stagelabels.LaneReasonAdoptionStalledPrefix + contributorLaneReasonWorkflowPath

// contributorPresence asks the sink for live presence; ok is false when the
// sink does not provide it.
func (c *Engine) contributorPresence(ctx context.Context, login string) (ContributorPresence, bool) {
	src, ok := c.contributorNudge.(ContributorPresenceSource)
	if !ok || src == nil || strings.TrimSpace(login) == "" {
		return ContributorPresence{}, false
	}
	return src.Presence(ctx, login), true
}

// adoptionDue reports whether the PR at head has sat nudged long enough with
// no new head, and was not already adopted at this head; reason says which
// rule fired. With live presence: a registered relay that is online keeps
// the online grace (its hive may be mid-fix); a registered relay that is
// offline is adopted once it has been gone AdoptAfterOffline (counted from
// its last heartbeat or the first nudge, whichever is later); an author with
// no profile here (another hive posted the PR) is adopted after
// AdoptAfterUnregistered. Without presence the nudge-time graces apply.
func adoptionDue(rec stagelabels.Record, head string, policy StageLabelPolicy, now time.Time, presence ContributorPresence, hasPresence bool) (bool, string) {
	start := rec.NudgeStart()
	if head == "" || rec.NudgeHead != head || rec.NudgeCount == 0 || start.IsZero() {
		return false, ""
	}
	if rec.Adopted(head) || rec.AdoptedInPlace() {
		return false, ""
	}
	if !hasPresence {
		return now.Sub(start) >= adoptionGrace(rec, policy), AdoptReasonGrace
	}
	switch {
	case presence.Registered && presence.Online:
		grace := policy.GraceOnline
		if grace <= 0 {
			grace = 2 * time.Hour
		}
		return now.Sub(start) >= grace, AdoptReasonGrace
	case presence.Registered:
		wait := policy.AdoptAfterOffline
		if wait <= 0 {
			wait = 15 * time.Minute
		}
		since := start
		if presence.LastSeen.After(since) {
			since = presence.LastSeen
		}
		return now.Sub(since) >= wait, AdoptReasonOffline
	default:
		wait := policy.AdoptAfterUnregistered
		if wait <= 0 {
			wait = 15 * time.Minute
		}
		return now.Sub(start) >= wait, AdoptReasonUnregistered
	}
}

// adoptReasonText is the human-readable half of the adoption note.
func adoptReasonText(reason string, grace time.Duration, online bool) string {
	switch reason {
	case AdoptReasonOffline:
		return "your hive has been offline since the nudge"
	case AdoptReasonUnregistered:
		return "no hive of yours is registered here and nothing moved after the nudge"
	case AdoptReasonSignoffRehome:
		return "the hive's fix on this branch cannot carry a sign-off the squash merge accepts (the hive signs as the App, the squash lands as you)"
	case AdoptReasonRebaseRehome:
		return "the adopted branch still needs a rebase after the fix window"
	case AdoptReasonPushRehome:
		return "the hive can no longer push to the contributor branch"
	case AdoptReasonInPlaceStalled:
		return "the review swarm's finding on this head went unaddressed past the stage dwell and the in-place fix lane only repairs CI"
	}
	if online {
		return fmt.Sprintf("CI stayed red for %s after the nudge and the relay nudge went unanswered", formatGrace(grace))
	}
	return fmt.Sprintf("CI stayed red for %s after the nudge and no relay session was reachable", formatGrace(grace))
}

// adoptionBlocked reports a label that forbids adoption (the hive must not
// take over a PR a human parked).
func adoptionBlocked(labels []string) string {
	for _, l := range labels {
		switch strings.ToLower(strings.TrimSpace(l)) {
		case "hold", "do-not-merge", "do-not-merge/hold":
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// adoptionDiffRefusal reads the contributor lane's diff fences (CI
// workflow/action definitions, test deletion) for the head about to be
// adopted. It returns the contributor-lane reason, or "" when the diff reads
// clean. A fence verdict does not refuse adoption — adoption is not a merge;
// it is recorded and carried so the merge stays manual
// (automerge_adoption_fence.go) — but a verdict the fence could not reach
// does defer it (adoptionFenceDefers): the files API answers for the PR's
// current head while adoption acts on obs.HeadSHA, so a head that moved
// since the lane recorded it is reported as head-changed-since-eval rather
// than judged by another head's diff; the fetched PR also carries
// changed_files, so an incomplete list (more files than the API will ever
// return) is a standing files-evidence verdict with a nil error. A transport
// failure reports files-evidence with the error set, so callers can tell a
// transient read fault from a verdict; both are re-read on the next sweep.
func (c *Engine) adoptionDiffRefusal(ctx context.Context, obs stageObservation) (string, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, obs.Owner, obs.Repo, obs.Number)
	if err != nil {
		return contributorLaneReasonFilesEvidence, err
	}
	if head := pr.GetHead().GetSHA(); head == "" || head != obs.HeadSHA {
		return adoptionReasonHeadChanged, nil
	}
	files, err := ListChangedFiles(ctx, c.gh, obs.Owner, obs.Repo, pr)
	if errors.Is(err, hgithub.ErrIncompletePRFileList) {
		return contributorLaneReasonFilesEvidence, nil
	}
	if err != nil {
		return contributorLaneReasonFilesEvidence, err
	}
	return contributorLaneDiffReason(files), nil
}

// adoptionReasonHeadChanged is adoptionDiffRefusal's verdict for a PR whose
// head moved after the lane observed it.
const adoptionReasonHeadChanged = "head-changed-since-eval"

// adoptionCloses extracts the closing-keyword lines from the original body.
func adoptionCloses(body string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" || seen[strings.ToLower(line)] {
			return
		}
		seen[strings.ToLower(line)] = true
		out = append(out, line)
	}
	for _, m := range adoptClosesPattern.FindAllString(body, -1) {
		add(m)
	}
	for _, m := range adoptHiveRunPattern.FindAllStringSubmatch(body, -1) {
		if len(m) == 2 {
			add("Fixes #" + m[1])
		}
	}
	return out
}

func adoptionNeedsDCORehome(obs stageObservation, canPushInPlace bool) bool {
	return adoptionDCORepairMode(obs, canPushInPlace) == stagelabels.AdoptModeSupersede
}

func adoptionDCORepairMode(obs stageObservation, canPushInPlace bool) string {
	if obs.Detail.DCO != stagelabels.DCONo || obs.Detail.DCOUnsigned <= 0 {
		return ""
	}
	if canPushInPlace && obs.Detail.DCOUnsignedHive == obs.Detail.DCOUnsigned && obs.Detail.DCOUnsignedOther == 0 {
		return stagelabels.AdoptModeInPlace
	}
	return stagelabels.AdoptModeSupersede
}

// adoptedHeadDCORed reports whether an in-place-adopted PR's head carries
// unsigned commits at all (the cheap pre-check before resolving push access).
func adoptedHeadDCORed(obs stageObservation, rec stagelabels.Record) bool {
	return rec.AdoptedInPlace() && obs.Detail.DCO == stagelabels.DCONo && obs.Detail.DCOUnsigned > 0
}

// adoptedHeadNeedsDCORehome decides, for an in-place-adopted PR, whether the
// unsigned commits can be rewritten on the contributor branch (all App-linked
// and the branch still pushable) or the work must be re-homed.
func adoptedHeadNeedsDCORehome(obs stageObservation, rec stagelabels.Record, canPushInPlace bool) bool {
	return adoptedHeadDCORed(obs, rec) && adoptionDCORepairMode(obs, canPushInPlace) == stagelabels.AdoptModeSupersede
}

func adoptionRehomeCommitMessage(obs stageObservation) string {
	title := strings.TrimSpace(obs.Title)
	if title == "" {
		title = fmt.Sprintf("Adopt #%d", obs.Number)
	}
	var b strings.Builder
	b.WriteString(title)
	fmt.Fprintf(&b, "\n\nSupersedes #%d (adopted; re-pushed with DCO sign-off, original authorship preserved).\n", obs.Number)
	for _, c := range adoptionCloses(obs.Body) {
		b.WriteString(c)
		b.WriteString("\n")
	}
	author := strings.TrimSpace(obs.Author)
	if author != "" {
		fmt.Fprintf(&b, "\nCredit: original work by @%s.\n", author)
	}
	return strings.TrimSpace(b.String())
}

func AdoptionDCORehomeComment(obs stageObservation, hiveID string, superseding int) string {
	return fmt.Sprintf("%s by=%s reason=%s mode=%s superseded_by=%d head=%s -->\n@%s this adopted PR still has unsigned commits, so the hive is re-homing it as #%d on a hive-owned DCO-clean branch. Closing this PR so DCO does not stall the adopted work in place.",
		AdoptionMarkerPrefix, nonEmpty(hiveID, "hive"), AdoptReasonDCORehome, stagelabels.AdoptModeSupersede, superseding, obs.HeadSHA, obs.Author, superseding)
}

// AdoptionPRBody is the superseding App PR's body.
func AdoptionPRBody(obs stageObservation, rec stagelabels.Record, dcoRehome bool) string {
	var b strings.Builder
	if rec.AdoptDiffFence != "" {
		// Read back by carriedFenceGate: the App lanes never merge this
		// PR unattended, whoever pushes to it.
		b.WriteString(CarriedFenceMarker(rec.AdoptDiffFence, obs.Number))
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Supersedes #%d (%s; original commits by @%s preserved).\n\n", obs.Number, adoptionBodyCause(rec.AdoptReason), obs.Author)
	for _, c := range adoptionCloses(obs.Body) {
		b.WriteString(c)
		b.WriteString("\n")
	}
	if len(adoptionCloses(obs.Body)) > 0 {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "This branch is #%d's head `%s` as pushed by @%s", obs.Number, shortSHA(obs.HeadSHA), obs.Author)
	if obs.HeadRepo != "" && !strings.EqualFold(obs.HeadRepo, obs.Display) {
		fmt.Fprintf(&b, " (fork `%s`)", obs.HeadRepo)
	}
	b.WriteString(", unchanged. Last lane reason on the original: `")
	b.WriteString(nonEmpty(rec.Reason, obs.Reason))
	b.WriteString("`.\n\n")
	b.WriteString("**Hive fix agent:** rebase this branch on `")
	b.WriteString(nonEmpty(obs.BaseRef, "the base branch"))
	b.WriteString("`, fix the failing checks / review asks, and push to THIS branch. Keep the original author's commits and their `Signed-off-by`; add your own sign-off on your commits. Do not open another PR.\n")
	if rec.AdoptDiffFence != "" {
		fmt.Fprintf(&b, "\n**Merge stays manual** (`%s`, carried from #%d): %s. The hive repairs CI here but will not merge this PR; a maintainer reviews and merges it once green. Do not remove the `%s` marker.\n", rec.AdoptDiffFence, obs.Number, nonEmpty(decisionCause(rec.AdoptDiffFence), "the contributor lane's diff fence tripped"), CarriedFenceMarkerPrefix)
	}
	if verdictBlocks(obs.Detail) {
		fmt.Fprintf(&b, "\n**Open review finding carried over from #%d:** the review swarm's latest verdict on `%s` requested changes", obs.Number, shortSHA(obs.HeadSHA))
		if obs.Detail.VerdictKnown && strings.TrimSpace(obs.Detail.Verdict.Verdict) != "" {
			fmt.Fprintf(&b, " (`%s`)", strings.ToLower(strings.TrimSpace(obs.Detail.Verdict.Verdict)))
		}
		fmt.Fprintf(&b, "; read the swarm's review on #%d, address it here, and let the swarm re-review this PR. Green CI alone is not done.\n", obs.Number)
	}
	if rec.AdoptReason == AdoptReasonSignoffRehome {
		b.WriteString("\n**Sign-off attribution:** the original PR would squash the App's signed fixes as the contributor. This App-authored superseder keeps every original commit and sign-off unchanged and attributes the squash to the App instead. DCO and all merge gates still apply here.\n")
	}
	if dcoRehome {
		b.WriteString("\n**DCO repair required:** this adoption has unsigned non-merge commits and the hive cannot repair them by amending the contributor branch. Re-home the work onto a hive-owned branch from `")
		b.WriteString(nonEmpty(obs.BaseRef, "the base branch"))
		b.WriteString("`: cherry-pick each original commit with `git cherry-pick -s` (or squash with `git commit --author` preserving the original author), ensure every replacement commit has `Signed-off-by`, push the DCO-clean branch, open/keep one superseding PR with the same title, then comment on and close #")
		b.WriteString(strconv.Itoa(obs.Number))
		b.WriteString(". Suggested replacement commit message:\n\n```text\n")
		b.WriteString(adoptionRehomeCommitMessage(obs))
		b.WriteString("\n```\n")
	}
	return b.String()
}

// adoptionBodyCause is the parenthetical opening the superseding PR body.
func adoptionBodyCause(reason string) string {
	switch reason {
	case AdoptReasonSignoffRehome:
		return "adopted in place, re-homed because a hive commit on the contributor branch cannot carry a squash-safe sign-off"
	case AdoptReasonRebaseRehome:
		return "adopted in place, re-homed to finish the rebase on a hive-owned branch"
	case AdoptReasonPushRehome:
		return "adopted in place, re-homed because the contributor branch is no longer pushable"
	case AdoptReasonInPlaceStalled:
		return "adopted in place, re-homed because a review finding on the adopted head sat unaddressed past the stage dwell"
	case AdoptReasonDCORehome:
		return "adopted in place, re-homed for DCO repair"
	}
	return "adopted after CI went red and the contributor was unavailable"
}

// AdoptionStalledComment is the one note left on an adopted PR handed to a
// person: it names the lane reason the hive was stuck on, so nobody reads
// "waiting on a maintainer" into a PR the hive still owes a fix.
func AdoptionStalledComment(obs stageObservation, rec stagelabels.Record, reason string, sat time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s head=%s reason=%s -->\n", AdoptionStalledMarker, obs.HeadSHA, reason)
	fmt.Fprintf(&b, "The hive adopted this PR (`%s`) but its head `%s` has not moved for %s and the hive has nothing it can push: `%s`.\n\n", stagelabels.AdoptedByHive.Label(), shortSHA(obs.HeadSHA), formatSat(sat), reason)
	b.WriteString("**Blocker:** ")
	b.WriteString(adoptionBlockerText(obs, rec))
	b.WriteString("\n\nMoving to `")
	b.WriteString(stagelabels.NeedsDecision.Label())
	b.WriteString("` so a person sees it. This is not a merge-approval wait: the lane has not admitted this head.")
	return b.String()
}

// AdoptionWorkflowRefusedComment is the one note left on a PR whose head the
// hive will not publish upstream: supersession pushes it to a branch in the
// base repository, where its workflow / action edits would run with
// repository context (hivecommons/hive#14055).
func AdoptionWorkflowRefusedComment(obs stageObservation) string {
	return fmt.Sprintf("%s head=%s reason=%s -->\n"+
		"The hive will not publish head `%s` upstream: it edits GitHub workflow / action files, which would run with this repository's context once pushed to a branch here. "+
		"The hive will not re-home or supersede this head.\n\n"+
		"**Ask:** a maintainer pushes the fix to this PR's branch and merges it, or cherry-picks the change onto a maintainer-owned branch. "+
		"Moving to `%s` so a person sees it.",
		AdoptionWorkflowRefusedMarker, obs.HeadSHA, adoptionWorkflowRefusedReason, shortSHA(obs.HeadSHA), stagelabels.NeedsDecision.Label())
}

// mergeCommitRefusalText is the ask in the hive-signoff-attribution stall
// note: why the hive did not select a merge commit itself
// (stageDetail.MergeCommitRefused) and what a person can do.
func mergeCommitRefusalText(obs stageObservation, author string) string {
	var b strings.Builder
	switch obs.Detail.MergeCommitRefused {
	case mergeCommitRefusedSquashOnly:
		fmt.Fprintf(&b, "Every commit already carries a sign-off matching its own author, so a merge commit would land them without rewriting history, but `%s` is squash-only (`auto_merge.squash_only`), so the hive did not select one. ", obs.Display)
		fmt.Fprintf(&b, "**Ask:** either lift the squash-only policy for this repo (the hive then merges with a merge commit on its next sweep) or @%s re-authors the hive's commits under their own sign-off and pushes a new head.", author)
	case mergeCommitRefusedRole:
		fmt.Fprintf(&b, "The hive selects a merge commit for this shape only on a `%s` PR; this one is not, so it will not merge it unattended. ", stagelabels.RoleTrusted.Label())
		fmt.Fprintf(&b, "**Ask:** every commit already carries a sign-off matching its own author, so a maintainer can merge this PR with a merge commit (`gh pr merge %d --merge`, not squash) and DCO holds without rewriting history; otherwise @%s re-authors the hive's commits under their own sign-off and pushes a new head.", obs.Number, author)
	case mergeCommitRefusedHiveOnly:
		fmt.Fprintf(&b, "Every non-merge commit on the head is the hive's, so there is no contributor authorship for a merge commit to preserve. **Ask:** a maintainer merges with a merge commit (`gh pr merge %d --merge`, not squash), or @%s pushes a head of their own.", obs.Number, author)
	default:
		fmt.Fprintf(&b, "**Ask:** every commit already carries a sign-off matching its own author, so a maintainer can merge this PR with a merge commit (`gh pr merge %d --merge`, not squash) and DCO holds without rewriting history; otherwise @%s re-authors the hive's commits under their own sign-off and pushes a new head.", obs.Number, author)
	}
	return b.String()
}

// adoptionBlockerText spells out what the lane reason stands for.
func adoptionBlockerText(obs stageObservation, rec stagelabels.Record) string {
	switch {
	case adoptedHeadSignoffUnfixable(obs, rec):
		author := nonEmpty(obs.Author, "the contributor")
		msg := fmt.Sprintf("%d commit(s) on `%s` carry the App's `Signed-off-by` while a squash merge would land them as @%s, so the `Sign-off survives the squash` gate stays red. "+
			"The normal recovery is an App-authored superseding PR, preserving the original commits and sign-offs while changing squash attribution. ",
			obs.Detail.DCOSquashUnsafeHive, shortSHA(obs.HeadSHA), author)
		switch {
		case adoptionBlocked(obs.Labels) != "":
			msg += fmt.Sprintf("The `%s` label prevents superseding; clear it to let the hive retry on its next sweep. ", adoptionBlocked(obs.Labels))
		case verdictNeedsHuman(obs.Detail):
			msg += "The review swarm requires a human decision before superseding; resolve that verdict to let the hive retry on its next sweep. "
		case obs.HeadSHA != rec.AdoptedHead:
			msg += "The observed head differs from the adopted head; the hive must settle ownership of the new head before superseding. "
		default:
			msg += "If the write failed, restore branch/PR-create access and retry adoption; do not forge the contributor's sign-off. "
		}
		msg += mergeCommitRefusalText(obs, author)
		return msg
	case verdictBlocks(obs.Detail):
		msg := fmt.Sprintf("the review swarm's latest verdict on `%s` is `%s` and no fix landed", shortSHA(obs.HeadSHA), strings.ToLower(strings.TrimSpace(obs.Detail.Verdict.Verdict)))
		if n := len(obs.Detail.Verdict.HumanSummaries); n > 0 {
			msg += ": " + strings.Join(obs.Detail.Verdict.HumanSummaries, "; ")
		}
		return msg + "."
	case verdictNeedsHuman(obs.Detail):
		msg := "the review swarm asked for a human decision"
		if q := strings.TrimSpace(obs.Detail.Verdict.DecisionQuestion); q != "" {
			msg += ": " + q
		}
		return msg + "."
	case rec.AdoptionAttemptedNothingOpened(obs.HeadSHA):
		return fmt.Sprintf("the hive tried to re-home this PR onto `%s%d` (`%s`) and could not open the superseding PR.", adoptBranchPrefix, obs.Number, rec.AdoptReason)
	}
	if name := obs.Detail.RedCheck(obs.HeadSHA); name != "" {
		return fmt.Sprintf("check `%s` is red on this head and no fix landed.", name)
	}
	if strings.HasPrefix(obs.LaneReason, "required-check-failing:") || strings.HasPrefix(obs.LaneReason, "check-failing:") {
		return fmt.Sprintf("`%s` on this head and no fix landed.", obs.LaneReason)
	}
	return fmt.Sprintf("lane reason `%s` on this head and nothing changed.", nonEmpty(obs.LaneReason, obs.Reason))
}

// AdoptionComment is the one note left on the original PR when adopted by
// supersession.
func AdoptionComment(obs stageObservation, hiveID string, superseding int, reason string, grace time.Duration, online bool) string {
	return fmt.Sprintf("%s by=%s reason=%s mode=%s superseded_by=%d head=%s -->\n@%s the hive has adopted this PR (`%s`): %s. "+
		"It is being finished as #%d, which carries your commits (and sign-off) unchanged; the hive will rebase and fix CI there. "+
		"This PR is closed automatically once #%d merges. If you would rather finish it yourself, push a new head here and say so on #%d.",
		AdoptionMarkerPrefix, nonEmpty(hiveID, "hive"), reason, stagelabels.AdoptModeSupersede, superseding, obs.HeadSHA, obs.Author, stagelabels.AdoptedByHive.Label(), adoptReasonText(reason, grace, online), superseding, superseding, superseding)
}

// AdoptionInPlaceComment is the one note left when the hive adopts the PR on
// its own branch.
func AdoptionInPlaceComment(obs stageObservation, hiveID, reason string, grace time.Duration, online bool) string {
	return fmt.Sprintf("%s by=%s reason=%s mode=%s head=%s -->\n@%s the hive has adopted this PR (`%s`): %s. "+
		"The hive will rebase this branch on `%s` and fix CI here; your commits and sign-off stay. "+
		"Push a new head yourself at any time and the hive yields it back to you.",
		AdoptionMarkerPrefix, nonEmpty(hiveID, "hive"), reason, stagelabels.AdoptModeInPlace, obs.HeadSHA, obs.Author, stagelabels.AdoptedByHive.Label(), adoptReasonText(reason, grace, online), nonEmpty(obs.BaseRef, "the base branch"))
}

// AdoptionYieldMarker opens the note posted when the contributor pushes over
// an in-place adoption.
const AdoptionYieldMarker = "<!-- hive:adoption-yield"

// AdoptionYieldComment is that note.
func AdoptionYieldComment(obs stageObservation) string {
	return fmt.Sprintf("%s head=%s -->\n@%s this PR has a new head without confirmed hive push evidence, so the hive has handed this PR back to you (`%s` → `%s`). It will nudge again if CI stays red.",
		AdoptionYieldMarker, obs.HeadSHA, obs.Author, stagelabels.AdoptedByHive.Label(), stagelabels.ReturnedToContributor.Label())
}

func formatGrace(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return d.Round(time.Minute).String()
}

// formatSat renders how long a head has sat: whole hours as "3h", else
// minute-rounded without the trailing seconds ("3h1m", "15m").
func formatSat(d time.Duration) string {
	if d < time.Minute {
		d = time.Minute
	}
	return strings.TrimSuffix(formatGrace(d), "0s")
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// adoptContributorPR runs the stage-074 check for one clanker PR in stage
// 072 and, when adoption is due, performs it: in place (the hive's fix lane
// pushes to the PR's own branch) when GitHub permits and the policy allows,
// else by supersession. It returns the number of comments posted. Failures
// are logged and the attempt is still recorded (one per head) so a broken
// API never loops on the same PR.
func (c *Engine) adoptContributorPR(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, presence ContributorPresence, hasPresence bool, now time.Time) (int, error) {
	rec, ok := store.Get(key)
	if !ok || obs.Lane == stageLaneSelf || obs.Author == "" || obs.BaseRef == "" {
		return 0, nil
	}
	requested := hasLabel(obs.Labels, delegatedecision.LabelAdoptSupersede)
	due, reason := adoptionDue(rec, obs.HeadSHA, policy, now, presence, hasPresence)
	if requested && obs.HeadSHA != "" && !(rec.Adopted(obs.HeadSHA) && rec.AdoptedBy > 0) {
		// An explicit owner-delegate decision bypasses the nudge/grace and
		// re-homes a stalled in-place adoption through the same supersede path.
		due, reason = true, AdoptReasonOffline
	}
	if !due {
		return 0, nil
	}
	if l := adoptionBlocked(obs.Labels); l != "" {
		c.info("stage labels: adoption due but blocked by label", "repo", obs.Display, "pr", obs.Number, "label", l)
		return 0, nil
	}
	if maintainer, err := c.authorMaintainsRepo(ctx, obs, now); maintainer || err != nil {
		c.info("stage labels: adoption due but the author maintains the repository; not adopting", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "deferred", err != nil)
		return 0, nil
	}
	fence, err := c.adoptionDiffRefusal(ctx, obs)
	if fence != "" && (err != nil || adoptionFenceDefers(fence)) {
		// The fence could not judge this head (it moved, or its file list
		// is incomplete / unreadable). Not recorded as an attempt: the PR
		// stays on its 072 route and the next sweep re-reads.
		c.info("stage labels: adoption due but deferred, diff fence unreadable", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "reason", fence, "error", err)
		return 0, nil
	}
	if fence != "" {
		// A fenced diff is adopted all the same — adoption is not a merge.
		// The fence travels with the head (record + superseding PR body) so
		// the merge stays a person's (automerge_adoption_fence.go).
		c.info("stage labels: adopting contributor PR despite diff fence; merge stays manual", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "fence", fence)
	}
	grace := adoptionGrace(rec, policy)
	appLogin := strings.TrimSpace(c.transport.AppBotLogin())
	canPushInPlace := false
	if policy.AdoptInPlace && !requested {
		canPushInPlace = c.canPushToPRBranch(ctx, obs)
	}
	dcoRehome := adoptionNeedsDCORehome(obs, canPushInPlace)
	// Record the attempt first: whatever happens below, this head is tried once.
	store.MarkAdopted(key, obs.HeadSHA, 0, now)
	store.NoteAdoptDiffFence(key, fence)
	rec.AdoptDiffFence = fence
	if fence == contributorLaneReasonWorkflowPath && !(policy.AdoptInPlace && canPushInPlace && !dcoRehome) {
		// A terminal verdict, not a deferral: supersession would publish
		// the workflow edits upstream. The recorded attempt clears a
		// delegated request; the head goes to a person (085) with one note.
		store.NoteAdoptReason(key, reason)
		// A note for another reason (a stalled adoption) does not explain
		// the refusal of a maintainer's explicit request, so it still posts.
		noted := rec.AdoptionEscalationNoted(obs.HeadSHA) && !(requested && rec.AdoptEscalatedReason != adoptionWorkflowRefusedReason)
		store.MarkAdoptionEscalated(key, obs.HeadSHA, adoptionWorkflowRefusedReason)
		c.warn("stage labels: refusing workflow-path supersession; escalating to needs-decision", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "reason", reason, "noted", noted)
		if noted {
			return 0, nil
		}
		if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(AdoptionWorkflowRefusedComment(obs))}); err != nil {
			c.warn("stage labels: workflow-path refusal note failed", "repo", obs.Display, "pr", obs.Number, "error", err)
			return 0, err
		}
		return 1, nil
	}
	if appLogin == "" {
		c.warn("stage labels: cannot adopt contributor PR, App login unknown", "repo", obs.Display, "pr", obs.Number)
		return 0, nil
	}
	c.info("stage labels: adopting contributor PR", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "reason", reason, "registered", presence.Registered, "online", presence.Online, "lane_reason", obs.Reason, "fence", fence)

	if policy.AdoptInPlace && canPushInPlace && !dcoRehome {
		store.MarkAdoptedInPlace(key, obs.HeadSHA, reason, now)
		body := AdoptionInPlaceComment(obs, policy.HiveID, reason, grace, rec.NudgeOnline) + adoptionFenceNote(fence)
		if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			c.warn("stage labels: adoption comment failed", "repo", obs.Display, "pr", obs.Number, "error", err)
			return 0, err
		}
		c.info("stage labels: contributor PR adopted in place", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "reason", reason, "fence", fence)
		return 1, nil
	}

	return c.supersedeAdoptedPR(ctx, store, key, obs, rec, policy, reason, grace, rec.NudgeOnline, dcoRehome, now)
}

// delegatedAdoptionAttempted reports whether adoptContributorPR recorded an
// attempt between the two snapshots: MarkAdopted stamps head/by/at before
// any write, so a changed adoption record means this head was tried.
func delegatedAdoptionAttempted(before, after stagelabels.Record) bool {
	return after.AdoptedHead != before.AdoptedHead ||
		after.AdoptedBy != before.AdoptedBy ||
		!after.AdoptedAt.Equal(before.AdoptedAt) ||
		after.AdoptMode != before.AdoptMode
}

func (c *Engine) supersedeAdoptedPR(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, rec stagelabels.Record, policy StageLabelPolicy, reason string, grace time.Duration, online bool, dcoRehome bool, now time.Time) (int, error) {
	maintainer, err := c.authorMaintainsRepo(ctx, obs, now)
	if err != nil {
		return 0, nil
	}
	if maintainer {
		// Backstop for every re-home path (dco-rehome, in-place-stalled):
		// a maintainer's PR is never closed in favour of a hive copy.
		c.info("stage labels: not superseding a repository maintainer's PR; adoption released", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "reason", reason)
		return c.releaseMaintainerAdoption(ctx, store, key, obs, rec.AdoptedBy)
	}
	owner, repo, number := obs.Owner, obs.Repo, obs.Number
	rec.AdoptReason = reason
	if stored, ok := store.Get(key); ok && stored.AdoptDiffFence != "" {
		// A second takeover (dco-rehome, signoff-rehome, in-place-stalled)
		// carries the fence the first adoption recorded.
		rec.AdoptDiffFence = stored.AdoptDiffFence
	}
	// Supersession publishes the head in the upstream repository, where
	// pull_request workflows run with repository context. A manual-merge
	// marker cannot protect that earlier execution. Re-read evidence here
	// so every takeover path checks the exact head before any ref mutation.
	fence, err := c.adoptionDiffRefusal(ctx, obs)
	if err != nil || adoptionFenceDefers(fence) || fence == contributorLaneReasonWorkflowPath {
		c.info("stage labels: supersession deferred before publishing unsafe or unreadable head", "repo", obs.Display, "pr", number, "head", obs.HeadSHA, "reason", fence, "error", err)
		return 0, err
	}
	branch := adoptBranchPrefix + strconv.Itoa(number)
	fullRepo := owner + "/" + repo
	_, err = effects.Execute(ctx, c.mutation, effects.Claim{
		Repo: fullRepo, Kind: effects.KindBranchPush, Target: branch, Actor: "stage_labels",
		Inputs: map[string]string{"pr": strconv.Itoa(number), "head_sha": obs.HeadSHA, "adopt": "true"},
	}, func(ctx context.Context) (effects.Result, error) {
		_, _, apiErr := c.gh.Git.CreateRef(ctx, owner, repo, &gh.Reference{
			Ref:    gh.Ptr("refs/heads/" + branch),
			Object: &gh.GitObject{SHA: gh.Ptr(obs.HeadSHA)},
		})
		if apiErr != nil && isGitHubStatus(apiErr, http.StatusUnprocessableEntity) {
			// The branch exists from an earlier attempt: point it at this head.
			_, _, apiErr = c.gh.Git.UpdateRef(ctx, owner, repo, &gh.Reference{
				Ref:    gh.Ptr("refs/heads/" + branch),
				Object: &gh.GitObject{SHA: gh.Ptr(obs.HeadSHA)},
			}, true)
		}
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		return effects.Result{Provenance: obs.HeadSHA}, nil
	})
	if err != nil {
		c.warn("stage labels: adoption failed to push branch", "repo", obs.Display, "pr", number, "branch", branch, "error", err)
		return 0, err
	}

	var created *gh.PullRequest
	_, err = effects.Execute(ctx, c.mutation, effects.Claim{
		Repo: fullRepo, Kind: effects.KindPullRequestCreate, Target: branch, Actor: "stage_labels",
		Inputs: map[string]string{"base": obs.BaseRef, "supersedes": strconv.Itoa(number), "head_sha": obs.HeadSHA},
	}, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		created, _, apiErr = c.gh.PullRequests.Create(ctx, owner, repo, &gh.NewPullRequest{
			Title: gh.Ptr(nonEmpty(strings.TrimSpace(obs.Title), fmt.Sprintf("Adopt #%d", number))),
			Head:  gh.Ptr(branch),
			Base:  gh.Ptr(obs.BaseRef),
			Body:  gh.Ptr(AdoptionPRBody(obs, rec, dcoRehome)),
		})
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		return effects.Result{Provenance: created.GetHTMLURL()}, nil
	})
	if err != nil || created == nil {
		c.warn("stage labels: adoption failed to open App PR", "repo", obs.Display, "pr", number, "branch", branch, "error", err)
		return 0, err
	}
	superseding := created.GetNumber()
	store.MarkAdopted(key, obs.HeadSHA, superseding, now)
	c.info("stage labels: contributor PR adopted", "repo", obs.Display, "pr", number, "head", obs.HeadSHA, "superseded_by", superseding, "branch", branch, "fence", rec.AdoptDiffFence)

	body := AdoptionComment(obs, policy.HiveID, superseding, reason, grace, online)
	if dcoRehome {
		body = AdoptionDCORehomeComment(obs, policy.HiveID, superseding)
	}
	body += adoptionFenceNote(rec.AdoptDiffFence)
	if _, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		c.warn("stage labels: adoption comment failed", "repo", obs.Display, "pr", number, "superseded_by", superseding, "error", err)
		return 0, err
	}
	if dcoRehome {
		if _, _, err := c.gh.PullRequests.Edit(ctx, owner, repo, number, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil {
			c.warn("stage labels: closing DCO re-homed PR failed", "repo", obs.Display, "pr", number, "superseded_by", superseding, "error", err)
			return 1, err
		}
	}
	return 1, nil
}

// settleAdoption polls the superseding App PR for a PR in stage 074 (at
// most every adoptCheckInterval): merged → close the original with a note;
// closed unmerged → forget the adoption so the original returns to 072.
func (c *Engine) settleAdoption(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, now time.Time) (int, error) {
	rec, ok := store.Get(key)
	if !ok {
		return 0, nil
	}
	maintainer, err := c.authorMaintainsRepo(ctx, obs, now)
	if err != nil {
		// No evidence either way: neither re-home nor release this pass.
		return 0, nil
	}
	if maintainer {
		// Supersede adoption taken before this rule, or before the
		// author's permission was raised: hand the PR back.
		c.info("stage labels: adoption of a repository maintainer's PR released", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "superseded_by", rec.AdoptedBy)
		return c.releaseMaintainerAdoption(ctx, store, key, obs, rec.AdoptedBy)
	}
	if adoptedHeadDCORed(obs, rec) {
		canPushInPlace := c.canPushToPRBranch(ctx, obs)
		if adoptedHeadNeedsDCORehome(obs, rec, canPushInPlace) {
			fence, err := c.adoptionDiffRefusal(ctx, obs)
			if fence != "" && (err != nil || adoptionFenceDefers(fence)) {
				// Not a verdict: the head moved, or its file list is
				// incomplete / unreadable. Keep the adoption so the next
				// sweep re-reads, rather than releasing it and re-adopting
				// with a duplicate comment.
				c.warn("stage labels: DCO re-home deferred, diff fence unreadable", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "reason", fence, "error", err)
				return 0, nil
			}
			if fence == contributorLaneReasonWorkflowPath {
				// Terminal for this head: refuse publication, keep the
				// in-place adoption and hand the re-home to a person (085).
				store.NoteAdoptDiffFence(key, fence)
				noted := rec.AdoptionEscalationNoted(obs.HeadSHA)
				store.MarkAdoptionEscalated(key, obs.HeadSHA, adoptionWorkflowRefusedReason)
				c.warn("stage labels: refusing workflow-path DCO re-home; escalating to needs-decision", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "noted", noted)
				if noted {
					return 0, nil
				}
				if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(AdoptionWorkflowRefusedComment(obs))}); err != nil {
					c.warn("stage labels: workflow-path refusal note failed", "repo", obs.Display, "pr", obs.Number, "error", err)
					return 0, err
				}
				return 1, nil
			}
			if fence != "" {
				// The fence travels into the superseding PR's body; the
				// self-authored lane hands that PR's merge to a person.
				c.info("stage labels: re-homing adopted PR despite diff fence; merge stays manual", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "fence", fence)
				store.NoteAdoptDiffFence(key, fence)
			}
			c.info("stage labels: re-homing adopted PR for DCO repair", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "unsigned", obs.Detail.DCOUnsigned, "unsigned_hive", obs.Detail.DCOUnsignedHive, "can_push", canPushInPlace)
			// Record the attempt first, as adoptContributorPR does: this
			// head is re-homed once, whatever the API says below.
			store.MarkAdopted(key, obs.HeadSHA, 0, now)
			store.NoteAdoptReason(key, AdoptReasonDCORehome)
			return c.supersedeAdoptedPR(ctx, store, key, obs, rec, policy, AdoptReasonDCORehome, 0, false, true, now)
		}
	}
	if rec.AdoptedBy <= 0 {
		return 0, nil
	}
	return c.settleSuperseder(ctx, store, key, obs, rec, now)
}

// settleSuperseder polls the superseding App PR recorded on an adoption (at
// most every adoptCheckInterval) and settles the original: merged → close
// note + close; closed unmerged → forget the adoption. It runs for a PR at
// 074 and, since hivecommons/hive#14471, for any other stage the lane
// derives while the superseder is on record: a `needs-human` label (085) or
// a contributor push after the adoption (072) used to stop the poll, and the
// original stayed open with "closed automatically once #N merges" on it.
func (c *Engine) settleSuperseder(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, rec stagelabels.Record, now time.Time) (int, error) {
	if obs.Merged || !rec.SupersederPollDue(now, adoptCheckInterval) {
		return 0, nil
	}
	store.MarkAdoptChecked(key, now)
	sup, _, err := c.gh.PullRequests.Get(ctx, obs.Owner, obs.Repo, rec.AdoptedBy)
	if err != nil {
		c.warn("stage labels: adoption superseder lookup failed", "repo", obs.Display, "pr", obs.Number, "superseded_by", rec.AdoptedBy, "error", err)
		return 0, err
	}
	switch {
	case sup.GetMerged():
		return c.closeSupersededOriginal(ctx, store, key, obs, rec, sup, now)
	case strings.EqualFold(sup.GetState(), "closed"):
		c.warn("stage labels: adoption superseder closed unmerged; returning PR to the contributor", "repo", obs.Display, "pr", obs.Number, "superseded_by", rec.AdoptedBy)
		store.ClearAdoption(key)
	}
	return 0, nil
}

// supersederCarriesHead reports whether the merged superseder landed the
// original's current head: the adoption took that head, or the superseder's
// own head is it (the fix lane re-pushed the contributor's later commits,
// as on hivecommons/hive#14403 → #14461).
func supersederCarriesHead(rec stagelabels.Record, obs stageObservation, sup *gh.PullRequest) bool {
	if obs.HeadSHA == "" {
		return true
	}
	return rec.Adopted(obs.HeadSHA) || strings.EqualFold(sup.GetHead().GetSHA(), obs.HeadSHA)
}

// AdoptionSupersederMergedDivergedMarker opens the one note left on an
// original whose superseder merged without its latest commits.
const AdoptionSupersederMergedDivergedMarker = "<!-- hive:adopted-merged-diverged"

// closeSupersededOriginal posts the close note on the original and closes
// it, recording the supersession on the store first so a failed or refused
// close never re-posts. A close GitHub refuses (403/404: the App cannot
// close this fork PR) leaves the original open with the note and the
// record marked SupersededOpen, which drops the activity monitor row.
func (c *Engine) closeSupersededOriginal(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, rec stagelabels.Record, sup *gh.PullRequest, now time.Time) (int, error) {
	if !supersederCarriesHead(rec, obs, sup) {
		if rec.SupersederMergedNotedHead == obs.HeadSHA {
			return 0, nil
		}
		store.MarkSupersederMergedNoted(key, obs.HeadSHA)
		body := fmt.Sprintf("%s superseded_by=%d head=%s -->\n#%d (the hive's adoption of this PR, with @%s's commits as of `%s`) has merged, but this PR has moved on to `%s` since. Not closing it automatically: if the later commits are already covered, close this PR; otherwise rebase them on `%s` for review.",
			AdoptionSupersederMergedDivergedMarker, rec.AdoptedBy, obs.HeadSHA, rec.AdoptedBy, obs.Author, shortSHA(rec.AdoptedHead), shortSHA(obs.HeadSHA), nonEmpty(obs.BaseRef, "the base branch"))
		if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			c.warn("stage labels: adoption diverged note failed", "repo", obs.Display, "pr", obs.Number, "superseded_by", rec.AdoptedBy, "error", err)
			return 0, err
		}
		c.warn("stage labels: adoption superseder merged but the original moved on; left open with a note", "repo", obs.Display, "pr", obs.Number, "superseded_by", rec.AdoptedBy, "adopted_head", rec.AdoptedHead, "head", obs.HeadSHA)
		return 1, nil
	}
	body := fmt.Sprintf("%s\nClosing: #%d (the hive's adoption of this PR, with @%s's commits) has merged. Thank you!", adoptionCloseMarker, rec.AdoptedBy, obs.Author)
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		c.warn("stage labels: adoption close note failed", "repo", obs.Display, "pr", obs.Number, "error", err)
		return 0, err
	}
	// The note is out: whatever the close below does, never post it again.
	store.MarkSuperseded(key, rec.AdoptedBy, true, now)
	if _, _, err := c.gh.PullRequests.Edit(ctx, obs.Owner, obs.Repo, obs.Number, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil {
		if isGitHubStatus(err, http.StatusForbidden) || isGitHubStatus(err, http.StatusNotFound) {
			c.warn("stage labels: cannot close superseded PR (permission); left open with the note, tracker row dropped", "repo", obs.Display, "pr", obs.Number, "superseded_by", rec.AdoptedBy, "error", err)
			return 1, nil
		}
		c.warn("stage labels: closing adopted PR failed", "repo", obs.Display, "pr", obs.Number, "error", err)
		return 1, err
	}
	store.MarkSuperseded(key, rec.AdoptedBy, false, now)
	c.info("stage labels: adopted PR closed after superseder merged", "repo", obs.Display, "pr", obs.Number, "superseded_by", rec.AdoptedBy)
	return 1, nil
}

// maintainerPermTTL is how long authorMaintainsRepo trusts a permission
// read: one GET per (repo, author) per sweep window, not per PR per sweep.
const maintainerPermTTL = 10 * time.Minute

type maintainerPermEntry struct {
	maintainer bool
	at         time.Time
}

// repoMaintainerLevel reports whether a collaborator permission answer
// (`permission`, and the finer `role_name`) is admin or maintain.
func repoMaintainerLevel(permission, role string) bool {
	permission, role = strings.ToLower(strings.TrimSpace(permission)), strings.ToLower(strings.TrimSpace(role))
	return permission == "admin" || role == "admin" || role == "maintain"
}

// authorMaintainsRepo reports whether the PR author holds admin or maintain
// on the PR's repository. Adoption stands in for a contributor's relay that
// went quiet; a maintainer owns the branch and merges it themselves, so the
// hive never takes their PR over (projectbluefin/utah-packages#619, adopted
// as contributor-offline 16 minutes after its nudge and re-homed). A failed
// read is not cached and returns its error: callers defer, so the hive
// neither takes a PR over nor releases an existing adoption without
// evidence. The cache lock covers map access only, never the API call.
func (c *Engine) authorMaintainsRepo(ctx context.Context, obs stageObservation, now time.Time) (bool, error) {
	if c == nil || c.gh == nil || obs.Author == "" || obs.Owner == "" || obs.Repo == "" {
		return false, nil
	}
	key := strings.ToLower(obs.Owner + "/" + obs.Repo + "#" + obs.Author)
	c.maintainerPermMu.Lock()
	if e, ok := c.maintainerPerm[key]; ok && now.Sub(e.at) < maintainerPermTTL {
		c.maintainerPermMu.Unlock()
		return e.maintainer, nil
	}
	c.maintainerPermMu.Unlock()
	level, _, err := c.gh.Repositories.GetPermissionLevel(ctx, obs.Owner, obs.Repo, obs.Author)
	if err != nil {
		c.warn("stage labels: could not read the author's repository permission; adoption deferred", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "error", err)
		return false, err
	}
	maintainer := repoMaintainerLevel(level.GetPermission(), level.GetRoleName())
	c.maintainerPermMu.Lock()
	if c.maintainerPerm == nil {
		c.maintainerPerm = map[string]maintainerPermEntry{}
	}
	for k, e := range c.maintainerPerm {
		if now.Sub(e.at) >= maintainerPermTTL {
			delete(c.maintainerPerm, k)
		}
	}
	c.maintainerPerm[key] = maintainerPermEntry{maintainer: maintainer, at: now}
	c.maintainerPermMu.Unlock()
	return maintainer, nil
}

// AdoptionMaintainerSupersederMarker is stamped on the note that closes the
// hive's superseding PR when the original turns out to be a maintainer's.
const AdoptionMaintainerSupersederMarker = "<!-- hive:adoption-maintainer-superseder -->"

// releaseMaintainerAdoption hands a maintainer's PR back. A still-open hive
// superseder is closed with a note first, so the release never leaves an
// untracked hive duplicate of the maintainer's PR mergeable; if that close
// fails the record (and its AdoptedBy) is kept and the next pass retries.
// Returns the number of comments posted.
func (c *Engine) releaseMaintainerAdoption(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, superseding int) (int, error) {
	comments := 0
	if superseding > 0 {
		n, err := c.closeMaintainerSuperseder(ctx, obs, superseding)
		comments += n
		if err != nil {
			return comments, err
		}
	}
	store.ClearAdoption(key)
	return comments, nil
}

// closeMaintainerSuperseder closes the hive's still-open superseding PR with
// a note pointing back at the maintainer's original; a merged or
// already-closed superseder is left alone. Returns the comments posted.
func (c *Engine) closeMaintainerSuperseder(ctx context.Context, obs stageObservation, superseding int) (int, error) {
	sup, _, err := c.gh.PullRequests.Get(ctx, obs.Owner, obs.Repo, superseding)
	if err != nil {
		c.warn("stage labels: maintainer adoption superseder lookup failed", "repo", obs.Display, "pr", obs.Number, "superseded_by", superseding, "error", err)
		return 0, err
	}
	if sup.GetMerged() || strings.EqualFold(sup.GetState(), "closed") {
		return 0, nil
	}
	body := fmt.Sprintf("%s\nClosing: #%d is a repository maintainer's PR, which the hive does not take over. Fixes and merge stay with @%s there.", AdoptionMaintainerSupersederMarker, obs.Number, obs.Author)
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, superseding, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		c.warn("stage labels: maintainer adoption superseder note failed", "repo", obs.Display, "pr", obs.Number, "superseded_by", superseding, "error", err)
		return 0, err
	}
	if _, _, err := c.gh.PullRequests.Edit(ctx, obs.Owner, obs.Repo, superseding, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil {
		c.warn("stage labels: closing maintainer adoption superseder failed", "repo", obs.Display, "pr", obs.Number, "superseded_by", superseding, "error", err)
		return 1, err
	}
	c.info("stage labels: closed superseder of maintainer PR", "repo", obs.Display, "pr", obs.Number, "superseded_by", superseding)
	return 1, nil
}

// canPushToPRBranch reports whether the App can push to the PR's head: a
// head in the base repo always; a fork head only with "allow edits by
// maintainers", which the list payload omits, so one GET resolves it.
func (c *Engine) canPushToPRBranch(ctx context.Context, obs stageObservation) bool {
	if obs.HeadRepo == "" {
		return false
	}
	if c.baseSyncTouchesWorkflows(ctx, obs) {
		return false
	}
	if strings.EqualFold(obs.HeadRepo, obs.Owner+"/"+obs.Repo) {
		return true
	}
	pr, _, err := c.gh.PullRequests.Get(ctx, obs.Owner, obs.Repo, obs.Number)
	if err != nil {
		c.warn("stage labels: adoption could not read maintainer_can_modify; superseding", "repo", obs.Display, "pr", obs.Number, "error", err)
		return false
	}
	return pr.GetMaintainerCanModify()
}

// pushAccessLost reports only a confirmed denial on a fork. Unlike the
// conservative adopt-time mode choice, a recurring sweep must not supersede
// a live adoption on an API error or unrelated base-side workflow drift.
func (c *Engine) pushAccessLost(ctx context.Context, obs stageObservation) bool {
	if obs.HeadRepo == "" || strings.EqualFold(obs.HeadRepo, obs.Owner+"/"+obs.Repo) {
		return false
	}
	pr, _, err := c.gh.PullRequests.Get(ctx, obs.Owner, obs.Repo, obs.Number)
	if err != nil {
		c.warn("stage labels: could not read maintainer_can_modify; keeping in-place adoption this sweep", "repo", obs.Display, "pr", obs.Number, "error", err)
		return false
	}
	return pr.MaintainerCanModify != nil && !pr.GetMaintainerCanModify()
}

// baseSyncTouchesWorkflows reports whether the base has moved past a change
// under .github/workflows/ since the PR's merge-base. The App token cannot
// push such a base-sync to the PR branch (no `workflows` permission, #6985),
// so in-place adoption would be a dead end (#12865). A compare failure counts
// as touching: a superseding PR is always deliverable.
func (c *Engine) baseSyncTouchesWorkflows(ctx context.Context, obs stageObservation) bool {
	if obs.HeadSHA == "" || obs.BaseRef == "" {
		return false
	}
	opts := &gh.ListOptions{PerPage: 300}
	for {
		cmp, resp, err := c.gh.Repositories.CompareCommits(ctx, obs.Owner, obs.Repo, obs.HeadSHA, obs.BaseRef, opts)
		if err != nil {
			c.warn("stage labels: adoption could not compare head to base for workflow changes; superseding", "repo", obs.Display, "pr", obs.Number, "error", err)
			return true
		}
		for _, f := range cmp.Files {
			if strings.HasPrefix(f.GetFilename(), ".github/workflows/") {
				c.info("stage labels: base-sync touches .github/workflows/ which the App cannot push; superseding", "repo", obs.Display, "pr", obs.Number, "file", f.GetFilename())
				return true
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return false
		}
		opts.Page = resp.NextPage
	}
}

// settleInPlaceAdoption handles a head change on an in-place-adopted PR with a
// fail-closed attribution rule. The hive keeps ownership only when the new
// commit carries GitHub-controlled evidence linking it to the App bot; the
// stage store remembers heads that already passed that check so a re-observed
// SHA is not re-fetched, but it is not a separate evidence source. Forgeable
// git author/committer names and emails never count. If the commit lookup
// fails, the pusher is unknown and the hive keeps adoption to avoid bounce
// loops. The first deploy after this rule also keeps adoption until this repo
// has recorded any App-linked head; after that, missing hive evidence means
// the contributor is back, so the hive yields once and the ordinary 072 flow
// re-arms.
// Returns the number of comments posted.
func (c *Engine) settleInPlaceAdoption(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, prev stagelabels.Record, policy StageLabelPolicy, now time.Time) (int, error) {
	if prev.AdoptedInPlace() {
		maintainer, err := c.authorMaintainsRepo(ctx, obs, now)
		if err != nil {
			// No evidence either way: keep the adoption as it is.
			return 0, nil
		}
		if maintainer {
			// Taken over before this rule, or before the author's permission
			// was raised: hand the PR back rather than keep or re-home it.
			c.info("stage labels: in-place adoption of a repository maintainer's PR released", "repo", obs.Display, "pr", obs.Number, "author", obs.Author)
			return c.releaseMaintainerAdoption(ctx, store, key, obs, prev.AdoptedBy)
		}
	}
	if !prev.AdoptedInPlace() || obs.HeadSHA == "" || obs.HeadSHA == prev.AdoptedHead {
		return 0, nil
	}
	if prev.YieldedHead == obs.HeadSHA {
		return 0, nil
	}
	if prev.HivePushedHead(obs.HeadSHA) {
		store.MarkAdoptedHead(key, obs.HeadSHA)
		c.info("stage labels: adopted PR advanced by recorded hive push", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA)
		return 0, nil
	}
	app := c.transport.AppBotLogin()
	commit, _, err := c.gh.Repositories.GetCommit(ctx, obs.Owner, obs.Repo, obs.HeadSHA, nil)
	if err != nil {
		// Unknown pusher: keep the adoption rather than bounce the PR.
		c.warn("stage labels: adopted head changed; pusher unknown, keeping adoption", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "error", err)
		store.MarkAdoptedHead(key, obs.HeadSHA)
		return 0, nil
	}
	if commitByApp(commit, app) {
		store.MarkHivePushedHead(key, obs.HeadSHA)
		store.MarkAdoptedHead(key, obs.HeadSHA)
		c.info("stage labels: adopted PR advanced by the hive", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "pusher", commitIdentity(commit))
		return 0, nil
	}
	if !policy.AdoptionStrictAttribution {
		store.MarkAdoptedHead(key, obs.HeadSHA)
		c.warn("stage labels: adopted head changed without hive evidence; strict attribution disabled, keeping adoption", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "pusher", commitIdentity(commit))
		return 0, nil
	}
	if !store.HasHivePushEvidence(obs.Owner + "/" + obs.Repo) {
		store.MarkAdoptedHead(key, obs.HeadSHA)
		c.warn("stage labels: adopted head changed without hive evidence; no repo evidence recorded yet, keeping adoption", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "pusher", commitIdentity(commit))
		return 0, nil
	}
	store.MarkYielded(key, obs.HeadSHA)
	c.info("stage labels: head changed without hive push evidence; yielding", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "pusher", commitIdentity(commit))
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(AdoptionYieldComment(obs))}); err != nil {
		c.warn("stage labels: adoption yield note failed", "repo", obs.Display, "pr", obs.Number, "error", err)
		return 0, err
	}
	return 1, nil
}

// commitIdentity renders the commit's pusher for logs: the GitHub login when
// linked, else the git author name/email.
func commitIdentity(commit *gh.RepositoryCommit) string {
	if commit == nil {
		return ""
	}
	if l := commit.GetAuthor().GetLogin(); l != "" {
		return l
	}
	if l := commit.GetCommitter().GetLogin(); l != "" {
		return l
	}
	if c := commit.GetCommit(); c != nil {
		if a := c.GetAuthor(); a != nil {
			if name := strings.TrimSpace(a.GetName()); name != "" {
				return name
			}
			return strings.TrimSpace(a.GetEmail())
		}
	}
	return ""
}

// commitByApp reports whether GitHub linked the commit to the App's bot login.
// A verified signature is useful only with that GitHub-controlled linkage; git
// names/emails and unsigned linked contributor commits never establish hive
// ownership.
func commitByApp(commit *gh.RepositoryCommit, login string) bool {
	if commit == nil || login == "" {
		return false
	}
	if commitByLogin(commit, login) {
		return true
	}
	if v := commit.GetCommit().GetVerification(); v != nil && v.GetVerified() {
		return commitByLogin(commit, login)
	}
	return false
}

// commitByLogin reports whether the commit's GitHub author or committer is
// login ([bot] suffix tolerated).
func commitByLogin(commit *gh.RepositoryCommit, login string) bool {
	if commit == nil {
		return false
	}
	want := normalizeBotLogin(login)
	if want == "" {
		return false
	}
	return normalizeBotLogin(commit.GetAuthor().GetLogin()) == want || normalizeBotLogin(commit.GetCommitter().GetLogin()) == want
}

func normalizeBotLogin(login string) string {
	login = strings.ToLower(strings.TrimSpace(login))
	return strings.TrimSuffix(login, "[bot]")
}

// Escaping stage 074 (hivecommons/hive#12858).
//
// An in-place adoption hands the PR's own branch to the hive's CI-repair
// lane. Two things that lane never does left adopted PRs pinned at 074 with
// nothing working them: it cannot deliver a commit the squash merge will
// accept when the PR's author is a person (the hive signs as the App, the
// squash lands as the author, and the sign-off gate strips the fix), and it
// does not act on a review-swarm finding on a green head. Both convert the
// adoption to a supersession — the hive's own PR carries the contributor's
// commits and the finding, and the hive's normal review-fix route finishes
// it. Whatever is left sitting at the adopted head past the 074 dwell with
// CI not running and a real blocker named is handed to a person as stage
// 085 with the exact lane reason, noted once per head, so the dashboard
// shows it instead of a silent 074. An optional check going red is not a
// blocker (the merge gate ignores it; hivecommons/hive#13040), and the
// escalation is re-evaluated every sweep: when the reason clears the PR
// drops back to 074 and the lane carries on.

// adoptionEscape is what escapeStalledAdoption decided for one sweep.
type adoptionEscape struct {
	// Escalated means the PR is a stage-085 item for Reason, not a 074 one.
	Escalated bool
	Reason    string
}

// adoptionCIPending reports whether the lane saw CI still running on the
// head: a fix may be in flight, so the head is not stale yet.
func adoptionCIPending(obs stageObservation) bool {
	return classifyCIReason(obs.LaneReason) == ciReasonPending
}

// adoptionOptionalCheckOnly reports whether the only thing the lane holds
// against the head is an optional check: the merge gate does not read it,
// so it is nothing a person must decide (hivecommons/hive#13040).
func adoptionOptionalCheckOnly(obs stageObservation) bool {
	return strings.HasPrefix(nonEmpty(obs.Reason, obs.LaneReason), stageReasonOptionalCheckFailing) && !verdictBlocks(obs.Detail) && !verdictNeedsHuman(obs.Detail)
}

// adoptionStale reports whether the adopted head has sat unchanged for the
// 074 dwell with CI not running. A record whose head clock was never
// stamped (pre-hivecommons/hive#13040) is not stale: AdoptedAt may predate
// the head by days; escapeStalledAdoption stamps it first.
func adoptionStale(obs stageObservation, rec stagelabels.Record, policy StageLabelPolicy, now time.Time) bool {
	since := rec.AdoptedHeadSince()
	if since.IsZero() || obs.HeadSHA == "" || obs.HeadSHA != rec.AdoptedHead || rec.AdoptedHeadClockUnset() {
		return false
	}
	if adoptionCIPending(obs) {
		return false
	}
	if obs.Detail.MergeCommit && !stagelabels.HasMergeCommitLabel(obs.Labels) {
		// The lane just elected a merge commit for this head; the label
		// it is about to write re-runs the attribution check. Nothing has
		// sat yet under that plan.
		return false
	}
	return now.Sub(since) >= stagelabels.Dwell(stagelabels.AdoptedByHive, policy.Dwell)
}

// adoptedHeadSignoffUnfixable reports a head whose App sign-off cannot
// survive a squash attributed to the contributor. Re-home it without
// rewriting commits: the superseding PR is App-authored, so the App's own
// sign-off can survive that squash. All merge gates still run there.
// A DCO-red head is left to settleAdoption's DCO re-home.
//
// Not unfixable when the stage pass elected a merge commit for the head
// (obs.Detail.MergeCommit, automerge_merge_commit.go): the hive merges it
// itself with `merge-method/merge`, so the adoption carries on — including
// one escalated before the election existed, which the sweep re-evaluates
// and releases to 074.
func adoptedHeadSignoffUnfixable(obs stageObservation, rec stagelabels.Record) bool {
	return rec.AdoptedInPlace() && obs.HeadSHA != "" && !adoptedHeadDCORed(obs, rec) && obs.Detail.DCOSquashUnsafeHive > 0 && !obs.Detail.MergeCommit
}

// inPlaceRehomeReason is the reason an in-place adoption converts to a
// supersession this sweep, or "" to stay in place. A DCO-red head is left to
// settleAdoption's DCO re-home, which carries the repair instructions. A
// squash-unsafe hive sign-off re-homes immediately; rebase work first gets
// the normal fixer window on the contributor branch.
func inPlaceRehomeReason(obs stageObservation, rec stagelabels.Record, policy StageLabelPolicy, now time.Time) string {
	if !rec.AdoptedInPlace() || obs.HeadSHA == "" || obs.HeadSHA != rec.AdoptedHead || adoptedHeadDCORed(obs, rec) || adoptionBlocked(obs.Labels) != "" || verdictNeedsHuman(obs.Detail) {
		return ""
	}
	if adoptedHeadSignoffUnfixable(obs, rec) {
		return AdoptReasonSignoffRehome
	}
	if humanConflictReasons[obs.Reason] || humanConflictReasons[obs.LaneReason] || hasLabelFold(obs.Labels, "needs-rebase") {
		if adoptionStale(obs, rec, policy, now) {
			return AdoptReasonRebaseRehome
		}
	}
	if verdictBlocks(obs.Detail) && adoptionStale(obs, rec, policy, now) {
		return AdoptReasonInPlaceStalled
	}
	return ""
}

// adoptionStalledReason is the stage-085 reason for an adoption nobody is
// working, or "": a supersession that opened no PR, or an adopted head that
// sat past the dwell with CI idle and a real blocker named. A live
// superseder is progress; an optional check alone is not a blocker.
func adoptionStalledReason(obs stageObservation, rec stagelabels.Record, policy StageLabelPolicy, now time.Time) string {
	if rec.AdoptedBy > 0 || obs.HeadSHA == "" {
		return ""
	}
	if rec.AdoptionAttemptedNothingOpened(obs.HeadSHA) {
		return stagelabels.LaneReasonAdoptionStalledPrefix + "supersede-failed:" + nonEmpty(rec.AdoptReason, obs.Reason)
	}
	if adoptedHeadSignoffUnfixable(obs, rec) {
		return stagelabels.LaneReasonAdoptionStalledPrefix + stagelabels.LaneReasonHiveSignoffAttribution
	}
	if adoptionOptionalCheckOnly(obs) || !adoptionStale(obs, rec, policy, now) {
		return ""
	}
	return stagelabels.LaneReasonAdoptionStalledPrefix + nonEmpty(obs.Reason, obs.LaneReason)
}

// escapeStalledAdoption runs before the 074 pin for a PR the hive owns whose
// lane verdict is still below the merge gate. It converts a dead-end
// in-place adoption to a supersession, or escalates an adoption nobody is
// working to 085 with a comment naming the lane reason; the re-home and the
// note happen once per head. An escalation holds only while its reason
// does: the sweep re-evaluates it and releases the PR to 074 when the
// blocker clears (hivecommons/hive#13040). Returns the decision and the
// number of comments posted.
func (c *Engine) escapeStalledAdoption(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, now time.Time) (adoptionEscape, int, error) {
	rec, ok := store.Get(key)
	if !ok {
		return adoptionEscape{}, 0, nil
	}
	if store.StampAdoptedHeadAt(key, obs.HeadSHA) {
		// Pre-#13040 record: the head clock starts now, not at AdoptedAt.
		c.info("stage labels: adopted head had no clock; starting the 074 dwell from this sweep", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "adopted_at", rec.AdoptedAt)
		rec, _ = store.Get(key)
	}
	rehomeReason := inPlaceRehomeReason(obs, rec, policy, now)
	if rehomeReason == "" && rec.AdoptedInPlace() && obs.HeadSHA == rec.AdoptedHead && !adoptedHeadDCORed(obs, rec) && adoptionBlocked(obs.Labels) == "" && !verdictNeedsHuman(obs.Detail) && c.pushAccessLost(ctx, obs) {
		rehomeReason = AdoptReasonPushRehome
	}
	if rehomeReason == "" && adoptionStalledReason(obs, rec, policy, now) != "" {
		// Before a red head is handed to a person, try the recovery a
		// since-fixed base or shared-CI flake needs (hivecommons/hive#14500):
		// an attempt holds the escalation while CI runs on the result, and
		// releases one already in place for the same head.
		if recovery := c.recoverStaleRedAdoption(ctx, store, key, obs, rec); recovery.Attempted {
			if rec.AdoptionEscalated(obs.HeadSHA) {
				store.ClearAdoptionEscalation(key)
				c.info("stage labels: adoption escalation released for recovery; back to adopted-by-hive", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "was", rec.AdoptEscalatedReason, "recovery", recovery.Reason)
			}
			return adoptionEscape{}, 0, nil
		}
	}
	if rec.AdoptionEscalated(obs.HeadSHA) {
		// A workflow-path refusal is a verdict on the head itself: it holds
		// until the head moves, whatever the re-home or stall reasons say.
		if rec.AdoptEscalatedReason == adoptionWorkflowRefusedReason || (rehomeReason == "" && adoptionStalledReason(obs, rec, policy, now) != "") {
			return adoptionEscape{Escalated: true, Reason: rec.AdoptEscalatedReason}, 0, nil
		}
		store.ClearAdoptionEscalation(key)
		c.info("stage labels: adoption escalation reason cleared; back to adopted-by-hive", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "was", rec.AdoptEscalatedReason, "lane_reason", nonEmpty(obs.Reason, obs.LaneReason), "merge_commit", obs.Detail.MergeCommit)
		rec, _ = store.Get(key)
	}
	comments := 0
	var firstErr error
	reason := rehomeReason
	if reason != "" {
		// Read the author's permission before the attempt is recorded: an
		// unreadable answer leaves the in-place adoption untouched, and a
		// maintainer's adoption is released by settleInPlaceAdoption.
		if maintainer, err := c.authorMaintainsRepo(ctx, obs, now); maintainer || err != nil {
			return adoptionEscape{}, 0, nil
		}
		// Re-read the current diff before copying a head. A concurrent push
		// or incomplete evidence defers the write; merge fences travel with it.
		fence, err := c.adoptionDiffRefusal(ctx, obs)
		if err != nil || adoptionFenceDefers(fence) {
			return adoptionEscape{}, 0, err
		}
		if fence == contributorLaneReasonWorkflowPath {
			// Terminal for this head: refuse publication, keep the in-place
			// adoption and hand the re-home to a person (085) with one note.
			store.NoteAdoptDiffFence(key, fence)
			noted := rec.AdoptionEscalationNoted(obs.HeadSHA)
			store.MarkAdoptionEscalated(key, obs.HeadSHA, adoptionWorkflowRefusedReason)
			c.warn("stage labels: refusing workflow-path re-home; escalating to needs-decision", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "reason", reason, "noted", noted)
			esc := adoptionEscape{Escalated: true, Reason: adoptionWorkflowRefusedReason}
			if noted {
				return esc, 0, nil
			}
			if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(AdoptionWorkflowRefusedComment(obs))}); err != nil {
				c.warn("stage labels: workflow-path refusal note failed", "repo", obs.Display, "pr", obs.Number, "error", err)
				return esc, 0, err
			}
			return esc, 1, nil
		}
		if fence != "" {
			store.NoteAdoptDiffFence(key, fence)
		}
		c.info("stage labels: in-place adoption cannot be finished on the contributor branch; superseding", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "reason", reason, "lane_reason", obs.Reason, "squash_unsafe_hive", obs.Detail.DCOSquashUnsafeHive)
		// Record the attempt first, as every adoption path does: one
		// re-home per head whatever the API says.
		store.MarkAdopted(key, obs.HeadSHA, 0, now)
		store.NoteAdoptReason(key, reason)
		n, err := c.supersedeAdoptedPR(ctx, store, key, obs, rec, policy, reason, 0, false, false, now)
		comments += n
		if err == nil {
			return adoptionEscape{}, comments, nil
		}
		firstErr = err
		rec, _ = store.Get(key)
	}
	reason = adoptionStalledReason(obs, rec, policy, now)
	if reason == "" {
		return adoptionEscape{}, comments, firstErr
	}
	noted := rec.AdoptionEscalationNoted(obs.HeadSHA)
	store.MarkAdoptionEscalated(key, obs.HeadSHA, reason)
	c.info("stage labels: adopted PR sits with nothing the hive can push; escalating to needs-decision", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "reason", reason, "head_since", rec.AdoptedHeadSince(), "noted", noted)
	if noted {
		// The blocker flapped on a head already noted: re-escalate silently.
		return adoptionEscape{Escalated: true, Reason: reason}, comments, firstErr
	}
	body := AdoptionStalledComment(obs, rec, reason, now.Sub(rec.AdoptedHeadSince()))
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		c.warn("stage labels: adoption escalation note failed", "repo", obs.Display, "pr", obs.Number, "error", err)
		if firstErr == nil {
			firstErr = err
		}
	} else {
		comments++
	}
	return adoptionEscape{Escalated: true, Reason: reason}, comments, firstErr
}
