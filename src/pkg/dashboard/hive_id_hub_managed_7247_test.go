package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// The Hive ID is not a display name. On a hub-managed spoke it is the hub's
// PRIMARY KEY for this hive: registry entry and ownership, heartbeat signing
// and its replay guard, SSO token verification, self-upgrade and branch
// switching, alert acknowledgements, and quota accounting all key off it.
// Changing it locally does not rename the hive, it ORPHANS it — the spoke
// drops off the hub while believing itself healthy. These tests pin that the
// edit is refused server-side and never offered client-side (#7247).

func hiveIDTestServer(t *testing.T, hubEnabled bool, hubURL string) *Server {
	t.Helper()
	srv := newFullServer(t)
	srv.deps.Config.HiveID = "hive-bold-hawk"
	srv.deps.Config.Hub.Enabled = hubEnabled
	srv.deps.Config.Hub.URL = hubURL
	return srv
}

func hiveIDPutRequest(t *testing.T, id string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/hive-id", strings.NewReader(`{"id":"`+id+`"}`))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	return req
}

// TestHiveIDSetRefusedOnHubManagedSpoke is the core guard: the write must be
// rejected, and with 409 Conflict rather than 403 — the caller's credentials
// are valid, the request conflicts with the hive's managed state.
func TestHiveIDSetRefusedOnHubManagedSpoke(t *testing.T) {
	srv := hiveIDTestServer(t, true, "https://hive.hivecommons.dev")

	req := hiveIDPutRequest(t, "hive-new-name")
	w := httptest.NewRecorder()
	srv.handleHiveIDSet(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("hub-managed spoke accepted a Hive ID change with status %d, want %d (409 Conflict)", w.Code, http.StatusConflict)
	}
	if srv.deps.Config.HiveID != "hive-bold-hawk" {
		t.Errorf("Hive ID was mutated to %q on a hub-managed spoke; the hive is now orphaned from the hub", srv.deps.Config.HiveID)
	}
	if body := w.Body.String(); !strings.Contains(body, "hub") {
		t.Errorf("refusal does not tell the operator the hub owns this value: %s", body)
	}
}

// TestHiveIDSetAllowedOnSelfHostedHive is the counterweight. Without it the
// test above could be satisfied by refusing the edit unconditionally, which
// would break every self-hosted hive.
func TestHiveIDSetAllowedOnSelfHostedHive(t *testing.T) {
	srv := hiveIDTestServer(t, false, "")

	req := hiveIDPutRequest(t, "hive-new-name")
	w := httptest.NewRecorder()
	srv.handleHiveIDSet(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("self-hosted hive was refused a Hive ID change: status %d, body %s", w.Code, w.Body.String())
	}
	if srv.deps.Config.HiveID != "hive-new-name" {
		t.Errorf("Hive ID = %q, want the newly set value", srv.deps.Config.HiveID)
	}
}

// TestHiveIDLockRequiresBothHubEnabledAndURL pins the exact detection rule.
// Hub.Enabled with no URL is a half-configured hive that cannot reach any hub,
// so nothing owns its ID and it must stay editable.
func TestHiveIDLockRequiresBothHubEnabledAndURL(t *testing.T) {
	cases := []struct {
		name       string
		enabled    bool
		url        string
		wantLocked bool
	}{
		{"hub managed", true, "https://hive.hivecommons.dev", true},
		{"hub disabled with url", false, "https://hive.hivecommons.dev", false},
		{"hub enabled without url", true, "", false},
		{"hub enabled with blank url", true, "   ", false},
		{"self hosted", false, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Hub.Enabled = tc.enabled
			cfg.Hub.URL = tc.url
			locked, reason := hiveIDLockedByHub(cfg)
			if locked != tc.wantLocked {
				t.Errorf("hiveIDLockedByHub() locked = %v, want %v", locked, tc.wantLocked)
			}
			if locked && reason == "" {
				t.Error("locked with no reason — the operator would see an unexplained refusal")
			}
			if !locked && reason != "" {
				t.Errorf("unlocked but carries reason %q", reason)
			}
		})
	}
}

