package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence/benchmarks"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// Benchmark publication (hivecommons/hive#17694). The spoke converts its
// review evidence bundles into anonymized leaderboard records
// (pkg/benchmarks) and, hourly, pushes the ones newer than the hub's last
// acknowledgement to POST /api/benchmarks/ingest on the hub — the same
// spoke→hub bearer pattern as NPS and task-status, chosen over a hub pull
// because the hub holds no bearer for spokes and most spoke URLs are
// cluster-private. The hub batches and commits to hivecommons/benchmarks.

const (
	// benchmarksPushInterval is the spoke→hub cadence; the hub publishes
	// hourly too, so the public site lags the fleet by ≤ ~2 h.
	benchmarksPushInterval = time.Hour
	// benchmarksPushInitialDelay lets the evidence walk and the heartbeat
	// registration settle before the first push.
	benchmarksPushInitialDelay = 3 * time.Minute
	// benchmarksPushTimeout bounds one hub POST.
	benchmarksPushTimeout = 60 * time.Second
	// benchmarksPushMaxRecords caps one batch; the rest go next hour.
	benchmarksPushMaxRecords = 2000
	// benchmarksHubIngestPath is the hub route.
	benchmarksHubIngestPath = "/api/benchmarks/ingest"
	// benchmarksRepoVisibilityTTL bounds how long a repo's public/private
	// answer is reused.
	benchmarksRepoVisibilityTTL = 24 * time.Hour
	// benchmarksExportMaxSinceDays bounds ?since= so one request cannot
	// re-emit years of history on a hot path.
	benchmarksExportDefaultWindow = 90 * 24 * time.Hour
)

// benchmarksPublishStatePath persists the last hub acknowledgement so a
// restart does not resend history. A var for tests.
var benchmarksPublishStatePath = filepath.Join(ghpkg.ReviewLinksDir, "benchmarks-publish.json")

// BenchmarksStatus is the /api/status block and the Settings → Benchmarks
// panel source.
type BenchmarksStatus struct {
	HiveHash      string `json:"hive_hash"`
	Publish       bool   `json:"publish"`
	RepoConsent   bool   `json:"repo_consent"`
	LastPushAt    string `json:"last_push_at,omitempty"`
	LastPushCount int    `json:"last_push_count"`
	TotalPushed   int    `json:"total_pushed"`
	LastError     string `json:"last_error,omitempty"`
	WatermarkAt   string `json:"watermark_at,omitempty"`
	SiteURL       string `json:"site_url"`
}

// benchmarksSiteURL is where the records end up.
const benchmarksSiteURL = "https://hivecommons.dev/benchmarks/"

type benchmarksPublishState struct {
	// Watermark is the newest bundle UpdatedAt the hub has acknowledged.
	Watermark   time.Time `json:"watermark"`
	LastPushAt  time.Time `json:"last_push_at"`
	LastCount   int       `json:"last_count"`
	TotalPushed int       `json:"total_pushed"`
	LastError   string    `json:"last_error,omitempty"`
}

type benchmarksPublisher struct {
	mu    sync.Mutex
	state benchmarksPublishState
	vis   map[string]benchmarksVisibility
}

type benchmarksVisibility struct {
	public bool
	at     time.Time
}

func (s *Server) benchmarks() *benchmarksPublisher {
	s.benchmarksOnce.Do(func() {
		p := &benchmarksPublisher{vis: map[string]benchmarksVisibility{}}
		if data, err := os.ReadFile(benchmarksPublishStatePath); err == nil {
			_ = json.Unmarshal(data, &p.state)
		}
		s.benchmarksPub = p
	})
	return s.benchmarksPub
}

func (p *benchmarksPublisher) save() {
	data, err := json.Marshal(p.state)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(benchmarksPublishStatePath), 0o755)
	_ = os.WriteFile(benchmarksPublishStatePath, data, 0o644)
}

