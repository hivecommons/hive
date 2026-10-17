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
//   - withheld because an open PR covers it  → stage/040-covered-by-pr; the
//     covering PR's own stage (read from the shared stage store the
//     automerge sweep fills) is narrated in the reason, never written as
//     a second label — 050+ are PR-only stages (hivecommons/hive#12451)
//   - parked on a person by an escalation label (needs-human,
//     needs-decision, needs-spec, or the retired needs-direction alias) → stage/085-needs-decision,
//     the same "a human decision is owed" stage PRs use; the label that
//     parked it is the reason
//   - carrying hive/likely-done (merged PR, awaiting confirmation), or
//     filtered out of the pipeline by any other exempt label → no stage.
//
// The reconciler plans against the labels the list payload already carries
// and writes only the difference, so an unchanged issue costs nothing; when
// a write is due it goes through stagelabels.Enforce, which re-reads the
// issue so exactly one stage label survives (hivecommons/hive#12512). It
// also strips the retired `claimed`, `hive/in-progress` and `🐝 covered by
// PR` labels: one cycle after upgrade every open issue carries its stage
// label instead. Health (`stage/stalled`) is tracked for the issue's own
// stages (010–030); a covered issue (040) leaves health to the PR, where the
// reason is legible. For those issue stages the dwell is still measured
// from stage entry: the issue list payload carries no comment count or
// activity timestamp the hive did not bump itself (hivecommons/hive#12491),
// so the PR engine's no-activity clock (Store.NoteActivity, require_quiet)
// does not apply here.

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
	// Healed counts issues found carrying more than one stage label.
	Healed int
	// ClaimedByHealed counts backend-named `hive/claimed-by-*` labels
	// rewritten to the holder's login (see healBackendClaimedByLabels).
	ClaimedByHealed int
	// DirectionMigrated counts issues whose legacy `needs-direction` label
	// was replaced by the needs-decision label (hivecommons/hive#12683).
	DirectionMigrated int
}

// issueStageRESTCaller tags the writes for /api/gh-rate-limits.
const issueStageRESTCaller = "hive:issue_stage_labels"

// Work-detail buckets the reconciler maps (see appendRepoWorkIssueDetail).
const (
	workBucketClaimed     = "claimed"
	workBucketClaimedByPR = "claimed_by_pr"
	workBucketFiltered    = "filtered"
)

// IssueStageInput is the one shape both actionable items and work-detail
// rows reduce to before classification.
type IssueStageInput struct {
	Repo      string
	Number    int
	Labels    []string
	Bucket    string // "" for an issue still in the actionable set
	Reason    string // the bucket's reason; for "filtered", the label that parked it
	ClaimKind string
	ClaimedBy string
	Claim     *IssueClaimContext
}

// IssueStageFor classifies one enumerated issue. covered reports whether an
// open PR the lanes have staged covers it (health then belongs to the PR,
// whose stage the reason names). prStage resolves the covering PR's current
// stage from the shared store. The issue itself never goes past
// 040-covered-by-pr: 050+ are PR stages (stagelabels.Table, PR: true) and
// mirroring them onto the issue is what left hivecommons/hive#12451 wearing
// stage/050-review-pending next to its own stages (hivecommons/hive#12451).
func IssueStageFor(in IssueStageInput, prStage func(repo string, number int) stagelabels.Stage) (stage stagelabels.Stage, covered bool, reason string) {
	if issueHasLabel(in.Labels, LikelyDoneLabel) {
		return stagelabels.None, false, "merged PR awaiting confirmation"
	}
	if label := parkedOnPersonLabel(in); label != "" {
		return stagelabels.NeedsDecision, false, "parked on a person: " + label
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
				return stagelabels.CoveredByPR, true, "open PR #" + strconv.Itoa(in.Claim.PRNumber) + " covers it (PR at " + s.String() + ")"
			}
		}
		return stagelabels.CoveredByPR, false, "open PR covers it"
	}
	return stagelabels.None, false, "outside the pipeline: " + in.Bucket
}

// parkedOnPersonLabel returns the escalation label that parks the issue on
// a person, or "". A parked issue is hard-suppressed at enumeration (bucket
// "filtered", reason = the configured label's canonical name), so the label
// is read from the labels the issue carries and, for an operator-renamed
// label, from the bucket reason. hivecommons/hive#12481 carried
// needs-direction and stage/030-pr-in-progress together for five hours: the
// reconciler cleared the stage to none, which read as "actionable" to a
// person while the hive had in fact handed it to one.
func parkedOnPersonLabel(in IssueStageInput) string {
	if label := hardSuppressIssueLabel(in.Labels); label != "" {
		return label
	}
	if in.Bucket == workBucketFiltered && hardSuppressIssueLabel([]string{in.Reason}) != "" {
		return in.Reason
	}
	return ""
}

