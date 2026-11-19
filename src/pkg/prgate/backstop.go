package prgate

import (
	"context"
	"strings"
)

// The backstop sweep re-posts the gate on open PR heads the webhook path
// missed: a delivery lost while the hive restarted, a burst dropped by the
// in-flight bound, a github.token autofix push. It works from the open PR
// list, so it needs no memory of earlier sweeps.
const (
	// MaxBackstopPRsPerRepo bounds the open PRs one sweep lists per
	// repository (one page).
	MaxBackstopPRsPerRepo = 100
	// MaxBackstopPostsPerSweep bounds the heads one sweep hands to the
	// poster across all repositories; the rest wait for the next sweep.
	MaxBackstopPostsPerSweep = 25
)

// OpenPR is one open pull request as the backstop lists it.
type OpenPR struct {
	Number  int
	HeadSHA string
	Title   string
	Draft   bool
}

// BackstopAPI is what the sweep needs from GitHub beyond API.
type BackstopAPI interface {
	// ListOpenPRs lists at most limit open PRs of repo, most recently
	// updated first.
	ListOpenPRs(ctx context.Context, repo string, limit int) ([]OpenPR, error)
	// HeadStatuses returns the latest commit status of each gate context on
	// sha, keyed by context. Other contexts may be omitted.
	HeadStatuses(ctx context.Context, repo, sha string) (map[string]Status, error)
}

// SweepResult counts one backstop sweep.
type SweepResult struct {
	Repos int
	// Listed is the open PRs listed; Drafts of them were skipped.
	Listed, Drafts int
	// Known heads needed no request: this process already posted the gate.
	Known int
	// Present heads already carried the planned statuses (read from GitHub).
	Present int
	// Missed heads were handed to the poster; Posted of them got a status.
	Missed, Posted int
	// Deferred heads were left for the next sweep by the per-sweep bound.
	Deferred int
	Errors   int
}

// Sweep posts the gate on every open, non-draft head of repos that does not
// already carry it. Each missed head goes through HandleEvent with the listed
// head pinned, so the PR is re-read and a head that moved, a draft or a closed
// PR is skipped exactly as for a webhook event. A repository in off mode, with
// the grant missing or in its 403 cooldown costs no request.
func (g *Gate) Sweep(ctx context.Context, b BackstopAPI, repos []string) SweepResult {
	var res SweepResult
	if g == nil || b == nil {
		return res
	}
	budget := MaxBackstopPostsPerSweep
	for _, repo := range repos {
		if ctx.Err() != nil {
			break
		}
		if Effective(g.ConfiguredMode(repo)) == ModeOff || g.RepoForbidden(repo) {
			continue
		}
		g.RefreshGrant(ctx)
		mode := g.mode(repo)
		if mode == ModeOff {
			continue
		}
		res.Repos++
		prs, err := b.ListOpenPRs(ctx, repo, MaxBackstopPRsPerRepo)
		if err != nil {
			res.Errors++
			g.Log.Warn("pr-gate: backstop could not list open PRs", "repo", repo, "error", err)
			continue
		}
		for _, pr := range prs {
			res.Listed++
			if pr.Draft {
				res.Drafts++
				continue
			}
			if !headSHARe.MatchString(pr.HeadSHA) || pr.Number <= 0 {
				continue
			}
			plan := Plan(mode, pr.Title)
			if g.allSent(repo, pr.HeadSHA, plan) {
				res.Known++
				continue
			}
			if budget == 0 {
				res.Deferred++
				continue
			}
			have, err := b.HeadStatuses(ctx, repo, pr.HeadSHA)
			if err != nil {
				res.Errors++
				g.Log.Warn("pr-gate: backstop could not read the head's statuses", "repo", repo, "number", pr.Number, "head", pr.HeadSHA, "error", err)
				continue
			}
			if carries(have, plan) {
				res.Present++
				g.markSent(repo, pr.HeadSHA, plan)
				continue
			}
			budget--
			res.Missed++
			out := g.HandleEvent(ctx, repo, pr.Number, pr.HeadSHA)
			if len(out.Posted) > 0 {
				res.Posted++
				g.Log.Info("pr-gate: backstop posted the gate on a head the hive had missed", "repo", repo, "number", pr.Number, "head", pr.HeadSHA, "contexts", strings.Join(out.Posted, ","))
			}
			if out.Err != nil {
				res.Errors++
				if g.RepoForbidden(repo) {
					break
				}
			}
		}
	}
	if res.Missed > 0 || res.Errors > 0 || res.Deferred > 0 {
		g.Log.Info("pr-gate: backstop sweep", "repos", res.Repos, "listed", res.Listed, "missed", res.Missed, "posted", res.Posted, "deferred", res.Deferred, "errors", res.Errors)
	}
	return res
}

func sentKey(repo, sha, statusContext string) string {
	return strings.ToLower(repo) + "@" + sha + "|" + statusContext
}

// allSent reports whether this process already posted every planned status on
// sha with the same state and description.
func (g *Gate) allSent(repo, sha string, plan []Status) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, st := range plan {
		if prev, ok := g.sent[sentKey(repo, sha, st.Context)]; !ok || prev != (posted{st.State, st.Description}) {
			return false
		}
	}
	return len(plan) > 0
}

// markSent records statuses found on GitHub as posted, so the next sweep and
// the next identical event cost no request.
func (g *Gate) markSent(repo, sha string, plan []Status) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.sent)+len(plan) > maxSentEntries {
		g.sent = map[string]posted{}
	}
	for _, st := range plan {
		g.sent[sentKey(repo, sha, st.Context)] = posted{st.State, st.Description}
	}
}

// carries reports whether have holds every planned status with the planned
// state and description: a head whose title changed since the last post (a
// missed `edited` event) does not.
func carries(have map[string]Status, plan []Status) bool {
	if len(plan) == 0 {
		return true
	}
	for _, st := range plan {
		h, ok := have[st.Context]
		if !ok || h.State != st.State || h.Description != st.Description {
			return false
		}
	}
	return true
}
