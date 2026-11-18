package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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
	// benchmarksPushMaxBatchBytes caps one batch's serialized records. It
	// sits well under the 1 MB request-body default of common ingress
	// proxies (nginx client_max_body_size), which a 2000-record batch at
	// ~900 B/record (~1.8 MB) exceeds.
	benchmarksPushMaxBatchBytes = 384 << 10
	// benchmarksPushMaxBatchesPerTick bounds hub POSTs in one tick; any
	// remainder follows next hour (the watermark is already advanced).
	benchmarksPushMaxBatchesPerTick = 50
	// benchmarksPushMaxShrinkAttempts bounds halve-and-retry rounds for one
	// batch after HTTP 413. 2000 halves to 1 in 11 steps.
	benchmarksPushMaxShrinkAttempts = 12
	// benchmarksPushMaxUnprovenSkips is how many single records may be skipped
	// on 413 before any batch of the tick was accepted. Skipping is only
	// safe once the path is proven to work; if every record is refused the
	// cause is the path (a proxy cap), not the records, and nothing is dropped.
	benchmarksPushMaxUnprovenSkips = 3
	// benchmarksErrBodyMaxBytes truncates the hub response body quoted in
	// last_error and logs so a proxy HTML page cannot bloat status.
	benchmarksErrBodyMaxBytes = 200
	// benchmarksEnvelopeOverheadBytes approximates the payload JSON around
	// the records array (hive id, hash, watermark, keys).
	benchmarksEnvelopeOverheadBytes = 512
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

// benchmarksBatchEnd returns the exclusive end index of the batch that
// starts at start: at most maxRecords records and at most maxBytes of
// serialized records (sizes[i] is record i's JSON length). A batch always
// holds at least one record, so a single oversized record still gets sent.
func benchmarksBatchEnd(sizes []int, start, maxRecords, maxBytes int) int {
	total := benchmarksEnvelopeOverheadBytes
	end := start
	for end < len(sizes) && end-start < maxRecords {
		next := total + sizes[end] + 1 // +1 for the array comma
		if end > start && next > maxBytes {
			break
		}
		total = next
		end++
	}
	return end
}

// benchmarksTruncateBody renders a hub response body for last_error: control
// characters and runs of whitespace collapse to one space and the result is
// cut to benchmarksErrBodyMaxBytes.
func benchmarksTruncateBody(raw []byte, bearer string) string {
	text := string(raw)
	if bearer != "" {
		text = strings.ReplaceAll(text, bearer, "[redacted]")
	}
	text = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, text)), " ")
	if len(text) > benchmarksErrBodyMaxBytes {
		text = strings.ToValidUTF8(text[:benchmarksErrBodyMaxBytes], "") + "..."
	}
	return text
}

// benchmarksSendResult is the outcome of one hub POST.
type benchmarksSendResult struct {
	status int
	// failure is the last_error text; empty on a 2xx.
	failure string
}

// postBenchmarksBatch POSTs one batch. The failure text carries the HTTP
// status and the truncated response body so the owner can tell the hub
// handler ("payload too large") from a proxy in front of it.
func (s *Server) postBenchmarksBatch(ctx context.Context, hubURL, bearer string, recs []benchmarks.Record, watermark time.Time) benchmarksSendResult {
	cfg := s.deps.Config
	body, err := json.Marshal(benchmarksIngestPayload{
		HiveID:    cfg.HiveID,
		HiveHash:  benchmarks.HiveHash(cfg.HiveID),
		Watermark: watermark.UTC().Format(time.RFC3339),
		Records:   recs,
	})
	if err != nil {
		return benchmarksSendResult{failure: "marshal: " + err.Error()}
	}
	reqCtx, cancel := context.WithTimeout(ctx, benchmarksPushTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, hubURL+benchmarksHubIngestPath, bytes.NewReader(body))
	if err != nil {
		return benchmarksSendResult{failure: "request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return benchmarksSendResult{failure: "post: " + err.Error()}
	}
	defer closeHTTPBody(resp.Body)
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, benchmarksErrBodyMaxBytes))
		return benchmarksSendResult{status: resp.StatusCode}
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, benchmarksErrBodyMaxBytes*2))
	failure := "hub answered " + resp.Status
	if text := benchmarksTruncateBody(snippet, bearer); text != "" {
		failure += ": " + text
	}
	return benchmarksSendResult{status: resp.StatusCode, failure: failure}
}

