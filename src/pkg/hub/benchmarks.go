package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/evidence/benchmarks"
)

// Fleet benchmark publication (hivecommons/hive#17694). Spokes POST their
// anonymized per-PR records here hourly (pkg/dashboard/api_benchmarks.go);
// the hub merges them by record id into /data/benchmarks/<hive_hash>/<yyyy-mm>.json
// and, hourly, commits every changed month file to the data repo through the
// GitHub Contents API as the hub's own identity. Direct pushes to main: the
// target is a data-only repo whose Pages workflow rebuilds on push.

const (
	benchmarksIngestPath = "/api/benchmarks/ingest"
	benchmarksStatusPath = "/api/hub/benchmarks"
	// benchmarksIngestMaxBodyBytes caps one spoke batch (2000 records ≈ 2 MB).
	benchmarksIngestMaxBodyBytes = 8 << 20
	// benchmarksPublishInterval is the hub→GitHub cadence.
	benchmarksPublishInterval = time.Hour
	// benchmarksPublishInitialDelay lets the first spoke batches land.
	benchmarksPublishInitialDelay = 10 * time.Minute
	// benchmarksCommitTimeout bounds one Contents API call.
	benchmarksCommitTimeout = 45 * time.Second
	// benchmarksRepoEnv overrides the target repo (owner/name).
	benchmarksRepoEnv = "HIVE_BENCHMARKS_REPO"
	// benchmarksRepoDefault is the public leaderboard data repo.
	benchmarksRepoDefault = "hivecommons/benchmarks"
	// benchmarksDataPrefix is the path inside the repo.
	benchmarksDataPrefix = "data/fleet"
	// benchmarksDisableEnv turns the GitHub publisher off (ingest still stores).
	benchmarksDisableEnv = "HIVE_BENCHMARKS_PUBLISH_DISABLED"
	// benchmarksMaxFilesPerRun bounds commits per hour so a cold start does
	// not burn the installation's write budget in one go.
	benchmarksMaxFilesPerRun = 60
)

var (
	// benchmarksStoreDir is the on-PVC mirror of the repo's data/fleet tree.
	benchmarksStoreDir = "/data/benchmarks"
	// benchmarksStatePath remembers the last publish for the status page.
	benchmarksStatePath = "/data/benchmarks-publish.json"
	// benchmarksGitHubAPI is the Contents API base; a var for tests.
	benchmarksGitHubAPI = "https://api.github.com"
	hiveHashRe          = regexp.MustCompile(`^[0-9a-f]{16}$`)
	monthRe             = regexp.MustCompile(`^\d{4}-\d{2}$`)
)

// BenchmarksPublishState is the hub status block.
type BenchmarksPublishState struct {
	LastIngestAt     string         `json:"last_ingest_at,omitempty"`
	LastPublishAt    string         `json:"last_publish_at,omitempty"`
	LastPublishFiles int            `json:"last_publish_files"`
	LastError        string         `json:"last_error,omitempty"`
	Repo             string         `json:"repo"`
	PendingFiles     int            `json:"pending_files"`
	HiveCount        int            `json:"hive_count"`
	RecordCount      int            `json:"record_count"`
	Hives            map[string]int `json:"hives,omitempty"`
	Enabled          bool           `json:"enabled"`
}

type benchmarksHub struct {
	mu      sync.Mutex
	pending map[string]bool // relative file paths (hash/month.json) changed since last publish
	state   BenchmarksPublishState
	loaded  bool
}

var hubBenchmarks = &benchmarksHub{pending: map[string]bool{}}

func (b *benchmarksHub) load() {
	if b.loaded {
		return
	}
	b.loaded = true
	data, err := os.ReadFile(benchmarksStatePath)
	if err != nil {
		return
	}
	var st struct {
		BenchmarksPublishState
		Pending []string `json:"pending"`
	}
	if json.Unmarshal(data, &st) == nil {
		b.state = st.BenchmarksPublishState
		for _, p := range st.Pending {
			b.pending[p] = true
		}
	}
}

func (b *benchmarksHub) save() {
	var pending []string
	for p := range b.pending {
		pending = append(pending, p)
	}
	sort.Strings(pending)
	data, err := json.Marshal(struct {
		BenchmarksPublishState
		Pending []string `json:"pending"`
	}{b.state, pending})
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(benchmarksStatePath), 0o755)
	_ = os.WriteFile(benchmarksStatePath, data, 0o644)
}

type benchmarksIngestBody struct {
	HiveID    string              `json:"hive_id"`
	HiveHash  string              `json:"hive_hash"`
	Watermark string              `json:"watermark"`
	Records   []benchmarks.Record `json:"records"`
}

