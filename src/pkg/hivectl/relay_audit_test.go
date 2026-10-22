package hivectl

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

const auditFixture = `{"v":1,"key":"k1","hub":"wss://a/contribute","repository":"github/github.com/org/repo","task_id":"t1","outcome":"success","started_at":"2026-10-01T10:00:00Z","ended_at":"2026-10-01T10:05:00Z"}
{"v":2,"level":"lifecycle","event":"assigned","ts":"2026-10-01T10:00:00Z","hub":"wss://a/contribute","task_id":"t1","task_gen":1,"backend":"claude","capture":"shim"}
{"v":2,"level":"commands","event":"command","ts":"2026-10-01T10:01:00Z","task_id":"t1","task_gen":1,"op":"git","cmd":"git push origin HEAD","exit":0,"remote":"origin","refspec":"HEAD","reason":"push the fix","reason_source":"transcript"}
{"v":2,"level":"reasoning","event":"command","ts":"2026-10-01T10:02:00Z","task_id":"t1","task_gen":1,"op":"git","cmd":"git fetch origin","exit":1}
{"v":2,"level":"lifecycle","event":"revoked","ts":"2026-10-02T10:00:00Z","hub":"wss://a/contribute","task_id":"t2","reason":"hub cancelled"}
{"v":2,"level":"bogus","event":"command","ts":"2026-10-02T10:00:00Z","task_id":"t2"}
not json
`