// IssueStageInputs flattens an enumeration result into one row per open
// issue the cycle saw, actionable items first, then every work-detail row.
func IssueStageInputs(result *ActionableResult) []IssueStageInput {
	if result == nil {
		return nil
	}
	var out []IssueStageInput
	for _, issue := range result.Issues.Items {
		out = append(out, IssueStageInput{Repo: issue.Repo, Number: issue.Number, Labels: issue.Labels, ClaimKind: issue.ClaimKind, ClaimedBy: issue.ClaimedBy, Claim: issue.ClaimContext})
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
			out = append(out, IssueStageInput{Repo: row.Repo, Number: row.Number, Labels: row.Labels, Bucket: row.Bucket, Reason: row.Reason, ClaimKind: row.ClaimKind, ClaimedBy: row.ClaimedBy, Claim: row.ClaimContext})
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
		res.ClaimedByHealed += healBackendClaimedByLabels(ctx, client, in, logger)
		if migrateNeedsDirection(ctx, client, in, logger) {
			res.DirectionMigrated++
		}
		stage, covered, reason := IssueStageFor(in, prStage)
		var want stagelabels.Desired
		var tr stagelabels.Transition
		if stage == stagelabels.None {
			store.Forget(key)
		} else {
			tr = store.Observe(key, stage, reason)
			rec, _ := store.Get(key)
			want = stagelabels.Desired{Stage: stage}
			if !covered {
				want.Stalled = stagelabels.Stalled(rec, policy.Dwell, now)
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
		// Enforce re-reads the issue before writing so a stage a claim hook
		// placed since the list payload was fetched is replaced, not joined
		// (hivecommons/hive#12512), and any duplicate it finds is healed.
		// IssueItem clamps the stage to the issue stages (hivecommons/hive#12451).
		out, err := stagelabels.Enforce(ctx, client.StageLabeler(), stagelabels.IssueItem, owner, name, in.Number, in.Labels, want)
		if err != nil && logger != nil {
			logger.Warn("issue stage labels: write failed", "repo", in.Repo, "issue", in.Number, "stage", want.Stage.String(), "error", err)
		}
		res.Writes += len(out.Removed)
		if len(out.Added) > 0 {
			res.Writes++
		}
		if out.Healed {
			res.Healed++
		}
		if logger != nil && (out.Changed() || err != nil) {
			logger.Info("issue stage labels reconciled", "repo", in.Repo, "issue", in.Number, "stage", out.Stage.String(), "added", out.Added, "removed", out.Removed, "healed", out.Healed, "clamped", out.Clamped, "wanted", out.Wanted.String(), "reason", reason, "bucket", in.Bucket)
		}
		// A regression between the issue's OWN stages (a claim or kick that
		// lapsed without a PR) is the hive's failure and gets the one-line
		// note; a covering PR's own falls are narrated on the PR.
		if tr.Regressed && !covered && tr.Prev.Before(stagelabels.CoveredByPR) {
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

// migrateNeedsDirection is the one-shot retirement of the legacy
// `needs-direction` label (hivecommons/hive#12683): an issue carrying it gets
// the configured needs-decision label and loses `needs-direction`. The add
// goes first and the remove only follows a successful add, so an issue
// mid-migration never un-parks; both writes re-read the issue and are
// journaled against that read (AddLabelsFresh / RemoveLabelsFresh), so an
// issue carrying both labels costs one remove and an unlabelled one costs
// nothing. Readers keep accepting `needs-direction` as an alias. Reports
// whether the legacy label was removed.
func migrateNeedsDirection(ctx context.Context, client *Client, in IssueStageInput, logger *slog.Logger) bool {
	if client == nil || !issueHasLabel(in.Labels, issueNeedsDirectionLabel) {
		return false
	}
	want := client.needsDecisionLabel()
	if err := client.AddLabelsFresh(ctx, in.Repo, in.Number, stagelabels.None, want); err != nil {
		if logger != nil {
			logger.Warn("needs-direction migration: adding needs-decision failed", "repo", in.Repo, "issue", in.Number, "label", want, "error", err)
		}
		return false
	}
	if err := client.RemoveLabelsFresh(ctx, in.Repo, in.Number, issueNeedsDirectionLabel); err != nil {
		if logger != nil {
			logger.Warn("needs-direction migration: removing needs-direction failed", "repo", in.Repo, "issue", in.Number, "error", err)
		}
		return false
	}
	if logger != nil {
		logger.Info("needs-direction migrated", "repo", in.Repo, "issue", in.Number, "added", want)
	}
	return true
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
