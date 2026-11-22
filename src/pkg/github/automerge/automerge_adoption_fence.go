package automerge

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
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
// The merge lanes read it back with carriedFenceReason, and the edit_pr relay
// re-adds it when a body rewrite drops it (hivecommons/hive#19401).
const CarriedFenceMarkerPrefix = hgithub.CarriedFenceMarkerPrefix

const (
	// carriedFenceRESTCaller tags the adopting PR's file re-read.
	carriedFenceRESTCaller = "hive:carried_fence_recheck"
	// carriedFenceDropLogCap bounds the once-per-head drop log memory; the
	// map is reset when it fills, which at worst repeats a log line.
	carriedFenceDropLogCap = 4096
)

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
//
// The marker records what the superseded PR's diff did, not what the
// adopting PR's diff does now: the hive's own fixes may have dropped the
// workflow edit or restored the tests (hivecommons/hive#19252 carried
// workflow-path from #19243 while touching no workflow file). So the
// carried reason is re-evaluated against the adopting PR's current diff on
// every head (carriedFenceStillApplies) and dropped for a head whose diff
// no longer trips it, with one log line per head. Re-evaluation per head
// means a later push that trips the fence again re-arms it. A file list
// that cannot be read, or is incomplete, keeps the fence (fail closed).
func (c *Engine) carriedFenceGate(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest) string {
	reason := carriedFenceReason(pr.GetBody())
	if reason == "" {
		return ""
	}
	if c.carriedFenceDropped(ctx, displayRepo, owner, repo, pr, reason) {
		return ""
	}
	c.info("automerge sweep held PR carrying a contributor-lane diff fence; merge stays manual", "repo", displayRepo, "pr", pr.GetNumber(), "fence", reason)
	return reason
}

// carriedFenceDropped reports whether the carried fence reason no longer
// applies to the adopting PR's current diff. Any read failure, an
// incomplete file list or a missing client keeps the fence.
func (c *Engine) carriedFenceDropped(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, reason string) bool {
	if c == nil || c.gh == nil || pr == nil {
		return false
	}
	head := strings.TrimSpace(pr.GetHead().GetSHA())
	if head == "" {
		return false
	}
	files, err := ListChangedFiles(hgithub.WithRESTCaller(ctx, carriedFenceRESTCaller), c.gh, owner, repo, pr)
	if err != nil {
		c.warn("carried diff fence: could not re-read the adopting PR's files; fence kept", "repo", displayRepo, "pr", pr.GetNumber(), "head", head, "fence", reason, "error", err)
		return false
	}
	if carriedFenceStillApplies(reason, files) {
		return false
	}
	if c.noteCarriedFenceDrop(owner, repo, pr.GetNumber(), head) {
		c.info("carried diff fence dropped: the adopting PR's current diff no longer trips it", "repo", displayRepo, "pr", pr.GetNumber(), "head", head, "fence", reason)
	}
	return true
}

// carriedFenceStillApplies re-runs the contributor lane's check for reason
// against files. workflow-path holds while any changed file, or a renamed
// file's previous path, is under the workflow / action directories;
// tests-removed holds while the diff still trips the tests-removed fence.
// An unknown reason always applies.
func carriedFenceStillApplies(reason string, files []intent.ChangedFile) bool {
	switch reason {
	case contributorLaneReasonWorkflowPath:
		for _, f := range files {
			if intent.PathMatchesAny(f.Filename, contributorLaneWorkflowPaths) {
				return true
			}
			if prev := strings.TrimSpace(f.PreviousFilename); prev != "" && intent.PathMatchesAny(prev, contributorLaneWorkflowPaths) {
				return true
			}
		}
		return false
	case contributorLaneReasonTestsRemoved:
		return contributorLaneTestsRemovedReason(files) != ""
	}
	return true
}

// noteCarriedFenceDrop records that the carried fence was dropped for head
// and reports whether this is the first time, so the drop is logged once
// per head rather than every sweep.
func (c *Engine) noteCarriedFenceDrop(owner, repo string, number int, head string) bool {
	key := owner + "/" + repo + "#" + strconv.Itoa(number) + "@" + strings.ToLower(head)
	c.carriedFenceDropMu.Lock()
	defer c.carriedFenceDropMu.Unlock()
	if c.carriedFenceDropLogged == nil {
		c.carriedFenceDropLogged = map[string]bool{}
	}
	if c.carriedFenceDropLogged[key] {
		return false
	}
	if len(c.carriedFenceDropLogged) >= carriedFenceDropLogCap {
		c.carriedFenceDropLogged = map[string]bool{}
	}
	c.carriedFenceDropLogged[key] = true
	return true
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
