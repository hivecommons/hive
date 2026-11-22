package automerge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// humanMergePathLiftMarker opens the comment the stage-label pass leaves when
// it lifts a human-merge-path hold the PR's current head no longer explains:
// the head touches no listed path, or it qualifies for the pin-only
// exception. The head SHA follows it, so each head is noticed (and lifted)
// at most once, whichever reason lifted it; the pin-only release enforces
// that too (hgithub.HumanMergePathLiftMarker).
const humanMergePathLiftMarker = hgithub.HumanMergePathLiftMarker

// humanMergePathStaleReason is the hold reason the row and the 085 comment
// carry while a stale human-merge-path hold stays on (a second hold source,
// or the write-back gate refused the lift): never "it changes a human-merge
// path" for a head that changes none.
const humanMergePathStaleReason = "a human-merge path an earlier head changed; the current head touches none"

// otherHoldSourcesTransport is the optional transport hook that lists a held
// PR's hold sources besides the human-merge-path refusal
// (hgithub.Client.OtherHoldSourcesForPR). Without it no hold is lifted.
type otherHoldSourcesTransport interface {
	OtherHoldSourcesForPR(repo string, number int, labels []string, comments []*gh.IssueComment) []string
}

// humanMergePathLiftNotice is the lift comment's text after its marker line.
func humanMergePathLiftNotice(head string) string {
	return "Hold lifted: this PR no longer touches a path in `" + hgithub.HumanMergePathsConfigKey +
		"` at " + head + ". The hive set `hold` when an earlier head changed one; the merge lanes take the PR from here. A hold a person sets is never lifted this way."
}

// humanMergePathPinOnlyLiftNotice is the lift comment's text after its marker
// line when the current head qualifies for auto_merge.human_merge_pin_only.
func humanMergePathPinOnlyLiftNotice(head string) string {
	return "Hold lifted: the head at " + head + " qualifies for the pin-only exception in `" + humanMergePinOnlyConfigKey +
		"`. Every change is an approved pin replacement in verified commits by a trusted author, anchored in the upstream repository. The hive set `hold` when an earlier head did not qualify; the merge lanes take the PR from here and check the exception again before they merge. A hold a person sets is never lifted this way."
}

// hasHumanMergePathLiftComment reports whether the App already left the lift
// notice for head.
func hasHumanMergePathLiftComment(comments []*gh.IssueComment, appBot, head string) bool {
	return hgithub.HasHumanMergePathLiftNotice(comments, appBot, head)
}

// canonicalHoldLabelTransport is the optional transport hook that names the
// hive's own hold label (hgithub.Client.CanonicalHoldLabel). Without it the
// pin-only release treats the label as `hold` and lifts only a single hold.
type canonicalHoldLabelTransport interface {
	CanonicalHoldLabel() string
}

// hiveHoldLabel is the hive's own hold label, "" when the transport does not
// say.
func (c *Engine) hiveHoldLabel() string {
	if c == nil {
		return ""
	}
	if t, ok := c.transport.(canonicalHoldLabelTransport); ok {
		return t.CanonicalHoldLabel()
	}
	return ""
}

// staleHumanMergePathHold is the provenance a hive-set human-merge-path hold
// is recorded with once the current head touches no such path.
func staleHumanMergePathHold(prov *stagelabels.HoldProvenance) *stagelabels.HoldProvenance {
	return &stagelabels.HoldProvenance{By: stagelabels.HoldByHive, Source: stagelabels.HoldSourceLabel, Reason: humanMergePathStaleReason, Since: prov.Since}
}

