package prgate

import (
	"context"
	"strings"
	"time"
)

// Sweep bounds. The backstop reads at most maxSweepHeads open PRs per
// repository per pass and posts for at most maxSweepPosts of them, so a repo
// with a large backlog catches up over several passes instead of in one burst.
const (
	maxSweepHeads = 200
	maxSweepPosts = 20
	// maxEscalationProbe is how many of the newest open heads are checked for
	// the required context when deciding whether to escalate.
	maxEscalationProbe = 3
	// maxKnownHeads bounds the memory of heads already seen with a gate status.
	maxKnownHeads = 4096
)

// OpenHead is an open PR head the sweep considers.
type OpenHead struct {
	Number  int
	HeadSHA string
	Draft   bool
}

// SweepAPI is the extra GitHub surface the backstop sweep needs. An API that
// does not implement it simply has no sweep.
type SweepAPI interface {
	// ListOpenHeads lists the repository's open PRs, newest first.
	ListOpenHeads(ctx context.Context, repo string, limit int) ([]OpenHead, error)
	// HeadHasContext reports whether a commit status or a check run with this
	// exact name exists on sha.
	HeadHasContext(ctx context.Context, repo, sha, name string) (bool, error)
}

// SweepResult counts one pass.
type SweepResult struct {
	Repos, Heads, Posted, Present int
	Escalated                     bool
}

// EscalationMessage is the log line the fleet report's
// github_app_statuses_permission_missing class is fed by (pkg/fleetreport).
const EscalationMessage = "pr-gate: cannot post the required PR-contents status: the GitHub App lacks the statuses: write permission"

// EscalationRepeats is how many times one escalating pass states the line: the
// repeat count of the fleet-report class it feeds (checked by a pkg/fleetreport test).
const EscalationRepeats = 5

// primaryContext is the context the gate leaves on a head in mode.
func primaryContext(mode Mode) string {
	if mode == ModeEnforce {
		return ContextVerify
	}
	return ContextVerifyShadow
}

// Sweep is the backstop for missed events (a dropped delivery, a restart, a
// github.token push that fires no pull_request event): for every open,
// non-draft PR head in repos that has no gate status, it runs the same pass an
// event runs. Heads this process already posted or saw are skipped without a
// request. While the gate cannot post (grant missing, write refused) it posts
// nothing; from the cutoff, if the required context is also absent on recent
// heads, it raises the escalation line once per pass.
func (g *Gate) Sweep(ctx context.Context, repos []string) SweepResult {
	var res SweepResult
	if g == nil {
		return res
	}
	api, ok := g.API.(SweepAPI)
	if !ok {
		return res
	}
	g.RefreshGrant(ctx)
	for _, repo := range repos {
		if ctx.Err() != nil {
			return res
		}
		if g.ConfiguredMode(repo) == ModeOff {
			continue
		}
		res.Repos++
		heads, err := api.ListOpenHeads(ctx, repo, maxSweepHeads)
		if err != nil {
			g.Log.Warn("pr-gate: sweep could not list open PRs", "repo", repo, "error", err)
			continue
		}
		if g.cannotPost(repo) {
			if g.escalate(ctx, api, repo, heads) {
				res.Escalated = true
			}
			continue
		}
		mode := g.mode(repo)
		posts := 0
		for _, h := range heads {
			if h.Draft || !headSHARe.MatchString(h.HeadSHA) {
				continue
			}
			res.Heads++
			if g.knownHead(repo, h.HeadSHA, mode) {
				continue
			}
			if posts >= maxSweepPosts {
				break
			}
			has, err := api.HeadHasContext(ctx, repo, h.HeadSHA, primaryContext(mode))
			if err != nil {
				g.Log.Warn("pr-gate: sweep could not read a head's statuses", "repo", repo, "head", h.HeadSHA, "error", err)
				break
			}
			if has {
				g.rememberHead(repo, h.HeadSHA, mode)
				res.Present++
				continue
			}
			posts++
			out := g.HandleEvent(ctx, repo, h.Number, h.HeadSHA)
			if len(out.Posted) > 0 {
				g.rememberHead(repo, h.HeadSHA, mode)
				res.Posted++
			}
			if out.Err != nil {
				break
			}
		}
	}
	if res.Posted > 0 {
		g.Log.Info("pr-gate: sweep posted for heads that had no gate status", "heads", res.Heads, "posted", res.Posted)
	}
	return res
}

// cannotPost reports whether a write is known to fail for repo right now.
func (g *Gate) cannotPost(repo string) bool {
	return g.GrantMissing() || g.RepoForbidden(repo)
}

func headKey(repo, sha string, mode Mode) string {
	return strings.ToLower(repo) + "@" + sha + "|" + string(mode)
}

func (g *Gate) knownHead(repo, sha string, mode Mode) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.known[headKey(repo, sha, mode)]
	return ok
}

func (g *Gate) rememberHead(repo, sha string, mode Mode) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.known == nil || len(g.known) >= maxKnownHeads {
		g.known = map[string]struct{}{}
	}
	g.known[headKey(repo, sha, mode)] = struct{}{}
}

// escalate raises the escalation line when the cutoff has passed, the gate
// cannot post, and the required context is absent on recent heads: the case
// where merges are stalled and nobody has been told. It returns whether it did.
func (g *Gate) escalate(ctx context.Context, api SweepAPI, repo string, heads []OpenHead) bool {
	if g.Now().Before(Cutoff) {
		return false
	}
	probed := 0
	for _, h := range heads {
		if h.Draft || !headSHARe.MatchString(h.HeadSHA) {
			continue
		}
		if probed >= maxEscalationProbe {
			break
		}
		probed++
		has, err := api.HeadHasContext(ctx, repo, h.HeadSHA, ContextVerify)
		if err != nil {
			return false
		}
		if !has {
			// The fleet report files a log-fed class only after this many
			// repeats inside its window; the sweep has already established
			// that merges are stalled, so it states the line at that
			// threshold instead of waiting several passes for it.
			for n := 1; n <= EscalationRepeats; n++ {
				g.Log.Warn(EscalationMessage, "repo", repo, "head", h.HeadSHA, "grant_missing", g.GrantMissing(),
					"forbidden", g.RepoForbidden(repo), "evidence", n, "of", EscalationRepeats, "hint", MissingGrantHint)
			}
			return true
		}
	}
	return false
}

// SweepInterval is how often the server runs the backstop.
const SweepInterval = 5 * time.Minute

// SweepStartDelay is how long after the gate is built the first pass runs.
const SweepStartDelay = time.Minute
