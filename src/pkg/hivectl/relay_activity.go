package hivectl

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Durable local contribution activity (#13293, spec #13153 §6).
//
// The contributor relay appends one JSONL record per terminal task attempt to
// relay-activity.jsonl beside profiles.yml and hubs-seen.json. The file is
// owner-only (0600 in a 0700 directory), holds no prompts, payloads, tokens or
// log text, and is pruned by the relay to the newest RelayActivityMaxEntries
// records inside RelayActivityRetentionDays — so every count read here covers
// retained history, never lifetime totals.

const (
	relayActivityFileName = "relay-activity.jsonl"

	// Defaults of the relay's HIVE_RELAY_ACTIVITY_MAX_ENTRIES and
	// HIVE_RELAY_ACTIVITY_RETENTION_DAYS (bin/contributor-relay.js).
	RelayActivityMaxEntries    = 1000
	RelayActivityRetentionDays = 90

	ActivitySuccess      = "success"
	ActivityFailure      = "failure"
	ActivityCancelled    = "cancelled"
	ActivityScopeRefused = "scope-refused"
)

// ErrRelayActivityUnavailable reports that no local history exists, which
// callers must show as unavailable rather than as zero activity.
var ErrRelayActivityUnavailable = errors.New("local relay activity unavailable")

// RelayActivityRetention describes the relay's default pruning policy.
func RelayActivityRetention() string {
	return fmt.Sprintf("last %d days, up to %d task attempts", RelayActivityRetentionDays, RelayActivityMaxEntries)
}

// RelayActivityPath reports where the relay records terminal task attempts.
// HIVE_RELAY_ACTIVITY_FILE overrides the default, as it does for the relay.
func (s *ProfileStore) RelayActivityPath() string {
	if p := os.Getenv("HIVE_RELAY_ACTIVITY_FILE"); p != "" {
		return p
	}
	return filepath.Join(s.dir, relayActivityFileName)
}

// RelayActivityRecord is one terminal task attempt as the relay wrote it.
type RelayActivityRecord struct {
	Key           string    `json:"key"`
	Hive          string    `json:"hub"`
	ContributorID string    `json:"contributor_id,omitempty"`
	Repository    string    `json:"repository,omitempty"`
	Repo          string    `json:"repo,omitempty"`
	Number        int       `json:"number,omitempty"`
	Kind          string    `json:"kind,omitempty"`
	TaskID        string    `json:"task_id"`
	TaskGen       any       `json:"task_gen,omitempty"`
	Outcome       string    `json:"outcome"`
	StartedAt     time.Time `json:"started_at"`
	EndedAt       time.Time `json:"ended_at"`
	Backend       string    `json:"backend,omitempty"`
	Model         string    `json:"model,omitempty"`
}

// LoadRelayActivity reads the activity store, skipping blank, malformed or
// incomplete lines and reporting how many it skipped. A missing file is
// ErrRelayActivityUnavailable.
func (s *ProfileStore) LoadRelayActivity() ([]RelayActivityRecord, int, error) {
	f, err := os.Open(s.RelayActivityPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, ErrRelayActivityUnavailable
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read relay activity file %s: %w", s.RelayActivityPath(), err)
	}
	defer f.Close()
	var records []RelayActivityRecord
	skipped := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record RelayActivityRecord
		err := json.Unmarshal([]byte(line), &record)
		var probe struct {
			Event string `json:"event"`
		}
		if err == nil && json.Unmarshal([]byte(line), &probe) == nil && probe.Event != "" {
			// Lifecycle and command rows of the leveled audit stream
			// (#13456) share the file; they are not attempts.
			continue
		}
		if err != nil || record.TaskID == "" || record.Outcome == "" || record.EndedAt.IsZero() {
			skipped++
			continue
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, fmt.Errorf("read relay activity file %s: %w", s.RelayActivityPath(), err)
	}
	return records, skipped, nil
}

// ClearRelayActivity deletes local history only; profiles.yml, consent and
// repository selection are untouched.
func (s *ProfileStore) ClearRelayActivity() error {
	err := os.Remove(s.RelayActivityPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear relay activity file %s: %w", s.RelayActivityPath(), err)
	}
	return nil
}

// RepositoryActivity is the rollup for one canonical repository.
type RepositoryActivity struct {
	Repository   string         `json:"repository" yaml:"repository"`
	Hives        []string       `json:"hives" yaml:"hives"`
	Completed    int            `json:"completed" yaml:"completed"`
	Outcomes     map[string]int `json:"outcomes" yaml:"outcomes"`
	LastActivity time.Time      `json:"last_activity" yaml:"last_activity"`
}

// RelayActivityRollup summarizes retained local history.
type RelayActivityRollup struct {
	Available    bool                 `json:"available" yaml:"available"`
	Scope        string               `json:"scope" yaml:"scope"`
	Retention    string               `json:"retention" yaml:"retention"`
	Attempts     int                  `json:"attempts" yaml:"attempts"`
	Completed    int                  `json:"completed" yaml:"completed"`
	Outcomes     map[string]int       `json:"outcomes" yaml:"outcomes"`
	LastActivity time.Time            `json:"last_activity" yaml:"last_activity"`
	Repositories []RepositoryActivity `json:"repositories" yaml:"repositories"`
}

func relayActivityKey(r RelayActivityRecord) string {
	if r.Key != "" {
		return r.Key
	}
	phase := "run"
	if r.Outcome == ActivityScopeRefused {
		phase = "offer"
	}
	return strings.Join([]string{r.Hive, r.ContributorID, r.TaskID, fmt.Sprint(r.TaskGen), phase}, "|")
}

func relayActivityRepository(r RelayActivityRecord) string {
	if r.Repository != "" {
		if id, err := NormalizeRepositoryID(r.Repository); err == nil {
			return id
		}
		return strings.ToLower(r.Repository)
	}
	if r.Repo != "" {
		return strings.ToLower(r.Repo)
	}
	return "(unknown)"
}

// RollupRelayActivity counts each task attempt once (by subscription plus
// Hive task/lease generation) and groups by canonical repository, so a
// repository reached through several hives is one row. Only success counts as
// completed: local completion, not upstream approval or a merged PR.
func RollupRelayActivity(records []RelayActivityRecord) RelayActivityRollup {
	rollup := RelayActivityRollup{
		Available: true,
		Scope:     "retained history",
		Retention: RelayActivityRetention(),
		Outcomes:  map[string]int{},
	}
	seen := map[string]bool{}
	repos := map[string]*RepositoryActivity{}
	for _, r := range records {
		key := relayActivityKey(r)
		if seen[key] {
			continue
		}
		seen[key] = true
		id := relayActivityRepository(r)
		repo := repos[id]
		if repo == nil {
			repo = &RepositoryActivity{Repository: id, Outcomes: map[string]int{}}
			repos[id] = repo
		}
		if r.Hive != "" && !slices.Contains(repo.Hives, r.Hive) {
			repo.Hives = append(repo.Hives, r.Hive)
		}
		repo.Outcomes[r.Outcome]++
		rollup.Outcomes[r.Outcome]++
		rollup.Attempts++
		if r.Outcome == ActivitySuccess {
			repo.Completed++
			rollup.Completed++
		}
		if r.EndedAt.After(repo.LastActivity) {
			repo.LastActivity = r.EndedAt
		}
		if r.EndedAt.After(rollup.LastActivity) {
			rollup.LastActivity = r.EndedAt
		}
	}
	rollup.Repositories = make([]RepositoryActivity, 0, len(repos))
	for _, repo := range repos {
		sort.Strings(repo.Hives)
		rollup.Repositories = append(rollup.Repositories, *repo)
	}
	sort.Slice(rollup.Repositories, func(i, j int) bool {
		return rollup.Repositories[i].Repository < rollup.Repositories[j].Repository
	})
	return rollup
}
