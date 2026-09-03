package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// ── #4263: off/shadow/enforce runtime rollout — settings surface, captured
// mode/generation, and fixed-commit soak telemetry ────────────────────────────

const convergence4263Token = "shared-secret-token-4263"

func convergenceRequest(s *Server, method, path, body string, owner bool) *httptest.ResponseRecorder {
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if owner {
		req.Header.Set("Authorization", "Bearer "+s.authToken)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// TestConvergenceConfig_DefaultIsOff pins the compatibility guarantee: a config
// with no convergence block reads back mode "off" with effective mode "off".
func TestConvergenceConfig_DefaultIsOff(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	s := newFullServer(t)
	s.authToken = convergence4263Token

	w := convergenceRequest(s, http.MethodGet, "/api/config/convergence", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp["mode"] != "off" || resp["effective_mode"] != "off" {
		t.Fatalf("default must be off/off, got %v", resp)
	}
	modes, _ := resp["modes"].([]interface{})
	if len(modes) != 3 {
		t.Fatalf("modes must list off/shadow/enforce, got %v", resp["modes"])
	}
}

// TestConvergenceConfig_NonOwnerRejected: both the settings surface and the
// soak telemetry are owner-only through the full middleware stack.
func TestConvergenceConfig_NonOwnerRejected(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	s := newFullServer(t)
	s.authToken = convergence4263Token

	for _, probe := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/config/convergence", ""},
		{http.MethodPut, "/api/config/convergence", `{"mode":"enforce"}`},
		{http.MethodGet, "/api/convergence/soak", ""},
	} {
		w := convergenceRequest(s, probe.method, probe.path, probe.body, false)
		if w.Code == http.StatusOK {
			t.Fatalf("%s %s without owner credentials must be rejected, got 200", probe.method, probe.path)
		}
	}
	if s.deps.Config.ConvergenceMode() != config.ConvergenceModeOff {
		t.Fatal("a rejected non-owner PUT must not change the live mode")
	}
}

// TestConvergenceConfig_InvalidModeRejectedBeforeMutation: an invalid mode is a
// 400 and neither the live config nor the persisted overlay changes — the
// previous effective mode remains active, and an unknown value can never
// silently select enforcement.
func TestConvergenceConfig_InvalidModeRejectedBeforeMutation(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	s := newFullServer(t)
	s.authToken = convergence4263Token
	s.deps.Config.Convergence.Mode = config.ConvergenceModeShadow

	for _, bad := range []string{"enforced", "on", "garbage", ""} {
		w := convergenceRequest(s, http.MethodPut, "/api/config/convergence", `{"mode":"`+bad+`"}`, true)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("mode %q = %d, want 400; body=%q", bad, w.Code, w.Body.String())
		}
	}
	if got := s.deps.Config.ConvergenceMode(); got != config.ConvergenceModeShadow {
		t.Fatalf("live mode after rejected updates = %q, want shadow (unchanged)", got)
	}
}

// TestConvergenceConfig_OwnerUpdateIsLiveAndPersisted: a valid owner PUT
// mutates the live config (the next captured pass sees it — no rebuild or
// restart) and round-trips through the GET.
func TestConvergenceConfig_OwnerUpdateIsLiveAndPersisted(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	s := newFullServer(t)
	s.authToken = convergence4263Token

	w := convergenceRequest(s, http.MethodPut, "/api/config/convergence", `{"mode":" Enforce "}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if got := s.deps.Config.ConvergenceMode(); got != config.ConvergenceModeEnforce {
		t.Fatalf("live mode = %q, want enforce", got)
	}
	// The very next captured pass judges under the new mode.
	mode, gen, _, _ := s.CaptureConvergenceMode(s.deps.Config.ConvergenceMode())
	if mode != config.ConvergenceModeEnforce || gen == 0 {
		t.Fatalf("next captured pass = (%q, %d), want enforce with a live generation", mode, gen)
	}

	var resp map[string]interface{}
	g := convergenceRequest(s, http.MethodGet, "/api/config/convergence", "", true)
	if err := json.Unmarshal(g.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp["mode"] != "enforce" || resp["effective_mode"] != "enforce" {
		t.Fatalf("GET after PUT = %v, want enforce", resp)
	}
}

// TestConvergenceModeTracker_OnePairPerPassAndTransitionOnce pins the captured
// (mode, generation) contract: the first capture is not a transition, a real
// change advances the generation and reports changed exactly once, and an
// unchanged mode keeps the same pair for every candidate in a pass.
func TestConvergenceModeTracker_OnePairPerPassAndTransitionOnce(t *testing.T) {
	s := &Server{}

	mode, gen, changed, _ := s.CaptureConvergenceMode(config.ConvergenceModeOff)
	if mode != "off" || gen != 1 || changed {
		t.Fatalf("first capture = (%q,%d,changed=%v), want (off,1,false)", mode, gen, changed)
	}
	// Same mode: same pair, no transition.
	mode, gen, changed, _ = s.CaptureConvergenceMode(config.ConvergenceModeOff)
	if mode != "off" || gen != 1 || changed {
		t.Fatalf("repeat capture = (%q,%d,changed=%v), want (off,1,false)", mode, gen, changed)
	}
	// Flip to shadow: one transition, one generation bump.
	mode, gen, changed, prev := s.CaptureConvergenceMode(config.ConvergenceModeShadow)
	if mode != "shadow" || gen != 2 || !changed || prev != "off" {
		t.Fatalf("transition = (%q,%d,changed=%v,prev=%q), want (shadow,2,true,off)", mode, gen, changed, prev)
	}
	// The next identical pass must NOT report the transition again (no spam).
	_, _, changed, _ = s.CaptureConvergenceMode(config.ConvergenceModeShadow)
	if changed {
		t.Fatal("an unchanged mode must not re-report the transition")
	}
	// Flip-flop back to off is distinguishable from never having left it.
	_, gen, _, _ = s.CaptureConvergenceMode(config.ConvergenceModeOff)
	if gen != 3 {
		t.Fatalf("generation after off→shadow→off = %d, want 3", gen)
	}
	// An invalid effective value fails safe to off, never to enforcement.
	mode, _, _, _ = s.CaptureConvergenceMode("garbage")
	if mode != "off" {
		t.Fatalf("invalid effective mode captured %q, want off", mode)
	}
}

// TestConvergenceSoak_RecordSnapshotSeedBounded pins the telemetry ring:
// commit attribution is stamped, order is newest-first, a save/load round trip
// preserves rows across restart, and retention is bounded.
func TestConvergenceSoak_RecordSnapshotSeedBounded(t *testing.T) {
	s := &Server{}
	s.RecordConvergenceSoak(ConvergenceSoakEntry{Timestamp: 1, Mode: "shadow", Generation: 1, RawIssues: 2, Admitted: 1, Blocked: 1})
	s.RecordConvergenceSoak(ConvergenceSoakEntry{Timestamp: 2, Mode: "enforce", Generation: 2, RawIssues: 2, Admitted: 1, Blocked: 1, Enforced: true, WouldDiffer: true})

	hist := s.ConvergenceSoakHistory()
	if len(hist) != 2 || hist[0].Timestamp != 2 || hist[1].Timestamp != 1 {
		t.Fatalf("snapshot must be newest-first: %+v", hist)
	}
	if hist[0].Commit == "" || hist[0].EnrolledPath != convergenceSoakEnrolledPath {
		t.Fatalf("commit/enrolled-path attribution missing: %+v", hist[0])
	}

	// Durability across restart: marshal → fresh server → seed → same rows.
	data, err := json.Marshal(hist)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var restored []ConvergenceSoakEntry
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fresh := &Server{}
	fresh.SeedConvergenceSoak(restored)
	got := fresh.ConvergenceSoakHistory()
	if len(got) != 2 || got[0].Timestamp != 2 || got[0].Mode != "enforce" || !got[0].WouldDiffer {
		t.Fatalf("restart round trip lost rows: %+v", got)
	}

	// Bounded retention: overfill keeps only the newest entries.
	over := &Server{}
	for i := 0; i < convergenceSoakMaxEntries+10; i++ {
		over.RecordConvergenceSoak(ConvergenceSoakEntry{Timestamp: int64(i)})
	}
	bounded := over.ConvergenceSoakHistory()
	if len(bounded) != convergenceSoakMaxEntries {
		t.Fatalf("retention = %d entries, want %d", len(bounded), convergenceSoakMaxEntries)
	}
	if bounded[0].Timestamp != int64(convergenceSoakMaxEntries+9) {
		t.Fatalf("newest entry lost by the cap: %+v", bounded[0])
	}
}

// TestConvergenceSoak_EndpointServesAttributedRows: the owner read path serves
// commit, mode/generation, enrolled path, and the recorded rows.
func TestConvergenceSoak_EndpointServesAttributedRows(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	s := newFullServer(t)
	s.authToken = convergence4263Token
	s.RecordConvergenceSoak(ConvergenceSoakEntry{Timestamp: 42, Mode: "shadow", Generation: 1, RawIssues: 3, Admitted: 2, Blocked: 1, WouldDiffer: true})

	w := convergenceRequest(s, http.MethodGet, "/api/convergence/soak", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET soak = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	var resp struct {
		Commit       string                 `json:"commit"`
		Mode         string                 `json:"mode"`
		EnrolledPath string                 `json:"enrolled_path"`
		Entries      []ConvergenceSoakEntry `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp.Commit == "" || resp.Mode == "" || resp.EnrolledPath != convergenceSoakEnrolledPath {
		t.Fatalf("missing commit/mode attribution: %+v", resp)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].Blocked != 1 || !resp.Entries[0].WouldDiffer {
		t.Fatalf("rows not served: %+v", resp.Entries)
	}
}
