package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/celtrigger"
	"github.com/hivecommons/hive/pkg/config"
	"gopkg.in/yaml.v3"
)

func eventTriggerTestServer(t *testing.T) *Server {
	t.Helper()
	s := covCServer(t)
	dir := t.TempDir()
	s.deps.Config.SourcePath = filepath.Join(dir, "hive.yaml")
	oldRuntime, oldOverlay := config.RuntimeConfigFile, config.DashboardOverlayFile
	config.RuntimeConfigFile = filepath.Join(dir, "hive.yaml.runtime")
	config.DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	t.Cleanup(func() {
		config.RuntimeConfigFile, config.DashboardOverlayFile = oldRuntime, oldOverlay
	})
	return s
}

func putEventTriggers(s *Server, body, version string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/config/triggers", strings.NewReader(body))
	markOwnerRequest(req)
	req.Header.Set("If-Match", version)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestEventTriggersPresetPersistenceAndPreservation(t *testing.T) {
	s := eventTriggerTestServer(t)
	cfg := s.deps.Config
	legacy := config.TriggerRule{Name: "advanced", Expr: `event.labels.exists(x, x.startsWith("team/"))`, Agent: "legacy-agent", Priority: -17}
	cfg.Triggers = []config.TriggerRule{legacy}
	scannerBefore := cfg.Agents["scanner"]
	cfg.Governor.EvalIntervalS = 731
	get := doOwnerGet(s, "/api/config/triggers")
	meta := decodeJSON(t, get)
	if get.Code != http.StatusOK || meta["version"] != get.Header().Get("ETag") || meta["requires_restart"] != false {
		t.Fatalf("get: %d %s", get.Code, get.Body.String())
	}
	repositories, ok := meta["repositories"].([]any)
	if !ok || len(repositories) != 1 {
		t.Fatalf("invalid repository metadata: %v", meta["repositories"])
	}
	repo, ok := repositories[0].(map[string]any)
	if !ok {
		t.Fatalf("invalid repository entry: %v", repositories[0])
	}
	if repo["value"] != "repo1" || repo["label"] != "myorg/repo1" {
		t.Fatalf("must retain configured event.repo spelling: %v", repo)
	}
	rules := append(cfg.TriggerRulesSnapshot(), config.TriggerRule{Name: "scanner-first-seen", Agent: "scanner", Expr: "overwritten by scoped preset", Priority: 29})
	presetIndex := 1
	body, err := json.Marshal(map[string]any{"rules": rules, "preset": eventTriggerPreset{Index: &presetIndex, Kind: "issue.opened", Repo: "repo1"}})
	if err != nil {
		t.Fatal(err)
	}
	s.deps.PersistFunc = func() { t.Error("must not request a second asynchronous config write") }
	rec := putEventTriggers(s, string(body), get.Header().Get("ETag"))
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	want := []config.TriggerRule{legacy, {Name: "scanner-first-seen", Agent: "scanner", Expr: `event.kind == "issue.opened" && event.repo == "repo1"`, Priority: 29}}
	if !reflect.DeepEqual(cfg.TriggerRulesSnapshot(), want) || !reflect.DeepEqual(cfg.Agents["scanner"], scannerBefore) || cfg.Governor.EvalIntervalS != 731 {
		t.Fatal("save changed unrelated rules, priorities, agent or governor config")
	}
	data, err := os.ReadFile(cfg.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted config.Config
	if err := yaml.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted.Triggers, want) || persisted.Governor.EvalIntervalS != 731 {
		t.Fatalf("durable rules/settings not preserved: %+v", persisted.Triggers)
	}
	engine, err := celtrigger.CompileFromConfig(&persisted)
	if err != nil {
		t.Fatal(err)
	}
	got := engine.MatchAgents(celtrigger.NormalizedEvent{Kind: "issue.opened", Repo: "repo1"})
	if !reflect.DeepEqual(got, []string{"scanner"}) {
		t.Fatalf("scoped preset failed to match real configured repo: %v", got)
	}
	if got := engine.MatchAgents(celtrigger.NormalizedEvent{Kind: "issue.opened", Repo: "other"}); len(got) != 0 {
		t.Fatalf("preset broadened repository scope: %v", got)
	}
	stale := putEventTriggers(s, `{"rules":[]}`, get.Header().Get("ETag"))
	if stale.Code != http.StatusPreconditionFailed || !reflect.DeepEqual(cfg.TriggerRulesSnapshot(), want) {
		t.Fatalf("stale revision overwrote concurrent rules: %d", stale.Code)
	}
	duplicate, err := json.Marshal(map[string]any{"rules": append(cfg.TriggerRulesSnapshot(), legacy)})
	if err != nil {
		t.Fatal(err)
	}
	rejected := putEventTriggers(s, string(duplicate), rec.Header().Get("ETag"))
	if rejected.Code != http.StatusBadRequest || !strings.Contains(rejected.Body.String(), "existing target agent") ||
		!reflect.DeepEqual(cfg.TriggerRulesSnapshot(), want) {
		t.Fatalf("new copies must not inherit legacy validation exceptions: %d %s", rejected.Code, rejected.Body.String())
	}
	// An advanced edit retains order and untouched legacy expressions/targets.
	want[1].Expr = `event.kind == "pr.opened" && event.repo == "repo1" && !event.is_draft`
	body, err = json.Marshal(map[string]any{"rules": want})
	if err != nil {
		t.Fatal(err)
	}
	edited := putEventTriggers(s, string(body), rec.Header().Get("ETag"))
	if edited.Code != http.StatusOK || !reflect.DeepEqual(cfg.TriggerRulesSnapshot(), want) {
		t.Fatalf("advanced edit: %d %s", edited.Code, edited.Body.String())
	}
	removed := putEventTriggers(s, `{"rules":[]}`, edited.Header().Get("ETag"))
	if removed.Code != http.StatusOK || len(cfg.TriggerRulesSnapshot()) != 0 {
		t.Fatalf("remove all: %d %s", removed.Code, removed.Body.String())
	}
}

func TestEventTriggersRejectInvalidAndUnpersistedRules(t *testing.T) {
	s := eventTriggerTestServer(t)
	cfg := s.deps.Config
	before := []config.TriggerRule{{Name: "existing", Agent: "scanner", Expr: "true", Priority: 8}}
	cfg.Triggers = append([]config.TriggerRule{}, before...)
	version := config.TriggerRulesVersion(before)
	for _, tc := range []struct {
		name, body, message string
	}{
		{"missing list", `{}`, "invalid body"},
		{"unknown enabled", `{"rules":[{"name":"r","agent":"scanner","expr":"true","enabled":true}]}`, "no enabled"},
		{"priority type", `{"rules":[{"name":"r","agent":"scanner","expr":"true","priority":"high"}]}`, "integer priority"},
		{"fractional priority", `{"rules":[{"name":"r","agent":"scanner","expr":"true","priority":1.5}]}`, "integer priority"},
		{"trailing object", `{"rules":[]} {}`, "one JSON"},
		{"empty name", `{"rules":[{"agent":"scanner","expr":"true"}]}`, "name is required"},
		{"unknown agent", `{"rules":[{"name":"r","agent":"missing","expr":"true"}]}`, "existing target agent"},
		{"unknown field", `{"rules":[{"name":"r","agent":"scanner","expr":"event.missing == 1"}]}`, "invalid CEL"},
		{"syntax", `{"rules":[{"name":"r","agent":"scanner","expr":"event.kind =="}]}`, "invalid CEL"},
		{"non bool", `{"rules":[{"name":"r","agent":"scanner","expr":"event.number + 1"}]}`, "must return bool"},
		{"wildcard repo", `{"rules":[{"name":"r","agent":"scanner","expr":"true"}],"preset":{"index":0,"kind":"issue.opened","repo":"*"}}`, "one repository"},
		{"wrong repo spelling", `{"rules":[{"name":"r","agent":"scanner","expr":"true"}],"preset":{"index":0,"kind":"issue.opened","repo":"myorg/repo1"}}`, "one repository"},
		{"unsupported preset", `{"rules":[{"name":"r","agent":"scanner","expr":"true"}],"preset":{"index":0,"kind":"issue.labeled","repo":"repo1"}}`, "invalid first-seen"},
		{"missing preset index", `{"rules":[{"name":"r","agent":"scanner","expr":"true"}],"preset":{"kind":"issue.opened","repo":"repo1"}}`, "invalid first-seen"},
		{"negative preset index", `{"rules":[],"preset":{"index":-1,"kind":"issue.opened","repo":"repo1"}}`, "invalid first-seen"},
		{"outside preset index", `{"rules":[],"preset":{"index":0,"kind":"issue.opened","repo":"repo1"}}`, "invalid first-seen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := putEventTriggers(s, tc.body, version)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.message) {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if !reflect.DeepEqual(cfg.TriggerRulesSnapshot(), before) {
				t.Fatal("rejected update mutated live config")
			}
		})
	}
	missing := putEventTriggers(s, `{"rules":[]}`, "")
	if missing.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing revision: %d", missing.Code)
	}
	cfg.SourcePath = "" // Deterministic failure before any persistent layer is written.
	failed := putEventTriggers(s, `{"rules":[]}`, version)
	if failed.Code != http.StatusInternalServerError || !strings.Contains(failed.Body.String(), "could not persist") || !reflect.DeepEqual(cfg.TriggerRulesSnapshot(), before) {
		t.Fatalf("failed persistence accepted: %d %s", failed.Code, failed.Body.String())
	}
	cfg.SourcePath = t.TempDir()
	// Every layer must fail regardless of test order. An empty directory is
	// not enough for the runtime path: saveLocked removes it and retries
	// (the root-owned-file recovery), and the retry succeeds. A file under
	// a directory that does not exist fails the write, the remove and the
	// retry alike.
	missingDir := filepath.Join(t.TempDir(), "missing")
	config.RuntimeConfigFile = filepath.Join(missingDir, "hive.yaml.runtime")
	config.DashboardOverlayFile = filepath.Join(missingDir, "dashboard-overlay.yaml")
	failed = putEventTriggers(s, `{"rules":[]}`, version)
	if failed.Code != http.StatusInternalServerError || !reflect.DeepEqual(cfg.TriggerRulesSnapshot(), before) ||
		strings.Contains(failed.Body.String(), cfg.SourcePath) {
		t.Fatalf("storage failure mutated rules or exposed diagnostics: %d %s", failed.Code, failed.Body.String())
	}
}

func TestEventTriggersRequireVerifiedOwner(t *testing.T) {
	s := eventTriggerTestServer(t)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		for _, role := range []string{"", "read", "read-write", "merger", "owner"} {
			t.Run(method+"/"+role, func(t *testing.T) {
				req := httptest.NewRequest(method, "/api/config/triggers", strings.NewReader(`{"rules":[]}`))
				req.Header.Set("X-Hive-Role", role) // A claimed owner without trusted authentication must also fail.
				req.Header.Set("If-Match", config.TriggerRulesVersion(nil))
				rec := httptest.NewRecorder()
				s.mux.ServeHTTP(rec, req)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("unverified %s %s: %d", role, method, rec.Code)
				}
			})
		}
	}
}