// liftStaleHumanMergePathHold re-matches the current head of a PR the hive
// held for auto_merge.human_merge_paths against those paths, with the reads
// and matcher the lanes' gate uses (humanMergePathGate). stale is true when
// the complete changed-file list matches none of them. It then removes
// `hold` and posts one notice for the head, but only when prov is the hive's
// own human-merge-path hold, no other hold source exists, the head has not
// been noticed before, the write-back gate allows human_merge_path_release,
// and the head did not move while the files were read. A head that still
// matches goes to liftPinOnlyHumanMergePathHold (stale false). comments are
// the PR's complete comment list when the caller already read it (the hold
// provenance read), nil to have it listed here. A hold a person set never
// reaches this (the caller only passes a hive-set provenance). Anything it
// cannot establish keeps the hold.
func (c *Engine) liftStaleHumanMergePathHold(ctx context.Context, obs stageObservation, prov *stagelabels.HoldProvenance, comments []*gh.IssueComment) (lifted, stale bool, err error) {
	if c == nil || c.gh == nil || c.transport == nil || !prov.SelfImposed() || !prov.HumanMergePath() {
		return false, false, nil
	}
	repo := obs.Owner + "/" + obs.Repo
	patterns := c.currentHumanMergePaths(repo)
	if len(patterns) == 0 {
		return false, false, nil
	}
	pr, _, err := c.gh.PullRequests.Get(ctx, obs.Owner, obs.Repo, obs.Number)
	if err != nil {
		c.debug("stage labels: human-merge-path hold kept; current head unreadable", "repo", obs.Display, "pr", obs.Number, "error", err)
		return false, false, nil
	}
	files, err := ListChangedFiles(ctx, c.gh, obs.Owner, obs.Repo, pr)
	if err != nil {
		c.debug("stage labels: human-merge-path hold kept; current head's files unreadable", "repo", obs.Display, "pr", obs.Number, "error", err)
		return false, false, nil
	}
	head := pr.GetHead().GetSHA()
	if head == "" {
		return false, false, nil
	}
	if matches := intent.HumanMergePathMatches(files, patterns); len(matches) > 0 {
		lifted, err = c.liftPinOnlyHumanMergePathHold(ctx, obs, pr, files, matches, comments)
		return lifted, false, err
	}
	_, appBot, ok, err := c.humanMergePathLiftCleared(ctx, obs, head, comments, "stage labels: human-merge-path hold no longer applies but another hold source keeps it")
	if err != nil || !ok {
		return false, true, err
	}
	body := humanMergePathLiftMarker + head + " -->\n" + humanMergePathLiftNotice(head)
	item := writebackItemForObservation(obs)
	item.HiveAuthored = strings.EqualFold(normalizeBotLogin(obs.Author), normalizeBotLogin(appBot))
	if d := c.writebackAllowed(repo, item, writeback.KindHumanMergePathRelease, ""); !d.Allowed {
		c.info("stage labels: human-merge-path hold no longer applies; lift refused by the write-back gate", "repo", obs.Display, "pr", obs.Number, "head", head, "reason", d.Reason)
		return false, true, nil
	}
	// The files endpoint is not SHA-addressable: confirm the head did not
	// move while it was paged before taking the hold off.
	current, _, err := c.gh.PullRequests.Get(ctx, obs.Owner, obs.Repo, obs.Number)
	if err != nil {
		return false, true, fmt.Errorf("re-reading head: %w", err)
	}
	if current.GetHead().GetSHA() != head {
		return false, true, nil
	}
	if _, err := c.gh.Issues.RemoveLabelForIssue(ctx, obs.Owner, obs.Repo, obs.Number, url.PathEscape(hgithub.HumanMergePathHoldLabel)); err != nil && !isGitHubStatus(err, http.StatusNotFound) { // writeback:gated
		return false, true, fmt.Errorf("removing %s label: %w", hgithub.HumanMergePathHoldLabel, err)
	}
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
		return true, true, fmt.Errorf("posting lift comment: %w", err)
	}
	c.noteOwnStageComment(obs.Owner, obs.Repo, obs.Number)
	c.info("stage labels: lifted human-merge-path hold; current head touches no human-merge path", "repo", obs.Display, "pr", obs.Number, "head", head)
	return true, true, nil
}

