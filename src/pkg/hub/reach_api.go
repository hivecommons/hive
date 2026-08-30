package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/reach"
)

// ============================================================================
// /api/reach — read-only PR reach telemetry (#3994, phase 2b of #3973)
// ============================================================================
//
// Joins merged PRs (changed files → components, D3) against the per-hive
// component_reach reports phase 2a (#3993) persists from the heartbeat, via
// the ancestry rule: a hive counts only when the commit it reports RUNNING
// contains the PR's merge commit (the #3816 anchoring rule — merged is not
// deployed). Co-deployed PRs share one window and are labeled shared (D4).
//
// Read-only by construction: no GitHub writes, no state mutation beyond the
// caches the underlying clients already keep.

// reachRepoOwner / reachRepoName pin the repo the reach join is defined
// over — the same repo commit_order.go compares against.
const (
	reachRepoOwner = "kubestellar"
	reachRepoName  = "hive"
)

// defaultReachRecentPRs / maxReachRecentPRs bound the ?recent= listing.
// Twenty covers a typical week of merges into one branch; one hundred caps
// the GitHub file-listing fan-out an admin query can trigger.
const (
	defaultReachRecentPRs = 20
	maxReachRecentPRs     = 100
)

// reachRequestTimeout bounds one /api/reach request end-to-end (GitHub PR
// fetches + compare-API ancestry resolves). Generous because a cold cache
// may resolve several ancestry pairs, but finite so a wedged upstream can
// never pin the handler.
const reachRequestTimeout = 30 * time.Second

// reachRepoDirEnv optionally points at a local git clone with real history.
// When set, ancestry resolves via `git merge-base --is-ancestor` against
// that clone (the epic's resolved OQ-3 answer). The hub container image
// ships no clone, so the default is the compare-API adapter below, which
// reuses commit_order.go's fetcher and permanent cache.
const reachRepoDirEnv = "HIVE_REACH_REPO_DIR"

// newReachAncestry picks the ancestry backend: a configured local clone
// when reachRepoDirEnv is set, otherwise the GitHub compare API.
func newReachAncestry(logger *slog.Logger) reach.Ancestry {
	if dir := os.Getenv(reachRepoDirEnv); dir != "" {
		return reach.NewGitAncestry(dir)
	}
	return compareAncestry{logger: logger}
}

// compareAncestry answers reach.Ancestry with the hub's EXISTING ancestry
// primitive: the GitHub compare API behind commit_order.go. It shares that
// file's permanent cache (ancestry is immutable) and its fetcher var (so
// tests stub one seam for both callers). Unlike commitAtOrAheadOfTarget it
// resolves SYNCHRONOUSLY — /api/reach runs in a request handler that holds
// no HubServer mutex, so blocking on one bounded API call is safe and a
// definite answer beats "false for now".
type compareAncestry struct{ logger *slog.Logger }

// IsAncestor reports whether ancestor is contained in descendant, per the
// compare API ("ahead"/"identical" of base=ancestor, head=descendant —
// the exact commitAtOrAheadOfTarget semantics).
func (c compareAncestry) IsAncestor(ancestor, descendant string) (bool, error) {
	if ancestor == "" || descendant == "" {
		// An unknown commit must never read as reached.
		return false, fmt.Errorf("reach ancestry: empty commit (ancestor=%q descendant=%q)", ancestor, descendant)
	}
	if sameCommit(ancestor, descendant) {
		return true, nil
	}
	key := commitOrderKey{target: shortSHA(ancestor), reported: shortSHA(descendant)}

	commitOrderMu.Lock()
	if atOrAhead, ok := commitOrderCache[key]; ok {
		commitOrderMu.Unlock()
		return atOrAhead, nil
	}
	// Capture the fetcher under the mutex tests stub it under (see
	// fetchCommitCompareStatus's contract in commit_order.go).
	fetch := fetchCommitCompareStatus
	commitOrderMu.Unlock()

	status, err := fetch(key.target, key.reported, c.logger)
	if err != nil {
		// Errors are never cached (commit_order.go discipline): a transient
		// failure retries on the next request instead of poisoning the pair.
		return false, err
	}
	atOrAhead := status == "ahead" || status == "identical"

	commitOrderMu.Lock()
	if len(commitOrderCache) >= commitOrderCacheMax {
		commitOrderCache = map[commitOrderKey]bool{}
	}
	commitOrderCache[key] = atOrAhead
	commitOrderMu.Unlock()
	return atOrAhead, nil
}

