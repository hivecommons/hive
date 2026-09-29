package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// NPS (Net Promoter Score) feedback, hub side (issue #9610).
//
// Spokes forward a dashboard user's voluntary NPS response to
// POST /api/nps/ingest over the same per-hive heartbeat bearer they already
// present on /api/heartbeat and /api/task-status. The hub stores a rolling
// window of responses and serves the aggregate, free text included, ONLY to hub
// admins at GET /api/admin/nps. There is no unauthenticated read path at all
// (the kubestellar/console endpoint this is modeled on leaked free text through
// an open GET, console #16486).
//
// A stored response carries the hive it came from and never a user identity:
// the spoke does not send one and the hub has no field to keep it in.

const (
	// npsIngestPath is the spoke-to-hub ingest route.
	npsIngestPath = "/api/nps/ingest"
	// npsAdminPath is the admin-only aggregate route.
	npsAdminPath = "/api/admin/nps"

	// npsScoreMin / npsScoreMax bound the 4-point emoji scale
	// (1 Not great, 2 Meh, 3 Good, 4 Love it).
	npsScoreMin = 1
	npsScoreMax = 4
	// npsPromoterMin is the lowest score counted as a promoter.
	npsPromoterMin = 4
	// npsPassiveMin is the lowest score counted as a passive; below it is a
	// detractor.
	npsPassiveMin = 2

	// npsIngestMaxBodyBytes caps the bytes the hub READS from an ingest body.
	// Enforced on bytes actually read, never on Content-Length, so a chunked
	// or lying request cannot make the hub buffer more (console #16666).
	npsIngestMaxBodyBytes = 4096
	// npsMaxFeedbackRunes caps stored free text. Matches the spoke's cap; the
	// hub re-applies it because it never trusts a spoke to have done so.
	npsMaxFeedbackRunes = 500
	// npsMaxVersionRunes caps the stored dashboard version string.
	npsMaxVersionRunes = 64

	// npsHubRateWindow and npsHubMaxPerHivePerWindow bound how many responses
	// one hive can land per window. The spoke already limits each signed-in
	// user to one per window; this is the hub's own backstop, so even a
	// compromised spoke credential can inject only a handful of scores a day.
	npsHubRateWindow          = 24 * time.Hour
	npsHubMaxPerHivePerWindow = 10

	// npsMaxResponsesPerHive and npsMaxResponsesTotal are the rolling-window
	// storage caps. The oldest response is dropped first.
	npsMaxResponsesPerHive = 1000
	npsMaxResponsesTotal   = 10000

	// npsRecentCount is how many recent responses the admin view returns.
	npsRecentCount = 20
	// npsPercentScale converts a ratio to a percentage / NPS point.
	npsPercentScale = 100
	// npsAverageDecimals is the rounding precision of average scores.
	npsAverageDecimals = 10
	// npsTrendMonthLayout buckets the monthly trend ("2026-09").
	npsTrendMonthLayout = "2006-01"

	npsCategoryPromoter  = "promoter"
	npsCategoryPassive   = "passive"
	npsCategoryDetractor = "detractor"
)

// npsStorePath is where responses persist on the hub data volume. A var so
// tests can point it at a temp dir.
var npsStorePath = "/data/hub-nps.json"

// npsRecord is one stored response. Category is deliberately NOT stored: it is
// re-derived from Score on every read, so a change to the bucketing never
// needs a data migration.
type npsRecord struct {
	HiveID           string `json:"hive_id"`
	Score            int    `json:"score"`
	Feedback         string `json:"feedback,omitempty"`
	Timestamp        string `json:"timestamp"`
	DashboardVersion string `json:"dashboard_version,omitempty"`
}

type npsFile struct {
	Responses []npsRecord `json:"responses"`
}

// npsStore is the lazily loaded, file-backed response log. The path is
// re-checked on every access (like hubAdminGrants) so a test that swaps
// npsStorePath gets a fresh store.
type npsStore struct {
	mu      sync.Mutex
	loaded  bool
	path    string
	records []npsRecord
}

var hubNPS = &npsStore{}

// loadLocked reads the store from disk if it is not loaded for the current
// path. A missing file is an empty store; an unreadable one is an error so a
// transient read failure can never be "fixed" by overwriting real data.
func (st *npsStore) loadLocked() error {
	path := npsStorePath
	if st.loaded && st.path == path {
		return nil
	}
	st.records = nil
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read nps store: %w", err)
	}
	if err == nil {
		var f npsFile
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("parse nps store: %w", err)
		}
		st.records = f.Responses
	}
	st.path = path
	st.loaded = true
	return nil
}