// TestHiveIDLockNilConfigDoesNotPanic — the handlers run before config is
// loaded in some startup paths.
func TestHiveIDLockNilConfigDoesNotPanic(t *testing.T) {
	if locked, _ := hiveIDLockedByHub(nil); locked {
		t.Error("nil config reported as hub-managed")
	}
}

// TestHiveIDGetReportsEditability pins the field the UI renders from. Without
// it the server could refuse the write while the dashboard still offers the
// control, which is the confusing half-fix.
func TestHiveIDGetReportsEditability(t *testing.T) {
	for _, tc := range []struct {
		name         string
		hubEnabled   bool
		wantEditable bool
	}{
		{"hub managed", true, false},
		{"self hosted", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := hiveIDTestServer(t, tc.hubEnabled, "https://hive.hivecommons.dev")
			if !tc.hubEnabled {
				srv.deps.Config.Hub.URL = ""
			}
			w := httptest.NewRecorder()
			srv.handleHiveIDGet(w, httptest.NewRequest(http.MethodGet, "/api/hive-id", nil))

			var got struct {
				ID         string `json:"id"`
				Editable   bool   `json:"editable"`
				LockReason string `json:"lockReason"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v (body %s)", err, w.Body.String())
			}
			if got.Editable != tc.wantEditable {
				t.Errorf("editable = %v, want %v", got.Editable, tc.wantEditable)
			}
			if !got.Editable && got.LockReason == "" {
				t.Error("not editable but no lockReason for the UI to display")
			}
		})
	}
}

// --- ID format (#7247) ---

// TestHiveIDRejectsNonInfrastructureSafeValues pins the tightened validator.
// The old one was displayNamePattern, which allows spaces and uppercase — but
// the Hive ID lands in Kubernetes namespaces and hosted spoke subdomains,
// where neither is legal, and it contradicted the UI's own documented
// "hive-adjective-noun" hint.
func TestHiveIDRejectsNonInfrastructureSafeValues(t *testing.T) {
	rejected := []struct {
		name string
		id   string
	}{
		{"space", "hive bold hawk"},
		{"uppercase", "Hive-Bold-Hawk"},
		{"underscore", "hive_bold_hawk"},
		{"leading hyphen", "-hive-bold"},
		{"trailing hyphen", "hive-bold-"},
		{"dot", "hive.bold.hawk"},
		{"slash", "hive/bold"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			srv := hiveIDTestServer(t, false, "")
			req := hiveIDPutRequest(t, tc.id)
			w := httptest.NewRecorder()
			srv.handleHiveIDSet(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("accepted %q as a Hive ID (status %d); it is not usable as a namespace or subdomain label", tc.id, w.Code)
			}
		})
	}

	accepted := []string{"hive-bold-hawk", "hive123", "h", "hive-1"}
	for _, id := range accepted {
		t.Run("accept-"+id, func(t *testing.T) {
			srv := hiveIDTestServer(t, false, "")
			req := hiveIDPutRequest(t, id)
			w := httptest.NewRecorder()
			srv.handleHiveIDSet(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("rejected legitimate Hive ID %q: status %d, body %s", id, w.Code, w.Body.String())
			}
		})
	}
}

// --- UI ---

// TestHiveIDUIRespectsServerEditability pins that the dashboard renders from
// the server's flag rather than deciding for itself.
func TestHiveIDUIRespectsServerEditability(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"const hiveIdEditable = (window._lastStatus || {}).hiveIdEditable !== false;",
		"const hiveIdLockReason = (window._lastStatus || {}).hiveIdLockReason || '';",
		"${hiveIdEditable ? '' : 'readonly disabled'}",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — the Hive ID field no longer honours the hub lock (#7247)", snippet)
		}
	}
}

// TestHiveIDUIFailsSafeOnOlderSpokes pins the `!== false` comparison
// specifically. A truthy test (`.hiveIdEditable`) would read a MISSING field as
// "not editable"; `!== false` reads missing as editable. The distinction
// matters for an older spoke whose status payload predates the flag — and the
// chosen direction must stay deliberate rather than being "simplified" later.
func TestHiveIDUIFailsSafeOnOlderSpokes(t *testing.T) {
	html := indexHTML(t)
	if strings.Contains(html, "if ((window._lastStatus || {}).hiveIdEditable) {") {
		t.Error("Hive ID editability is read as a truthy test; a status payload without the field would silently lock the control on every self-hosted hive (#7247)")
	}
	if !strings.Contains(html, "(window._lastStatus || {}).hiveIdEditable === false") {
		t.Error("saveHiveId no longer guards on an explicit === false; a stale DOM could fire a request that only fails as a generic error (#7247)")
	}
}

// TestStatusPayloadCarriesHiveIDEditability is the end-to-end pin: the flag the
// UI reads must actually be emitted on the wire. Without this, every test above
// can pass while the dashboard receives nothing and falls back to its default.
func TestStatusPayloadCarriesHiveIDEditability(t *testing.T) {
	raw, err := json.Marshal(&StatusPayload{HiveID: "hive-bold-hawk", HiveIDEditable: false, HiveIDLockReason: hiveIDHubLockReason})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["hiveIdEditable"]; !ok {
		t.Error("status payload does not emit hiveIdEditable; the UI flag would always be absent (#7247)")
	}
	if got["hiveIdLockReason"] != hiveIDHubLockReason {
		t.Errorf("hiveIdLockReason = %v, want the shared reason string", got["hiveIdLockReason"])
	}

	// hiveIdEditable must NOT be omitempty: false is the meaningful value, and
	// omitempty would drop it exactly when the lock is in force -- which the
	// UI's `!== false` default would then read as editable.
	if !strings.Contains(string(raw), `"hiveIdEditable":false`) {
		t.Error("hiveIdEditable:false was omitted from the payload; the lock would never reach the browser (#7247)")
	}
}

// TestBuildFrontendStatusWiresHiveIDLock closes the gap that mutation testing
// exposed: every other test here passes while BuildFrontendStatus hardcodes
// HiveIDEditable, because none of them actually run the builder. The status
// payload is the ONLY channel by which the lock reaches the dashboard, so if
// this wiring is wrong the UI silently offers an edit that always 409s.
func TestBuildFrontendStatusWiresHiveIDLock(t *testing.T) {
	newCfg := func(hubEnabled bool, hubURL string) *config.Config {
		cfg := &config.Config{
			Project:  config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}},
			Agents:   map[string]config.AgentConfig{"scanner": {Backend: "claude", Model: "sonnet"}},
			Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{"idle": {Cadences: map[string]config.Cadence{"scanner": "15m"}}}},
			HiveID:   "hive-bold-hawk",
		}
		cfg.Hub.Enabled = hubEnabled
		cfg.Hub.URL = hubURL
		return cfg
	}

	for _, tc := range []struct {
		name         string
		cfg          *config.Config
		wantEditable bool
	}{
		{"hub managed", newCfg(true, "https://hive.hivecommons.dev"), false},
		{"self hosted", newCfg(false, ""), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gov := governor.New(tc.cfg.Governor, tc.cfg.Agents, nil)
			payload := BuildFrontendStatus(gov.GetState(), nil, nil, tc.cfg, nil, gov, nil, nil, nil, nil)
			if payload == nil {
				t.Fatal("nil payload")
			}
			if payload.HiveIDEditable != tc.wantEditable {
				t.Errorf("HiveIDEditable = %v, want %v — the dashboard would render the wrong control (#7247)",
					payload.HiveIDEditable, tc.wantEditable)
			}
			if !tc.wantEditable && payload.HiveIDLockReason == "" {
				t.Error("locked but no HiveIDLockReason reached the browser")
			}
			if tc.wantEditable && payload.HiveIDLockReason != "" {
				t.Errorf("editable but carries lock reason %q", payload.HiveIDLockReason)
			}
		})
	}
}
