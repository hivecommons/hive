package dashboard

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/upstreamwatch"
)

// upstreamWatchStatePath is the upstream watch's durable state, written by
// the eval-tick pass in cmd/hive/upstream_watch.go. The dashboard only ever
// READS it. Production always uses this path; a var (not const) only so tests
// can redirect it via SetUpstreamWatchStatePathForTest, mirroring
// SetFleetReportStatePathForTest.
var upstreamWatchStatePath = "/data/upstream-watch.json"

// upstreamWatchRecentLimit caps the recent-items list per repo so a long-lived
// watch cannot grow the response without bound.
const upstreamWatchRecentLimit = 20

// SetUpstreamWatchStatePathForTest redirects the upstream-watch state the
// dashboard reads to path for the lifetime of t.
func SetUpstreamWatchStatePathForTest(t interface {
	Helper()
	Cleanup(func())
}, path string) {
	t.Helper()
	old := upstreamWatchStatePath
	upstreamWatchStatePath = path
	t.Cleanup(func() { upstreamWatchStatePath = old })
}

// UpstreamWatchItem is one recent upstream ref as the divergence view renders
// it: what upstream change it was, which fork issue carries it, and where the
// ref stands (hivecommons/hive#9969).
type UpstreamWatchItem struct {
	// Ref is the watch's dedupe key: "upstream#<pr>" or "release:<tag>".
	Ref string `json:"ref"`
	// UpstreamRef is the human-readable upstream-qualified form,
	// "owner/repo#123" or "owner/repo@<tag>".
	UpstreamRef string `json:"upstream_ref,omitempty"`
	// UpstreamURL links the upstream PR or release; DiffURL is the upstream
	// patch a porting agent reads, empty for a release.
	UpstreamURL string `json:"upstream_url,omitempty"`
	DiffURL     string `json:"diff_url,omitempty"`
	// Status is the recorded outcome: filed / skipped / ported / dismissed.
	Status string `json:"status"`
	// State is the divergence state the panel groups by: surfaced (a fork
	// issue is open for it), ported, dismissed or skipped.
	State string `json:"state"`
	// IssueNumber / IssueURL are the fork issue, zero/empty when none was
	// opened (a skipped ref).
	IssueNumber int    `json:"issue_number,omitempty"`
	IssueURL    string `json:"issue_url,omitempty"`
	// RecordedAt is when the watch last recorded this ref.
	RecordedAt string `json:"recorded_at,omitempty"`
}

// UpstreamWatchRepo is one watched fork's divergence from its upstream.
type UpstreamWatchRepo struct {
	Repo     string `json:"repo"`
	Upstream string `json:"upstream,omitempty"`
	// Watermark is the upstream timestamp everything at or before which has
	// been handled; LastRunAt is when the watch last polled this repo. Both
	// are empty when the repo is configured but has never run.
	Watermark string `json:"watermark,omitempty"`
	LastRunAt string `json:"last_run_at,omitempty"`
	// Surfaced counts refs that produced a fork issue; the rest count by
	// recorded status.
	Surfaced  int `json:"surfaced"`
	Ported    int `json:"ported"`
	Dismissed int `json:"dismissed"`
	Skipped   int `json:"skipped"`
	// Recent lists refs newest-first, capped at upstreamWatchRecentLimit.
	Recent []UpstreamWatchItem `json:"recent,omitempty"`
	// Config is the repo's upstream_watch.repos entry as configured, so the
	// Features-tab editor can prefill it. Upstream above is the resolved
	// value; Config.Upstream stays empty when the fork parent is used.
	Config config.UpstreamWatchRepo `json:"config"`
}

// UpstreamWatchStatus is the GET /api/upstream-watch payload.
type UpstreamWatchStatus struct {
	// Enabled mirrors upstream_watch.enabled; Configured is true when at
	// least one repo is declared. The panel renders its "not configured"
	// shell from these instead of guessing from an empty list.
	Enabled    bool   `json:"enabled"`
	Configured bool   `json:"configured"`
	StatePath  string `json:"state_path"`
	// StateError is a human-readable reason the state file could not be read
	// (a corrupt file stops the watch, and that must be visible here rather
	// than looking like "nothing has happened yet").
	StateError string              `json:"state_error,omitempty"`
	Repos      []UpstreamWatchRepo `json:"repos,omitempty"`
	// Interval is upstream_watch.interval as a Go duration string, empty when
	// unset (the default applies).
	Interval string `json:"interval,omitempty"`
	// ProjectRepos is project.repos: the only repos the editor may add, since
	// validateUpstreamWatch rejects any other key.
	ProjectRepos []string `json:"project_repos,omitempty"`
}