// handleBenchmarksIngest accepts one spoke batch. Auth is the per-hive
// heartbeat bearer bound to hive_id, exactly as NPS ingest. The hive hash is
// recomputed from hive_id, never trusted from the body.
func (s *HubServer) handleBenchmarksIngest(w http.ResponseWriter, r *http.Request) {
	if s.hubSecret == "" {
		npsJSONError(w, "benchmarks ingest requires a configured hub secret", http.StatusServiceUnavailable)
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		npsJSONError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, benchmarksIngestMaxBodyBytes+1))
	if err != nil {
		npsJSONError(w, "read error", http.StatusBadRequest)
		return
	}
	if len(body) > benchmarksIngestMaxBodyBytes {
		npsJSONError(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	var p benchmarksIngestBody
	if err := json.Unmarshal(body, &p); err != nil {
		npsJSONError(w, "invalid payload", http.StatusBadRequest)
		return
	}
	hiveID := sanitizeHeartbeatField(p.HiveID)
	if hiveID == "" || hiveID != p.HiveID || !isValidName(hiveID) {
		npsJSONError(w, "invalid hive_id", http.StatusBadRequest)
		return
	}
	if !s.verifyHeartbeatBearer(strings.TrimPrefix(auth, "Bearer "), hiveID) {
		npsJSONError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.npsHiveRegistered(hiveID) {
		npsJSONError(w, "unknown hive - heartbeat first", http.StatusForbidden)
		return
	}
	hash := benchmarks.HiveHash(hiveID)
	stored, rejected, err := storeBenchmarkRecords(hash, p.Records)
	if err != nil {
		if s.logger != nil {
			s.logger.Error("benchmarks: store failed", "hive_hash", hash, "error", err)
		}
		npsJSONError(w, "store failed", http.StatusInternalServerError)
		return
	}
	hubBenchmarks.mu.Lock()
	hubBenchmarks.load()
	hubBenchmarks.state.LastIngestAt = time.Now().UTC().Format(time.RFC3339)
	for _, f := range stored {
		hubBenchmarks.pending[f] = true
	}
	hubBenchmarks.save()
	hubBenchmarks.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "accepted": len(p.Records) - rejected, "rejected": rejected, "files": len(stored)})
}

// storeBenchmarkRecords merges records by id into <hash>/<month>.json files
// and returns the relative paths it changed. A record whose source.hive_id
// is not the authenticated hash is rejected: a spoke publishes only as itself.
func storeBenchmarkRecords(hash string, recs []benchmarks.Record) (changed []string, rejected int, err error) {
	byMonth := map[string][]benchmarks.Record{}
	for _, r := range recs {
		if r.Source.HiveID != hash || r.ID == "" {
			rejected++
			continue
		}
		r.Source.HiveID = hash
		byMonth[benchmarkMonth(r)] = append(byMonth[benchmarkMonth(r)], r)
	}
	for month, add := range byMonth {
		rel := filepath.Join(hash, month+".json")
		path := filepath.Join(benchmarksStoreDir, rel)
		existing := map[string]benchmarks.Record{}
		if data, rerr := os.ReadFile(path); rerr == nil {
			var cur []benchmarks.Record
			if json.Unmarshal(data, &cur) == nil {
				for _, r := range cur {
					existing[r.ID] = r
				}
			}
		}
		for _, r := range add {
			existing[r.ID] = r
		}
		merged := make([]benchmarks.Record, 0, len(existing))
		for _, r := range existing {
			merged = append(merged, r)
		}
		sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
		data, merr := json.Marshal(merged)
		if merr != nil {
			return changed, rejected, merr
		}
		if werr := os.MkdirAll(filepath.Dir(path), 0o755); werr != nil {
			return changed, rejected, werr
		}
		if werr := os.WriteFile(path, append(data, '\n'), 0o644); werr != nil {
			return changed, rejected, werr
		}
		changed = append(changed, filepath.ToSlash(rel))
	}
	sort.Strings(changed)
	return changed, rejected, nil
}

// benchmarkMonth files a record under the month it merged, else opened,
// else was published.
func benchmarkMonth(r benchmarks.Record) string {
	for _, ts := range []string{r.Outcome.MergedAt, r.Outcome.OpenedAt, r.PublishedAt} {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			return t.UTC().Format("2006-01")
		}
	}
	return time.Now().UTC().Format("2006-01")
}

// benchmarksTargetRepo is owner/name of the data repo.
func benchmarksTargetRepo() string {
	if v := strings.TrimSpace(os.Getenv(benchmarksRepoEnv)); v != "" {
		return v
	}
	return benchmarksRepoDefault
}

func benchmarksPublishEnabled() bool {
	return strings.TrimSpace(os.Getenv(benchmarksDisableEnv)) == ""
}