// repoPublic answers through the hive's GitHub client with a daily cache.
// Unknown (API error, no client) is private: the record stays anonymous.
func (s *Server) benchmarksRepoPublic(ctx context.Context, repo string) bool {
	p := s.benchmarks()
	now := time.Now()
	p.mu.Lock()
	if v, ok := p.vis[repo]; ok && now.Sub(v.at) < benchmarksRepoVisibilityTTL {
		p.mu.Unlock()
		return v.public
	}
	p.mu.Unlock()
	public := false
	if s.deps != nil && s.deps.GHClient != nil {
		if owner, name, ok := strings.Cut(repo, "/"); ok {
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if r, _, err := s.deps.GHClient.GetRepo(rctx, owner, name); err == nil && r != nil {
				public = !r.GetPrivate()
			}
			cancel()
		}
	}
	p.mu.Lock()
	p.vis[repo] = benchmarksVisibility{public: public, at: now}
	p.mu.Unlock()
	return public
}

// benchmarksOptions builds the converter options from live config.
func (s *Server) benchmarksOptions(ctx context.Context, since time.Time) benchmarks.Options {
	cfg := s.deps.Config
	release, _ := fleetReportBuildInfo()
	return benchmarks.Options{
		HiveID:         cfg.HiveID,
		HiveVersion:    release,
		Since:          since,
		RepoConsent:    cfg.Benchmarks.RepoConsentEnabled(),
		RepoPublic:     func(repo string) bool { return s.benchmarksRepoPublic(ctx, repo) },
		SandboxRuntime: cfg.BenchmarkSandboxRuntime,
		CostLookup:     benchmarksCostLookup(s.deps.LaunchCost),
	}
}

// benchmarksRecords converts every bundle updated at/after since.
func (s *Server) benchmarksRecords(ctx context.Context, since time.Time) ([]benchmarks.Record, time.Time, error) {
	verifier, err := s.reviewEvidenceVerifier()
	if err != nil {
		return nil, time.Time{}, err
	}
	bundles, _, err := loadEvidenceBundles(ghpkg.ReviewEvidenceRoot, verifier)
	if err != nil {
		return nil, time.Time{}, err
	}
	var newest time.Time
	for _, b := range bundles {
		if b != nil && b.UpdatedAt.After(newest) {
			newest = b.UpdatedAt
		}
	}
	return benchmarks.Convert(bundles, s.benchmarksOptions(ctx, since)), newest, nil
}

// handleBenchmarksExport serves GET /api/benchmarks/export?since=<RFC3339|YYYY-MM-DD>.
// Owner-authenticated like every /api route. Returns no records when
// benchmarks.publish is false so the endpoint and the hub agree.
func (s *Server) handleBenchmarksExport(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config not loaded", http.StatusServiceUnavailable)
		return
	}
	cfg := s.deps.Config
	since := time.Now().Add(-benchmarksExportDefaultWindow)
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t, err = time.Parse("2006-01-02", raw)
		}
		if err != nil {
			jsonError(w, "since must be RFC3339 or YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		since = t
	}
	resp := map[string]any{
		"hive_hash":    benchmarks.HiveHash(cfg.HiveID),
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"publish":      cfg.Benchmarks.PublishEnabled(),
		"since":        since.UTC().Format(time.RFC3339),
		"records":      []benchmarks.Record{},
		"count":        0,
	}
	if cfg.Benchmarks.PublishEnabled() {
		recs, _, err := s.benchmarksRecords(r.Context(), since)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			jsonError(w, "load evidence: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if recs == nil {
			recs = []benchmarks.Record{}
		}
		resp["records"] = recs
		resp["count"] = len(recs)
	}
	jsonResponse(w, resp)
}

// BenchmarksStatusBlock is the /api/status view.
func (s *Server) BenchmarksStatusBlock() *BenchmarksStatus {
	if s.deps == nil || s.deps.Config == nil {
		return nil
	}
	cfg := s.deps.Config
	p := s.benchmarks()
	p.mu.Lock()
	st := p.state
	p.mu.Unlock()
	out := &BenchmarksStatus{
		HiveHash:      benchmarks.HiveHash(cfg.HiveID),
		Publish:       cfg.Benchmarks.PublishEnabled(),
		RepoConsent:   cfg.Benchmarks.RepoConsentEnabled(),
		LastPushCount: st.LastCount,
		TotalPushed:   st.TotalPushed,
		LastError:     st.LastError,
		SiteURL:       benchmarksSiteURL,
	}
	if !st.LastPushAt.IsZero() {
		out.LastPushAt = st.LastPushAt.UTC().Format(time.RFC3339)
	}
	if !st.Watermark.IsZero() {
		out.WatermarkAt = st.Watermark.UTC().Format(time.RFC3339)
	}
	return out
}