func (st *npsStore) saveLocked() error {
	data, err := json.Marshal(npsFile{Responses: st.records})
	if err != nil {
		return fmt.Errorf("marshal nps store: %w", err)
	}
	path := npsStorePath
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create nps store dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write nps store: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename nps store: %w", err)
	}
	return nil
}

// errNPSRateLimited is returned by add when the hive is over its window quota.
var errNPSRateLimited = errors.New("nps: hive rate limit exceeded")

// add appends rec after enforcing the per-hive rate limit, trims to the
// rolling caps, and persists. On a persist failure the in-memory log is rolled
// back so memory never claims a response the disk does not have.
func (st *npsStore) add(rec npsRecord, now time.Time) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.loadLocked(); err != nil {
		return err
	}
	if npsCountSince(st.records, rec.HiveID, now.Add(-npsHubRateWindow)) >= npsHubMaxPerHivePerWindow {
		return errNPSRateLimited
	}
	prev := st.records
	next := make([]npsRecord, 0, len(prev)+1)
	next = append(next, prev...)
	next = append(next, rec)
	st.records = npsTrim(next, rec.HiveID)
	if err := st.saveLocked(); err != nil {
		st.records = prev
		return err
	}
	return nil
}

// snapshot returns a copy of the stored records, oldest first.
func (st *npsStore) snapshot() ([]npsRecord, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.loadLocked(); err != nil {
		return nil, err
	}
	out := make([]npsRecord, len(st.records))
	copy(out, st.records)
	return out, nil
}

// npsCountSince counts hiveID's records whose timestamp is at or after since.
// An unparseable timestamp counts (fail closed: it cannot be proven old).
func npsCountSince(records []npsRecord, hiveID string, since time.Time) int {
	n := 0
	for _, r := range records {
		if r.HiveID != hiveID {
			continue
		}
		ts, err := time.Parse(time.RFC3339, r.Timestamp)
		if err != nil || !ts.Before(since) {
			n++
		}
	}
	return n
}

// npsTrim applies the rolling caps: first the per-hive cap for the hive that
// just grew (dropping its oldest), then the global cap (dropping the oldest
// overall). records must be oldest-first.
func npsTrim(records []npsRecord, hiveID string) []npsRecord {
	perHive := 0
	for _, r := range records {
		if r.HiveID == hiveID {
			perHive++
		}
	}
	if drop := perHive - npsMaxResponsesPerHive; drop > 0 {
		kept := make([]npsRecord, 0, len(records)-drop)
		for _, r := range records {
			if drop > 0 && r.HiveID == hiveID {
				drop--
				continue
			}
			kept = append(kept, r)
		}
		records = kept
	}
	if over := len(records) - npsMaxResponsesTotal; over > 0 {
		records = records[over:]
	}
	return records
}

// npsCategory buckets a raw 1-4 score: 4 promoter, 2-3 passive, else detractor.
func npsCategory(score int) string {
	switch {
	case score >= npsPromoterMin:
		return npsCategoryPromoter
	case score >= npsPassiveMin:
		return npsCategoryPassive
	default:
		return npsCategoryDetractor
	}
}

// npsRound rounds half toward +Inf, matching JavaScript's Math.round, so the
// hub's numbers agree with the console implementation this mirrors (Go's
// math.Round would turn an NPS of -12.5 into -13 where the console shows -12).
func npsRound(x float64) int {
	return int(math.Floor(x + 0.5))
}

func npsRoundAverage(x float64) float64 {
	return math.Floor(x*npsAverageDecimals+0.5) / npsAverageDecimals
}

// npsSanitizeFeedback keeps free text readable (it is rendered as TEXT, never
// markup, in the admin view) while dropping control characters other than
// newline, trimming, and capping the length in runes.
func npsSanitizeFeedback(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c == '\r' {
			continue
		}
		if c != '\n' && (unicode.IsControl(c) || c == unicode.ReplacementChar) {
			continue
		}
		b.WriteRune(c)
	}
	return truncateRunes(strings.TrimSpace(b.String()), npsMaxFeedbackRunes)
}