// humanMergePathLiftCleared runs the checks every human-merge-path lift
// makes before it writes: the pass observed head (an older one waits for
// the next pass), no other hold source (othersLog is the log line when one
// keeps the hold), and no lift notice yet for head. A head the hive lifted
// once and that is held again stays held: whatever held it again owns it
// now. comments are the PR's comments when the caller has them (nil lists
// them). It returns the comments and the App's login; ok false keeps the
// hold.
func (c *Engine) humanMergePathLiftCleared(ctx context.Context, obs stageObservation, head string, comments []*gh.IssueComment, othersLog string) (_ []*gh.IssueComment, appBot string, ok bool, err error) {
	if obs.HeadSHA != "" && obs.HeadSHA != head {
		return nil, "", false, nil
	}
	others, ok := c.transport.(otherHoldSourcesTransport)
	if !ok {
		return nil, "", false, nil
	}
	if comments == nil {
		comments, err = c.listAllIssueComments(ctx, obs.Owner, obs.Repo, obs.Number)
		if err != nil {
			return nil, "", false, fmt.Errorf("listing comments: %w", err)
		}
	}
	if sources := others.OtherHoldSourcesForPR(obs.Owner+"/"+obs.Repo, obs.Number, obs.Labels, comments); len(sources) > 0 {
		c.info(othersLog, "repo", obs.Display, "pr", obs.Number, "head", head, "sources", strings.Join(sources, ","))
		return nil, "", false, nil
	}
	appBot = strings.TrimSpace(c.transport.AppBotLogin())
	if hasHumanMergePathLiftComment(comments, appBot, head) {
		return nil, "", false, nil
	}
	return comments, appBot, true, nil
}

// liftPinOnlyHumanMergePathHold lifts the hive's human-merge-path hold from a
// head that still touches a listed path, once the repository's enabled
// auto_merge.human_merge_pin_only exception admits that head. The gate
// (humanMergePathGate) holds a pin bump that fails the exception, for
// example when the upstream floating tag moved before the bot's head caught
// up; every lane then skips the held PR before it reaches the gate, so a
// corrected head would otherwise never be checked again. It judges the head
// with the gate's own check (humanMergePinOnlyExempt) on the gate's file
// list and matches. The guards are the stale lift's
// (humanMergePathLiftCleared, the write-back gate's
// human_merge_path_release), and the label comes off through the shared
// pin-only release (hgithub.ReleasePinOnlyHumanMergePathHold), which re-reads
// the head and accepts only a hold whose every transition is the App's.
//
// The hive may lift the hold, and the gate may put it back when the lane
// re-checks that head (the tag moved again, an API error). The gate posts no
// second notice then, so the head it re-held is taken to be the head of the
// newest lift notice: every head that already has a lift notice stays held
// (humanMergePathLiftCleared, and the release itself), and a later head that
// qualifies is lifted. Should the bot push between the lift and the lane's
// next pass, the head the gate re-held carries no lift notice and is judged
// once more; the lane re-checks the exception before it merges either way.
// Such cycles are accepted only on a hive with a hive_id: without one the
// dashboard and admin hold write the same `hold` as the App with no notice,
// so only the gate's single hold is lifted (hiveHoldLabel).
//
// A repository without the exception, or a diff that cannot qualify, costs
// no further API call, and a hold the release would keep (a person touched
// the label, the gate held this very head) costs no commit or upstream read.
// Anything it cannot establish keeps the hold.
func (c *Engine) liftPinOnlyHumanMergePathHold(ctx context.Context, obs stageObservation, pr *gh.PullRequest, files []intent.ChangedFile, matches []string, comments []*gh.IssueComment) (bool, error) {
	if _, _, candidate, why := c.humanMergePinOnlyCandidate(obs.Owner, obs.Repo, pr, files, matches); !candidate {
		if why != "" {
			c.debug("stage labels: human-merge-path hold kept; current head is not a pin-only change", "repo", obs.Display, "pr", obs.Number, "config", humanMergePinOnlyConfigKey, "reason", why)
		}
		return false, nil
	}
	head := pr.GetHead().GetSHA()
	comments, appBot, ok, err := c.humanMergePathLiftCleared(ctx, obs, head, comments, "stage labels: human-merge-path hold kept for another hold source; pin-only exception not checked")
	if err != nil || !ok {
		return false, err
	}
	holdLabel := c.hiveHoldLabel()
	releasable, err := hgithub.PinOnlyHumanMergePathHoldReleasable(ctx, c.gh, obs.Owner, obs.Repo, obs.Number, appBot, head, holdLabel, comments)
	if err != nil {
		return false, fmt.Errorf("pin-only release provenance: %w", err)
	}
	if !releasable {
		c.logPinOnlyLiftOnce(obs, head, "provenance", "stage labels: human-merge-path hold kept; the pin-only release cannot lift it (a person touched the label, the gate held this head, or the hive cannot tell its own re-hold from a person's hold)", "repo", obs.Display, "pr", obs.Number, "head", head)
		return false, nil
	}
	if exempt, why := c.humanMergePinOnlyExempt(ctx, obs.Owner, obs.Repo, pr, files, matches); !exempt {
		c.logPinOnlyLiftOnce(obs, head, "not-applied", "stage labels: human-merge-path hold kept; pin-only exception not applied to the current head", "repo", obs.Display, "pr", obs.Number, "head", head, "config", humanMergePinOnlyConfigKey, "reason", why)
		return false, nil
	}
	repo := obs.Owner + "/" + obs.Repo
	allowed := func(item writeback.Item) bool {
		d := c.writebackAllowed(repo, item, writeback.KindHumanMergePathRelease, "")
		if !d.Allowed {
			c.logPinOnlyLiftOnce(obs, head, "writeback", "stage labels: current head qualifies for the pin-only exception; lift refused by the write-back gate", "repo", obs.Display, "pr", obs.Number, "head", head, "reason", d.Reason)
		}
		return d.Allowed
	}
	released, err := hgithub.ReleasePinOnlyHumanMergePathHold(ctx, c.gh, allowed, obs.Owner, obs.Repo, obs.Number, appBot, head, holdLabel)
	if err != nil {
		return false, fmt.Errorf("pin-only release: %w", err)
	}
	if !released {
		c.debug("stage labels: current head qualifies for the pin-only exception; hold kept, the head moved or the hold changed during the release", "repo", obs.Display, "pr", obs.Number, "head", head)
		return false, nil
	}
	// The label comes off before the notice is posted. Known limit: when
	// the post fails, this head has no lift notice, so after the gate
	// re-holds it the pass may judge and lift it once more. Posting first
	// is worse: a removal that then fails leaves a notice that keeps this
	// head held for good.
	body := humanMergePathLiftMarker + head + " -->\n" + humanMergePathPinOnlyLiftNotice(head)
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
		return true, fmt.Errorf("posting lift comment: %w", err)
	}
	c.stageTrackerOrShared().forgetPinOnlyLiftLog(stageKey(obs.Owner, obs.Repo, obs.Number))
	c.noteOwnStageComment(obs.Owner, obs.Repo, obs.Number)
	c.info("stage labels: lifted human-merge-path hold; current head qualifies for the pin-only exception", "repo", obs.Display, "pr", obs.Number, "head", head, "config", humanMergePinOnlyConfigKey)
	return true, nil
}

