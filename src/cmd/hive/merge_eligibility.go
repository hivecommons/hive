package main

// Merge eligibility: deciding whether a pull request may be auto-merged --
// hold gates, required-check state, mergeability classification into buckets,
// the authz binding for the trusted merger, and the merge-eligible report file.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	// automaxprocs sets GOMAXPROCS to match the container's CPU quota (Linux
	// CFS) at init. Without it the Go runtime sizes its P count to the whole
	// NODE's core count, so on a many-core IKS worker a pod limited to a few
	// CPUs spawns far more runnable Ps than its CFS quota can service; when the
	// quota is exhausted mid-period EVERY goroutine — including the netpoller
	// that answers the :3002 liveness probe and the heartbeat loop — is
	// throttled until the next CFS period, which stacks on top of the NFS
	// stalls to push probe latency past the kubelet timeout. Matching GOMAXPROCS
	// to the quota removes that self-inflicted throttling.
	//
	// This is called explicitly rather than via the package's blank import
	// because that import's init writes a line to the default logger (stderr)
	// unconditionally. `hive` re-execs itself as a Git transport shim, and the
	// setup path captures a child's stdout and stderr into a single buffer to
	// parse (e.g. `symbolic-ref --short origin/HEAD`), so an init-time banner
	// is indistinguishable from Git's answer and corrupts the parsed branch
	// name. Setting it with a no-op logger keeps the GOMAXPROCS behaviour and
	// drops the banner.

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/review"
)

// trustedMergerFunc resolves a GitHub login against the hive's authorized-users
// allowlist and reports whether it holds at least config.RoleMerger — the same
// bar requireMergerOrOwnerRole enforces on the dashboard queue endpoint (audit
// F3).
//
// Fails CLOSED: a nil config or a login absent from the allowlist is NOT
// trusted, so an unclassifiable actor can never merge. cfg is read on every
// call so a config reload that grants or revokes the merger tier takes effect
// without a restart.
func trustedMergerFunc(cfg *config.Config) github.MergerAuthorizer {
	return func(login string) bool {
		if cfg == nil || strings.TrimSpace(login) == "" {
			return false
		}
		role, ok := cfg.Dashboard.AuthorizedRole(login)
		if !ok {
			return false
		}
		return config.RoleAtLeast(role, config.RoleMerger)
	}
}

// mergeEligiblePath is a var (not a const) only so tests can point
// mergeTargetEligible at a temp file; production never reassigns it.
var mergeEligiblePath = "/var/run/hive-metrics/merge-eligible.json"

// acmmHoldGatedMinLevel / acmmHoldGatedMaxLevel bracket the ACMM levels whose
// merge policy is "hold-gated" — every agent-opened PR gets a "hold" label and
// no agent merges (see src/pkg/config/packs/level-{3,4,5}.yaml). L1/L2 are
// "manual" (agents open no PRs) and L6 is "auto-merge on green CI, no hold
// label", so both fall outside this range. Used by the F6 hold-label decider.
const (
	acmmHoldGatedMinLevel = 3
	acmmHoldGatedMaxLevel = 5
)

// shouldHoldAgentPR keeps public outreach claims human-reviewed even at L6,
// where ordinary agent PRs may auto-merge. The general ACMM hold gate remains
// unchanged for all roles at L3-L5.
func shouldHoldAgentPR(agentName string, level int) bool {
	if strings.EqualFold(strings.TrimSpace(agentName), "outreach") {
		return true
	}
	return level >= acmmHoldGatedMinLevel && level <= acmmHoldGatedMaxLevel
}

// mergeableJSONUnknown is the explicit wire value for "mergeability was never
// determined". It is spelled out rather than left as "" so a consumer reading
// merge-eligible.json cannot mistake an unpopulated field for a definitive
// "no" — the failure mode that made every PR read as unmergeable.
const mergeableJSONUnknown = "unknown"