// npsTally is the promoter/passive/detractor breakdown shared by the overall
// summary, each trend month, and each hive.
type npsTally struct {
	Total        int     `json:"total"`
	NPSScore     int     `json:"nps_score"`
	Promoters    int     `json:"promoters"`
	Passives     int     `json:"passives"`
	Detractors   int     `json:"detractors"`
	AverageScore float64 `json:"average_score"`
}

func npsTallyOf(records []npsRecord) npsTally {
	t := npsTally{Total: len(records)}
	if t.Total == 0 {
		return t
	}
	sum := 0
	for _, r := range records {
		sum += r.Score
		switch npsCategory(r.Score) {
		case npsCategoryPromoter:
			t.Promoters++
		case npsCategoryPassive:
			t.Passives++
		default:
			t.Detractors++
		}
	}
	t.NPSScore = npsRound(float64(t.Promoters-t.Detractors) / float64(t.Total) * npsPercentScale)
	t.AverageScore = npsRoundAverage(float64(sum) / float64(t.Total))
	return t
}

type npsTrendPoint struct {
	Month string `json:"month"`
	npsTally
}

type npsHiveBreakdown struct {
	HiveID   string `json:"hive_id"`
	HiveName string `json:"hive_name,omitempty"`
	LastAt   string `json:"last_at,omitempty"`
	npsTally
}

type npsRecentResponse struct {
	HiveID           string `json:"hive_id"`
	HiveName         string `json:"hive_name,omitempty"`
	Score            int    `json:"score"`
	Category         string `json:"category"`
	Feedback         string `json:"feedback,omitempty"`
	Timestamp        string `json:"timestamp"`
	DashboardVersion string `json:"dashboard_version,omitempty"`
}

// npsAggregation is the GET /api/admin/nps response.
type npsAggregation struct {
	npsTally
	PromoterPct  int                 `json:"promoter_pct"`
	PassivePct   int                 `json:"passive_pct"`
	DetractorPct int                 `json:"detractor_pct"`
	ScoreMax     int                 `json:"score_max"`
	Trend        []npsTrendPoint     `json:"trend"`
	PerHive      []npsHiveBreakdown  `json:"per_hive"`
	Recent       []npsRecentResponse `json:"recent"`
}

// buildNPSAggregation computes the admin view from oldest-first records.
// names maps hive ID to a display name; a missing name is simply omitted.
func buildNPSAggregation(records []npsRecord, names map[string]string) npsAggregation {
	agg := npsAggregation{
		npsTally: npsTallyOf(records),
		ScoreMax: npsScoreMax,
		Trend:    []npsTrendPoint{},
		PerHive:  []npsHiveBreakdown{},
		Recent:   []npsRecentResponse{},
	}
	if agg.Total == 0 {
		return agg
	}
	agg.PromoterPct = npsRound(float64(agg.Promoters) / float64(agg.Total) * npsPercentScale)
	agg.PassivePct = npsRound(float64(agg.Passives) / float64(agg.Total) * npsPercentScale)
	agg.DetractorPct = npsRound(float64(agg.Detractors) / float64(agg.Total) * npsPercentScale)

	byMonth := map[string][]npsRecord{}
	byHive := map[string][]npsRecord{}
	for _, r := range records {
		if ts, err := time.Parse(time.RFC3339, r.Timestamp); err == nil {
			m := ts.UTC().Format(npsTrendMonthLayout)
			byMonth[m] = append(byMonth[m], r)
		}
		byHive[r.HiveID] = append(byHive[r.HiveID], r)
	}
	months := make([]string, 0, len(byMonth))
	for m := range byMonth {
		months = append(months, m)
	}
	sort.Strings(months)
	for _, m := range months {
		agg.Trend = append(agg.Trend, npsTrendPoint{Month: m, npsTally: npsTallyOf(byMonth[m])})
	}

	for id, rs := range byHive {
		agg.PerHive = append(agg.PerHive, npsHiveBreakdown{
			HiveID:   id,
			HiveName: names[id],
			LastAt:   rs[len(rs)-1].Timestamp,
			npsTally: npsTallyOf(rs),
		})
	}
	sort.Slice(agg.PerHive, func(i, j int) bool {
		if agg.PerHive[i].Total != agg.PerHive[j].Total {
			return agg.PerHive[i].Total > agg.PerHive[j].Total
		}
		return agg.PerHive[i].HiveID < agg.PerHive[j].HiveID
	})

	for i := len(records) - 1; i >= 0 && len(agg.Recent) < npsRecentCount; i-- {
		r := records[i]
		agg.Recent = append(agg.Recent, npsRecentResponse{
			HiveID:           r.HiveID,
			HiveName:         names[r.HiveID],
			Score:            r.Score,
			Category:         npsCategory(r.Score),
			Feedback:         r.Feedback,
			Timestamp:        r.Timestamp,
			DashboardVersion: r.DashboardVersion,
		})
	}
	return agg
}