// pinOnlyLiftLoggedCap bounds StageTracker.pinOnlyLiftLogged: a PR that
// closes while held never comes back to drop its entry, so past the cap the
// memory starts over (each verdict then logs at info once more).
const pinOnlyLiftLoggedCap = 2048

// logPinOnlyLiftOnce logs a pin-only lift verdict at info the first time a
// pass reaches it for this PR's head, and at debug on every later pass, so a
// PR held for a person does not repeat the line each pass. The memory lives
// on the process-shared stage tracker: the queued sweep builds a fresh
// engine for every stage pass.
func (c *Engine) logPinOnlyLiftOnce(obs stageObservation, head, verdict, msg string, args ...any) {
	t := c.stageTrackerOrShared()
	key := stageKey(obs.Owner, obs.Repo, obs.Number)
	seen := head + " " + verdict
	t.mu.Lock()
	first := t.pinOnlyLiftLogged[key] != seen
	if first {
		if t.pinOnlyLiftLogged == nil || len(t.pinOnlyLiftLogged) >= pinOnlyLiftLoggedCap {
			t.pinOnlyLiftLogged = map[string]string{}
		}
		t.pinOnlyLiftLogged[key] = seen
	}
	t.mu.Unlock()
	if first {
		c.info(msg, args...)
		return
	}
	c.debug(msg, args...)
}

// forgetPinOnlyLiftLog drops key's pin-only lift log memory: the PR wears no
// hold any more, or the pass just lifted it.
func (t *StageTracker) forgetPinOnlyLiftLog(key string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.pinOnlyLiftLogged, key)
	t.mu.Unlock()
}
