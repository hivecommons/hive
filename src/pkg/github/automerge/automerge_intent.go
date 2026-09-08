package automerge

import (
	"context"
	"fmt"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/intent"
)

// Skip reasons reported by the self-authored sweep's intent-tier gate.
const (
	// autoMergeReasonIntentUnauthorized: the PR's intent tier refused the
	// merge (e.g. Tier1 without a linked issue) and intent.enforce is on.
	autoMergeReasonIntentUnauthorized = "intent-unauthorized"
	// autoMergeReasonIntentEvidence: the changed-file list needed to classify
	// the PR could not be fetched, so the tier is unknown. Fails CLOSED.
	autoMergeReasonIntentEvidence = "intent-evidence"
)

// IntentGate is the intent-tier authorization policy the self-authored sweep
// enforces before the App merges its own PR (#6258).
//
// The human merge lane is tier-gated upstream: writeIntentVerdicts classifies
// every agent PR (intent.Classify), builds its evidence
// (intent.BuildEvidenceForRepo) and writeMergeEligible drops any PR whose
// verdict intent.Verdict.BlocksMerge reports, so nothing a merger can queue
// ever reaches trySweepQueuedPR at a tier the policy refuses. The
// self-authored sweep lists the App's PRs independently of merge-eligible.json
// and, before this gate, merged on open + non-draft + App-authored +
// mergeable + green + head-SHA re-verify alone — intent.EvaluateForAppSelfMerge,
// the tier gate written for exactly this path, was never called. This gate
// wires it in with the SAME refusal predicate (BlocksMerge) the human lane
// uses, so a PR cannot merge via the self-authored sweep at a tier the normal
// path would refuse.
//
// Enforce is read on every evaluation (same pattern as MergerAuthorizer) so a
// config reload takes effect without restarting the sweep. When it reports
// false the gate is advisory: the verdict is still computed and logged, but —
// matching writeMergeEligible's `if enforceIntent` — it never withholds a
// merge. A nil *IntentGate on the Engine means no intent policy is installed
// and the sweep behaves as it did before #6258 (see Options.IntentGate).
type IntentGate struct {
	// Config is the tier classification config (config.IntentConfig
	// patterns and feature signals).
	Config intent.Config
	// Enforce reports whether intent.enforce is on. nil is treated as false.
	Enforce func() bool
	// BeadStores supplies linked-issue / approved-plan bead evidence exactly
	// as the human lane's BuildEvidenceForRepo call receives it. nil means
	// body-only evidence.
	BeadStores map[string]*beads.Store
}

func (g *IntentGate) enforced() bool {
	return g != nil && g.Enforce != nil && g.Enforce()
}

// SetIntentGate installs (or, with nil, removes) the intent-tier gate
// consulted by trySweepSelfAuthoredPR.
func (c *Engine) SetIntentGate(gate *IntentGate) {
	if c == nil {
		return
	}
	c.intentGateMu.Lock()
	defer c.intentGateMu.Unlock()
	c.intentGate = gate
}

func (c *Engine) currentIntentGate() *IntentGate {
	if c == nil {
		return nil
	}
	c.intentGateMu.RLock()
	defer c.intentGateMu.RUnlock()
	return c.intentGate
}

// ListChangedFiles returns the complete changed-file list of pr as intent
// evidence. It pages through the files API and refuses an incomplete list
// (GitHub reported more changed files than the API returned): tier
// classification on a partial file list could miss a guardrail path, so the
// caller must treat the error as "tier unknown" and fail closed.
func ListChangedFiles(ctx context.Context, client *gh.Client, owner, repo string, pr *gh.PullRequest) ([]intent.ChangedFile, error) {
	if client == nil || pr == nil {
		return nil, fmt.Errorf("listing PR files: no client or PR")
	}
	number := pr.GetNumber()
	var files []intent.ChangedFile
	fileOpts := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := client.PullRequests.ListFiles(ctx, owner, repo, number, fileOpts)
		if err != nil {
			return nil, fmt.Errorf("listing PR files: %w", err)
		}
		for _, f := range page {
			files = append(files, intent.ChangedFile{
				Filename:  f.GetFilename(),
				Status:    f.GetStatus(),
				Additions: f.GetAdditions(),
				Deletions: f.GetDeletions(),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		fileOpts.Page = resp.NextPage
	}
	if reported := pr.GetChangedFiles(); reported > len(files) {
		return nil, fmt.Errorf("incomplete PR file list: GitHub reported %d changed files but API returned %d; intent alignment requires the complete changed-file list", reported, len(files))
	}
	return files, nil
}

// selfMergeIntentGate answers whether the intent-tier policy lets the App
// merge its own PR. It returns ("", nil) when the merge may proceed and a
// non-empty skip reason otherwise; a non-nil error means the tier could not
// be established (evidence fetch failed) and the merge is withheld.
//
// The verdict comes from intent.EvaluateForAppSelfMerge with callAllowed=true:
// the caller has already proven pr is the App's own PR (author ==
// AppBotLogin), which is the one precondition that function's contract puts
// on its caller. That drops ONLY the human-approval requirement (Prow forbids
// the App approving its own PR, so Tier2/Tier3 would otherwise deadlock) and
// keeps every other tier gate: Tier0 additive-only, Tier1 linked issue,
// Tier2 approved plan or self-merge authorization.
func (c *Engine) selfMergeIntentGate(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, author string, labels []string) (string, error) {
	gate := c.currentIntentGate()
	if gate == nil {
		return "", nil
	}
	enforce := gate.enforced()
	number := pr.GetNumber()
	files, err := ListChangedFiles(ctx, c.gh, owner, repo, pr)
	if err != nil {
		if !enforce {
			c.warn("self-authored automerge sweep intent evidence unavailable (advisory)", "repo", displayRepo, "pr", number, "error", err)
			return "", nil
		}
		return autoMergeReasonIntentEvidence, err
	}
	fullRepo := owner + "/" + repo
	class := intent.Classify(intent.PR{
		Title:       pr.GetTitle(),
		Body:        pr.GetBody(),
		Labels:      labels,
		Files:       files,
		Author:      author,
		AgentAuthor: true,
	}, gate.Config)
	// HumanApproval is false by construction: nobody can approve the App's
	// own PR under Prow, and EvaluateForAppSelfMerge exists precisely so this
	// path does not depend on it. Never widen evidence here.
	evidence := intent.BuildEvidenceForRepo(pr.GetBody(), fullRepo, gate.BeadStores, false)
	verdict := intent.EvaluateForAppSelfMerge(class, evidence, true)
	if !verdict.Authorized {
		c.info("self-authored automerge sweep intent authorization denied", "repo", displayRepo, "pr", number, "tier", verdict.Tier, "reason", verdict.Reason, "enforce", enforce)
	}
	if verdict.BlocksMerge(enforce) {
		return autoMergeReasonIntentUnauthorized, nil
	}
	return "", nil
}