// StartBenchmarksPublisher commits pending month files to the data repo
// every hour. Registered from StartBackgroundPollers.
func (s *HubServer) StartBenchmarksPublisher(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(benchmarksPublishInitialDelay):
	}
	ticker := time.NewTicker(benchmarksPublishInterval)
	defer ticker.Stop()
	for {
		s.publishBenchmarks(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// publishBenchmarks pushes every pending file; a file that fails stays
// pending for the next run.
func (s *HubServer) publishBenchmarks(ctx context.Context) {
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	if !benchmarksPublishEnabled() {
		return
	}
	hubBenchmarks.mu.Lock()
	hubBenchmarks.load()
	var files []string
	for f := range hubBenchmarks.pending {
		files = append(files, f)
	}
	hubBenchmarks.mu.Unlock()
	if len(files) == 0 {
		return
	}
	sort.Strings(files)
	if len(files) > benchmarksMaxFilesPerRun {
		files = files[:benchmarksMaxFilesPerRun]
	}
	token := hubGitHubAuthTokenForRequest(ctx)
	if token == "" {
		s.benchmarksSetError("no hub GitHub identity (App or token) configured")
		return
	}
	client := hubGitHubHTTPClient()
	repo := benchmarksTargetRepo()
	done := 0
	var lastErr error
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := commitBenchmarkFile(ctx, client, token, repo, rel); err != nil {
			lastErr = err
			logger.Warn("benchmarks: publish failed", "file", rel, "error", err)
			continue
		}
		done++
		hubBenchmarks.mu.Lock()
		delete(hubBenchmarks.pending, rel)
		hubBenchmarks.mu.Unlock()
	}
	hubBenchmarks.mu.Lock()
	hubBenchmarks.state.LastPublishAt = time.Now().UTC().Format(time.RFC3339)
	hubBenchmarks.state.LastPublishFiles = done
	hubBenchmarks.state.LastError = ""
	if lastErr != nil {
		hubBenchmarks.state.LastError = lastErr.Error()
	}
	hubBenchmarks.save()
	hubBenchmarks.mu.Unlock()
	logger.Info("benchmarks: published to data repo", "repo", repo, "files", done, "failed", len(files)-done)
}

func (s *HubServer) benchmarksSetError(msg string) {
	hubBenchmarks.mu.Lock()
	hubBenchmarks.load()
	hubBenchmarks.state.LastError = msg
	hubBenchmarks.save()
	hubBenchmarks.mu.Unlock()
}

// commitBenchmarkFile PUTs one file through the Contents API, reading the
// current blob sha first so the write is an update, not a conflict.
func commitBenchmarkFile(ctx context.Context, client *http.Client, token, repo, rel string) error {
	if !validBenchmarkRel(rel) {
		return fmt.Errorf("refusing to publish path %q", rel)
	}
	data, err := os.ReadFile(filepath.Join(benchmarksStoreDir, filepath.FromSlash(rel)))
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/repos/%s/contents/%s/%s", benchmarksGitHubAPI, repo, benchmarksDataPrefix, rel)
	cctx, cancel := context.WithTimeout(hubGitHubCallerContext(ctx, "benchmarks"), benchmarksCommitTimeout)
	defer cancel()
	sha := ""
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if resp, gerr := client.Do(req); gerr == nil {
		var cur struct {
			SHA     string `json:"sha"`
			Content string `json:"content"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&cur)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			sha = cur.SHA
			if existing, derr := base64.StdEncoding.DecodeString(strings.ReplaceAll(cur.Content, "\n", "")); derr == nil && bytes.Equal(existing, data) {
				return nil // already published
			}
		}
	}
	hash := strings.SplitN(rel, "/", 2)[0]
	payload := map[string]any{
		"message": fmt.Sprintf("🌱 fleet: hourly benchmark records for %s (%s)", hash, strings.TrimSuffix(filepath.Base(rel), ".json")),
		"content": base64.StdEncoding.EncodeToString(data),
	}
	if sha != "" {
		payload["sha"] = sha
	}
	body, _ := json.Marshal(payload)
	req, err = http.NewRequestWithContext(cctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("contents API %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func validBenchmarkRel(rel string) bool {
	hash, file, ok := strings.Cut(rel, "/")
	return ok && hiveHashRe.MatchString(hash) && strings.HasSuffix(file, ".json") && monthRe.MatchString(strings.TrimSuffix(file, ".json"))
}

// handleBenchmarksStatus serves GET /api/hub/benchmarks: last ingest and
// publish times plus per-hive record counts from the on-disk mirror.
func (s *HubServer) handleBenchmarksStatus(w http.ResponseWriter, _ *http.Request) {
	hubBenchmarks.mu.Lock()
	hubBenchmarks.load()
	st := hubBenchmarks.state
	st.PendingFiles = len(hubBenchmarks.pending)
	hubBenchmarks.mu.Unlock()
	st.Repo = benchmarksTargetRepo()
	st.Enabled = benchmarksPublishEnabled()
	st.Hives = map[string]int{}
	entries, err := os.ReadDir(benchmarksStoreDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		npsJSONError(w, "read store", http.StatusInternalServerError)
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !hiveHashRe.MatchString(e.Name()) {
			continue
		}
		n := 0
		months, _ := os.ReadDir(filepath.Join(benchmarksStoreDir, e.Name()))
		for _, m := range months {
			data, rerr := os.ReadFile(filepath.Join(benchmarksStoreDir, e.Name(), m.Name()))
			if rerr != nil {
				continue
			}
			var recs []json.RawMessage
			if json.Unmarshal(data, &recs) == nil {
				n += len(recs)
			}
		}
		st.Hives[e.Name()] = n
		st.RecordCount += n
	}
	st.HiveCount = len(st.Hives)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}
