package github

// Issue stage labels (hivecommons/hive#12436, Part 2 of #12429). PR1 gave
// every pull request a `stage/NNN-*` label computed by the automerge lanes;
// this file does the same for issues from what the enumeration cycle already
// decided about each one — no extra GitHub reads:
//
//   - still in the actionable set            → stage/010-actionable
//   - withheld by a live issue claim         → stage/020-claimed, or
//     stage/030-pr-in-progress when the claim marker is kind=agent
//   - withheld by the in-progress sweep      → stage/030-pr-in-progress
//   - withheld because an open PR covers it  → the covering PR's own stage
//     (read from the shared stage store the automerge sweep fills) or
//     stage/040-covered-by-pr until the lanes have seen that PR
//   - carrying hive/likely-done (merged PR, awaiting confirmation), or
//     filtered out of the pipeline by an exempt label → no stage.
//
// The reconciler plans against the labels the list payload already carries
// and writes only the difference, so an unchanged issue costs nothing. It
// also strips the retired `claimed`, `hive/in-progress` and `🐝 covered by
// PR` labels: one cycle after upgrade every open issue carries its stage
// label instead. Health (`stage/stalled`, `stage/regressed`) is tracked for
// the issue's own stages (010–030); an issue mirroring its PR's stage
// leaves health to the PR, where the reason is legible.

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// IssueStagePolicy is the live `governor.labels.stage` policy as the issue
// reconciler needs it.
type IssueStagePolicy struct {
	Enabled bool
	// Dwell overrides the stage table's expected dwell per stage slug.
	Dwell map[string]time.Duration
}

// SetIssueStageLabels installs the live policy resolver. Nil disables.
func (c *Client) SetIssueStageLabels(fn func() IssueStagePolicy) {
	if c == nil {
		return
	}
	c.issueStage = fn
}

func (c *Client) issueStagePolicy() IssueStagePolicy {
	if c == nil || c.issueStage == nil {
		return IssueStagePolicy{}
	}
	return c.issueStage()
}

// IssueStageResult counts what one reconcile pass did.
type IssueStageResult struct {
	Items    int
	Writes   int
	Comments int
}

// issueStageRESTCaller tags the writes for /api/gh-rate-limits.
const issueStageRESTCaller = "hive:issue_stage_labels"

// Work-detail buckets the reconciler maps (see appendRepoWorkIssueDetail).
const (
	workBucketClaimed     = "claimed"
	workBucketClaimedByPR = "claimed_by_pr"
)

// IssueStageInput is the one shape both actionable items and work-detail
// rows reduce to before classification.
type IssueStageInput struct {
	Repo      string
	Number    int
	Labels    []string
	Bucket    string // "" for an issue still in the actionable set
	ClaimKind string
	Claim     *IssueClaimContext
}

// IssueStageFor classifies one enumerated issue. mirrored reports whether
// the stage is the covering PR's (health then belongs to the PR). prStage
// resolves the covering PR's current stage from the shared store.
func IssueStageFor(in IssueStageInput, prStage func(repo string, number int) stagelabels.Stage) (stage stagelabels.Stage, mirrored bool, reason string) {
	if issueHasLabel(in.Labels, LikelyDoneLabel) {
		return stagelabels.None, false, "merged PR awaiting confirmation"
	}
	switch in.Bucket {
	case "":
		return stagelabels.Actionable, false, "actionable"
	case workBucketClaimed:
		if strings.EqualFold(in.ClaimKind, string(claims.KindAgent)) {
			return stagelabels.PRInProgress, false, "live agent claim"
		}
		return stagelabels.Claimed, false, "live issue claim"
	case inProgressWorkBucket:
		return stagelabels.PRInProgress, false, "in-progress start mark"
	case workBucketClaimedByPR:
		if in.Claim != nil && in.Claim.PRNumber > 0 && prStage != nil {
			prRepo := in.Claim.PRRepo
			if prRepo == "" {
				prRepo = in.Repo
			}
			if s := prStage(prRepo, in.Claim.PRNumber); s != stagelabels.None && !s.Before(stagelabels.CoveredByPR) {
				return s, true, "mirrors PR #" + strconv.Itoa(in.Claim.PRNumber)
			}
		}
		return stagelabels.CoveredByPR, false, "open PR covers it"
	}
	return stagelabels.None, false, "outside the pipeline: " + in.Bucket
}

// IssueStageInputs flattens an enumeration result into one row per open
// issue the cycle saw, actionable items first, then every work-detail row.
func IssueStageInputs(result *ActionableResult) []IssueStageInput {
	if result == nil {
		return nil
	}
	var out []IssueStageInput
	for _, issue := range result.Issues.Items {
		out = append(out, IssueStageInput{Repo: issue.Repo, Number: issue.Number, Labels: issue.Labels, ClaimKind: issue.ClaimKind, Claim: issue.ClaimContext})
	}
	repos := make([]string, 0, len(result.WorkDetailsByRepo))
	for repo := range result.WorkDetailsByRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		for _, row := range result.WorkDetailsByRepo[repo].Issues {
			if row.Number <= 0 {
				continue
			}
			out = append(out, IssueStageInput{Repo: row.Repo, Number: row.Number, Labels: row.Labels, Bucket: row.Bucket, ClaimKind: row.ClaimKind, Claim: row.ClaimContext})
		}
	}
	return out
}