// pushBenchmarks sends the records newer than the watermark in byte- and
// count-bounded batches, advancing the watermark after every accepted batch
// so partial progress survives a later failure. On HTTP 413 the batch is
// halved and retried; a single record that still gets 413 is skipped and
// logged so it cannot wedge the publisher.
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
	// Oldest first so the watermark can advance monotonically per batch.
	sort.SliceStable(recs, func(i, j int) bool {
		if !recs[i].UpdatedAt.Equal(recs[j].UpdatedAt) {
			return recs[i].UpdatedAt.Before(recs[j].UpdatedAt)
		}
		return recs[i].ID < recs[j].ID
	})
	sizes := make([]int, len(recs))
	for i := range recs {
		b, err := json.Marshal(recs[i])
		if err != nil {
			s.benchmarksFail(p, "marshal record: "+err.Error())
			return
		}
		sizes[i] = len(b)
	}

	maxRecords := benchmarksPushMaxRecords
	pushed, unproven := 0, 0
	lastFailure := ""
	for batches, start := 0, 0; start < len(recs) && batches < benchmarksPushMaxBatchesPerTick; batches++ {
		end := benchmarksBatchEnd(sizes, start, maxRecords, benchmarksPushMaxBatchBytes)
		var res benchmarksSendResult
		var wm time.Time
		for attempt := 0; ; attempt++ {
			wm = benchmarksBatchWatermark(recs, end, newest)
			res = s.postBenchmarksBatch(ctx, hubURL, bearer, recs[start:end], wm)
			if res.failure == "" || res.status != http.StatusRequestEntityTooLarge {
				break
			}
			if end-start == 1 {
				break
			}
			if attempt >= benchmarksPushMaxShrinkAttempts {
				break
			}
			// Halve and remember the smaller size for the rest of the tick.
			maxRecords = (end - start + 1) / 2
			end = start + maxRecords
			logger.Info("benchmarks: hub rejected batch as too large, halving", "records", maxRecords, "detail", res.failure)
		}
		if res.failure != "" {
			if res.status == http.StatusRequestEntityTooLarge && end-start == 1 {
				if pushed == 0 {
					// Unproven path: defer the watermark; a later accepted
					// batch covers this record, otherwise nothing is dropped.
					unproven++
					lastFailure = res.failure
					if unproven >= benchmarksPushMaxUnprovenSkips {
						s.benchmarksFail(p, lastFailure)
						return
					}
					start = end
					continue
				}
				logger.Warn("benchmarks: skipping record the hub rejects even alone", "record_id", recs[start].ID, "bytes", sizes[start], "detail", res.failure)
				s.benchmarksAdvance(p, wm, 0)
				start = end
				continue
			}
			s.benchmarksFail(p, res.failure)
			return
		}
		pushed += end - start
		unproven = 0
		s.benchmarksAdvance(p, wm, end-start)
		start = end
	}
	if pushed == 0 && unproven > 0 {
		s.benchmarksFail(p, lastFailure)
		return
	}
	if pushed > 0 {
		logger.Info("benchmarks: pushed records to hub", "count", pushed, "watermark", p.watermark().UTC().Format(time.RFC3339))
	}
}

// benchmarksBatchWatermark is the watermark to record once recs[:end] are
// acknowledged. The last batch takes the newest bundle time seen (as before).
// A mid-run batch takes its last record's time; when the next record shares
// that time the watermark steps back one nanosecond so the tie is resent
// rather than dropped (the hub merges by record id, so a resend is harmless).
func benchmarksBatchWatermark(recs []benchmarks.Record, end int, newest time.Time) time.Time {
	if end >= len(recs) {
		return newest
	}
	last := recs[end-1].UpdatedAt
	if recs[end].UpdatedAt.After(last) {
		return last
	}
	return last.Add(-time.Nanosecond)
}

func (p *benchmarksPublisher) watermark() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state.Watermark
}

// benchmarksAdvance records an accepted batch (or a skipped record, n=0).
func (s *Server) benchmarksAdvance(p *benchmarksPublisher, wm time.Time, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if wm.After(p.state.Watermark) {
		p.state.Watermark = wm
	}
	if n > 0 {
		p.state.LastPushAt = time.Now()
		p.state.LastCount = n
		p.state.TotalPushed += n
	}
	p.state.LastError = ""
	p.save()
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
