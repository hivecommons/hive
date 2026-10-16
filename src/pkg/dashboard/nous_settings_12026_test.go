package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestGovernorFeatures_NousSettingsPut covers the Settings → Features
// "Nous / Experiment Lab" controls (hivecommons/hive#12026): bad values are
// rejected before anything is mutated, good values round-trip through
// PUT /api/config/governor/features and GET /api/config/governor, and the
// Features tab renders every nous.* key.
func TestGovernorFeatures_NousSettingsPut(t *testing.T) {
	s := covApiServer(t)
	before := s.deps.Config.Nous

	for _, bad := range []map[string]any{
		{"nousDispatcher": "carrier-pigeon"},
		{"nousGatePollSeconds": -1},
		{"nousFetchAllowlist": []string{"*.example.com"}},
		{"nousFetchAllowlist": []string{"https://example.com"}},
		{"nousFetchAllowlist": []string{"127.0.0.1"}},
		{"nousArtifactRepo": "not-a-repo"},
		{"nousArtifactRepo": "owner/name.git"},
		{"nousArtifactBranch": "refs/heads/main"},
		{"nousArtifactBranch": "a..b"},
		{"nousArtifactPath": "../escape"},
		{"nousArtifactSyncIntervalSeconds": -5},
		// A valid value alongside an invalid one must not be applied.
		{"nousGatePollSeconds": 30, "nousDispatcher": "nope"},
	} {
		rec := doPut(s, "/api/config/governor/features", bad)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT %v = %d, want 400 (%s)", bad, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "nous") {
			t.Fatalf("PUT %v error does not name the nous key: %s", bad, rec.Body.String())
		}
	}
	if !reflect.DeepEqual(before, s.deps.Config.Nous) {
		t.Fatalf("a rejected PUT changed the config: %+v", s.deps.Config.Nous)
	}

	rec := doPut(s, "/api/config/governor/features", map[string]any{
		"nousDispatcher":                  " SDK ",
		"nousGatePollSeconds":             30,
		"nousFetchAllowlist":              []string{" GitHub.com ", "pypi.org", ""},
		"nousArtifactRepo":                " acme/nous-artifacts ",
		"nousArtifactBranch":              "main",
		"nousArtifactPath":                "campaigns/hive",
		"nousArtifactSyncIntervalSeconds": 600,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d - %s", rec.Code, rec.Body.String())
	}
	n := s.deps.Config.Nous
	if n.Dispatcher != config.NousDispatcherSDK || n.GatePollSeconds != 30 {
		t.Fatalf("dispatcher/gate poll not persisted: %+v", n)
	}
	if !reflect.DeepEqual(n.FetchAllowlist, []string{"github.com", "pypi.org"}) {
		t.Fatalf("fetch allowlist not normalised: %v", n.FetchAllowlist)
	}
	if n.ArtifactRepo.Repo != "acme/nous-artifacts" || n.ArtifactRepo.Branch != "main" || n.ArtifactRepo.Path != "campaigns/hive" || n.ArtifactRepo.SyncIntervalSeconds != 600 {
		t.Fatalf("artifact repo not persisted: %+v", n.ArtifactRepo)
	}

	// An explicitly empty allowlist is deny-all and must survive as a
	// non-nil empty list rather than collapsing to the github.com default.
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"nousFetchAllowlist": []string{}}); rec.Code != http.StatusOK {
		t.Fatalf("PUT empty allowlist = %d - %s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Nous.FetchAllowlist; got == nil || len(got) != 0 {
		t.Fatalf("empty allowlist stored as %#v, want non-nil empty", got)
	}
	if eff := s.deps.Config.Nous.ResolveFetchAllowlist(nil); len(eff) != 0 {
		t.Fatalf("empty allowlist resolves to %v, want deny-all", eff)
	}

	// Empty dispatcher means "default" and is stored as such.
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"nousDispatcher": ""}); rec.Code != http.StatusOK {
		t.Fatalf("PUT empty dispatcher = %d - %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Nous.Dispatcher != "" {
		t.Fatalf("empty dispatcher stored as %q", s.deps.Config.Nous.Dispatcher)
	}

	// GET reflects the stored values and carries what the UI needs to
	// render the select and the effective-value hints.
	get := doGet(s, "/api/config/governor")
	if get.Code != http.StatusOK {
		t.Fatalf("GET = %d", get.Code)
	}
	var payload struct {
		Features map[string]any `json:"features"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	f := payload.Features
	if f["nousGatePollSeconds"] != float64(30) || f["nousArtifactRepo"] != "acme/nous-artifacts" || f["nousArtifactEnabled"] != true {
		t.Fatalf("GET features missing nous values: %v", f)
	}
	vals, _ := f["nousDispatcherValues"].([]any)
	if len(vals) != len(config.ValidNousDispatchers) || vals[0] != config.DefaultNousDispatcher {
		t.Fatalf("nousDispatcherValues = %v", f["nousDispatcherValues"])
	}
	for _, key := range []string{"nousDispatcherEffective", "nousGatePollSecondsEffective", "nousFetchAllowlistEffective", "nousArtifactSyncMinSeconds"} {
		if _, ok := f[key]; !ok {
			t.Errorf("GET features missing %s", key)
		}
	}

	allow, err := os.ReadFile("settings_parity_allowlist.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(allow), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "nous.") || strings.HasPrefix(trimmed, "agents.<key>.engine") {
			t.Errorf("settings_parity_allowlist.txt still allow-lists %q; the control exists now", trimmed)
		}
	}

	html, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"nousDispatcher", "nousGatePollSeconds", "nousFetchAllowlist", "nousArtifactRepo", "nousArtifactBranch", "nousArtifactPath", "nousArtifactSyncIntervalSeconds"} {
		if !strings.Contains(string(html), `data-arg1="`+key+`"`) {
			t.Errorf("Settings UI has no control for %s", key)
		}
	}
	for _, yamlKey := range []string{"nous.dispatcher", "nous.gate_poll_seconds", "nous.fetch_allowlist[]", "nous.artifact_repo.repo", "nous.artifact_repo.branch", "nous.artifact_repo.path", "nous.artifact_repo.sync_interval_seconds", "agents.<key>.engine"} {
		if !strings.Contains(string(html), yamlKey) {
			t.Errorf("Settings UI does not label the %s key", yamlKey)
		}
	}
	// The settings search must find the group for the words an operator
	// would type (hivecommons/hive#12026 started from "nous" → 0 matches).
	// Since #12037 the Features tab is organised into headed sections, so the
	// entry files under the "Experimental" heading and keeps its own label.
	hit := strings.Index(string(html), "section: 'Experimental', label: 'Nous / Experiment Lab'")
	if hit < 0 {
		t.Fatal("settings search has no static hit for the Nous group")
	}
	line := string(html)[hit:]
	line = line[:strings.Index(line, "\n")]
	for _, word := range []string{"nous", "experiment", "gate", "artifact"} {
		if !strings.Contains(strings.ToLower(line), word) {
			t.Errorf("Nous search hit does not match %q", word)
		}
	}
}

// TestAgentConfigGeneral_ExperimentEngine covers agents.<name>.engine in the
// agent dialog: it is rejected on a non-experimenter, accepted (and
// lower-cased) on an experimenter, and clearable.
func TestAgentConfigGeneral_ExperimentEngine(t *testing.T) {
	s := acfgServer(t)

	if rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"engine": "nous"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("engine on a scanner = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"role": config.RoleExperimenter, "engine": "warp-drive"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown engine = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Agents["scanner"]; got.Role != "" || got.Engine != "" {
		t.Fatalf("a rejected PUT changed the agent: %+v", got)
	}

	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"role": config.RoleExperimenter, "engine": " NOUS "})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d - %s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Agents["scanner"]; got.Role != config.RoleExperimenter || got.Engine != config.ExperimentEngineNous {
		t.Fatalf("engine not persisted: %+v", got)
	}

	get := doGet(s, "/api/config/agent/scanner")
	if get.Code != http.StatusOK {
		t.Fatalf("GET = %d", get.Code)
	}
	var payload struct {
		General map[string]any `json:"general"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.General["engine"] != config.ExperimentEngineNous {
		t.Fatalf("GET general.engine = %v", payload.General["engine"])
	}
	if vals, _ := payload.General["engineValues"].([]any); len(vals) == 0 {
		t.Fatalf("GET general.engineValues = %v", payload.General["engineValues"])
	}

	if rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"engine": ""}); rec.Code != http.StatusOK {
		t.Fatalf("clear engine = %d - %s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Agents["scanner"].Engine; got != "" {
		t.Fatalf("engine not cleared: %q", got)
	}
}