// SyncIssueStageLabels reconciles the stage labels on every open issue the
// enumeration cycle saw. It runs after the claim guards and the in-progress
// sweep so the buckets are final. store may be nil (the shared store is
// used). Best-effort: a failed write is logged and retried next cycle.
func SyncIssueStageLabels(ctx context.Context, client *Client, result *ActionableResult, store *stagelabels.Store, now time.Time, logger *slog.Logger) IssueStageResult {
	var res IssueStageResult
	if client == nil || result == nil {
		return res
	}
	policy := client.issueStagePolicy()
	if !policy.Enabled {
		return res
	}
	if store == nil {
		store = stagelabels.Shared()
	}
	ctx = WithRESTCaller(ctx, issueStageRESTCaller)
	prStage := func(repo string, number int) stagelabels.Stage {
		owner, name := client.splitRepo(repo)
		rec, ok := store.Get(stagelabels.Key(owner+"/"+name, number))
		if !ok {
			return stagelabels.None
		}
		return rec.Stage
	}
	seen := map[string]bool{}
	for _, in := range IssueStageInputs(result) {
		owner, name := client.splitRepo(in.Repo)
		key := stagelabels.Key(owner+"/"+name, in.Number)
		if seen[key] {
			continue
		}
		seen[key] = true
		res.Items++
		stage, mirrored, reason := IssueStageFor(in, prStage)
		var want stagelabels.Desired
		var tr stagelabels.Transition
		if stage == stagelabels.None {
			store.Forget(key)
		} else {
			tr = store.Observe(key, stage, reason)
			rec, _ := store.Get(key)
			want = stagelabels.Desired{Stage: stage}
			if !mirrored {
				want.Stalled = stagelabels.Stalled(rec, policy.Dwell, now)
				want.Regressed = rec.Regressed()
			}
		}
		add, remove := stagelabels.Plan(in.Labels, want)
		if len(add)+len(remove) == 0 {
			continue
		}
		if err := client.ensureStageLabelFamily(ctx, in.Repo); err != nil {
			if logger != nil {
				logger.Warn("issue stage labels: provisioning failed", "repo", in.Repo, "error", err)
			}
			continue
		}
		for _, l := range remove {
			if err := client.RemoveLabel(ctx, in.Repo, in.Number, l); err != nil {
				if logger != nil {
					logger.Warn("issue stage labels: remove failed", "repo", in.Repo, "issue", in.Number, "label", l, "error", err)
				}
				continue
			}
			res.Writes++
		}
		if len(add) > 0 {
			if err := client.AddLabels(ctx, in.Repo, in.Number, add); err != nil {
				if logger != nil {
					logger.Warn("issue stage labels: add failed", "repo", in.Repo, "issue", in.Number, "labels", add, "error", err)
				}
			} else {
				res.Writes++
			}
		}
		if logger != nil {
			logger.Info("issue stage labels reconciled", "repo", in.Repo, "issue", in.Number, "stage", want.Stage.String(), "added", add, "removed", remove, "reason", reason, "bucket", in.Bucket)
		}
		// A regression between the issue's OWN stages (a claim or kick that
		// lapsed without a PR) is the hive's failure and gets the one-line
		// note; a fall from a mirrored PR stage is narrated on the PR.
		if tr.Regressed && !mirrored && tr.Prev.Before(stagelabels.CoveredByPR) {
			if rec, ok := store.Get(key); ok && !rec.RegressionCommented {
				if err := client.CreateIssueComment(ctx, in.Repo, in.Number, stagelabels.RegressionComment(tr.Prev, tr.Next, reason)); err != nil {
					if logger != nil {
						logger.Warn("issue stage labels: regression comment failed", "repo", in.Repo, "issue", in.Number, "error", err)
					}
				} else {
					store.MarkRegressionCommented(key)
					res.Comments++
				}
			}
		}
	}
	if err := store.Save(); err != nil && logger != nil {
		logger.Warn("issue stage labels: persisting state failed", "error", err)
	}
	return res
}

// ensureStageLabelFamily provisions (or recolours) the stage/author label
// family in a repo once per process: one list call, then one create or
// patch per label that is missing or whose colour drifted.
func (c *Client) ensureStageLabelFamily(ctx context.Context, repo string) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	key := strings.ToLower(owner + "/" + name)
	if c.stageFamily.has(key) {
		return nil
	}
	existing := map[string]*gh.Label{}
	opts := &gh.ListOptions{PerPage: 100}
	for {
		labels, resp, err := c.client.Issues.ListLabels(ctx, owner, name, opts)
		if err != nil {
			return err
		}
		for _, l := range labels {
			if l != nil {
				existing[strings.ToLower(l.GetName())] = l
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	for _, def := range stagelabels.AllLabelDefs() {
		cur, ok := existing[strings.ToLower(def.Name)]
		switch {
		case !ok:
			_, _, err := c.client.Issues.CreateLabel(ctx, owner, name, &gh.Label{Name: gh.Ptr(def.Name), Color: gh.Ptr(def.Color), Description: gh.Ptr(def.Description)})
			if err != nil && !githubStatusError(err, http.StatusUnprocessableEntity) {
				return err
			}
		case !strings.EqualFold(cur.GetColor(), def.Color):
			if _, _, err := c.client.Issues.EditLabel(ctx, owner, name, cur.GetName(), &gh.Label{Name: gh.Ptr(def.Name), Color: gh.Ptr(def.Color), Description: gh.Ptr(def.Description)}); err != nil {
				return err
			}
		}
	}
	c.stageFamily.mark(key)
	c.coveredByPRLabel.mark(key)
	return nil
}
