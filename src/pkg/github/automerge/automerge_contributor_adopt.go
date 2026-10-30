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
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/config"
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

// adoptRefsNotePattern matches a soft `Refs #N — partial` line: the
// reference carries into the copy as a bare `Refs #N` so the issue keeps
// its "covered by" link (hivecommons/hive#15499 read "no linked issue"
// because its original #15400 only had an annotated Refs line).
var adoptRefsNotePattern = regexp.MustCompile(`(?im)^\s*refs\s+((?:[\w.-]+/[\w.-]+)?#\d+)\s*[—–-]\s*\S.*$`)

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
	// AdoptReasonOperator is an owner-delegate's `adoption/supersede`
	// request: no nudge or grace, the operator asked (hivecommons/hive#15135).
	AdoptReasonOperator = "operator-asked"
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
	// AdoptReasonOperatorForced marks an operator's "Adopt now": adoption
	// taken at once by supersession, regardless of the author's presence,
	// the grace window and maintainer_policy. The marker carries by=@login.
	AdoptReasonOperatorForced = "operator-forced"
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

// adoptionDue reports whether the PR at head is due for automatic
// adoption and which rule fired. It is AutoAdoption — the one predicate
// the tracker's Adopt offer and adopt-at projection also read
// (automerge_adoption_clock.go) — under the policy's clock.
func adoptionDue(rec stagelabels.Record, head string, policy StageLabelPolicy, now time.Time, presence ContributorPresence, hasPresence bool) (bool, string) {
	v := AutoAdoption(rec, head, policy.AdoptionClock(), now, presence, hasPresence)
	if !v.Due {
		return false, ""
	}
	return true, v.Reason
}

// adoptionOfflineGrace is policy.AdoptOfflineGrace with its default.
func adoptionOfflineGrace(policy StageLabelPolicy) time.Duration {
	return policy.AdoptionClock().offlineGrace()
}

// adoptionRetryReason names why a retried adoption fired.
func adoptionRetryReason(presence ContributorPresence, hasPresence bool) string {
	switch {
	case !hasPresence:
		return AdoptReasonGrace
	case presence.Registered:
		return AdoptReasonOffline
	}
	return AdoptReasonUnregistered
}

