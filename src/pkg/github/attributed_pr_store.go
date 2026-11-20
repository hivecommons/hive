package github

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The attributed-PR store keeps the parsed attribution and outcome fields of
// closed hive-attributed PRs on disk, so the PRs-by-model and effective-models
// windows are not limited to what the bounded live scan still lists
// (hivecommons/hive#19365). It holds no PR bodies.
const (
	AttributedPRStoreFile = "attributed-pr-store.json"
	// attributedPRBackfillEnv is how far back the one-time backfill reaches;
	// "off" (or 0) skips the backfill.
	attributedPRBackfillEnv     = "HIVE_ATTRIBUTED_PR_BACKFILL"
	attributedPRDefaultBackfill = 30 * 24 * time.Hour
	// attributedPRBackfillPagesPerTick bounds the extra closed-PR listing
	// pages one repo pays per enumeration while its backfill is unfinished.
	attributedPRBackfillPagesPerTick = 5
	attributedPRStoreSchemaVersion   = 1
)

var (
	AttributedPRStorePath = filepath.Join(ReviewLinksDir, AttributedPRStoreFile)
	attributedPRStoreMu   sync.Mutex
)

type attributedPRStoreRepo struct {
	// CoveredFrom is the updated-time horizon the store fully covers for this
	// repo: every attributed PR updated since then is recorded.
	CoveredFrom time.Time `json:"covered_from"`
	// BackfillDone is true once a backfill reached the target horizon.
	BackfillDone bool `json:"backfill_done,omitempty"`
	// BackfillNextPage resumes an unfinished backfill (0 = not started).
	BackfillNextPage int                    `json:"backfill_next_page,omitempty"`
	Items            map[string]PullRequest `json:"items"`
}

type attributedPRStoreData struct {
	SchemaVersion int                               `json:"schema_version"`
	GeneratedAt   time.Time                         `json:"generated_at"`
	Repos         map[string]*attributedPRStoreRepo `json:"repos"`
}

func attributedPRBackfillWindow() time.Duration {
	raw := strings.TrimSpace(os.Getenv(attributedPRBackfillEnv))
	if raw == "" {
		return attributedPRDefaultBackfill
	}
	if raw == "off" || raw == "0" {
		return 0
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if days, err := strconv.Atoi(strings.TrimSuffix(raw, "d")); err == nil && days > 0 {
		return time.Duration(days) * 24 * time.Hour
	}
	return attributedPRDefaultBackfill
}

func loadAttributedPRStoreLocked(path string) *attributedPRStoreData {
	empty := &attributedPRStoreData{SchemaVersion: attributedPRStoreSchemaVersion, Repos: map[string]*attributedPRStoreRepo{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return empty
	}
	var f attributedPRStoreData
	if err := json.Unmarshal(data, &f); err != nil || f.SchemaVersion != attributedPRStoreSchemaVersion || f.Repos == nil {
		return empty
	}
	return &f
}

func saveAttributedPRStoreLocked(path string, f *attributedPRStoreData) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f.GeneratedAt = time.Now().UTC()
	data, err := json.Marshal(f)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tmp)
	}
}

// attributedPRBackfillState is a repo's backfill position.
type attributedPRBackfillState struct {
	Done     bool
	NextPage int
}

func attributedPRStoreBackfillState(path, repo string) attributedPRBackfillState {
	attributedPRStoreMu.Lock()
	defer attributedPRStoreMu.Unlock()
	r := loadAttributedPRStoreLocked(path).Repos[repo]
	if r == nil {
		return attributedPRBackfillState{}
	}
	return attributedPRBackfillState{Done: r.BackfillDone, NextPage: r.BackfillNextPage}
}

// recordAttributedClosedPRs upserts scanned closed PRs into the store.
// coveredFrom only ever moves the repo's horizon earlier. backfill, when
// non-nil, replaces the repo's backfill progress.
func recordAttributedClosedPRs(path, repo string, prs []PullRequest, coveredFrom time.Time, backfill *attributedPRBackfillState) {
	attributedPRStoreMu.Lock()
	defer attributedPRStoreMu.Unlock()
	f := loadAttributedPRStoreLocked(path)
	r := f.Repos[repo]
	if r == nil {
		r = &attributedPRStoreRepo{}
		f.Repos[repo] = r
	}
	if r.Items == nil {
		r.Items = map[string]PullRequest{}
	}
	changed := false
	for _, pr := range prs {
		if !pr.HiveAttributed {
			continue
		}
		key := strconv.Itoa(pr.Number)
		if old, ok := r.Items[key]; !ok || !reflect.DeepEqual(old, pr) {
			r.Items[key] = pr
			changed = true
		}
	}
	if !coveredFrom.IsZero() && (r.CoveredFrom.IsZero() || coveredFrom.Before(r.CoveredFrom)) {
		r.CoveredFrom = coveredFrom
		changed = true
	}
	if backfill != nil && (r.BackfillDone != backfill.Done || r.BackfillNextPage != backfill.NextPage) {
		r.BackfillDone = backfill.Done
		r.BackfillNextPage = backfill.NextPage
		changed = true
	}
	if changed {
		saveAttributedPRStoreLocked(path, f)
	}
}

// storedAttributedClosedPRs returns the stored closed records for repo.
func storedAttributedClosedPRs(path, repo string) []PullRequest {
	attributedPRStoreMu.Lock()
	defer attributedPRStoreMu.Unlock()
	r := loadAttributedPRStoreLocked(path).Repos[repo]
	if r == nil {
		return nil
	}
	out := make([]PullRequest, 0, len(r.Items))
	for _, pr := range r.Items {
		out = append(out, pr)
	}
	return out
}

// AttributedClosedPRCoverage is how far back the closed attributed-PR records
// reach: the live scan's lookback, extended to the store's horizon once every
// repo in the store has covered it. Consumers compare a window to it.
func AttributedClosedPRCoverage() time.Duration {
	span := attributedClosedPRLookback()
	attributedPRStoreMu.Lock()
	f := loadAttributedPRStoreLocked(AttributedPRStorePath)
	attributedPRStoreMu.Unlock()
	now := time.Now()
	least := time.Duration(-1)
	for _, r := range f.Repos {
		if r.CoveredFrom.IsZero() {
			return span
		}
		if d := now.Sub(r.CoveredFrom); least < 0 || d < least {
			least = d
		}
	}
	if least > span {
		return least
	}
	return span
}

// attributedPRStorePath is where this client keeps its attributed-PR store.
func (c *Client) attributedPRStorePath() string {
	if c != nil && c.attributedPRStoreFile != "" {
		return c.attributedPRStoreFile
	}
	return AttributedPRStorePath
}
