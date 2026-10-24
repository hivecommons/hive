package automerge

import (
	"regexp"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// Adoption is not a merge. The contributor lane's diff fences (CI workflow /
// action definitions edited, tests removed) exist so the hive never merges
// such a diff unattended; applying them to ADOPTION as well
// (hivecommons/hive#12850) left the stalled PR they were meant to protect
// with nobody working it: a clanker PR that deletes tests sat at 072 with
// red CI, `needs-rebase` and an absent author, refused by the fence on
// every sweep (hivecommons/hive#13212). The fence's real purpose is to stop
// laundering — a fenced diff re-homed into an App-authored PR that the
// self-authored lane then merges on its own — so the hive now adopts and
// carries the fence forward instead:
//
//   - in place, the PR stays contributor-authored and the contributor lane
//     re-reads the diff on every pass, so the same fence routes the green
//     head to stage 085 (manual merge) exactly as before;
//   - by supersession (or a dco-rehome / in-place-stalled / signoff-rehome
//     second takeover), the App PR's body carries a marker naming the
//     fence, and every App merge lane refuses to merge a PR carrying it
//     (carriedFenceGate) — the same 085 route, a person merges.
//
// Supersession additionally refuses workflow/action edits before publishing
// an upstream branch: merge gating alone cannot protect workflow execution
// in the repository context (hivecommons/hive#14055). In-place adoption
// remains available for those edits; where only supersession could finish
// the work, the refusal is a terminal verdict for the head: the attempt is
// recorded, one note asks a maintainer to push/merge or cherry-pick, and the
// PR goes to 085 until the head moves.
//
// Only a verdict the fence could not reach — the head moved since the lane
// recorded it, or the file list is incomplete / unreadable — still defers
// adoption to the next sweep.

// CarriedFenceMarkerPrefix opens the HTML comment the superseding App PR's
// body carries when the adopted head tripped a contributor-lane diff fence.
// The merge lanes read it back with carriedFenceReason.
const CarriedFenceMarkerPrefix = "<!-- hive:carried-fence"

var carriedFencePattern = regexp.MustCompile(regexp.QuoteMeta(CarriedFenceMarkerPrefix) + `\s+reason=(\S+)`)

// adoptionFenceDefers reports whether a non-empty adoptionDiffRefusal
// verdict is one that could not judge the head at all (it moved, or its
// file list is incomplete / unreadable): adoption waits for the next sweep.
// Every other verdict is a diff fence the adoption carries forward.
func adoptionFenceDefers(refusal string) bool {
	return refusal == adoptionReasonHeadChanged || refusal == contributorLaneReasonFilesEvidence
}

// carriedFenceReasons are the diff-fence verdicts a superseding PR may
// carry; anything else in a marker is ignored (a body is editable).
var carriedFenceReasons = map[string]bool{
	contributorLaneReasonTestsRemoved: true,
	contributorLaneReasonWorkflowPath: true,
}

// CarriedFenceMarker renders the body marker for a fence reason.
func CarriedFenceMarker(reason string, supersedes int) string {
	var b strings.Builder
	b.WriteString(CarriedFenceMarkerPrefix)
	b.WriteString(" reason=")
	b.WriteString(reason)
	if supersedes > 0 {
		b.WriteString(" from=#")
		b.WriteString(strconv.Itoa(supersedes))
	}
	b.WriteString(" -->")
	return b.String()
}

// carriedFenceReason returns the diff-fence reason a PR body carries, or "".
func carriedFenceReason(body string) string {
	m := carriedFencePattern.FindStringSubmatch(body)
	if len(m) != 2 || !carriedFenceReasons[m[1]] {
		return ""
	}
	return m[1]
}

// carriedFenceGate refuses to merge a PR whose body carries a diff fence
// from the contributor PR it superseded: the fence travelled with the diff,
// so the App lanes hand the merge to a person (stage 085) as the contributor
// lane would have. It returns the carried contributor-lane reason, or "".
func carriedFenceGate(displayRepo string, pr *gh.PullRequest, info func(string, ...any)) string {
	reason := carriedFenceReason(pr.GetBody())
	if reason == "" {
		return ""
	}
	if info != nil {
		info("automerge sweep held PR carrying a contributor-lane diff fence; merge stays manual", "repo", displayRepo, "pr", pr.GetNumber(), "fence", reason)
	}
	return reason
}

// adoptionFenceNote is the sentence the adoption comments add when the
// adopted head tripped a diff fence: the hive repairs, a person merges.
func adoptionFenceNote(fence string) string {
	if fence == "" {
		return ""
	}
	cause := decisionCause(fence)
	if cause == "" {
		cause = "the diff tripped the contributor lane's `" + fence + "` fence"
	}
	return " The merge stays manual: " + cause + "; once CI is green a maintainer reviews and merges it (stage 085)."
}