func writeAuditFixture(t *testing.T, store *ProfileStore) {
	t.Helper()
	if err := os.WriteFile(store.RelayActivityPath(), []byte(auditFixture), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestValidateAuditLevel(t *testing.T) {
	for _, level := range AuditLevels {
		if err := ValidateAuditLevel(level); err != nil {
			t.Fatalf("%s: %v", level, err)
		}
	}
	for _, bad := range []string{"", "verbose", "Commands"} {
		if ValidateAuditLevel(bad) == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
	if (Profile{}).EffectiveAuditLevel() != DefaultAuditLevel || DefaultAuditLevel != AuditLevelLifecycle {
		t.Fatal("an unset level must mean lifecycle")
	}
}

func TestSetAuditLevelValidatesAndFindsProfile(t *testing.T) {
	set := opsSet()
	if _, err := set.SetAuditLevel("acme", "loud"); err == nil {
		t.Fatal("invalid level accepted")
	}
	if _, err := set.SetAuditLevel("missing", AuditLevelCommands); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("missing profile: %v", err)
	}
	p, err := set.SetAuditLevel("ACME", AuditLevelCommands)
	if err != nil || p.AuditLevel != AuditLevelCommands || set.Profiles[0].AuditLevel != AuditLevelCommands {
		t.Fatalf("level not recorded: %+v %v", p, err)
	}
	set.Profiles[1].AuditLevel = "nope"
	if err := set.Validate(); err == nil || !strings.Contains(err.Error(), "audit level") {
		t.Fatalf("Validate must reject a bad stored level: %v", err)
	}
}

func TestAuditLevelProjectionFollowsHubOrder(t *testing.T) {
	store := NewProfileStore(t.TempDir())
	if err := os.WriteFile(store.EnvPath(), []byte("HIVE_RELAY_AUDIT_LEVEL=commands\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	set := opsSet()
	if err := store.Commit(set); err != nil {
		t.Fatal(err)
	}
	env, err := readEnvFile(store.EnvPath())
	if err != nil {
		t.Fatal(err)
	}
	if env.value("HIVE_RELAY_AUDIT_LEVEL") != "commands" {
		t.Fatalf("a hand-set global level must survive when no profile sets one, got %q", env.value("HIVE_RELAY_AUDIT_LEVEL"))
	}
	if _, err := set.SetAuditLevel("other", AuditLevelReasoning); err != nil {
		t.Fatal(err)
	}
	if _, _, err := set.Use("other"); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(set); err != nil {
		t.Fatal(err)
	}
	if env, err = readEnvFile(store.EnvPath()); err != nil {
		t.Fatal(err)
	}
	if env.value("HIVE_RELAY_AUDIT_LEVEL") != "reasoning,lifecycle" || env.value("HIVE_REGISTRATION_TOKEN") != "t2,t1" {
		t.Fatalf("levels must align with HIVE_HUB, got %q", env.value("HIVE_RELAY_AUDIT_LEVEL"))
	}
	set.Profiles[1].AuditLevel = ""
	if err := store.Commit(set); err != nil {
		t.Fatal(err)
	}
	if env, err = readEnvFile(store.EnvPath()); err != nil {
		t.Fatal(err)
	}
	if env.value("HIVE_RELAY_AUDIT_LEVEL") != "" {
		t.Fatalf("a stale per-hub list must be dropped, got %q", env.value("HIVE_RELAY_AUDIT_LEVEL"))
	}
}

func TestLoadRelayAuditReadsAllRowClasses(t *testing.T) {
	store := NewProfileStore(t.TempDir())
	if _, _, err := store.LoadRelayAudit(); !errors.Is(err, ErrRelayActivityUnavailable) {
		t.Fatalf("missing file must be unavailable, got %v", err)
	}
	writeAuditFixture(t, store)
	records, skipped, err := store.LoadRelayAudit()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 5 || skipped != 2 {
		t.Fatalf("records=%d skipped=%d, want 5 and 2", len(records), skipped)
	}
	if !records[0].IsTerminal() || records[0].Outcome != "success" || !records[0].TS.Equal(time.Date(2026, 10, 1, 10, 5, 0, 0, time.UTC)) {
		t.Fatalf("terminal row: %+v", records[0])
	}
	if records[1].Event != "assigned" || records[1].Backend != "claude" || records[1].Capture != "shim" {
		t.Fatalf("lifecycle row: %+v", records[1])
	}
	if !records[2].IsCommand() || records[2].Exit == nil || *records[2].Exit != 0 || records[2].Reason != "push the fix" {
		t.Fatalf("command row: %+v", records[2])
	}
}

func TestLoadRelayActivityIgnoresAuditRows(t *testing.T) {
	store := NewProfileStore(t.TempDir())
	writeAuditFixture(t, store)
	records, skipped, err := store.LoadRelayActivity()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || skipped != 1 {
		t.Fatalf("activity must count only terminal rows and only truly unreadable lines: records=%d skipped=%d", len(records), skipped)
	}
}

func TestFilterRelayAudit(t *testing.T) {
	store := NewProfileStore(t.TempDir())
	writeAuditFixture(t, store)
	records, _, err := store.LoadRelayAudit()
	if err != nil {
		t.Fatal(err)
	}
	if got := FilterRelayAudit(records, RelayAuditFilter{Task: "t2"}); len(got) != 1 || got[0].Event != "revoked" {
		t.Fatalf("task filter: %+v", got)
	}
	since := time.Date(2026, 10, 1, 10, 1, 0, 0, time.UTC)
	if got := FilterRelayAudit(records, RelayAuditFilter{Since: since}); len(got) != 4 {
		t.Fatalf("since filter kept %d rows, want 4", len(got))
	}
	got := FilterRelayAudit(records, RelayAuditFilter{Level: AuditLevelCommands})
	if len(got) != 4 {
		t.Fatalf("level filter kept %d rows, want 4", len(got))
	}
	for _, r := range got {
		if r.IsCommand() && r.Reason != "" {
			t.Fatalf("below reasoning a command row must not show its reason: %+v", r)
		}
		if r.Event == "revoked" && r.Reason == "" {
			t.Fatal("lifecycle reasons are not the reasoning column")
		}
	}
	if got := FilterRelayAudit(records, RelayAuditFilter{Level: AuditLevelOff}); len(got) != 1 || !got[0].IsTerminal() {
		t.Fatalf("off keeps only terminal rows: %+v", got)
	}
	if records[2].Reason == "" {
		t.Fatal("filtering must not mutate the input")
	}
}

func TestClearRelayAuditReasonsKeepsRows(t *testing.T) {
	store := NewProfileStore(t.TempDir())
	if n, err := store.ClearRelayAuditReasons(); err != nil || n != 0 {
		t.Fatalf("absent file: n=%d err=%v", n, err)
	}
	writeAuditFixture(t, store)
	n, err := store.ClearRelayAuditReasons()
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v, want 1", n, err)
	}
	data, err := os.ReadFile(store.RelayActivityPath())
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if strings.Contains(body, "push the fix") || strings.Contains(body, "reason_source") {
		t.Fatalf("reason column survived:\n%s", body)
	}
	if !strings.Contains(body, "hub cancelled") || !strings.Contains(body, "not json") || strings.Count(body, "\n") != strings.Count(auditFixture, "\n") {
		t.Fatalf("clear must keep every row and lifecycle reasons:\n%s", body)
	}
	info, err := os.Stat(store.RelayActivityPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("rewritten file must stay 0600: %v %v", info.Mode(), err)
	}
	records, _, err := store.LoadRelayAudit()
	if err != nil || len(records) != 5 || records[2].Op != "git" {
		t.Fatalf("rows changed: %d %v", len(records), err)
	}
	if n, _ := store.ClearRelayAuditReasons(); n != 0 {
		t.Fatalf("second clear found %d reasons", n)
	}
}