// adoptReasonText is the human-readable half of the adoption note.
func adoptReasonText(reason string, grace time.Duration, online bool) string {
	switch reason {
	case AdoptReasonOffline:
		return "your hive has been offline since the nudge"
	case AdoptReasonUnregistered:
		return "no hive of yours is registered here and nothing moved after the nudge"
	case AdoptReasonOperator:
		return "a maintainer asked the hive to take it over"
	case AdoptReasonSignoffRehome:
		return "the hive's fix on this branch cannot carry a sign-off the squash merge accepts (the hive signs as the App, the squash lands as you)"
	case AdoptReasonRebaseRehome:
		return "the adopted branch still needs a rebase after the fix window"
	case AdoptReasonPushRehome:
		return "the hive can no longer push to the contributor branch"
	case AdoptReasonInPlaceStalled:
		return "the review swarm's finding on this head went unaddressed past the stage dwell and the in-place fix lane only repairs CI"
	case AdoptReasonOperatorForced:
		if online {
			return "a maintainer asked the hive to take it over now rather than wait for your hive, which is online — no fault of yours"
		}
		return "a maintainer asked the hive to take it over now rather than wait out the offline grace window"
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
	for _, m := range adoptRefsNotePattern.FindAllStringSubmatch(body, -1) {
		if len(m) == 2 {
			add("Refs " + m[1])
		}
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

// adoptionDCORepairMode decides how a DCO-red head gets its sign-off:
// AdoptModeInPlace rewrites the hive's own unsigned commits on the PR
// branch, which the stage pass can only do through the git-data API of the
// base repository — a fork branch is pushable by an agent's git but not by
// the App's REST token, so a fork head re-homes (AdoptModeSupersede) even
// when every unsigned commit is the hive's (hivecommons/hive#14666).
func adoptionDCORepairMode(obs stageObservation, canPushInPlace bool) string {
	if obs.Detail.DCO != stagelabels.DCONo || obs.Detail.DCOUnsigned <= 0 {
		return ""
	}
	if canPushInPlace && headInBaseRepo(obs) && obs.Detail.DCOUnsignedHive == obs.Detail.DCOUnsigned && obs.Detail.DCOUnsignedOther == 0 {
		return stagelabels.AdoptModeInPlace
	}
	return stagelabels.AdoptModeSupersede
}

// headInBaseRepo reports whether the PR head lives in the base repository
// (not a fork), where the App's git-data API can rewrite it.
func headInBaseRepo(obs stageObservation) bool {
	return obs.HeadRepo != "" && strings.EqualFold(obs.HeadRepo, obs.Owner+"/"+obs.Repo)
}

// adoptedHeadNeedsDCORepair decides, for an in-place-adopted PR, whether the
// unsigned commits are the hive's own on a base-repo branch it rewrites in
// place with its sign-off.
func adoptedHeadNeedsDCORepair(obs stageObservation, rec stagelabels.Record, canPushInPlace bool) bool {
	return adoptedHeadDCORed(obs, rec) && adoptionDCORepairMode(obs, canPushInPlace) == stagelabels.AdoptModeInPlace
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

// AdoptionPRBody is the superseding App PR's body. resignedTip is the head of
// the re-signed replay the branch points at, or "" when the branch carries the
// contributor's head unchanged.
func AdoptionPRBody(obs stageObservation, rec stagelabels.Record, dcoRehome bool, resignedTip string) string {
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
	if resignedTip != "" {
		fmt.Fprintf(&b, ", re-signed as `%s` (content unchanged, `Signed-off-by` added). Last lane reason on the original: `", shortSHA(resignedTip))
	} else {
		b.WriteString(", unchanged. Last lane reason on the original: `")
	}
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
	if dcoRehome && resignedTip == "" {
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
	case AdoptReasonOperator:
		return "adopted at a maintainer's request"
	}
	return "adopted after CI went red and the contributor was unavailable"
}

// AdoptionStalledComment is the one note left on an adopted PR handed to a
// person: it names the lane reason the hive was stuck on, so nobody reads
// "waiting on a maintainer" into a PR the hive still owes a fix.
func AdoptionStalledComment(obs stageObservation, rec stagelabels.Record, reason string, sat time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s head=%s reason=%s -->\n", AdoptionStalledMarker, obs.HeadSHA, reason)
	if reason == stagelabels.LaneReasonAdoptionStalledPrefix+stagelabels.LaneReasonRebaseFailedTwice {
		// hivecommons/hive#15307: the copy opened and the fixer tried.
		fmt.Fprintf(&b, "The hive adopted this PR (`%s`) into its copy #%d, but the copy still conflicts with `%s` after the fixer's two rebase-resolve attempts: `%s`.\n\n", stagelabels.AdoptedByHive.Label(), rec.AdoptedBy, obs.BaseRef, reason)
		fmt.Fprintf(&b, "**Options:** adopt the commits into a fresh branch, or close this PR as superseded.\n\nMoving to `%s` so a person decides.", stagelabels.NeedsDecision.Label())
		return b.String()
	}
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
	case mergeCommitRefusedLabelsSuppressed:
		fmt.Fprintf(&b, "Every commit already carries a sign-off matching its own author, so the hive elected a merge commit, but the write-back gate keeps the hive's labels off this PR, so `%s` could not be applied and the attribution check still applies the squash rule. ", stagelabels.MergeCommitLabel)
		fmt.Fprintf(&b, "**Ask:** a maintainer adds `%s` (the check re-runs under the merge rule and the hive merges with a merge commit) or merges by hand with `gh pr merge %d --merge`, not squash; otherwise @%s re-authors the hive's commits under their own sign-off and pushes a new head.", stagelabels.MergeCommitLabel, obs.Number, author)
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
	return adoptionCommentBy(obs, nonEmpty(hiveID, "hive"), superseding, reason, grace, online)
}

// AdoptionForcedComment is the adoption note for an operator's "Adopt now":
// the marker names the operator (by=@login reason=operator-forced) and the
// text tells the author a maintainer took the PR over.
func AdoptionForcedComment(obs stageObservation, by string, superseding int, online bool) string {
	return adoptionCommentBy(obs, "@"+strings.TrimPrefix(strings.TrimSpace(by), "@"), superseding, AdoptReasonOperatorForced, 0, online)
}

func adoptionCommentBy(obs stageObservation, by string, superseding int, reason string, grace time.Duration, online bool) string {
	return fmt.Sprintf("%s by=%s reason=%s mode=%s superseded_by=%d head=%s -->\n@%s the hive has adopted this PR (`%s`): %s. "+
		"It is being finished as #%d, which carries your commits (and sign-off) unchanged; the hive will rebase and fix CI there. "+
		"This PR is closed automatically once #%d merges. If you would rather finish it yourself, push a new head here and say so on #%d.",
		AdoptionMarkerPrefix, by, reason, stagelabels.AdoptModeSupersede, superseding, obs.HeadSHA, obs.Author, stagelabels.AdoptedByHive.Label(), adoptReasonText(reason, grace, online), superseding, superseding, superseding)
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
	if !ok || obs.Author == "" || obs.BaseRef == "" {
		return 0, nil
	}
	requested := hasLabel(obs.Labels, delegatedecision.LabelAdoptSupersede)
	forcedBy, forced := rec.AdoptionForced()
	forced = forced && obs.HeadSHA != "" && !(rec.Adopted(obs.HeadSHA) && rec.AdoptedBy > 0)
	if obs.Lane == stageLaneSelf {
		// The self-authored sweep also carries trusted-bot PRs
		// (github-actions and friends): a delegated supersede of one of
		// those runs like any contributor PR. Only the App's own PR has no
		// branch to re-home; that request is consumed with one note
		// instead of a silent no-op every sweep (hivecommons/docs#500).
		if !requested && !forced {
			return 0, nil
		}
		if normalizeBotLogin(obs.Author) == normalizeBotLogin(c.transport.AppBotLogin()) {
			return c.refuseSelfAuthoredSupersede(ctx, store, key, obs, rec, now)
		}
		c.info("stage labels: delegated supersede of a trusted-bot PR on the self-authored lane", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA)
	}
	due, reason := adoptionDue(rec, obs.HeadSHA, policy, now, presence, hasPresence)
	if forced {
		// An operator's "Adopt now" (hivecommons/hive#15038): adopt at
		// once by supersession, whatever the author's presence, the grace
		// window and maintainer_policy say. Only a hold label stops it.
		due, reason = true, AdoptReasonOperatorForced
	} else if requested && obs.HeadSHA != "" && !(rec.Adopted(obs.HeadSHA) && rec.AdoptedBy > 0 && !rec.AdoptionCopyFailed()) {
		// An explicit owner-delegate decision bypasses the nudge/grace and
		// re-homes a stalled in-place adoption through the same supersede path.
		due, reason = true, AdoptReasonOperator
	}
	if !due {
		return 0, nil
	}
	if l := adoptionBlocked(obs.Labels); l != "" {
		c.info("stage labels: adoption due but blocked by label", "repo", obs.Display, "pr", obs.Number, "label", l)
		return 0, nil
	}
	if personAuthored(obs, policy) {
		// A person's own PR (hivecommons/hive#15202): no contributor hive
		// can be offline for it, and neither an operator request nor a
		// forced adoption changes who should fix it. Recorded so the
		// tracker row says why.
		store.NoteAdoptionBlocked(key, obs.HeadSHA, stagelabels.AdoptBlockedAuthorPerson, now)
		c.info("stage labels: adoption refused, a person pushed this PR themselves; asking the author instead", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "author_kind", obs.AuthorKind.String(), "reason", reason, "requested", requested, "forced", forced)
		return 0, nil
	}
	if forced {
		c.info("stage labels: operator forced adoption; maintainer policy, admission and presence bypassed", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "by", forcedBy, "online", presence.Online, "policy", policy.maintainerPolicy())
	} else if c.adoptionRefusedByAdmission(store, key, obs, requested, now) {
		return 0, nil
	} else if held, deferred := c.maintainerHoldsAdoption(ctx, obs, policy, requested, now); held || deferred {
		if held && !deferred {
			// Recorded so the activity monitor says why (hivecommons/hive#14524):
			// under ask the row offers Adopt, under never it names the policy.
			store.NoteAdoptionBlocked(key, obs.HeadSHA, stagelabels.AdoptBlockedAuthorMaintainer, now)
			c.info("stage labels: adoption due but the author maintains the repository; not adopting", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "policy", policy.maintainerPolicy(), "deferred", false)
		} else {
			c.info("stage labels: adoption due but the author's repository permission is unreadable; deferred", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "policy", policy.maintainerPolicy())
		}
		return 0, nil
	}
	store.ClearAdoptionBlocked(key)
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
	if policy.AdoptInPlace && !requested && !forced {
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
	// One line per adoption naming the trigger: automatic (the
	// AutoAdoption predicate fired on this sweep — the offer the tracker
	// showed, performed by the hive itself), operator-forced, or an
	// operator's adoption/supersede request.
	c.info("stage labels: adopting contributor PR", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "reason", reason, "trigger", adoptionTrigger(reason, obs.Reason), "registered", presence.Registered, "online", presence.Online, "lane_reason", obs.Reason, "fence", fence)

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

	online := rec.NudgeOnline
	if forced && hasPresence {
		// The forced note tells an online author the maintainer took over.
		online = presence.Online
	}
	return c.supersedeAdoptedPR(ctx, store, key, obs, rec, policy, reason, grace, online, dcoRehome, now)
}

// Adoption triggers, the `trigger` field of the adoption log line.
const (
	AdoptTriggerAutomatic = "automatic"
	AdoptTriggerForced    = "operator-forced"
	AdoptTriggerRequested = "operator-requested"
)

// adoptionTrigger names what fired an adoption: the operator's forced
// Adopt now, the operator's adoption/supersede request, or the automatic
// predicate — qualified by the lane reason that returned the PR to its
// author ("automatic:conflicting", "automatic:fork-not-editable").
func adoptionTrigger(reason, laneReason string) string {
	switch reason {
	case AdoptReasonOperatorForced:
		return AdoptTriggerForced
	case AdoptReasonOperator:
		return AdoptTriggerRequested
	}
	if laneReason = strings.TrimSpace(laneReason); laneReason != "" {
		return AdoptTriggerAutomatic + ":" + laneReason
	}
	return AdoptTriggerAutomatic
}

// AdoptionSelfRefusedMarker stamps the note a delegated supersede of the
// hive App's own PR gets: the request is consumed, nothing is re-homed.
const AdoptionSelfRefusedMarker = "<!-- hive:adoption-self-refused"

// AdoptionSelfRefusedComment renders that note.
func AdoptionSelfRefusedComment(obs stageObservation) string {
	return fmt.Sprintf("%s head=%s -->\n**Not superseding:** this PR is the hive's own (`%s`), so there is no contributor branch to re-home into a fresh hive PR. The `%s` request is cleared; push a fix to this branch or close it and open a new PR.\n",
		AdoptionSelfRefusedMarker, obs.HeadSHA, obs.Author, delegatedecision.LabelAdoptSupersede)
}

// refuseSelfAuthoredSupersede consumes a delegated supersede request on the
// App's own PR: one blocked note per head, logged, no adoption recorded.
func (c *Engine) refuseSelfAuthoredSupersede(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, rec stagelabels.Record, now time.Time) (int, error) {
	if obs.HeadSHA == "" {
		return 0, nil
	}
	if rec.AdoptionBlocked(obs.HeadSHA) == stagelabels.AdoptBlockedSelfAuthored {
		return 0, nil
	}
	c.warn("stage labels: delegated supersede requested on the hive's own PR; refusing", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA)
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(AdoptionSelfRefusedComment(obs))}); err != nil {
		c.warn("stage labels: self-authored supersede refusal note failed", "repo", obs.Display, "pr", obs.Number, "error", err)
		return 0, err
	}
	store.NoteAdoptionBlocked(key, obs.HeadSHA, stagelabels.AdoptBlockedSelfAuthored, now)
	return 1, nil
}

// delegatedAdoptionAttempted reports whether adoptContributorPR recorded an
// attempt between the two snapshots: MarkAdopted stamps head/by/at before
// any write, so a changed adoption record means this head was tried.
func delegatedAdoptionAttempted(before, after stagelabels.Record) bool {
	// A person-authored refusal is terminal for the request, not a
	// deferral: the label must not re-ask every sweep (hivecommons/hive#15202).
	if after.AdoptBlockedBy == stagelabels.AdoptBlockedAuthorPerson && after.AdoptBlockedHead != "" {
		return true
	}
	return after.AdoptedHead != before.AdoptedHead ||
		after.AdoptedBy != before.AdoptedBy ||
		!after.AdoptedAt.Equal(before.AdoptedAt) ||
		after.AdoptMode != before.AdoptMode
}

func (c *Engine) supersedeAdoptedPR(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, rec stagelabels.Record, policy StageLabelPolicy, reason string, grace time.Duration, online bool, dcoRehome bool, now time.Time) (int, error) {
	forcedBy := ""
	if reason == AdoptReasonOperatorForced {
		forcedBy, _ = rec.AdoptionForced()
	}
	if forcedBy != "" {
		// An operator's Adopt now is not released under maintainer_policy:
		// never — the person the policy defers to has decided.
	} else if maintainer, deferred := c.maintainerReleasesAdoption(ctx, obs, policy, now); deferred {
		return 0, nil
	} else if maintainer {
		// Backstop for every re-home path (dco-rehome, in-place-stalled)
		// under maintainer_policy: never — a maintainer's PR is never
		// closed in favour of a hive copy.
		c.info("stage labels: not superseding a repository maintainer's PR; adoption released", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "reason", reason)
		return c.releaseMaintainerAdoption(ctx, store, key, obs, rec.AdoptedBy)
	}
	owner, repo, number := obs.Owner, obs.Repo, obs.Number
	rec.AdoptReason = reason
	// A person's approval of this head travels to the copy
	// (hivecommons/hive#15136 follow-up): adopting is not un-approving.
	admittedBy := c.operatorAdmission(owner, repo, number, obs.HeadSHA)
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
	// A re-adoption after the hive's copy was closed unmerged
	// (recoverOrphanedAdoption) is a distinct effect from the adoption that
	// opened the dead copy: without the dead number in the claim, the
	// mutation journal replays the first claim as already applied, no copy
	// opens and the adoption is reverted.
	priorCopy := 0
	if stored, ok := store.Get(key); ok && stored.AdoptedBy > 0 && stored.AdoptOrphanedBy == stored.AdoptedBy {
		priorCopy = stored.AdoptedBy
	}
	claimInputs := priorCopyInputs
	// The claim names the attempt (hivecommons/hive#15307): the journal
	// otherwise reads a re-admitted head's push as "already applied" and
	// skips it, leaving no branch for the replay to merge into after an
	// earlier attempt deleted it ("hive has nothing it can push").
	attemptStamp := now.UTC().Format(time.RFC3339)
	// resignedTip stays "" when the branch falls back to the unsigned
	// contributor head; the body then keeps the manual DCO repair steps.
	var resignedTip string
	pushBranch := func(prior int) error {
		_, err := effects.Execute(ctx, c.mutation, effects.Claim{
			Repo: fullRepo, Kind: effects.KindBranchPush, Target: branch, Actor: "stage_labels",
			Inputs: claimInputs(prior, map[string]string{"pr": strconv.Itoa(number), "head_sha": obs.HeadSHA, "adopt": "true", "attempt": attemptStamp}),
		}, func(ctx context.Context) (effects.Result, error) {
			tip := obs.HeadSHA
			if dcoRehome {
				if signed, signErr := c.resignAdoptionCommits(ctx, obs); signErr != nil {
					c.warn("stage labels: re-signing adopted commits failed; branching from the unsigned head", "repo", obs.Display, "pr", number, "error", signErr)
				} else if signed != "" {
					tip = signed
					resignedTip = signed
				}
			}
			_, _, apiErr := c.gh.Git.CreateRef(ctx, owner, repo, &gh.Reference{
				Ref:    gh.Ptr("refs/heads/" + branch),
				Object: &gh.GitObject{SHA: gh.Ptr(tip)},
			})
			if apiErr != nil && isGitHubStatus(apiErr, http.StatusUnprocessableEntity) {
				// The branch exists from an earlier attempt: point it at this head.
				_, _, apiErr = c.gh.Git.UpdateRef(ctx, owner, repo, &gh.Reference{
					Ref:    gh.Ptr("refs/heads/" + branch),
					Object: &gh.GitObject{SHA: gh.Ptr(tip)},
				}, true)
			}
			if apiErr != nil {
				return effects.Result{}, effects.NotApplied(apiErr)
			}
			return effects.Result{Provenance: tip}, nil
		})
		if err != nil {
			// Close-first (hivecommons/hive#15135) orders push → copy → close,
			// so a failed push leaves the original open; nothing to reopen.
			c.warn("stage labels: adoption failed to push branch", "repo", obs.Display, "pr", number, "branch", branch, "error", err)
		}
		return err
	}
	if err := pushBranch(priorCopy); err != nil {
		return 0, err
	}
	// fixerResolves is set when the replay conflicted and the fixer's
	// rebase-resolve episode takes the conflict on the hive's copy.
	fixerResolves := false
	if supersedeHeadConflicts(obs) {
		// The head is red on a conflict: replay the base onto the hive
		// branch first so the copy opens mergeable. A git conflict used to
		// be terminal for this head (one note, 07x reason) — which handed
		// the PR back to a contributor who, by the time adoption fires,
		// cannot act (hivecommons/hive#15307). With a RebaseResolver wired
		// the copy opens as it is, on a branch the hive can push to, and
		// the fixer's rebase-resolve episode resolves it under the
		// two-attempt guard (#15104).
		conflicted, n, err := c.replaySupersedeOntoBase(ctx, store, key, obs, branch, priorCopy, attemptStamp, now)
		if err != nil {
			return n, err
		}
		if conflicted {
			if c.rebaseResolver == nil {
				return n, nil
			}
			fixerResolves = true
		}
	}

	// openCopy returns the PR it opened, or nil and the journal's recorded
	// provenance when the claim was replayed and the effect did not run.
	openCopy := func(prior int) (*gh.PullRequest, string, error) {
		var created *gh.PullRequest
		res, err := effects.Execute(ctx, c.mutation, effects.Claim{
			Repo: fullRepo, Kind: effects.KindPullRequestCreate, Target: branch, Actor: "stage_labels",
			Inputs: claimInputs(prior, map[string]string{"base": obs.BaseRef, "supersedes": strconv.Itoa(number), "head_sha": obs.HeadSHA}),
		}, func(ctx context.Context) (effects.Result, error) {
			var apiErr error
			created, _, apiErr = c.gh.PullRequests.Create(ctx, owner, repo, &gh.NewPullRequest{
				Title: gh.Ptr(nonEmpty(strings.TrimSpace(obs.Title), fmt.Sprintf("Adopt #%d", number))),
				Head:  gh.Ptr(branch),
				Base:  gh.Ptr(obs.BaseRef),
				Body:  gh.Ptr(AdoptionPRBody(obs, rec, dcoRehome, resignedTip) + adoptionCopyConflictNote(obs, fixerResolves) + adoptionCarriedApprovalNote(obs, admittedBy)),
			})
			if apiErr != nil {
				return effects.Result{}, effects.NotApplied(apiErr)
			}
			return effects.Result{Provenance: created.GetHTMLURL()}, nil
		})
		return created, res.Provenance, err
	}
	created, provenance, err := openCopy(priorCopy)
	if replayed := adoptionReplayedPRNumber(provenance); err == nil && created == nil && replayed > 0 && replayed != priorCopy {
		// The journal replayed an earlier claim and named the PR it opened:
		// an open one is this adoption's copy; a closed-unmerged one is dead,
		// so the copy is claimed afresh rather than read as a failure.
		pr, _, getErr := c.gh.PullRequests.Get(ctx, owner, repo, replayed)
		switch {
		case getErr != nil:
			err = getErr
		case strings.EqualFold(pr.GetState(), "open"):
			c.info("stage labels: adoption claim replayed an open App PR; adopting it", "repo", obs.Display, "pr", number, "branch", branch, "replayed", replayed)
			created = pr
		case !pr.GetMerged():
			c.info("stage labels: adoption claim replayed a closed App PR; opening a fresh copy", "repo", obs.Display, "pr", number, "branch", branch, "replayed", replayed)
			if err = pushBranch(replayed); err != nil {
				return 0, err
			}
			created, _, err = openCopy(replayed)
		}
	}
	if err != nil || created == nil {
		c.warn("stage labels: adoption failed to open App PR", "repo", obs.Display, "pr", number, "branch", branch, "error", err)
		return 0, err
	}
	superseding := created.GetNumber()
	store.MarkAdopted(key, obs.HeadSHA, superseding, now)
	store.NoteAdoptReason(key, reason)
	if admittedBy != "" {
		c.carryAdmissionToCopy(obs, admittedBy, superseding, nonEmpty(created.GetHead().GetSHA(), nonEmpty(resignedTip, obs.HeadSHA)), now)
	}
	c.info("stage labels: contributor PR adopted", "repo", obs.Display, "pr", number, "head", obs.HeadSHA, "superseded_by", superseding, "branch", branch, "fence", rec.AdoptDiffFence, "forced_by", forcedBy, "fixer_resolves_conflicts", fixerResolves)
	if fixerResolves {
		c.requestAdoptionCopyRebase(ctx, store, key, obs, branch, superseding, created.GetHead().GetSHA())
	}

	if policy.AdoptSupersedeCloseFirst {
		// hivecommons/hive#15135: one PR per issue — the original closes
		// now, with the note naming the copy, not when the copy merges.
		return c.closeSupersededOriginalFirst(ctx, store, key, obs, policy, superseding, reason, rec.AdoptDiffFence, now)
	}
	body := AdoptionComment(obs, policy.HiveID, superseding, reason, grace, online)
	if forcedBy != "" {
		body = AdoptionForcedComment(obs, forcedBy, superseding, online)
	}
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

// supersedeHeadConflicts reports whether the head being superseded is red
// on a merge conflict with its base (lane reason or needs-rebase label).
func supersedeHeadConflicts(obs stageObservation) bool {
	return humanConflictReasons[obs.Reason] || humanConflictReasons[obs.LaneReason] || hasLabelFold(obs.Labels, "needs-rebase")
}

// AdoptionSupersedeConflictMarker stamps the note posted when a delegated
// supersede could not replay the head onto a fresh base.
const AdoptionSupersedeConflictMarker = "<!-- hive:adoption-supersede-conflict"

// AdoptionSupersedeConflictComment renders that note with the concrete
// failure: the base, the head and the files both sides touched.
func AdoptionSupersedeConflictComment(obs stageObservation, branch string, files []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s head=%s -->\n", AdoptionSupersedeConflictMarker, obs.HeadSHA)
	fmt.Fprintf(&b, "**Supersede failed:** the hive could not open a fresh PR with these commits. Merging `%s` into the copy branch `%s` (at `%s`) stopped on a git conflict, so no superseding PR was opened and this PR stays open.\n\n", obs.BaseRef, branch, shortSHA(obs.HeadSHA))
	if len(files) > 0 {
		b.WriteString("Files changed on both sides:\n")
		for _, f := range files {
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Rebase this branch onto `%s` (or push a merge of it) and the lane re-evaluates the new head; a new `%s` label then retries the supersede. Reason: `%s`.\n", obs.BaseRef, delegatedecision.LabelAdoptSupersede, stagelabels.AdoptBlockedSupersedeConflict)
	return b.String()
}

// replaySupersedeOntoBase merges the base into the freshly pushed hive
// branch so the superseding PR opens mergeable. It returns conflicted=true
// after a 409: the adoption is forgotten (no live copy exists), the head is
// recorded as supersede-conflict so the request is consumed and the 07x
// reason names it, and one note is posted. Other API failures return the
// error with the attempt already recorded by the caller.
func (c *Engine) replaySupersedeOntoBase(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, branch string, priorCopy int, attemptStamp string, now time.Time) (conflicted bool, comments int, err error) {
	owner, repo, number := obs.Owner, obs.Repo, obs.Number
	var status int
	_, err = effects.Execute(ctx, c.mutation, effects.Claim{
		Repo: owner + "/" + repo, Kind: effects.KindBranchPush, Target: branch, Actor: "stage_labels",
		Inputs: priorCopyInputs(priorCopy, map[string]string{"pr": strconv.Itoa(number), "head_sha": obs.HeadSHA, "base": obs.BaseRef, "reason": "supersede-replay", "attempt": attemptStamp}),
	}, func(ctx context.Context) (effects.Result, error) {
		commit, resp, apiErr := c.gh.Repositories.Merge(ctx, owner, repo, &gh.RepositoryMergeRequest{
			Base:          gh.Ptr(branch),
			Head:          gh.Ptr(obs.BaseRef),
			CommitMessage: gh.Ptr(fmt.Sprintf("Merge %s into %s (hive supersede of #%d)", obs.BaseRef, branch, number)),
		})
		if resp != nil {
			status = resp.StatusCode
		}
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		return effects.Result{Provenance: commit.GetSHA()}, nil
	})
	switch {
	case err == nil:
		c.info("stage labels: supersede replayed the base onto the hive branch", "repo", obs.Display, "pr", number, "branch", branch, "base", obs.BaseRef, "status", status)
		return false, 0, nil
	case status != http.StatusConflict:
		c.warn("stage labels: supersede replay onto the base failed", "repo", obs.Display, "pr", number, "branch", branch, "base", obs.BaseRef, "error", err)
		return false, 0, err
	}
	files := c.mergeForwardConflictFiles(ctx, owner, repo, obs.BaseRef, obs.HeadSHA)
	if c.rebaseResolver != nil {
		c.info("stage labels: supersede replay conflicts with the base; opening the copy for the fixer's rebase-resolve episode", "repo", obs.Display, "pr", number, "head", obs.HeadSHA, "branch", branch, "base", obs.BaseRef, "files", files)
		return true, 0, nil
	}
	store.ClearAdoption(key)
	store.NoteAdoptionBlocked(key, obs.HeadSHA, stagelabels.AdoptBlockedSupersedeConflict, now)
	c.warn("stage labels: supersede replay conflicts with the base; not opening the copy", "repo", obs.Display, "pr", number, "head", obs.HeadSHA, "branch", branch, "base", obs.BaseRef, "files", files, "reason", stagelabels.AdoptBlockedSupersedeConflict)
	if _, delErr := c.gh.Git.DeleteRef(ctx, owner, repo, "refs/heads/"+branch); delErr != nil && !isGitHubStatus(delErr, http.StatusNotFound) && !isGitHubStatus(delErr, http.StatusUnprocessableEntity) {
		c.warn("stage labels: could not delete the conflicting hive branch", "repo", obs.Display, "pr", number, "branch", branch, "error", delErr)
	}
	if _, _, cErr := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(AdoptionSupersedeConflictComment(obs, branch, files))}); cErr != nil {
		c.warn("stage labels: supersede conflict note failed", "repo", obs.Display, "pr", number, "error", cErr)
		return true, 0, cErr
	}
	return true, 1, nil
}

// adoptionCopyConflictNote is the paragraph appended to the copy's body
// when it opened conflicting with its base.
func adoptionCopyConflictNote(obs stageObservation, conflicting bool) string {
	if !conflicting {
		return ""
	}
	return fmt.Sprintf("\n\n**Conflicts with `%s`:** the contributor head could not be replayed onto the base, so this copy opened as it is and the hive's fixer is resolving the conflicts on this branch (rebase-resolve episode, two attempts). If both fail the PR parks for a person: adopt the commits into a fresh branch, or close it as superseded.\n", obs.BaseRef)
}

// adoptionCarriedApprovalNote is the paragraph appended to the copy's body
// when a person had approved the original head: the approval stands for
// the content the copy carries unchanged.
func adoptionCarriedApprovalNote(obs stageObservation, login string) string {
	if login == "" {
		return ""
	}
	return fmt.Sprintf("\n\n**Approval carried over:** @%s approved #%d at head `%s`. The hive could not merge that head as-is (%s), so it re-authors it here; the approval travels with the content.\n", login, obs.Number, shortSHA(obs.HeadSHA), nonEmpty(headNotMergeableAsIs(obs), nonEmpty(obs.Reason, obs.LaneReason)))
}

// requestAdoptionCopyRebase opens the fixer's rebase-resolve episode on the
// hive's conflicting copy (hivecommons/hive#15307) and records on the
// original that its copy is in the fixer's hands. The copy is the hive's
// own PR on a base-repo branch, so the fixer router admits it whatever
// the original's head reachability was; the request names the adoption as
// requester and the shared two-attempt guard applies. A resolver error is
// logged: the copy stays on its conflicting path like any hive PR.
func (c *Engine) requestAdoptionCopyRebase(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, branch string, copy int, copyHead string) {
	store.NoteAdoptCopyConflicting(key, true)
	req := RebaseRequest{Repo: obs.Owner + "/" + obs.Repo, Number: copy, Base: obs.BaseRef, Head: branch, HeadSHA: nonEmpty(copyHead, obs.HeadSHA), Attempt: 1, MaxAttempts: DefaultMergeTrainPolicy.MaxRebaseAttempts, Lane: obs.Lane, RequestedBy: AdoptionRebaseRequester}
	if err := c.rebaseResolver.RequestRebase(ctx, req); err != nil {
		c.warn("stage labels: adoption copy rebase-resolve request failed; the copy keeps its conflicting path", "repo", obs.Display, "pr", obs.Number, "superseded_by", copy, "branch", branch, "error", err)
		return
	}
	c.info("stage labels: adoption copy conflicts with the base; fixer rebase-resolve episode requested", "repo", obs.Display, "pr", obs.Number, "superseded_by", copy, "branch", branch, "base", obs.BaseRef, "head", req.HeadSHA, "requested_by", AdoptionRebaseRequester, "max_attempts", req.MaxAttempts)
}

// adoptionCopyRebaseExhausted reports whether the fixer's rebase-resolve
// episode on the hive's copy of this adoption used both attempts without
// the copy reading mergeable (the original's 085 reason
// adoption-stalled:rebase-failed-twice; hivecommons/hive#15307).
func (c *Engine) adoptionCopyRebaseExhausted(obs stageObservation, rec stagelabels.Record) bool {
	if !rec.AdoptCopyConflicting || rec.AdoptedBy <= 0 || c.rebaseEpisode == nil {
		return false
	}
	ep, ok := c.rebaseEpisode(obs.Owner+"/"+obs.Repo, rec.AdoptedBy)
	return ok && ep.Exhausted
}

// adoptionCopyRebaseFailedTwice reports whether obs is the hive's own
// adoption copy (a hive/adopt-* branch the App opened), reads conflicting,
// and the fixer's rebase-resolve episode on it is exhausted.
func (c *Engine) adoptionCopyRebaseFailedTwice(obs stageObservation) bool {
	if c.rebaseEpisode == nil || obs.Lane != stageLaneSelf || !strings.HasPrefix(obs.HeadRef, adoptBranchPrefix) || !supersedeHeadConflicts(obs) {
		return false
	}
	ep, ok := c.rebaseEpisode(obs.Owner+"/"+obs.Repo, obs.Number)
	return ok && ep.Exhausted
}

// priorCopyInputs keys a claim by the closed copy it follows, so a
// re-adoption is not answered by the first adoption's journal entry.
func priorCopyInputs(prior int, inputs map[string]string) map[string]string {
	if prior > 0 {
		inputs["prior_copy"] = strconv.Itoa(prior)
	}
	return inputs
}

// adoptionReplayedPRNumber reads the PR number from a pull_request_create
// provenance URL (…/pull/N); 0 when there is none.
func adoptionReplayedPRNumber(provenance string) int {
	_, tail, ok := strings.Cut(strings.TrimSpace(provenance), "/pull/")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimRight(tail, "/"))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// resignAdoptionCommits replays the PR's commits with the App's
// Signed-off-by added where missing (the API equivalent of
// `git rebase --signoff`), keeping each commit's tree, author and message,
// and returns the new tip. Parents inside the PR map to their rewritten
// counterparts, so a base-sync merge commit (which the DCO checks exempt and
// hivecommons/hive#14500's recovery adds) replays with its tree and both
// parents rather than refusing the rewrite (hivecommons/hive#14666). It
// returns "" when the listing does not end at the observed head or a parent
// cannot be resolved, so the caller keeps the unsigned head.
func (c *Engine) resignAdoptionCommits(ctx context.Context, obs stageObservation) (string, error) {
	opts := &gh.ListOptions{PerPage: 100}
	var all []*gh.RepositoryCommit
	for page := 0; page < stageDCOMaxPages; page++ {
		commits, resp, err := c.gh.PullRequests.ListCommits(ctx, obs.Owner, obs.Repo, obs.Number, opts)
		if err != nil {
			return "", fmt.Errorf("listing commits for %s#%d: %w", obs.Display, obs.Number, err)
		}
		all = append(all, commits...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if len(all) == 0 || all[len(all)-1].GetSHA() != obs.HeadSHA {
		return "", nil
	}
	inPR := make(map[string]bool, len(all))
	for _, rc := range all {
		if rc == nil || len(rc.Parents) == 0 || rc.GetSHA() == "" {
			return "", nil
		}
		inPR[rc.GetSHA()] = true
	}
	trailer := hgithub.BotSignedOffByTrailer(c.transport.AppBotLogin())
	rewritten := make(map[string]string, len(all))
	for _, rc := range all {
		parents := make([]*gh.Commit, 0, len(rc.Parents))
		for _, p := range rc.Parents {
			sha := p.GetSHA()
			if inPR[sha] {
				// The listing is oldest-first; a parent inside the PR
				// that is not rewritten yet means the order is not
				// topological and the replay would detach it.
				mapped, ok := rewritten[sha]
				if !ok {
					return "", nil
				}
				sha = mapped
			}
			parents = append(parents, &gh.Commit{SHA: gh.Ptr(sha)})
		}
		msg := rc.GetCommit().GetMessage()
		if len(rc.Parents) == 1 && !signedOffByRE.MatchString(msg) {
			msg = strings.TrimRight(msg, "\n") + "\n\n" + trailer + "\n"
		}
		created, _, err := c.gh.Git.CreateCommit(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), obs.Owner, obs.Repo, &gh.Commit{
			Message: gh.Ptr(msg),
			Tree:    &gh.Tree{SHA: gh.Ptr(rc.GetCommit().GetTree().GetSHA())},
			Parents: parents,
			Author:  rc.GetCommit().GetAuthor(),
		}, nil)
		if err != nil {
			return "", fmt.Errorf("re-signing %s: %w", shortSHA(rc.GetSHA()), err)
		}
		rewritten[rc.GetSHA()] = created.GetSHA()
	}
	return rewritten[obs.HeadSHA], nil
}

// adoptionDCORepaired is the reason logged and recorded when the hive
// rewrote its own unsigned commits in place with its sign-off.
const adoptionDCORepaired = "dco-rehomed"

// repairAdoptedDCOInPlace rewrites the unsigned hive commits of an
// in-place-adopted base-repo head with the App's Signed-off-by and
// force-updates the PR branch to the new tip — once per head, with the ref
// re-read first so a push that raced the rewrite is not overwritten
// (hivecommons/hive#14666). The next sweep re-reads the new head's commits
// and the DCO gate clears. Returns 0: a branch push is not a comment, so
// nothing is added to the caller's count.
func (c *Engine) repairAdoptedDCOInPlace(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, rec stagelabels.Record) (int, error) {
	if rec.DCORepairAttempted(obs.HeadSHA) {
		return 0, nil
	}
	if obs.HeadRef == "" {
		c.warn("stage labels: in-place DCO repair skipped, head ref unknown", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA)
		return 0, nil
	}
	store.MarkDCORepair(key, obs.HeadSHA)
	c.info("stage labels: re-signing hive commits on adopted PR in place", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "branch", obs.HeadRef, "unsigned_hive", obs.Detail.DCOUnsignedHive, "reason", adoptionDCORepaired)
	ref := "refs/heads/" + obs.HeadRef
	var tip string
	_, err := effects.Execute(ctx, c.mutation, effects.Claim{
		Repo: obs.Owner + "/" + obs.Repo, Kind: effects.KindBranchPush, Target: obs.HeadRef, Actor: "stage_labels",
		Inputs: map[string]string{"pr": strconv.Itoa(obs.Number), "head_sha": obs.HeadSHA, "dco_repair": "true"},
	}, func(ctx context.Context) (effects.Result, error) {
		current, _, apiErr := c.gh.Git.GetRef(ctx, obs.Owner, obs.Repo, ref)
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		if got := current.GetObject().GetSHA(); got != obs.HeadSHA {
			return effects.Result{}, effects.NotApplied(fmt.Errorf("head moved to %s before the rewrite; re-evaluating", shortSHA(got)))
		}
		signed, signErr := c.resignAdoptionCommits(ctx, obs)
		if signErr != nil {
			return effects.Result{}, effects.NotApplied(signErr)
		}
		if signed == "" {
			return effects.Result{}, effects.NotApplied(fmt.Errorf("commits of %s cannot be replayed with sign-off", shortSHA(obs.HeadSHA)))
		}
		if _, _, apiErr := c.gh.Git.UpdateRef(ctx, obs.Owner, obs.Repo, &gh.Reference{
			Ref:    gh.Ptr(ref),
			Object: &gh.GitObject{SHA: gh.Ptr(signed)},
		}, true); apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		tip = signed
		return effects.Result{Provenance: signed}, nil
	})
	if err != nil {
		c.warn("stage labels: in-place DCO repair failed; branch keeps the unsigned head", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "branch", obs.HeadRef, "error", err)
		return 0, err
	}
	store.MarkHivePushedHead(key, tip)
	c.info("stage labels: adopted PR re-signed in place", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "new_head", tip, "branch", obs.HeadRef, "reason", adoptionDCORepaired)
	// A branch push, not a comment: nothing to add to the caller's count.
	return 0, nil
}

// settleAdoption polls the superseding App PR for a PR in stage 074 (at
// most every adoptCheckInterval): merged → close the original with a note;
// closed unmerged → forget the adoption so the original returns to 072.
func (c *Engine) settleAdoption(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, now time.Time) (int, error) {
	rec, ok := store.Get(key)
	if !ok {
		return 0, nil
	}
	if rec.AdoptReason == AdoptReasonOperatorForced {
		// An operator forced this adoption; maintainer_policy: never does
		// not hand it back.
	} else if maintainer, deferred := c.maintainerReleasesAdoption(ctx, obs, policy, now); deferred {
		// No evidence either way: neither re-home nor release this pass.
		return 0, nil
	} else if maintainer {
		// maintainer_policy: never — a supersede adoption taken before the
		// rule, or before the author's permission was raised, is handed back.
		c.info("stage labels: adoption of a repository maintainer's PR released", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "superseded_by", rec.AdoptedBy)
		return c.releaseMaintainerAdoption(ctx, store, key, obs, rec.AdoptedBy)
	}
	if adoptedHeadDCORed(obs, rec) {
		canPushInPlace := c.canPushToPRBranch(ctx, obs)
		if adoptedHeadNeedsDCORepair(obs, rec, canPushInPlace) {
			return c.repairAdoptedDCOInPlace(ctx, store, key, obs, rec)
		}
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
	return c.settleSuperseder(ctx, store, key, obs, rec, policy, now)
}

// settleSuperseder polls the superseding App PR recorded on an adoption (at
// most every adoptCheckInterval) and settles the original: merged → close
// note + close; closed unmerged → forget the adoption. It runs for a PR at
// 074 and, since hivecommons/hive#14471, for any other stage the lane
// derives while the superseder is on record: a `needs-human` label (085) or
// a contributor push after the adoption (072) used to stop the poll, and the
// original stayed open with "closed automatically once #N merges" on it.
// A copy closed unmerged is an orphaned adoption (hivecommons/hive#15052):
// recoverOrphanedAdoption re-adopts, reverts, or parks it.
func (c *Engine) settleSuperseder(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, rec stagelabels.Record, policy StageLabelPolicy, now time.Time) (int, error) {
	if obs.Merged || rec.AdoptionChildClosedTwice() || rec.AdoptionCopyFailed() || !rec.SupersederPollDue(now, adoptCheckInterval) {
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
		return c.recoverOrphanedAdoption(ctx, store, key, obs, rec, policy, now)
	}
	c.settleAdoptionCopyConflict(store, key, obs, rec, sup)
	return 0, nil
}

// settleAdoptionCopyConflict clears the copy-conflicting mark once GitHub
// reads the hive's copy as mergeable (the fixer's rebase took), so the
// original's row and the 085 guard stop naming the conflict.
func (c *Engine) settleAdoptionCopyConflict(store *stagelabels.Store, key string, obs stageObservation, rec stagelabels.Record, sup *gh.PullRequest) {
	if !rec.AdoptCopyConflicting || sup == nil || !adoptionCopyReadsMergeable(sup) {
		return
	}
	store.NoteAdoptCopyConflicting(key, false)
	c.info("stage labels: adoption copy reads mergeable again; conflict resolved", "repo", obs.Display, "pr", obs.Number, "superseded_by", sup.GetNumber(), "copy_head", sup.GetHead().GetSHA(), "mergeable_state", sup.GetMergeableState())
}

// adoptionCopyReadsMergeable is GitHub's answer on the copy: mergeable
// true, or a mergeable_state that is not "dirty"; an unknown (still
// computing) answer keeps the mark.
func adoptionCopyReadsMergeable(sup *gh.PullRequest) bool {
	if sup.Mergeable != nil {
		return sup.GetMergeable()
	}
	state := strings.ToLower(strings.TrimSpace(sup.GetMergeableState()))
	return state != "" && state != "dirty" && state != "unknown"
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
			// TODO(hivecommons/hive#14589): a 403 here may be the App
			// lacking pull_requests: write on this repository, or an
			// archived/locked PR; only the former is a markUnauthorized
			// case. Route it through markUnauthorized once the cause is
			// told apart (the note already asks a person to close it).
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
	if strings.HasSuffix(strings.ToLower(obs.Author), "[bot]") {
		// An App installation is never a collaborator: the permission
		// read 404s forever and would defer adoption on every sweep.
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

// personAuthored: the PR was pushed by a person — author/human, or one of
// this hive's owners — rather than by a clanker or a registered contributor
// relay. Such a PR is never adopted (hivecommons/hive#15202).
func personAuthored(obs stageObservation, policy StageLabelPolicy) bool {
	if obs.AuthorKind == authorkind.Human {
		return true
	}
	return policy.OwnerAuthor != nil && obs.Author != "" && policy.OwnerAuthor(obs.Author)
}

// maintainerPolicy resolves the policy's maintainer rule ("" = adopt).
func (p StageLabelPolicy) maintainerPolicy() string {
	switch strings.ToLower(strings.TrimSpace(p.AdoptionMaintainerPolicy)) {
	case config.AdoptionMaintainerPolicyAsk:
		return config.AdoptionMaintainerPolicyAsk
	case config.AdoptionMaintainerPolicyNever:
		return config.AdoptionMaintainerPolicyNever
	}
	return config.AdoptionMaintainerPolicyAdopt
}

// maintainerHoldsAdoption applies governor.labels.stage.adoption.maintainer_policy
// to an adoption that is due (hivecommons/hive#14524). adopt: the author's
// role is not read and never holds (a clanker PR whose hive disconnected
// is adoptable whoever its author is). never: a maintainer's PR is held
// outright. ask: held until a person asks for it — the owner's Adopt
// (the adoption/supersede request label, `requested`) lifts the hold.
// deferred is true when the role could not be read; the caller then
// neither adopts nor records anything.
func (c *Engine) maintainerHoldsAdoption(ctx context.Context, obs stageObservation, policy StageLabelPolicy, requested bool, now time.Time) (held, deferred bool) {
	switch policy.maintainerPolicy() {
	case config.AdoptionMaintainerPolicyAdopt:
		return false, false
	case config.AdoptionMaintainerPolicyAsk:
		if requested {
			return false, false
		}
	}
	maintainer, err := c.authorMaintainsRepo(ctx, obs, now)
	if err != nil {
		return false, true
	}
	return maintainer, false
}

// maintainerReleasesAdoption is the settle-side twin: only under
// maintainer_policy: never does an existing adoption of a maintainer's PR
// get released (or a re-home refused). Under adopt and ask the adoption
// stands — under ask it was a person's decision.
func (c *Engine) maintainerReleasesAdoption(ctx context.Context, obs stageObservation, policy StageLabelPolicy, now time.Time) (maintainer, deferred bool) {
	if policy.maintainerPolicy() != config.AdoptionMaintainerPolicyNever {
		return false, false
	}
	m, err := c.authorMaintainsRepo(ctx, obs, now)
	if err != nil {
		return false, true
	}
	return m, false
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
// under .github/workflows/ since the PR's merge-base. Pushing such a
// base-sync to a contributor's fork branch is refused for an App token
// without the `workflows` grant (#6985; an installation with Workflows:
// Read and write can merge approved workflow-path PRs into the base, #14664,
// but a fork push is a separate matter), so in-place adoption would be a
// dead end (#12865). A compare failure counts as touching: a superseding PR
// is always deliverable.
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
		if maintainer, deferred := c.maintainerReleasesAdoption(ctx, obs, policy, now); deferred {
			// No evidence either way: keep the adoption as it is.
			return 0, nil
		} else if maintainer {
			// maintainer_policy: never — taken over before the rule, or
			// before the author's permission was raised: hand the PR back
			// rather than keep or re-home it.
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
	return adoptionStalledReasonWith(obs, rec, policy, now, false)
}

// adoptionStalledReasonWith is adoptionStalledReason with the copy's
// rebase-resolve verdict: an exhausted episode on a still-conflicting copy
// (hivecommons/hive#15307) is the one stall a live superseder can carry.
func adoptionStalledReasonWith(obs stageObservation, rec stagelabels.Record, policy StageLabelPolicy, now time.Time, copyRebaseExhausted bool) string {
	if obs.HeadSHA == "" {
		return ""
	}
	if rec.AdoptedBy > 0 {
		if copyRebaseExhausted && rec.AdoptCopyConflicting {
			return stagelabels.LaneReasonAdoptionStalledPrefix + stagelabels.LaneReasonRebaseFailedTwice
		}
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
	copyExhausted := c.adoptionCopyRebaseExhausted(obs, rec)
	stalledReason := func(rec stagelabels.Record) string {
		return adoptionStalledReasonWith(obs, rec, policy, now, copyExhausted)
	}
	if rehomeReason == "" && stalledReason(rec) != "" {
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
		if rec.AdoptEscalatedReason == adoptionWorkflowRefusedReason || rec.AdoptEscalatedReason == stagelabels.LaneReasonAdoptionChildClosedTwice || rec.AdoptEscalatedReason == stagelabels.LaneReasonAdoptionCopyFailed || (rehomeReason == "" && stalledReason(rec) != "") {
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
		if maintainer, deferred := c.maintainerReleasesAdoption(ctx, obs, policy, now); maintainer || deferred {
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
	reason = stalledReason(rec)
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