// githubPRSource backs reach.PRSource with the existing pkg/github client
// (cached merged-PR facts — pkg/github/pr_reach.go). base is the branch PRs
// must have merged into: the hub's own branch, since that is the lineage the
// fleet it registers runs.
type githubPRSource struct {
	client *hivegithub.Client
	base   string
}

// NewGitHubPRSource wraps the hub's GitHub client as the /api/reach PR
// source. base is the merge-target branch (the hub's running branch).
func NewGitHubPRSource(client *hivegithub.Client, base string) reach.PRSource {
	return &githubPRSource{client: client, base: base}
}

// MergedPR implements reach.PRSource.
func (g *githubPRSource) MergedPR(ctx context.Context, number int) (reach.PRInfo, error) {
	pr, err := g.client.MergedPR(ctx, reachRepoOwner, reachRepoName, number)
	if err != nil {
		return reach.PRInfo{}, err
	}
	files, err := g.client.ListMergedPRFiles(ctx, reachRepoOwner, reachRepoName, number)
	if err != nil {
		return reach.PRInfo{}, err
	}
	return reach.PRInfo{
		Number:      pr.Number,
		Title:       pr.Title,
		MergeCommit: pr.MergeCommitSHA,
		MergedAt:    pr.MergedAt,
		Files:       files,
	}, nil
}

// RecentMergedPRs implements reach.PRSource.
func (g *githubPRSource) RecentMergedPRs(ctx context.Context, limit int) ([]reach.PRInfo, error) {
	merged, err := g.client.RecentMergedPRs(ctx, reachRepoOwner, reachRepoName, g.base, limit)
	if err != nil {
		return nil, err
	}
	infos := make([]reach.PRInfo, 0, len(merged))
	for _, pr := range merged {
		files, err := g.client.ListMergedPRFiles(ctx, reachRepoOwner, reachRepoName, pr.Number)
		if err != nil {
			return nil, err
		}
		infos = append(infos, reach.PRInfo{
			Number:      pr.Number,
			Title:       pr.Title,
			MergeCommit: pr.MergeCommitSHA,
			MergedAt:    pr.MergedAt,
			Files:       files,
		})
	}
	return infos, nil
}

// SetReachPRSource wires the GitHub-backed PR source (called from runHub
// when GitHub credentials exist; the endpoint answers 503 until then).
// Call before Start.
func (s *HubServer) SetReachPRSource(src reach.PRSource) {
	s.reachPRSource = src
}

// SetReachReporter wires phase 2a's heartbeat-fed reach store over the
// default stub. Call before Start. This is the ONE wiring line 2a needs
// to feed this endpoint real fleet data.
func (s *HubServer) SetReachReporter(r reach.ReachReporter) {
	if r != nil {
		s.reachReporter = r
	}
}

// RegistryReachReporter adapts the hub registry's per-hive component_reach
// storage (phase 2a, #3993 — sanitized heartbeat reports on RegistryEntry)
// to the reach.ReachReporter seam phase 2b reads through (#3994). This is
// the producer→consumer wiring that completes the epic (#3973) through 2b:
// the registry is the single source, snapshotted under the server lock, so
// the endpoint never observes a half-written report.
func (s *HubServer) RegistryReachReporter() reach.ReachReporter {
	return registryReachReporter{s: s}
}

type registryReachReporter struct{ s *HubServer }

func (r registryReachReporter) LatestReach() map[string][]reach.ComponentReach {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()
	out := make(map[string][]reach.ComponentReach)
	for _, h := range r.s.registry.Hives {
		if h.ComponentReach == nil || len(h.ComponentReach.Entries) == 0 {
			continue
		}
		list := make([]reach.ComponentReach, 0, len(h.ComponentReach.Entries))
		for _, e := range h.ComponentReach.Entries {
			// Timestamps were RFC3339-validated by sanitizeComponentReach on
			// receipt; a parse failure here would mean corrupted storage, and
			// the entry is skipped rather than reported with a zero time that
			// the never-ran/first-seen logic would misread as ancient.
			fs, errF := time.Parse(time.RFC3339, e.FirstSeen)
			ls, errL := time.Parse(time.RFC3339, e.LastSeen)
			if errF != nil || errL != nil {
				continue
			}
			list = append(list, reach.ComponentReach{
				Component:  e.Component,
				Commit:     e.Commit,
				SpansTotal: e.SpansTotal,
				SpansError: e.SpansError,
				FirstSeen:  fs,
				LastSeen:   ls,
			})
		}
		if len(list) > 0 {
			out[h.ID] = list
		}
	}
	return out
}

