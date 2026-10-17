package github

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// PR CI recovery trail (hivecommons/hive#12365 follow-up): the evidence
// card's claim-vs-verdict panel grades how a PR that was CI-red when the
// agent claimed done RECOVERED, not the red itself — on a hive that lets CI
// arbitrate, red at claim is the design. The trail is every head the PR
// carried from the claim head on, with what CI says about each now, so the
// panel can count pushes to green and time to green.

// PRCIHead is one PR head in the trail: when it was committed and what the
// merge gate would say about it now. State is judged on the REQUIRED checks
// only when the required set for the PR's base branch is known (green when
// every required context succeeded, red when one failed, pending when one
// is still running or has not reported yet, none when the head carries no
// check at all); otherwise it falls back to every gating check on the head.
// A red non-required check (a docs link check, a changelog guard, a
// cancelled optional build) never makes a head red here, exactly as it does
// not block the hive's merge gate — it is disclosed in NonRequiredRed.
type PRCIHead struct {
	SHA   string    `json:"sha"`
	At    time.Time `json:"at"`
	State string    `json:"state"`
	// MissingRequired names required contexts with no status and no check
	// run on this head. On a merged head that is a gate breach.
	MissingRequired []string `json:"missing_required,omitempty"`
	// RequiredRed names the required contexts that failed on this head.
	RequiredRed []string `json:"required_red,omitempty"`
	// NonRequiredRed names failed checks outside the required set. Disclosed,
	// never graded.
	NonRequiredRed []string `json:"non_required_red,omitempty"`
}

// PRCITrail is a PR's heads from the claim head onward plus its outcome.
type PRCITrail struct {
	State    string     `json:"state"` // open, closed or merged
	MergedAt time.Time  `json:"merged_at,omitzero"`
	MergedBy string     `json:"merged_by,omitempty"`
	ClosedAt time.Time  `json:"closed_at,omitzero"`
	Heads    []PRCIHead `json:"heads"`
	// Truncated is true when the PR had more heads after the claim than
	// prCITrailMaxHeads and the oldest were skipped (the trail still ends at
	// the current head).
	Truncated bool `json:"truncated,omitempty"`
	// Evaluated is the number of distinct commits, from the PR's first, that
	// CI evaluated (had any check or status) up to and including the first
	// green one, capped at PRCIConvergenceCap. PassesToGreen is Evaluated
	// when a green commit was found and 0 when none was within the cap.
	Evaluated     int  `json:"evaluated,omitempty"`
	PassesToGreen int  `json:"passes_to_green,omitempty"`
	ScanCapped    bool `json:"scan_capped,omitempty"`
	// RequiredKnown is true when head states were judged against the base
	// branch's required checks (RequiredChecks); false means the any-check
	// fallback was used because the required set could not be resolved.
	RequiredKnown  bool     `json:"required_known,omitempty"`
	RequiredChecks []string `json:"required_checks,omitempty"`
}

const (
	prCITrailMaxHeads = 6
	prCITrailPerPage  = 100
	prCITrailMaxPages = 3
	// PRCIConvergenceCap bounds the passes-to-green scan: anything past it
	// is bucketed as "4+" by the dashboard, so no finer count is needed.
	PRCIConvergenceCap = 4

	PRCIHeadGreen   = "green"
	PRCIHeadRed     = "red"
	PRCIHeadPending = "pending"
	PRCIHeadNone    = "none"
)

