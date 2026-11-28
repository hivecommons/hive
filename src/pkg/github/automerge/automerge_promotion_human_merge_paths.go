package automerge

// Human-merge paths on a promotion (auto_merge.promotions[].inherit_human_merge_paths).
//
// auto_merge.human_merge_paths is one list per repository, not per branch.
// With the rule's inherit_human_merge_paths off (the default) the list is
// asked on the way into the target, and a promotion that changes a listed
// file is left for a person, as on every lane. With it on, the promotion
// lane does not ask.
//
// What the hive proves for such a promotion is what it proves for every
// promotion: the head's tree, and the tree of GitHub's test merge, are the
// source branch's current tree, and the source takes changes only through a
// pull request. It does NOT check how or when a listed file's content
// reached the source. So the option is not "a person already merged this":
// a listed file can reach the target although no person merged that content
// under the list, when
//
//   - the path was added to human_merge_paths after the content was already
//     on the source;
//   - something the list does not bind landed it on the source (the list
//     binds only the hive's own merges: another bot's or GitHub's own
//     auto-merge, or a person with merge rights, is not asked);
//   - a human_merge_pin_only exception or a bypass actor landed it there;
//   - the source's own protection is weaker than the target's;
//   - the head is a single commit on the target tip and a person had
//     changed the listed file only on the target (a hot-fix): the source's
//     version replaces it (in the source-tip shape the test merge differs
//     from the source tree and the lane hands the promotion back);
//   - the source is itself the target of a rule with the option on.
//
// Workflows triggered by a push to the target run as soon as the hive
// merges, so the merge into the source is the last look a person gets.
// There is no carve-out for the workflow that opens promotions or for any
// other file: the owner chooses the option per rule, knowing that
// (src/docs/hive-merge.md, "Files a person must merge").
//
// The option lifts the hive's own human-merge-path stop and nothing else,
// and only on the promotion lane:
//
//   - the lane's gate (humanMergePathGate in trySweepSelfAuthoredPR) is not
//     asked for a head the lane owns, so the pull request is neither refused
//     as human-merge-path nor labelled needs-owner-merge
//     (promotionInheritedHumanMergePaths);
//   - a needs-owner-merge label the hive itself put on for that refusal
//     comes off in the stage pass, once per head, under the same provenance
//     check as the owner-approval release
//     (releaseInheritedHumanMergePathLabel). A label a person put on stays,
//     and so does the human-merge title rule: the lanes stop on both before
//     they reach the gate (humanMergeRequiredReason).
//
// Every other lane and the merge-request relay keep the list in full, so a
// pull request the lane does not own (the head stopped matching the source,
// the lane handed it back, the self-authored sweep is not running) takes
// today's path, human-merge paths included. Holds, blocked and exempt
// labels, the sentinel label, the review verdict hold, the intent gate,
// required checks, squash_only, the expected head SHA and both live tree
// checks are untouched.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// promotionInheritConfigKey names the option in log lines and notices.
const promotionInheritConfigKey = "auto_merge.promotions[].inherit_human_merge_paths"

// promotionInheritsHumanMergePaths reports whether the promotion rule that
// names pr (base, author, head ref; no GitHub call) has
// inherit_human_merge_paths on. It reads the live configuration, so a rule
// switched off since the pull request was routed answers false. It says
// nothing about content: the caller asks only for a head the lane owns.
func (c *Engine) promotionInheritsHumanMergePaths(owner, repo string, pr *gh.PullRequest) bool {
	rule, ok := c.promotionRule(owner, repo, pr)
	return ok && rule.InheritHumanMergePaths
}

// promotionRulesInherit reports whether any of rules has
// inherit_human_merge_paths on.
func promotionRulesInherit(rules []config.PromotionRule) bool {
	for _, rule := range rules {
		if rule.InheritHumanMergePaths {
			return true
		}
	}
	return false
}

