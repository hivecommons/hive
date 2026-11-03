package hivectl

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRelayActivityMissingIsUnavailable(t *testing.T) {
	store := NewProfileStore(t.TempDir())
	if _, _, err := store.LoadRelayActivity(); !errors.Is(err, ErrRelayActivityUnavailable) {
		t.Fatalf("missing history must be unavailable, got %v", err)
	}
	if err := store.ClearRelayActivity(); err != nil {
		t.Fatalf("clearing absent history: %v", err)
	}
}

func TestRelayActivityPathHonoursEnvOverride(t *testing.T) {
	dir := t.TempDir()
	store := NewProfileStore(dir)
	if got, want := store.RelayActivityPath(), filepath.Join(dir, relayActivityFileName); got != want {
		t.Fatalf("default path = %q, want %q", got, want)
	}
	override := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	t.Setenv("HIVE_RELAY_ACTIVITY_FILE", override)
	if got := store.RelayActivityPath(); got != override {
		t.Fatalf("override path = %q, want %q", got, override)
	}
	line := `{"key":"a","hub":"wss://h/contribute","task_id":"t1","outcome":"success","started_at":"2026-10-01T10:00:00Z","ended_at":"2026-10-01T10:05:00Z"}` + "\n"
	if err := os.WriteFile(override, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	records, _, err := store.LoadRelayActivity()
	if err != nil || len(records) != 1 {
		t.Fatalf("reader must follow the override: records=%d err=%v", len(records), err)
	}
	if err := store.ClearRelayActivity(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(override); !os.IsNotExist(err) {
		t.Fatalf("--clear must remove the overridden file, stat err=%v", err)
	}
}

func TestLoadRelayActivitySkipsMalformedLines(t *testing.T) {
	store := NewProfileStore(t.TempDir())
	body := `{"key":"a","hub":"wss://h/contribute","repository":"github/github.com/o/r","task_id":"t1","outcome":"success","started_at":"2026-10-01T10:00:00Z","ended_at":"2026-10-01T10:05:00Z"}
not json
{"task_id":"t2"}

{"key":"b","hub":"wss://h/contribute","repository":"github/github.com/o/r","task_id":"t3","outcome":"failure","started_at":"2026-10-02T10:00:00Z","ended_at":"2026-10-02T10:05:00Z"}
`
	if err := os.WriteFile(store.RelayActivityPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	records, skipped, err := store.LoadRelayActivity()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || skipped != 2 {
		t.Fatalf("records=%d skipped=%d, want 2 and 2", len(records), skipped)
	}
}

func TestRollupRelayActivityDedupesAndCountsOnlySuccess(t *testing.T) {
	store := NewProfileStore(t.TempDir())
	body := `{"key":"h1|c1|t1|1|run","hub":"wss://a/contribute","repository":"github/github.com/Org/Repo","task_id":"t1","task_gen":1,"outcome":"success","started_at":"2026-10-01T10:00:00Z","ended_at":"2026-10-01T10:05:00Z"}
{"key":"h1|c1|t1|1|run","hub":"wss://a/contribute","repository":"github/github.com/org/repo","task_id":"t1","task_gen":1,"outcome":"success","started_at":"2026-10-01T10:00:00Z","ended_at":"2026-10-01T10:05:00Z"}
{"key":"h2|c2|t9|4|run","hub":"wss://b/contribute","repository":"github/github.com/org/repo","task_id":"t9","task_gen":4,"outcome":"success","started_at":"2026-10-03T10:00:00Z","ended_at":"2026-10-03T11:00:00Z"}
{"key":"h1|c1|t2|1|run","hub":"wss://a/contribute","repository":"github/github.com/org/repo","task_id":"t2","task_gen":1,"outcome":"failure","started_at":"2026-10-02T10:00:00Z","ended_at":"2026-10-02T10:05:00Z"}
{"key":"h1|c1|t3|1|run","hub":"wss://a/contribute","repository":"github/github.com/org/other","task_id":"t3","task_gen":1,"outcome":"cancelled","started_at":"2026-10-02T10:00:00Z","ended_at":"2026-10-02T10:05:00Z"}
{"key":"h1|c1|t4|1|offer","hub":"wss://a/contribute","repository":"github/github.com/org/other","task_id":"t4","task_gen":1,"outcome":"scope-refused","started_at":"2026-10-04T10:00:00Z","ended_at":"2026-10-04T10:00:00Z"}
`
	if err := os.WriteFile(store.RelayActivityPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	records, _, err := store.LoadRelayActivity()
	if err != nil {
		t.Fatal(err)
	}
	rollup := RollupRelayActivity(records)
	if rollup.Attempts != 5 || rollup.Completed != 2 {
		t.Fatalf("attempts=%d completed=%d, want 5 and 2", rollup.Attempts, rollup.Completed)
	}
	if rollup.Scope != "retained history" || rollup.Retention == "" {
		t.Fatalf("rollup must state retained-history scope and retention: %+v", rollup)
	}
	if len(rollup.Repositories) != 2 {
		t.Fatalf("repositories=%+v", rollup.Repositories)
	}
	other, repo := rollup.Repositories[0], rollup.Repositories[1]
	if repo.Repository != "github/github.com/org/repo" || repo.Completed != 2 || repo.Outcomes[ActivityFailure] != 1 || len(repo.Hives) != 2 {
		t.Fatalf("shared repository not rolled up once: %+v", repo)
	}
	if repo.LastActivity.Format("2006-01-02T15") != "2026-10-03T11" {
		t.Fatalf("last activity = %v", repo.LastActivity)
	}
	if other.Completed != 0 || other.Outcomes[ActivityCancelled] != 1 || other.Outcomes[ActivityScopeRefused] != 1 {
		t.Fatalf("non-success outcomes must not count as completed: %+v", other)
	}
	if err := store.ClearRelayActivity(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadRelayActivity(); !errors.Is(err, ErrRelayActivityUnavailable) {
		t.Fatalf("cleared history must read as unavailable, got %v", err)
	}
}