// npsIngestPayload is what a spoke sends. There is no user field on purpose.
type npsIngestPayload struct {
	HiveID           string `json:"hive_id"`
	Score            int    `json:"score"`
	Feedback         string `json:"feedback,omitempty"`
	DashboardVersion string `json:"dashboard_version,omitempty"`
}

func npsJSONError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// handleNPSIngest accepts one NPS response from a spoke.
//
// Authentication is the per-hive heartbeat bearer, bound to the hive_id in the
// body exactly as /api/task-status binds it: a spoke can only submit as
// itself (console #13664/#13758 accepted scores from anyone). Unlike
// task-status, a hub with no master secret refuses outright instead of running
// open: there is no legitimate unauthenticated NPS source.
func (s *HubServer) handleNPSIngest(w http.ResponseWriter, r *http.Request) {
	if s.hubSecret == "" {
		npsJSONError(w, "nps ingest requires a configured hub secret", http.StatusServiceUnavailable)
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		npsJSONError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	presented := strings.TrimPrefix(auth, "Bearer ")

	// Read at most one byte past the cap, so an oversized body is detected
	// from the bytes themselves whatever Content-Length claims.
	body, err := io.ReadAll(io.LimitReader(r.Body, npsIngestMaxBodyBytes+1))
	if err != nil {
		npsJSONError(w, "read error", http.StatusBadRequest)
		return
	}
	if len(body) > npsIngestMaxBodyBytes {
		npsJSONError(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	var p npsIngestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		npsJSONError(w, "invalid payload", http.StatusBadRequest)
		return
	}
	hiveID := sanitizeHeartbeatField(p.HiveID)
	if hiveID == "" || hiveID != p.HiveID || !isValidName(hiveID) {
		npsJSONError(w, "invalid hive_id", http.StatusBadRequest)
		return
	}
	if !s.verifyHeartbeatBearer(presented, hiveID) {
		npsJSONError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.npsHiveRegistered(hiveID) {
		npsJSONError(w, "unknown hive — heartbeat first", http.StatusForbidden)
		return
	}
	if p.Score < npsScoreMin || p.Score > npsScoreMax {
		npsJSONError(w, fmt.Sprintf("score must be %d-%d", npsScoreMin, npsScoreMax), http.StatusBadRequest)
		return
	}
	rec := npsRecord{
		HiveID:           hiveID,
		Score:            p.Score,
		Feedback:         npsSanitizeFeedback(p.Feedback),
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
		DashboardVersion: truncateRunes(sanitizeHeartbeatField(p.DashboardVersion), npsMaxVersionRunes),
	}
	if err := hubNPS.add(rec, time.Now()); err != nil {
		if errors.Is(err, errNPSRateLimited) {
			npsJSONError(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		if s.logger != nil {
			s.logger.Error("nps: failed to store response", "hive", hiveID, "error", err)
		}
		npsJSONError(w, "failed to store response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "category": npsCategory(rec.Score)})
}

// npsHiveRegistered reports whether hiveID is in the registry. A valid bearer
// already proves identity; this additionally keeps the store keyed only by
// hives the hub actually knows.
func (s *HubServer) npsHiveRegistered(hiveID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.registry.Hives {
		if h.ID == hiveID {
			return true
		}
	}
	return false
}

// npsHiveNames maps registered hive IDs to display names for the admin view.
func (s *HubServer) npsHiveNames() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make(map[string]string, len(s.registry.Hives))
	for _, h := range s.registry.Hives {
		if h.Name != "" {
			names[h.ID] = h.Name
		}
	}
	return names
}

// handleAdminNPS serves the aggregate. Registered ONLY behind requireAdmin:
// this is the single read path for NPS data, free text included.
func (s *HubServer) handleAdminNPS(w http.ResponseWriter, r *http.Request) {
	records, err := hubNPS.snapshot()
	if err != nil {
		if s.logger != nil {
			s.logger.Error("nps: failed to load responses", "error", err)
		}
		npsJSONError(w, "failed to load responses", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(buildNPSAggregation(records, s.npsHiveNames()))
}