// promotionInheritedHumanMergePaths stands in for humanMergePathGate on the
// promotion lane when the rule's inherit_human_merge_paths is on. It returns
// the changed files on the repository's human-merge list, which the lane
// lets through (and records on the merge's audit entry), and no reason. It
// adds no label and posts no comment. A repository with no human-merge
// paths costs no API call.
//
// The complete changed-file list is still required, as the gate requires
// it: the hive records which listed files it let through, so a list it
// cannot read fails closed with autoMergeReasonHumanMergePathEvidence and
// the error, and the next pass reads it again.
func (c *Engine) promotionInheritedHumanMergePaths(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest) ([]string, string, error) {
	patterns := c.currentHumanMergePaths(owner + "/" + repo)
	if len(patterns) == 0 {
		return nil, "", nil
	}
	files, err := ListChangedFiles(ctx, c.gh, owner, repo, pr)
	if err != nil {
		return nil, autoMergeReasonHumanMergePathEvidence, err
	}
	matches := intent.HumanMergePathMatches(files, patterns)
	if len(matches) > 0 && c.markTerminalHoldLogged(owner, repo, pr.GetNumber(), pr.GetHead().GetSHA(), "promotion-human-merge-paths-inherited") {
		c.info("promotion lane: the rule has inherit_human_merge_paths on; files on the human-merge list not left for a person", "repo", displayRepo, "pr", pr.GetNumber(), "head", pr.GetHead().GetSHA(), "paths", matches, "config", promotionInheritConfigKey)
	}
	return matches, "", nil
}

// promotionInheritedLiftNotice is the lift comment's text after its marker
// line when the promotion lane owns the head under a rule whose
// inherit_human_merge_paths is on. It says what was checked (the trees) and
// what was not (how the listed files reached the source), and why the label
// goes: the rule's option, whoever of the hive's writers set the label and
// whenever.
func promotionInheritedLiftNotice(head string, rule config.PromotionRule) string {
	return "`" + hgithub.HumanMergePathLabel + "` removed: this pull request promotes `" + rule.Source + "` to `" + rule.Target + "`, and its head at " + head +
		" and GitHub's test merge of it carry exactly the current tree of `" + rule.Source + "`. " +
		"The hive's own label is removed because this repository's promotion rule for `" + rule.Target + "` has `inherit_human_merge_paths` on (`auto_merge.promotions`): with it, files on the human-merge list (`" + hgithub.HumanMergePathsConfigKey + "`) do not leave a promotion for a person. " +
		"The hive checked the trees only. It did not check how or when those files reached `" + rule.Source + "`. " +
		"The promotion lane merges the pull request once its checks are green and every other gate is clear, and checks both trees again right before it merges. " +
		"A label a person sets, and any `hold`, is never removed this way. To keep this promotion for a person, add `hold`."
}

// promotionObservedRuleInherits matches the stage pass's own observation
// against the repository's promotion rules, with no GitHub call: the base
// ref, head ref and author a lane recorded for the pull request. known is
// false when the observation lacks any of the three (a lane that recorded
// no payload), and the caller then reads the pull request. Otherwise inherits
// says whether a rule with inherit_human_merge_paths on names it, so a
// labelled pull request that is no promotion costs no read. An observation
// with no head repository is taken to be in the base repository; the read
// that follows a match decides for real.
func promotionObservedRuleInherits(rules []config.PromotionRule, obs stageObservation) (inherits, known bool) {
	base, ref, author := strings.TrimSpace(obs.BaseRef), strings.TrimSpace(obs.HeadRef), strings.TrimSpace(obs.Author)
	if base == "" || ref == "" || author == "" {
		return false, false
	}
	headRepo := strings.TrimSpace(obs.HeadRepo)
	if headRepo == "" {
		headRepo = obs.Owner + "/" + obs.Repo
	}
	pr := &gh.PullRequest{
		User: &gh.User{Login: gh.Ptr(author)},
		Head: &gh.PullRequestBranch{Ref: gh.Ptr(ref), Repo: &gh.Repository{FullName: gh.Ptr(headRepo)}},
		Base: &gh.PullRequestBranch{Ref: gh.Ptr(base)},
	}
	rule, ok := hgithub.PromotionRuleFor(rules, obs.Owner, obs.Repo, pr)
	return ok && rule.InheritHumanMergePaths, true
}

// promotionInheritNotCovered is promotionInheritVerified's answer for a pull
// request the option does not cover.
const promotionInheritNotCovered = "not-covered-by-the-option"