// PRCIRecoveryTrail lists the PR's commits, keeps the claim head and every
// commit after it (the newest prCITrailMaxHeads when there are more), and
// summarises CI for each. When claimHead is not among the PR's commits
// (force-pushed away), the trail is the newest heads. Sheds under REST
// budget pressure like the other dashboard-only lookups.
func (c *Client) PRCIRecoveryTrail(ctx context.Context, repo string, number int, claimHead string) (PRCITrail, error) {
	var out PRCITrail
	if c == nil || c.client == nil {
		return out, ErrNoGitHubClient
	}
	if c.shouldShedMTTRIssueFetches() {
		return out, fmt.Errorf("github REST budget below reserve %d; skipping CI trail for %s#%d", lowValueRESTBudgetFloor, repo, number)
	}
	owner, name := c.splitRepo(repo)
	var requiredSet map[string]bool
	st, err := c.GetPRState(ctx, repo, number)
	if err != nil {
		return out, err
	}
	out.State = strings.ToLower(st.State)
	if base := strings.TrimSpace(st.BaseRef); base != "" {
		configSet, configKnown := c.configRequiredChecksForRepo(repo)
		required, known := RequiredStatusCheckContexts(ctx, c.client, owner, name, base, configSet, configKnown)
		if known && len(required) > 0 {
			out.RequiredKnown = true
			for n := range required {
				out.RequiredChecks = append(out.RequiredChecks, n)
			}
			sort.Strings(out.RequiredChecks)
			requiredSet = required
		}
	}
	if !st.MergedAt.IsZero() {
		out.State = "merged"
		out.MergedAt = st.MergedAt.UTC()
		out.MergedBy = st.MergedBy
	}
	if !st.ClosedAt.IsZero() {
		out.ClosedAt = st.ClosedAt.UTC()
	}
	opts := &gh.ListOptions{PerPage: prCITrailPerPage}
	var commits []*gh.RepositoryCommit
	for page := 0; page < prCITrailMaxPages; page++ {
		got, resp, err := c.client.PullRequests.ListCommits(ctx, owner, name, number, opts)
		if err != nil {
			return out, fmt.Errorf("listing commits for %s#%d: %w", repo, number, err)
		}
		commits = append(commits, got...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	start := 0
	for i, cm := range commits {
		if strings.EqualFold(cm.GetSHA(), claimHead) {
			start = i
			break
		}
	}
	heads := commits[start:]
	if len(heads) > prCITrailMaxHeads {
		heads = heads[len(heads)-prCITrailMaxHeads:]
		out.Truncated = true
	}
	reads := map[string]PRCIHead{}
	readOf := func(sha string) (PRCIHead, error) {
		if h, ok := reads[sha]; ok {
			return h, nil
		}
		statuses, runs, err := fetchCommitChecks(ctx, c.client, owner, name, sha)
		if err != nil {
			return PRCIHead{}, fmt.Errorf("checks for %s@%s: %w", repo, sha, err)
		}
		h := prCIHeadRead(statuses, runs, requiredSet)
		reads[sha] = h
		return h, nil
	}
	for _, cm := range heads {
		h, err := readOf(cm.GetSHA())
		if err != nil {
			return out, err
		}
		h.SHA, h.At = cm.GetSHA(), cm.GetCommit().GetCommitter().GetDate().Time.UTC()
		out.Heads = append(out.Heads, h)
	}
	// Convergence: walk from the PR's first commit counting evaluated ones
	// until the first green, stopping once the cap is reached.
	for _, cm := range commits {
		if out.Evaluated >= PRCIConvergenceCap {
			out.ScanCapped = true
			break
		}
		h, err := readOf(cm.GetSHA())
		if err != nil {
			return out, err
		}
		if h.State == PRCIHeadNone {
			continue
		}
		out.Evaluated++
		if h.State == PRCIHeadGreen {
			out.PassesToGreen = out.Evaluated
			break
		}
	}
	return out, nil
}

// prCIHeadRead judges one head. With a known required set the verdict is
// the gate's: required red → red; required pending or not yet reported →
// pending; all required green → green. Without one, every gating check on
// the head counts (the pre-#12610 reading). A head GitHub has nothing on is
// none either way, so the convergence scan can skip commits CI never saw.
func prCIHeadRead(statuses []*gh.RepoStatus, runs []*gh.CheckRun, required map[string]bool) PRCIHead {
	all := summarizeChecks(statuses, runs)
	if len(required) == 0 {
		return PRCIHead{State: prCIHeadState(all)}
	}
	req := summarizeRequiredChecks(statuses, runs, required)
	h := PRCIHead{RequiredRed: req.Red, MissingRequired: req.Missing}
	for _, n := range all.Red {
		if !required[n] {
			h.NonRequiredRed = append(h.NonRequiredRed, n)
		}
	}
	switch {
	case all.Total == 0 && len(statuses) == 0 && len(runs) == 0:
		h.State = PRCIHeadNone
		h.MissingRequired = nil
	case len(req.Red) > 0:
		h.State = PRCIHeadRed
	case req.Pending > 0 || len(req.Missing) > 0:
		h.State = PRCIHeadPending
	default:
		h.State = PRCIHeadGreen
	}
	return h
}

func prCIHeadState(s CommitChecksSummary) string {
	switch {
	case len(s.Red) > 0:
		return PRCIHeadRed
	case s.Pending > 0:
		return PRCIHeadPending
	case s.Green > 0:
		return PRCIHeadGreen
	}
	return PRCIHeadNone
}