// reachResponse is the /api/reach payload.
type reachResponse struct {
	GeneratedAt time.Time `json:"generated_at"`
	// HivesReporting counts hives with a persisted component_reach report —
	// zero until phase 2a (#3993) merges and is wired.
	HivesReporting int `json:"hives_reporting"`
	// DeployedCommits is the digest-anchored deploy history derived from
	// what hives report running, oldest first (the D4 window boundaries).
	DeployedCommits []string              `json:"deployed_commits"`
	Reports         []reach.PRReachReport `json:"reports"`
}

// writeReachError emits the endpoint's uniform JSON error shape.
func writeReachError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// handleReach serves GET /api/reach — by PR (?pr=NNN) or the recent-PRs
// list (?recent=K, default defaultReachRecentPRs). Registered behind
// requireAdmin in registerSaaSRoutes: the payload names hives fleet-wide,
// exactly the cluster-health exposure class.
func (s *HubServer) handleReach(w http.ResponseWriter, r *http.Request) {
	src := s.reachPRSource
	if src == nil {
		writeReachError(w, http.StatusServiceUnavailable,
			"reach: no GitHub PR source configured (hub is running without GitHub credentials)")
		return
	}
	reporter := s.reachReporter
	if reporter == nil {
		reporter = &reach.StubReachReporter{}
	}

	ctx, cancel := context.WithTimeout(r.Context(), reachRequestTimeout)
	defer cancel()

	var prs []reach.PRInfo
	switch {
	case r.URL.Query().Get("pr") != "":
		number, err := strconv.Atoi(r.URL.Query().Get("pr"))
		if err != nil || number <= 0 {
			writeReachError(w, http.StatusBadRequest, "invalid pr parameter")
			return
		}
		pr, err := src.MergedPR(ctx, number)
		if err != nil {
			s.logger.Warn("reach: PR fetch failed", "pr", number, "error", err)
			writeReachError(w, http.StatusBadGateway, fmt.Sprintf("fetching PR %d: %v", number, err))
			return
		}
		prs = []reach.PRInfo{pr}
	default:
		limit := defaultReachRecentPRs
		if raw := r.URL.Query().Get("recent"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed <= 0 || parsed > maxReachRecentPRs {
				writeReachError(w, http.StatusBadRequest,
					fmt.Sprintf("invalid recent parameter (1..%d)", maxReachRecentPRs))
				return
			}
			limit = parsed
		}
		recent, err := src.RecentMergedPRs(ctx, limit)
		if err != nil {
			s.logger.Warn("reach: recent-PRs fetch failed", "error", err)
			writeReachError(w, http.StatusBadGateway, fmt.Sprintf("listing recent merged PRs: %v", err))
			return
		}
		prs = recent
	}

	reports := reporter.LatestReach()
	deployed := reach.DeployedCommits(reports)

	// D4 window assignment over the WHOLE queried set, so co-deployed PRs
	// label each other. Ancestry failure aborts: partial windows would
	// silently mis-attribute shared reach.
	windows, err := reach.AssignWindows(deployed, prs, s.reachAncestry)
	if err != nil {
		s.logger.Warn("reach: window assignment failed", "error", err)
		writeReachError(w, http.StatusBadGateway, fmt.Sprintf("resolving deploy windows: %v", err))
		return
	}

	// History (#3995, phase 2c) feeds the per-deploy-window error deltas.
	// s.reachHistory is nil on bare test servers; both the interface hop and
	// AttachErrorDeltas are nil-safe, and a nil history simply leaves
	// ErrorDeltas absent — unmeasured, never fabricated.
	var history reach.HistorySource
	if s.reachHistory != nil {
		history = s.reachHistory
	}
	analyzer := &reach.Analyzer{Ancestry: s.reachAncestry, Reporter: reporter, History: history}
	resp := reachResponse{
		GeneratedAt:     time.Now().UTC(),
		HivesReporting:  len(reports),
		DeployedCommits: deployed,
		Reports:         make([]reach.PRReachReport, 0, len(prs)),
	}
	for _, pr := range prs {
		report, err := analyzer.Analyze(pr)
		if err != nil {
			s.logger.Warn("reach: analysis failed", "pr", pr.Number, "error", err)
			writeReachError(w, http.StatusBadGateway, fmt.Sprintf("analyzing PR %d: %v", pr.Number, err))
			return
		}
		if assignment, ok := windows[pr.Number]; ok {
			report.DeployWindow = assignment.DeployedCommit
			report.SharedWith = assignment.SharedWith
		}
		// Error-rate deltas (#3995) attach AFTER window assignment — the
		// delta is keyed on the deploy window (D4), which Analyze alone
		// cannot know. Shared across the PRs in SharedWith by construction.
		analyzer.AttachErrorDeltas(&report)
		resp.Reports = append(resp.Reports, report)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