// promotionInheritVerified asks GitHub now, past the routing memo, whether
// the promotion lane owns pr under a rule whose inherit_human_merge_paths is
// on. It is the label release's own check, made the way the lane checks
// right before a merge (promotionMergeRecheck): the rule is read from the
// live configuration, and both tree checks run against GitHub
// (hgithub.VerifyPromotion, then hgithub.VerifyPromotionMerge on GitHub's
// test merge). The memo promotionLanePRIn keeps is neither read nor
// written, and nothing is audited: a refusal here only keeps a label.
//
// It also holds the lane's routing conditions (promotionLaneOwns): a
// self-authored sweep runs over this client, the author has no lane of its
// own, and the lane has not handed this head back. why is "" only when all
// of it holds. It is promotionInheritNotCovered for a pull request the
// option does not cover at all (no rule with the option names it, its
// author has a lane of its own, the lane is not running), which is not
// worth a log line; otherwise it names the check that did not hold, and err
// is the read failure behind it, if any.
func (c *Engine) promotionInheritVerified(ctx context.Context, owner, repo string, pr *gh.PullRequest) (rule config.PromotionRule, why string, err error) {
	if !c.selfAuthoredLaneRunning() {
		return config.PromotionRule{}, promotionInheritNotCovered, nil
	}
	if _, own := c.sweepLaneForAuthor(hgithub.SafeGetLogin(pr.GetUser())); own {
		return config.PromotionRule{}, promotionInheritNotCovered, nil
	}
	rule, ok := c.promotionRule(owner, repo, pr)
	if !ok || !rule.InheritHumanMergePaths {
		return config.PromotionRule{}, promotionInheritNotCovered, nil
	}
	if c.promotionHandedBack(owner, repo, pr.GetNumber(), strings.TrimSpace(pr.GetHead().GetSHA())) {
		return rule, "promotion-handed-back", nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, promotionCheckTimeout)
	defer cancel()
	check, err := hgithub.VerifyPromotion(checkCtx, c.gh, owner, repo, pr, rule)
	if check.Matched {
		check, err = hgithub.VerifyPromotionMerge(checkCtx, c.gh, owner, repo, pr, check)
	}
	if !check.Matched {
		why = check.Reason
		if why == "" {
			why = hgithub.PromotionReasonReadFailed
		}
		return rule, why, err
	}
	return rule, "", nil
}

// logInheritReleaseOnce logs why the inherited release kept a label at info
// the first time a pass reaches that verdict for the head, and at debug on
// every later pass, so a promotion that waits (for a person's hold, the
// write-back gate, GitHub's test merge) does not repeat the line each pass.
// The memory is the sweep memory every engine over the same client shares
// (markTerminalHoldLogged), keyed by pull request, head and verdict, so it
// does not share a slot with the pin-only lift's own once-log.
func (c *Engine) logInheritReleaseOnce(obs stageObservation, head, verdict, msg string, args ...any) {
	if c.markTerminalHoldLogged(obs.Owner, obs.Repo, obs.Number, head, "promotion-inherit-release:"+verdict) {
		c.info(msg, args...)
		return
	}
	c.debug(msg, args...)
}