// handleUpstreamWatch serves the upstream-watch divergence view: per watched
// fork, the upstream it follows, the watermark and last-run time, how much has
// been surfaced / ported / dismissed, and the recent refs with their fork
// issue. It is strictly read-only — it reads the config and the state file the
// eval-tick pass writes, and never polls GitHub, so it is cheap enough to sit
// behind a panel refresh. Writes go through handleUpstreamWatchConfigPut.
func (s *Server) handleUpstreamWatch(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var cfg *config.Config
	if s != nil && s.deps != nil {
		cfg = s.deps.Config
	}
	if cfg == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	out := UpstreamWatchStatus{
		Enabled:    cfg.UpstreamWatch.Enabled,
		Configured: len(cfg.UpstreamWatch.Repos) > 0,
		StatePath:  upstreamWatchStatePath,
	}
	if cfg.UpstreamWatch.Interval > 0 {
		out.Interval = cfg.UpstreamWatch.Interval.String()
	}
	out.ProjectRepos = append(out.ProjectRepos, cfg.Project.Repos...)
	state, err := upstreamwatch.NewFileStore(upstreamWatchStatePath).Load()
	if err != nil {
		out.StateError = err.Error()
		state = upstreamwatch.State{}
	}
	keys := make([]string, 0, len(cfg.UpstreamWatch.Repos))
	for key := range cfg.UpstreamWatch.Repos {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out.Repos = append(out.Repos, upstreamWatchRepoView(cfg, key, state))
	}
	jsonResponse(w, out)
}

// upstreamWatchRepoView projects one configured repo plus its recorded state
// into the response shape. A repo the watch has never run for still appears,
// with zero counts, so "configured but idle" is distinguishable from "not
// configured at all".
func upstreamWatchRepoView(cfg *config.Config, key string, state upstreamwatch.State) UpstreamWatchRepo {
	rc := cfg.UpstreamWatch.Repos[key]
	view := UpstreamWatchRepo{Repo: key, Upstream: strings.TrimSpace(rc.Upstream), Config: rc}
	sum, ok := state.Summary(key, upstreamWatchRecentLimit)
	if !ok {
		return view
	}
	if sum.Upstream != "" {
		// The resolved upstream (fork parent) wins over the configured one:
		// an empty upstream in config is the common case.
		view.Upstream = sum.Upstream
	}
	if !sum.Watermark.IsZero() {
		view.Watermark = sum.Watermark.UTC().Format(time.RFC3339)
	}
	if !sum.LastRunAt.IsZero() {
		view.LastRunAt = sum.LastRunAt.UTC().Format(time.RFC3339)
	}
	view.Surfaced, view.Ported = sum.Surfaced, sum.Ported
	view.Dismissed, view.Skipped = sum.Dismissed, sum.Skipped
	forkOwner, forkRepo, forkOK := upstreamWatchForkRepo(cfg, key)
	for _, rec := range sum.Recent {
		item := UpstreamWatchItem{
			Ref:         rec.Ref,
			Status:      string(rec.Status),
			State:       upstreamWatchState(rec.Status),
			IssueNumber: rec.IssueNumber,
		}
		if !rec.FiledAt.IsZero() {
			item.RecordedAt = rec.FiledAt.UTC().Format(time.RFC3339)
		}
		if view.Upstream != "" {
			item.UpstreamRef = upstreamwatch.UpstreamRef(view.Upstream, rec.Ref)
			item.UpstreamURL = upstreamwatch.UpstreamURL(view.Upstream, rec.Ref)
			item.DiffURL = upstreamwatch.DiffURL(view.Upstream, rec.Ref)
		}
		if forkOK && rec.IssueNumber > 0 {
			item.IssueURL = "https://github.com/" + forkOwner + "/" + forkRepo + "/issues/" + strconv.Itoa(rec.IssueNumber)
		}
		view.Recent = append(view.Recent, item)
	}
	return view
}

// upstreamWatchState maps a recorded ref status onto the divergence state the
// panel groups by. A ref that was filed has an open fork issue until the watch
// records it ported or dismissed, so "filed" reads as "surfaced".
func upstreamWatchState(status upstreamwatch.RefStatus) string {
	switch status {
	case upstreamwatch.StatusFiled:
		return "surfaced"
	case upstreamwatch.StatusPorted:
		return "ported"
	case upstreamwatch.StatusDismissed:
		return "dismissed"
	case upstreamwatch.StatusSkipped:
		return "skipped"
	default:
		return string(status)
	}
}

// upstreamWatchForkRepo resolves an upstream_watch.repos key to the fork's
// owner/repo the same way the eval-tick pass does: an org-qualified key is
// split, a bare one is qualified with project.org.
func upstreamWatchForkRepo(cfg *config.Config, key string) (owner, repo string, ok bool) {
	if strings.Contains(key, "/") {
		return upstreamwatch.SplitRepo(key)
	}
	org := strings.TrimSpace(cfg.Project.Org)
	key = strings.TrimSpace(key)
	if org == "" || key == "" {
		return "", "", false
	}
	return org, key, true
}