// mergeTargetEligible reports whether (repo, number) currently appears in the
// governor's merge-eligible.json AT the expected head SHA. It reads the file
// FRESH on every call (never caches) because eligibility is recomputed each
// governor cycle — a stale cache could authorize a PR that has since fallen out
// of the list. On any read/parse error it returns false (FAIL CLOSED): if we
// cannot prove the target is eligible, we must not authorize the merge.
//
// M4 (CWE-367, TOCTOU): the governor records the head SHA it observed when it
// deemed the PR eligible (eligiblePR.HeadSHA). A branch can move between that
// review and the merge relay firing, so matching (repo, number) alone would let
// a moved head merge at a commit the governor never vetted. We therefore also
// require the entry's stored head_sha to equal expectSHA. A mismatch — or a
// stored SHA that is empty (governor could not observe it) — fails closed; the
// relay's SHA pin then fails the merge cleanly if a stale request slips through.
//
// merge-eligible.json stores repos as "owner/repo"; a MergeRequest.Repo may be
// bare ("repo") or fully qualified ("owner/repo"). We match on the bare repo
// name (the segment after the last "/") plus the number, so both request forms
// resolve to the same eligible entry without depending on the org prefix.
func mergeTargetEligible(repo string, number int, expectSHA string) bool {
	data, err := os.ReadFile(mergeEligiblePath)
	if err != nil {
		return false // fail closed: no list ⇒ nothing is eligible
	}
	var payload struct {
		Items []struct {
			Number  int    `json:"number"`
			Repo    string `json:"repo"`
			HeadSHA string `json:"head_sha"`
		} `json:"merge_eligible"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return false // fail closed: unparseable list ⇒ deny
	}
	want := bareRepoName(repo)
	wantSHA := strings.TrimSpace(expectSHA)
	for _, it := range payload.Items {
		if it.Number == number && bareRepoName(it.Repo) == want {
			// M4: bind authorization to the governor-observed head. An empty
			// stored SHA cannot be proven to match, so it fails closed rather
			// than authorizing an unpinned head.
			return strings.TrimSpace(it.HeadSHA) != "" && strings.TrimSpace(it.HeadSHA) == wantSHA
		}
	}
	return false
}

// bareRepoName returns the repo segment after the last "/", so "owner/repo" and
// "repo" compare equal. Used to match a MergeRequest.Repo against the
// "owner/repo" entries in merge-eligible.json regardless of prefix.
func bareRepoName(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// bindMergeAuthz wraps the manager's agent/UID/CanMerge authorizer with the
// F4 target-binding checks (CWE-863). The inner authz owns the "may this agent
// merge at all" decision; this wrapper owns "is THIS specific target one the
// governor deemed eligible, at a pinned SHA". Both must pass before MergePR is
// reached. Ordering: run the agent/UID/CanMerge check first (cheapest, and it
// gives the clearest denial reason), then the SHA + eligible-list binding.
func bindMergeAuthz(inner func(agent string, fileUID int) error) github.MergeRequestAuthorizer {
	return func(agent string, fileUID int, repo string, number int, expectSHA string) error {
		if err := inner(agent, fileUID); err != nil {
			return err
		}
		// (a) Require a pinned head SHA. An empty expectSHA means "merge whatever
		// HEAD is now", which is the TOCTOU hole: a PR that was eligible when the
		// governor last looked could have had a malicious commit pushed since.
		// MergePR passes expectSHA as the required head SHA, so a moved head fails
		// cleanly — but only if we insist it is set.
		if strings.TrimSpace(expectSHA) == "" {
			return fmt.Errorf("merge target %s#%d has no expected head SHA — refusing to merge an unpinned head (TOCTOU guard)", repo, number)
		}
		// (b) Require the target to be in the governor's current merge-eligible
		// list AT the expected head SHA. This binds authorization to a PR the
		// hive actually deemed landable this cycle, at the exact commit it
		// reviewed, so an injected agent cannot request landing an arbitrary
		// reachable PR (e.g. its own) whose required checks happen to pass, nor
		// land an eligible PR at a head that moved after review (M4, CWE-367).
		// Read fresh + fail closed (see mergeTargetEligible).
		if !mergeTargetEligible(repo, number, expectSHA) {
			return fmt.Errorf("merge target %s#%d is not in the current merge-eligible list at head %s — only governor-approved PRs may be landed via the merge relay, and only at the reviewed head SHA", repo, number, expectSHA)
		}
		return nil
	}
}

// mergeableJSON renders a tri-state mergeability verdict for the
// merge-eligible.json marker, mapping the unknown zero value to an explicit
// "unknown" rather than an empty string.
func mergeableJSON(m github.Mergeable) string {
	if m == github.MergeableUnknown {
		return mergeableJSONUnknown
	}
	return string(m)
}

// anyRequiredCheckFailing reports whether any of a PR's failing check names
// is in the operator-declared required set.
func anyRequiredCheckFailing(failing []string, required map[string]bool) bool {
	for _, name := range failing {
		if required[name] {
			return true
		}
	}
	return false
}

// mergeBucket is where the merge-eligible classifier files a PR: the
// merge-eligible.json list, the ci-failing.json list, or neither.
type mergeBucket int

const (
	mergeBucketSkip mergeBucket = iota
	mergeBucketFailing
	mergeBucketEligible
)

// mergeGates bundles the per-tick inputs the classifier applies beyond the
// PR itself: the intent and review artifacts and the operator's
// required-check set.
type mergeGates struct {
	enforceIntent         bool
	intentVerdicts        map[string]intent.Verdict
	requireReviewApproval bool
	reviewArtifact        review.Artifact
	reviewLoaded          bool
	requiredChecks        map[string]bool
}

// classifyMergeEligibility is THE merge-eligibility rule: the one place that
// decides whether a PR goes to merge-eligible.json (the sweep would merge it
// now), ci-failing.json (its author has CI to fix), or neither. It returns
// the bucket and, for the dashboard, the same decision as a MergeVerdict with
// the reason spelled out (hivecommons/hive#7478): the pill is painted from
// this verdict, so green on the card means exactly what the sweep means by
// eligible, and never the looser "GitHub says mergeable".
//
// intentReason is non-empty only when the intent gate excluded the PR; the
// caller logs it (the log line carries the verdict tier, which this function
// does not need).
func classifyMergeEligibility(pr github.PullRequest, held bool, fullRepo string, g mergeGates) (bucket mergeBucket, verdict github.MergeVerdict, intentReason string) {
	// blockedOrOutstanding is the verdict for a PR the sweep will not take
	// for a reason of its own: the state still depends on what GitHub says,
	// because a conflicting PR is blocked whatever else is outstanding, and
	// one whose mergeability was never fetched is unknown, not amber. The
	// sweep's reason is kept in every state (hivecommons/hive#7515): on a
	// protected branch a red required check or a missing review is exactly
	// what makes GitHub say "blocked", so dropping it for the bare enum sent
	// the operator to GitHub to learn what this function already knew.
	blockedOrOutstanding := func(reason string) github.MergeVerdict {
		switch pr.Mergeable {
		case github.MergeableNo:
			return github.MergeVerdict{State: github.MergeVerdictBlocked, Reason: notMergeableReason(pr, reason)}
		case github.MergeableUnknown:
			return github.MergeVerdict{State: github.MergeVerdictUnknown, Reason: mergeabilityUnknownReason + "; " + reason}
		}
		return github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: reason}
	}

	if pr.Draft {
		return mergeBucketSkip, github.MergeVerdict{State: github.MergeVerdictBlocked, Reason: "draft — mark ready for review to enter the sweep"}, ""
	}
	if g.enforceIntent {
		if v, ok := g.intentVerdicts[fmt.Sprintf("%s/%d", fullRepo, pr.Number)]; ok && v.AgentPR && !v.MergeAllowed() {
			reason := v.Reason
			if v.Authorized && v.Alignment != nil && v.Alignment.Misaligned() {
				reason = intent.ReasonAlignmentMisaligned + ": " + v.Alignment.Rationale
			}
			return mergeBucketSkip, blockedOrOutstanding("intent verification: " + reason), reason
		}
	}

	if pr.CIStatus == "failure" {
		// A PR red ONLY on non-required checks (perma-red Playwright
		// shards, coverage) that GitHub itself reports mergeable is NOT a
		// failing PR — it is merge-eligible, mirroring the
		// pending-but-mergeable rule below. Without this, every dependabot
		// PR on a repo with permanently-red optional checks classified as
		// "failure", landed in ci-failing.json where no sweep or agent
		// would ever merge it, and accumulated indefinitely (observed on
		// kubestellar/console 2026-08-28: 16 dependabot PRs, oldest 11
		// days). Gated on an operator-declared required-check set: with no
		// set configured we cannot distinguish required from optional and
		// keep the old fail-closed behavior. The merge step re-enforces
		// branch protection, so this cannot merge anything GitHub blocks.
		onlyOptionalRed := len(g.requiredChecks) > 0 &&
			!anyRequiredCheckFailing(pr.FailingChecks, g.requiredChecks) &&
			pr.Mergeable == github.MergeableYes
		if !onlyOptionalRed {
			reason := "CI failing"
			if len(pr.FailingChecks) > 0 {
				reason += ": " + strings.Join(pr.FailingChecks, ", ")
			}
			if len(g.requiredChecks) == 0 && pr.Mergeable == github.MergeableYes {
				// GitHub calls it mergeable (unstable): nothing REQUIRED is
				// red. The sweep still refuses it because, with no
				// required-check set declared, it cannot tell optional from
				// required. Say so — this is the shape #7478 was filed on.
				reason += " (GitHub reports it mergeable; declare auto_merge.required_checks for the sweep to treat non-required checks as optional)"
			}
			return mergeBucketFailing, blockedOrOutstanding(reason), ""
		}
	}

	// The hold check sits AFTER the red classification on purpose
	// (hivecommons/hive#7438): a held PR must never become merge-eligible,
	// but a held RED PR is still its author's to repair. When this skip ran
	// first, a level-held agent PR with a failing check vanished from
	// ci-failing.json, its author never got a fix-before-new block for it,
	// and it sat red and held until a human did the agent's repair.
	if held {
		return mergeBucketSkip, blockedOrOutstanding("held: a hold label keeps it out of the sweep"), ""
	}

	// A PR whose CI is still "pending" is nonetheless merge-eligible when
	// GitHub itself reports it as mergeable (mergeStateStatus=unstable):
	// that state means every REQUIRED check has passed and only
	// non-required checks remain outstanding. Those non-required checks —
	// a cancelled Mobile Browser Tests, a still-running coverage-report,
	// perpetually-pending tide — can never complete on their own, so
	// waiting for CIStatus=="success" (all checks done) leaves cleanly
	// mergeable PRs frozen out of the sweep indefinitely (observed
	// 2026-08-04: three green console PRs stuck for hours). The merge step
	// re-enforces branch protection, so trusting the mergeable verdict here
	// cannot merge anything GitHub would actually block.
	if pr.CIStatus == "pending" && pr.Mergeable != github.MergeableYes {
		// Genuinely not ready: a required check is still running (or
		// mergeability is unknown/no). Leave it out of both buckets, as
		// before — it neither merges nor gets a fix dispatched.
		return mergeBucketSkip, blockedOrOutstanding("CI pending"), ""
	}

	// The review gate runs BEFORE the GitHub-says-no return below so that a
	// PR GitHub calls "blocked" for want of a review carries that reason
	// (hivecommons/hive#7515). Both paths file a MergeableNo PR in the skip
	// bucket, so the order changes only the verdict's wording.
	if g.requireReviewApproval {
		if !g.reviewLoaded {
			return mergeBucketSkip, blockedOrOutstanding("review approval required, but review-verdicts.json is unavailable"), ""
		}
		if !g.reviewArtifact.HasAggregateApproval(fullRepo, pr.Number, pr.HeadSHA) {
			return mergeBucketSkip, blockedOrOutstanding("awaiting review approval"), ""
		}
	}

	if pr.Mergeable == github.MergeableNo {
		// A conflicting PR cannot merge no matter how green its checks
		// are. Listing it as merge-eligible left the eligible count stuck
		// at N forever while nothing could actually merge (console
		// #23002/#23003, 2026-08-31: the only two build-gate-green PRs
		// were DIRTY go.mod dependabot bumps). Conflicts are the
		// rebase/needs-human path's job, not the sweep's — keep them out
		// of the eligible bucket. No sweep gate explains this one: for
		// "blocked" that means a branch-protection rule we do not read yet
		// (a review GitHub requires, a required check that never reported);
		// notMergeableReason says so rather than the bare enum.
		return mergeBucketSkip, github.MergeVerdict{State: github.MergeVerdictBlocked, Reason: notMergeableReason(pr, "")}, ""
	}

	// Eligible. The reason names what GitHub still shows outstanding that
	// the sweep chooses to ignore, so a green pill beside a red optional
	// check does not read as "all green".
	reason := "the sweep would merge this now"
	switch {
	case pr.CIStatus == "failure":
		reason += " — only non-required checks are red (" + strings.Join(pr.FailingChecks, ", ") + ")"
	case pr.CIStatus == "pending":
		reason += " — non-required checks still pending (GitHub: " + pr.MergeableState + ")"
	case pr.MergeableState == "unstable":
		reason += " — non-required checks outstanding (GitHub: unstable)"
	case pr.Mergeable == github.MergeableUnknown:
		reason += " — mergeability not yet fetched; the sweep re-checks it at merge time"
	}
	return mergeBucketEligible, github.MergeVerdict{State: github.MergeVerdictEligible, Reason: reason}, ""
}

// mergeabilityUnknownReason is the verdict prefix for a PR whose
// mergeability GitHub has not computed yet (or the fetch failed); the
// classifier appends the sweep's own reason after it.
const mergeabilityUnknownReason = "mergeability not yet computed by GitHub — re-checked next tick"

// notMergeableReason explains, in words an operator can act on, why GitHub
// reports a PR as not mergeable — what to do, not the API enum
// (hivecommons/hive#7515). sweepReason is the gate the sweep itself failed
// the PR on, or "" when every sweep gate passed:
//
//   - "blocked" folds every unsatisfied branch-protection rule into one
//     word. When the sweep has a reason it is almost always the rule
//     ("blocked — CI failing: build"); when GitHub's own facts name a
//     different rule (a review decision, a required check that never
//     reported) that rule is named too; with neither, say that a rule we
//     cannot read is unsatisfied rather than nothing at all.
//   - "dirty" and "behind" name the base branch and the fix (rebase /
//     update); a sweep reason is appended, since it still stands once the
//     branch is fixed.
//   - Any other state falls back to naming it.
func notMergeableReason(pr github.PullRequest, sweepReason string) string {
	base, from := pr.BaseRef, "the base branch"
	if base == "" {
		base, from = "the base branch", "it"
	}
	var msg string
	switch pr.MergeableState {
	case "blocked":
		// The branch-protection rule GitHub is hiding behind the word
		// "blocked", when the sweep collected enough to name it
		// (hivecommons/hive#7515 step 2). The wording itself lives in
		// github.PullRequest.BranchProtectionBlockReason — one place, under
		// test — not in this switch and not in the dashboard's JS.
		rule, ruleKnown := pr.BranchProtectionBlockReason()
		switch {
		case sweepReason == "" && ruleKnown:
			return "blocked — " + rule
		case sweepReason == "":
			return "blocked — all sweep gates pass; a branch-protection rule is unsatisfied"
		case ruleKnown && !strings.Contains(sweepReason, rule):
			// Both are true and neither subsumes the other: the sweep's own
			// gate is what it will act on, and GitHub's rule is what the
			// operator must also clear.
			return "blocked — " + sweepReason + "; GitHub also requires: " + rule
		}
		return "blocked — " + sweepReason
	case "dirty":
		msg = "has merge conflicts with " + base + " — needs a rebase"
	case "behind":
		msg = "behind " + base + " — needs an update from " + from
	case "":
		msg = "not mergeable on GitHub"
	default:
		msg = "not mergeable on GitHub (" + pr.MergeableState + ")"
	}
	if sweepReason != "" {
		msg += "; also " + sweepReason
	}
	return msg
}

func writeMergeEligible(actionable *github.ActionableResult, hold github.HoldResult, org string, escalatedPRs map[string]bool, enforceIntent bool, intentVerdicts map[string]intent.Verdict, requireReviewApproval bool, requiredChecks map[string]bool, logger *slog.Logger) map[string]github.MergeVerdict {
	verdicts := make(map[string]github.MergeVerdict)
	holdSet := make(map[string]bool)
	for _, h := range hold.Items {
		key := fmt.Sprintf("%s/%d", h.Repo, h.Number)
		holdSet[key] = true
	}

	type eligiblePR struct {
		Number int      `json:"number"`
		Repo   string   `json:"repo"`
		Title  string   `json:"title"`
		Author string   `json:"author"`
		Labels []string `json:"labels,omitempty"`
		// Mergeable is a tri-state string ("yes"/"no"/"unknown"), not a bool.
		// A bool here defaulted to false for every PR, because the value was
		// read from a list endpoint that never returns it.
		Mergeable string `json:"mergeable"`
		DCO       string `json:"dco"`
		// HeadSHA is the governor-observed head commit at the moment eligibility
		// was decided. mergeTargetEligible compares the relay's expected SHA
		// against this value (M4, CWE-367): a branch that moved after review
		// no longer matches and fails closed.
		HeadSHA string `json:"head_sha,omitempty"`
	}

	type failingPR struct {
		Number  int    `json:"number"`
		Repo    string `json:"repo"`
		Title   string `json:"title"`
		Author  string `json:"author"`
		HeadSHA string `json:"head_sha,omitempty"`
		// FailingChecks + Excerpt carry the raw CI evidence into the kick
		// work list so fix agents see the actual error, not just "red".
		FailingChecks []string `json:"failing_checks,omitempty"`
		Excerpt       string   `json:"excerpt,omitempty"`
		// Escalated marks PRs past the fix-loop breaker threshold: kick
		// builders list them separately and agents must NOT dispatch more
		// fix work for them.
		Escalated bool `json:"escalated,omitempty"`
		// Agent is the hive agent whose relay request opened this PR (from the
		// audit trail's agent_pr_created entries). The scheduler's
		// fix-before-new section routes each red PR back to its author; empty
		// means unattributed (kick builders default it to scanner).
		Agent string `json:"agent,omitempty"`
		// HeadRef / HeadRepo / FromFork say where the red branch actually
		// lives (hivecommons/hive#7386). The hive's App token pushes only to
		// the base repository, so a fork PR is comment-only for every agent:
		// ReachableAction spells that out ("push" | "comment-only") so no
		// kick consumer has to discover it with a failed push — the failure
		// mode that burned a scanner session and left a stray branch on the
		// base repo under the fork's head-ref name.
		HeadRef         string `json:"head_ref,omitempty"`
		HeadRepo        string `json:"head_repo,omitempty"`
		FromFork        bool   `json:"from_fork,omitempty"`
		ReachableAction string `json:"reachable_action"`
		// Held marks a PR carrying a hold label (the ACMM level gate's, or a
		// human's). The hold is a MERGE checkpoint, not a repair checkpoint
		// (hivecommons/hive#7438): a held red PR is still its author's to fix,
		// so it is listed here with the flag rather than dropped — the owning
		// agent's fix-before-new block says "fix CI, do not remove the hold".
		Held bool `json:"held,omitempty"`
	}

	prAgents := auditPRAgents(org, time.Now().Add(-auditPRAttributionWindow), "")

	var eligible []eligiblePR
	var failing []failingPR
	var reviewArtifact review.Artifact
	reviewLoaded := false
	if requireReviewApproval {
		var err error
		reviewArtifact, err = review.LoadArtifact("")
		if err != nil {
			logger.Warn("review approval required but review-verdicts.json is unavailable; merge eligibility will fail closed", "error", err)
		} else {
			reviewLoaded = true
		}
	}
	// Two populations, one classifier (hivecommons/hive#7438). PRs.Items are
	// the merge candidates. PRs.Held are PRs the hold gate removed from Items
	// — they can never become merge-eligible, but a RED one still has to reach
	// its authoring agent, otherwise it deadlocks: it stays red, so it stays
	// held, so nothing ever repairs it.
	type prCandidate struct {
		pr   github.PullRequest
		held bool
	}
	candidates := make([]prCandidate, 0, len(actionable.PRs.Items)+len(actionable.PRs.Held))
	for _, pr := range actionable.PRs.Items {
		candidates = append(candidates, prCandidate{pr: pr})
	}
	for _, pr := range actionable.PRs.Held {
		candidates = append(candidates, prCandidate{pr: pr, held: true})
	}

	gates := mergeGates{
		enforceIntent:         enforceIntent,
		intentVerdicts:        intentVerdicts,
		requireReviewApproval: requireReviewApproval,
		reviewArtifact:        reviewArtifact,
		reviewLoaded:          reviewLoaded,
		requiredChecks:        requiredChecks,
	}
	seen := make(map[string]bool, len(candidates))
	for _, cand := range candidates {
		pr := cand.pr
		key := fmt.Sprintf("%s/%d", pr.Repo, pr.Number)
		if seen[key] {
			continue
		}
		seen[key] = true
		// The hold can arrive either as membership in PRs.Held or as a row in
		// the hold snapshot; both mean the same thing here.
		held := cand.held || holdSet[key]
		fullRepo := fullRepoName(pr.Repo, org)

		bucket, verdict, intentReason := classifyMergeEligibility(pr, held, fullRepo, gates)
		verdicts[github.MergeVerdictKey(pr)] = verdict
		if intentReason != "" {
			iv := intentVerdicts[fmt.Sprintf("%s/%d", fullRepo, pr.Number)]
			logger.Info("excluding PR from merge-eligible due to intent verification", "repo", fullRepo, "number", pr.Number, "tier", iv.Tier, "reason", intentReason)
		}
		switch bucket {
		case mergeBucketSkip:
			continue
		case mergeBucketFailing:
			failing = append(failing, failingPR{
				Number:          pr.Number,
				Repo:            fullRepo,
				Title:           pr.Title,
				Author:          pr.Author,
				HeadSHA:         pr.HeadSHA,
				FailingChecks:   pr.FailingChecks,
				Excerpt:         pr.CIFailureExcerpt,
				Escalated:       escalatedPRs[escalation.Key(fullRepo, pr.Number)],
				Agent:           prAgents[fmt.Sprintf("%s#%d", fullRepo, pr.Number)],
				HeadRef:         pr.HeadRef,
				HeadRepo:        pr.HeadRepo,
				FromFork:        pr.FromFork,
				ReachableAction: github.ReachableAction(pr),
				Held:            held,
			})
			continue
		}

		dco := "unknown"
		for _, l := range pr.Labels {
			switch l {
			case "dco-signoff: yes":
				dco = "yes"
			case "dco-signoff: no":
				dco = "no"
			}
		}
		eligible = append(eligible, eligiblePR{
			Number:    pr.Number,
			Repo:      fullRepo,
			Title:     pr.Title,
			Author:    pr.Author,
			Labels:    pr.Labels,
			Mergeable: mergeableJSON(pr.Mergeable),
			DCO:       dco,
			HeadSHA:   pr.HeadSHA,
		})
	}

	_ = os.MkdirAll("/var/run/hive-metrics", 0o755)

	payload := map[string]any{
		"generated_at":   time.Now().UTC().Format(time.RFC3339),
		"merge_eligible": eligible,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		logger.Warn("failed to marshal merge-eligible", "error", err)
		return verdicts
	}
	atomicWrite(mergeEligiblePath, data)
	logger.Info("merge-eligible.json updated", "eligible", len(eligible), "ci_failing", len(failing), "total_prs", len(actionable.PRs.Items))

	failPayload := map[string]any{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"ci_failing":   failing,
	}
	failData, err := json.Marshal(failPayload)
	if err != nil {
		logger.Warn("failed to marshal ci-failing", "error", err)
		return verdicts
	}
	atomicWrite(ciFailingPath, failData)
	return verdicts
}

// normalizedAutoMergeLabel resolves the configured queue label, falling back
// to the shared default when the value is blank. Client.SetAutoMergeLabel
// ignores blank input (keeping whatever was set before) and
// Client.AutoMergeLabel falls back on read, but the cmd layer normalizes
// eagerly too so a partially-populated config can never propagate an unnamed
// label to a fresh client.
func normalizedAutoMergeLabel(label string) string {
	if label = strings.TrimSpace(label); label != "" {
		return label
	}
	return github.AutoMergeQueuedLabel
}