// releaseInheritedHumanMergePathLabel is the stage pass's half of
// inherit_human_merge_paths: it removes the needs-owner-merge label the hive
// itself put on for the human-merge-path refusal when the promotion lane
// owns the PR's current head under a rule whose inherit_human_merge_paths is
// on. That is the label the lane's own gate applied before the rule was
// switched on, or one another lane or the merge-request relay applied since:
// every lane stops on it (auto_merge.human_merge_labels) before its gate, so
// without this the promotion would stay a person's merge although the rule
// now says otherwise.
//
// The exits are ordered cheapest first, and the label comes off only after
// the last of them:
//
//  1. no rule of the repository has the option: nothing is read;
//  2. the pass's own observation (base ref, head ref, author) names no rule
//     with the option: nothing is read, so the other labelled pull requests
//     of a repository that uses the option cost no call
//     (promotionObservedRuleInherits);
//  3. no self-authored sweep runs over this client, or an attempt for this
//     head ended without removing the label less than
//     promotionInheritReleaseRecheck ago (promotionInheritReleaseTried);
//  4. the head already has a lift notice, or something else keeps the PR (a
//     person's `hold`, a pause, the reporter-trust gate): the comments the
//     pass already read decide (humanMergePathLiftClearedWith);
//  5. the pull request is read and the promotion is verified against GitHub
//     now, not from the routing memo (promotionInheritVerified): the live
//     rule and both tree checks.
//
// Then the label must be the hive's own, with the provenance the
// owner-approval release requires (hgithub.ReleaseInheritedHumanMergePathHold):
// every application of the label is the App's, the first of them beside the
// App's notice, and the label the PR wears now is the latest of them. A
// label a person added or re-added is never removed. That mode does not ask
// who removed the label in between: a person's removal that the App's gate
// answered by labelling the head again leaves an App-applied label, and
// that label is removed too. A person who wants the promotion kept adds
// `hold` (step 4): the label is already on, so there is no label event of
// theirs to tell their intent from the hive's.
//
// The write-back gate must allow human_merge_path_release, and each head is
// lifted at most once (step 4): a head the hive lifted and that is labelled
// again stays labelled, so two writers can never flap the label. It only
// removes a label. The lane still stops on a person's label or title, and
// checks both trees, the checks and every other gate before it merges.
// Anything it cannot establish keeps the label, and says why once per head
// (logInheritReleaseOnce). An attempt that ends that way is not repeated
// for the same head for promotionInheritReleaseRecheck, so a promotion whose
// release keeps being refused does not cost the reads on every pass.
func (c *Engine) releaseInheritedHumanMergePathLabel(ctx context.Context, obs stageObservation, prov *stagelabels.HoldProvenance, comments []*gh.IssueComment) (bool, error) {
	if c == nil || c.gh == nil || c.transport == nil || c.promotions == nil || !prov.SelfImposed() || !prov.HumanMergePath() {
		return false, nil
	}
	repo := obs.Owner + "/" + obs.Repo
	head := strings.TrimSpace(obs.HeadSHA)
	if head == "" {
		return false, nil
	}
	rules := c.promotions(repo)
	if !promotionRulesInherit(rules) {
		return false, nil
	}
	if inherits, known := promotionObservedRuleInherits(rules, obs); known && !inherits {
		return false, nil
	}
	if !c.selfAuthoredLaneRunning() || c.promotionInheritReleaseTried(obs.Owner, obs.Repo, obs.Number, head) {
		return false, nil
	}
	released, kept, err := c.attemptInheritedHumanMergePathRelease(ctx, obs, repo, head, comments)
	switch {
	case released:
		c.forgetPromotionInheritRelease(obs.Owner, obs.Repo, obs.Number)
	case kept:
		c.notePromotionInheritReleaseKept(obs.Owner, obs.Repo, obs.Number, head)
	}
	return released, err
}

// attemptInheritedHumanMergePathRelease is steps 4 and 5 of
// releaseInheritedHumanMergePathLabel and the release itself. kept is true
// when the attempt ended without removing the label for a reason the same
// head will meet again (another hold source, a lift notice already there,
// a live check that did not hold, the write-back gate, the label's
// history): the caller then waits promotionInheritReleaseRecheck before it
// reads GitHub for this head again. It is false when the next pass may
// well differ: a read failed outright (err), the head moved, or GitHub has
// not computed the test merge yet (hgithub.PromotionReasonMergeUnknown).
func (c *Engine) attemptInheritedHumanMergePathRelease(ctx context.Context, obs stageObservation, repo, head string, comments []*gh.IssueComment) (released, kept bool, err error) {
	_, appBot, cleared, err := c.humanMergePathLiftClearedWith(ctx, obs, head, comments, func(sources []string) {
		c.logInheritReleaseOnce(obs, head, "other-hold-source", "stage labels: the promotion rule has inherit_human_merge_paths on, but another hold source keeps the human-merge-path label", "repo", obs.Display, "pr", obs.Number, "head", head, "sources", strings.Join(sources, ","), "config", promotionInheritConfigKey)
	})
	if err != nil {
		return false, false, err
	}
	if !cleared {
		return false, true, nil
	}
	pr, _, err := c.gh.PullRequests.Get(ctx, obs.Owner, obs.Repo, obs.Number)
	if err != nil {
		return false, false, fmt.Errorf("reading PR: %w", err)
	}
	if pr.GetHead().GetSHA() != head {
		// The pass observed an older head; the next pass judges this one.
		return false, false, nil
	}
	rule, why, verifyErr := c.promotionInheritVerified(ctx, obs.Owner, obs.Repo, pr)
	if why == promotionInheritNotCovered {
		return false, true, nil
	}
	if why != "" {
		c.logInheritReleaseOnce(obs, head, "not-verified:"+why, "stage labels: the promotion rule has inherit_human_merge_paths on; human-merge-path label kept, the live promotion check did not hold", "repo", obs.Display, "pr", obs.Number, "head", head, "reason", why, "error", verifyErr)
		// GitHub recomputes a test merge it has not finished, and a read
		// that failed may succeed next time: the next pass asks again.
		// Every other outcome waits for the recheck.
		return false, verifyErr == nil && why != hgithub.PromotionReasonMergeUnknown, nil
	}
	gateRefused := false
	allowed := func(item writeback.Item) bool {
		d := c.writebackAllowed(repo, item, writeback.KindHumanMergePathRelease, "")
		if !d.Allowed {
			gateRefused = true
			c.logInheritReleaseOnce(obs, head, "writeback", "stage labels: the promotion rule has inherit_human_merge_paths on; label release refused by the write-back gate", "repo", obs.Display, "pr", obs.Number, "head", head, "reason", d.Reason)
		}
		return d.Allowed
	}
	released, err = hgithub.ReleaseInheritedHumanMergePathHold(ctx, c.gh, allowed, obs.Owner, obs.Repo, obs.Number, appBot, head)
	if err != nil {
		return false, false, fmt.Errorf("inherited release: %w", err)
	}
	if !released {
		// The write-back refusal has its own line above. Anything else is
		// the label's history: say so once for the head, since the label
		// then stays for a person and nothing else in the log explains it.
		if !gateRefused {
			c.logInheritReleaseOnce(obs, head, "history", "stage labels: the promotion rule has inherit_human_merge_paths on; label kept, its history is not the hive's own (a person applied it, the hive's notice was edited or repeated, or the head moved)", "repo", obs.Display, "pr", obs.Number, "head", head)
		}
		return false, true, nil
	}
	// The label comes off before the notice is posted, as in the pin-only
	// lift: a notice without the removal would keep this head labelled for
	// good.
	body := humanMergePathLiftMarker + head + " -->\n" + promotionInheritedLiftNotice(head, rule)
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
		return true, false, fmt.Errorf("posting lift comment: %w", err)
	}
	c.noteOwnStageComment(obs.Owner, obs.Repo, obs.Number)
	c.info("stage labels: removed human-merge-path label; the promotion rule has inherit_human_merge_paths on", "repo", obs.Display, "pr", obs.Number, "head", head, "source", rule.Source, "target", rule.Target, "label", hgithub.HumanMergePathLabel, "config", promotionInheritConfigKey)
	return true, false, nil
}