// StartBenchmarksPublisher runs the hourly spoke→hub push until ctx ends.
// It returns at once when there is no hub, no hive id or no hub credential.
func (s *Server) StartBenchmarksPublisher(ctx context.Context) {
	if s.deps == nil || s.deps.Config == nil {
		return
	}
	cfg := s.deps.Config
	hubURL := strings.TrimRight(strings.TrimSpace(cfg.Hub.URL), "/")
	if hubURL == "" || strings.TrimSpace(cfg.HiveID) == "" {
		return
	}
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(benchmarksPushInitialDelay):
		}
		ticker := time.NewTicker(benchmarksPushInterval)
		defer ticker.Stop()
		for {
			s.pushBenchmarks(ctx, hubURL, logger)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// benchmarksIngestPayload is the body of POST /api/benchmarks/ingest.
type benchmarksIngestPayload struct {
	HiveID    string              `json:"hive_id"`
	HiveHash  string              `json:"hive_hash"`
	Watermark string              `json:"watermark"`
	Records   []benchmarks.Record `json:"records"`
}

// pushBenchmarks sends one batch and advances the watermark on 2xx.
func (s *Server) pushBenchmarks(ctx context.Context, hubURL string, logger *slog.Logger) {
	cfg := s.deps.Config
	p := s.benchmarks()
	if !cfg.Benchmarks.PublishEnabled() {
		return
	}
	bearer := spoke.SpokeHeartbeatKey()
	if bearer == "" {
		return
	}
	p.mu.Lock()
	since := p.state.Watermark
	p.mu.Unlock()
	recs, newest, err := s.benchmarksRecords(ctx, since)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.benchmarksFail(p, "load evidence: "+err.Error())
		}
		return
	}
	if len(recs) == 0 {
		return
	}
	if len(recs) > benchmarksPushMaxRecords {
		recs = recs[:benchmarksPushMaxRecords]
		newest = since // partial batch: keep the watermark so the rest follows
	}
	body, err := json.Marshal(benchmarksIngestPayload{
		HiveID:    cfg.HiveID,
		HiveHash:  benchmarks.HiveHash(cfg.HiveID),
		Watermark: newest.UTC().Format(time.RFC3339),
		Records:   recs,
	})
	if err != nil {
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, benchmarksPushTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, hubURL+benchmarksHubIngestPath, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.benchmarksFail(p, "post: "+err.Error())
		return
	}
	defer closeHTTPBody(resp.Body)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		s.benchmarksFail(p, "hub answered "+resp.Status)
		return
	}
	p.mu.Lock()
	if newest.After(p.state.Watermark) {
		p.state.Watermark = newest
	}
	p.state.LastPushAt = time.Now()
	p.state.LastCount = len(recs)
	p.state.TotalPushed += len(recs)
	p.state.LastError = ""
	p.save()
	p.mu.Unlock()
	logger.Info("benchmarks: pushed records to hub", "count", len(recs), "watermark", newest.UTC().Format(time.RFC3339))
}

func (s *Server) benchmarksFail(p *benchmarksPublisher, msg string) {
	p.mu.Lock()
	p.state.LastError = msg
	p.save()
	p.mu.Unlock()
	if s.logger != nil {
		s.logger.Warn("benchmarks: push failed", "error", msg)
	}
}

// featuresBenchmarksResponse is the Settings → Features → Benchmarks panel
// source (config_features.js).
func (s *Server) featuresBenchmarksResponse(cfg *config.Config) map[string]interface{} {
	out := map[string]interface{}{
		"benchmarksPublish":     cfg.Benchmarks.PublishEnabled(),
		"benchmarksRepoConsent": cfg.Benchmarks.RepoConsentEnabled(),
		"benchmarksHiveHash":    benchmarks.HiveHash(cfg.HiveID),
	}
	if st := s.BenchmarksStatusBlock(); st != nil {
		out["benchmarksLastPushAt"] = st.LastPushAt
		out["benchmarksLastPushCount"] = st.LastPushCount
	}
	return out
}