// promotionInheritReleaseRecheck is how long an attempt to release the
// hive's label that ended without removing it keeps the stage pass from
// reading GitHub for the same pull request and head again. It is the merge
// lanes' own recheck interval for a refusal no retry cures at once
// (mergeRefusalRecheck), so a write-back mode, an opt-in label or a hold
// that changes is picked up without a restart.
const promotionInheritReleaseRecheck = mergeRefusalRecheck

func promotionInheritReleaseKey(owner, repo string, number int) string {
	return "promotion-inherit-release|" + strings.ToLower(owner+"/"+repo) + "#" + strconv.Itoa(number)
}

// promotionInheritReleaseTried reports whether an attempt for this pull
// request's head ended without removing the label less than
// promotionInheritReleaseRecheck ago. One mark is kept per pull request, in
// the remembered refusals of the sweep memory (in process: a restart
// forgets it, and the next pass tries again). A new head or the elapsed
// interval drops it; a mark for a pull request that left the pass is
// pruned with the expired refusals (mergeRefusalPrunable).
func (c *Engine) promotionInheritReleaseTried(owner, repo string, number int, head string) bool {
	key := promotionInheritReleaseKey(owner, repo, number)
	now := c.clock()
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	defer m.mergeRefusalsMu.Unlock()
	r, ok := m.mergeRefusals[key]
	if !ok {
		return false
	}
	if now.Sub(r.at) >= promotionInheritReleaseRecheck || !strings.EqualFold(r.head, strings.TrimSpace(head)) {
		delete(m.mergeRefusals, key)
		return false
	}
	return true
}

// notePromotionInheritReleaseKept remembers, for
// promotionInheritReleaseTried, that an attempt for head kept the label.
func (c *Engine) notePromotionInheritReleaseKept(owner, repo string, number int, head string) {
	c.rememberMergeRefusal(promotionInheritReleaseKey(owner, repo, number), mergeRefusal{head: strings.TrimSpace(head)})
}

// forgetPromotionInheritRelease drops the mark once the label is released.
func (c *Engine) forgetPromotionInheritRelease(owner, repo string, number int) {
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	defer m.mergeRefusalsMu.Unlock()
	delete(m.mergeRefusals, promotionInheritReleaseKey(owner, repo, number))
}
