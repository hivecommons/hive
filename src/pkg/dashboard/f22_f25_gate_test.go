package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Audit F22/F25 (2026-08-14). Two dashboard writers performed privileged
// mutations with NO role gate:
//
//   - PUT /api/config/governor/features (F22, Medium) — the body carries
//     otelEndpoint, otelHeaders, otelInsecure and tracingEndpoint, which feed a
//     live OTLP exporter. Un-gated, any read-write or merger member could point
//     the hive's whole trace stream at an attacker-controlled collector (spans
//     carry hive.agent, issue refs, model names, token counts) and strip TLS
//     off it. Eleven of the twelve governor-config writers already gated; this
//     was the lone outlier — the same asymmetry that established F16.
//   - DELETE /api/hives/{id} (F25, Low) — removed a peer from the federation
//     registry with no gate of any kind.
//
// As with F14/F16 these assert the INVARIANT at the source level as well as
// behaviourally, because the failure mode for this class of bug in this repo is
// a sync merge silently dropping a gate, not a logic bug. F14 regressed exactly
// that way and a behavioural test on one handler did not catch it.

// --- BEHAVIOURAL: REJECTED in one direction, ACCEPTED in the other -----------

// putFeatures issues PUT /api/config/governor/features with caller-controlled
// headers, so a test can pick the role rather than inheriting doPut's owner
// headers.
func putFeatures(s *Server, body map[string]any, mark func(*http.Request)) *httptest.ResponseRecorder {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(body)
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/features", &b)
	req.Header.Set("Content-Type", "application/json")
	if mark != nil {
		mark(req)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// TestF22GovernorFeaturesRejectsNonOwner is the negative half: every non-owner
// shape must be refused. Each subtest is a caller the audit named as able to
// reach the OTel exporter config before the fix.
func TestF22GovernorFeaturesRejectsNonOwner(t *testing.T) {
	body := map[string]any{
		"otelEnabled":  true,
		"otelEndpoint": "https://attacker.example.com/v1/traces",
		"otelInsecure": true,
		"otelHeaders":  map[string]string{"authorization": "stolen"},
	}

	cases := []struct {
		name string
		mark func(*http.Request)
	}{
		{"no role headers at all", nil},
		{"read-write session", func(r *http.Request) { r.Header.Set("X-Hive-Role", "read-write") }},
		{"merger session", func(r *http.Request) { r.Header.Set("X-Hive-Role", "merger") }},
		// The spoofable half: a client-supplied owner role WITHOUT the
		// server-only verification marker must not pass. A test that sent only
		// X-Hive-Role: owner and saw 200 would be passing for the wrong reason.
		{"owner role header without the verified marker", func(r *http.Request) {
			r.Header.Set("X-Hive-Role", "owner")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := covApiServer(t)
			rec := putFeatures(s, body, tc.mark)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("PUT governor/features as %s got %d, want 403 — "+
					"this caller can redirect the OTel trace stream (audit F22)", tc.name, rec.Code)
			}
			// The gate must refuse BEFORE mutating config, not after.
			if got := s.deps.Config.EffectiveOTel(); got.Endpoint == "https://attacker.example.com/v1/traces" {
				t.Fatalf("refused with 403 but the endpoint was still written: %+v", got)
			}
		})
	}
}

// TestF22GovernorFeaturesAcceptsOwner is the positive half. Without it, a gate
// that rejected EVERYTHING would satisfy the test above while breaking the
// Governor config dialog outright.
func TestF22GovernorFeaturesAcceptsOwner(t *testing.T) {
	s := covApiServer(t)
	rec := putFeatures(s, map[string]any{
		"otelEnabled":     true,
		"otelEndpoint":    "https://otel-collector.example.com:4318",
		"otelServiceName": "hive-ui",
	}, markOwnerRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner PUT governor/features got %d, want 200 — the gate is over-broad "+
			"and has broken the legitimate Governor config workflow; body=%s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.EffectiveOTel(); !got.Enabled || got.Endpoint != "https://otel-collector.example.com:4318" {
		t.Fatalf("owner update did not take effect: %+v", got)
	}
}

// TestF25HivesDeleteRejectsNonOwnerAcceptsOwner covers both directions for the
// federation-registry delete.
func TestF25HivesDeleteRejectsNonOwner(t *testing.T) {
	for _, tc := range []struct {
		name string
		mark func(*http.Request)
	}{
		{"no role headers at all", nil},
		{"read-write session", func(r *http.Request) { r.Header.Set("X-Hive-Role", "read-write") }},
		{"owner role header without the verified marker", func(r *http.Request) {
			r.Header.Set("X-Hive-Role", "owner")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupContributeEnv(t)
			s := newHivesTestServer(t)

			req := httptest.NewRequest(http.MethodDelete, "/api/hives/hive-f25-org-f25", nil)
			if tc.mark != nil {
				tc.mark(req)
			}
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)

			// 403, NOT 404: the gate must run before the registry lookup, so an
			// un-gated caller cannot even probe which hives exist.
			if rec.Code != http.StatusForbidden {
				t.Fatalf("DELETE /api/hives/{id} as %s got %d, want 403 (audit F25)", tc.name, rec.Code)
			}
		})
	}
}

// TestF25HivesDeleteAcceptsOwner is the positive control: the registered hive
// must actually be removable by an owner.
func TestF25HivesDeleteAcceptsOwner(t *testing.T) {
	setupContributeEnv(t)
	s := newHivesTestServer(t)

	body := `{"project_name":"f25","org":"f25-org","hub_url":"wss://x:3001/c"}`
	reg := httptest.NewRequest(http.MethodPost, "/api/hives/register", bytes.NewBufferString(body))
	reg.Header.Set("Content-Type", "application/json")
	regRec := httptest.NewRecorder()
	s.mux.ServeHTTP(regRec, reg)
	if regRec.Code != http.StatusOK {
		t.Fatalf("register got %d, want 200: %s", regRec.Code, regRec.Body.String())
	}

	del := httptest.NewRequest(http.MethodDelete, "/api/hives/hive-f25-org-f25", nil)
	markOwnerRequest(del)
	delRec := httptest.NewRecorder()
	s.mux.ServeHTTP(delRec, del)
	if delRec.Code != http.StatusOK {
		t.Fatalf("owner DELETE got %d, want 200 — the gate is over-broad and owners can no "+
			"longer manage the federation registry; body=%s", delRec.Code, delRec.Body.String())
	}
}

func newHivesTestServer(t *testing.T) *Server {
	t.Helper()
	s := NewServer(0, testLogger())
	s.registerContributeRoutes()
	return s
}

// --- SOURCE-ASSERTING INVARIANT ----------------------------------------------

// ownerGatedF22F25Handlers maps each handler this fix gated to its file.
var ownerGatedF22F25Handlers = map[string]string{
	"handleGovernorFeatures": "api_governor_features.go",
	"handleHivesDelete":      "api_contribute.go",
}

// TestF22F25HandlersAreOwnerGated is the source-level regression, mirroring
// TestF16PrivilegedHandlersAreOwnerGated. This is the test that survives a sync
// merge reverting the gate while the behavioural tests are also reverted.
func TestF22F25HandlersAreOwnerGated(t *testing.T) {
	for name, file := range ownerGatedF22F25Handlers {
		body := f16HandlerBody(t, f16ReadSource(t, file), name)
		if !strings.Contains(body, "requireOwnerRole(w, r)") {
			t.Errorf("%s (%s) has no requireOwnerRole gate — a non-owner write-tier member can "+
				"reach a privileged mutation (audit F22/F25). If a merge dropped this, restore "+
				"the gate rather than relaxing the test.", name, file)
		}
		// Weaker gates admit RoleReadWrite (or better) and are NOT sufficient.
		for _, weak := range []string{
			"s.requireContributorWrite(w, r)",
			"requireMergerOrOwnerRole(w, r)",
		} {
			if strings.Contains(body, weak) {
				t.Errorf("%s (%s) gates on %s, which admits a non-owner role; this surface is owner-only",
					name, file, weak)
			}
		}
	}
}

// governorConfigWriters is the full census of governor-config write handlers
// from audit F22: twelve writers, of which eleven were gated and `features` was
// the outlier. After this fix it must be 12 of 12. The count floor below is
// what catches a NEW un-gated writer being added alongside them later.
var governorConfigWriters = map[string]string{
	"handleGovernorSensing":       "api.go",
	"handleGovernorThresholds":    "api.go",
	"handleGovernorLabels":        "api.go",
	"handleGovernorBudget":        "api.go",
	"handleGovernorNotifications": "api.go",
	"handleGovernorHealth":        "api.go",
	"handleGovernorLogging":       "api.go",
	"handleGovernorAttribution":   "api.go",
	"handleGovernorHub":           "api.go",
	"handleGovernorTrajectory":    "api_trajectory.go",
	"handleGovernorSecurity":      "api_governor_security.go",
	"handleGovernorFeatures":      "api_governor_features.go",
}

// TestF22AllGovernorConfigWritersAreOwnerGated asserts the property the audit
// actually reasoned from: there is no un-gated outlier among the governor
// config writers. F22 existed because exactly one of twelve was missing a gate
// and nothing asserted the set was uniform.
func TestF22AllGovernorConfigWritersAreOwnerGated(t *testing.T) {
	const wantWriters = 12
	if got := len(governorConfigWriters); got != wantWriters {
		t.Errorf("census lists %d governor config writers, want %d — a writer was added or "+
			"removed; update this table deliberately and confirm the new one is gated", got, wantWriters)
	}

	srcCache := map[string]string{}
	gated := 0
	for name, file := range governorConfigWriters {
		src, ok := srcCache[file]
		if !ok {
			src = f16ReadSource(t, file)
			srcCache[file] = src
		}
		if strings.Contains(f16HandlerBody(t, src, name), "requireOwnerRole(w, r)") {
			gated++
			continue
		}
		t.Errorf("governor config writer %s (%s) is not owner-gated — this is the F22 shape: "+
			"one un-gated outlier among otherwise-uniform writers", name, file)
	}
	if gated < wantWriters {
		t.Errorf("%d of %d governor config writers are owner-gated, want %d of %d (audit F22)",
			gated, wantWriters, wantWriters, wantWriters)
	}
}

// TestF22F25OwnerGateCountFloor is the blunt instrument that survives renames.
func TestF22F25OwnerGateCountFloor(t *testing.T) {
	// Minimum requireOwnerRole call sites per file after F22/F25.
	// api_governor_features.go: features (this fix).
	// api_contribute.go: the four F14 contributor-management gates + hives delete.
	// api.go: the nine governor writers that live there.
	// api_trajectory.go: trajectory.
	want := map[string]int{
		"api_governor_features.go": 1,
		"api_contribute.go":        5,
		"api.go":                   9,
		"api_trajectory.go":        1,
	}
	gate := regexp.MustCompile(`requireOwnerRole\(w, r\)`)
	for file, min := range want {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if got := len(gate.FindAllString(string(raw), -1)); got < min {
			t.Errorf("%s has %d requireOwnerRole gates, want at least %d — a gate was removed "+
				"(audit F14 regressed exactly this way, via a sync merge)", file, got, min)
		}
	}
}

// --- POSITIVE CONTROLS -------------------------------------------------------
//
// Without these, "add requireOwnerRole to every handler in these files" would
// satisfy everything above while breaking legitimate workflows.

// TestF25HivesSelfServiceRoutesStayUngated is the substantive control for F25.
// register and heartbeat are actions a peer hive performs for ITSELF as part of
// federation onboarding — they are called by other hives, not by a logged-in
// owner in a browser, so owner-gating them would break federation entirely.
// Only delete, which acts on ANOTHER hive's entry, is owner-tier.
func TestF25HivesSelfServiceRoutesStayUngated(t *testing.T) {
	src := f16ReadSource(t, "api_contribute.go")
	for _, name := range []string{
		"handleHivesRegister",
		"handleHivesHeartbeat",
		"handleHivesOnboard",
		"handleHivesList",
	} {
		if strings.Contains(f16HandlerBody(t, src, name), "requireOwnerRole(w, r)") {
			t.Errorf("%s must NOT be owner-gated — register/heartbeat/onboard are self-service "+
				"actions a peer hive performs for itself and list is a read; owner-gating them "+
				"breaks federation onboarding", name)
		}
	}
}

// TestF22ReadOnlyConfigGetStaysUngated confirms the config READ path is still
// reachable by a non-owner, so the Governor dialog can still render for
// everyone even though only an owner may submit it.
func TestF22ReadOnlyConfigGetStaysUngated(t *testing.T) {
	s := covApiServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Error("GET /api/config is owner-gated — the Governor view would blank for every " +
			"non-owner; the F22 gate belongs on the PUT only")
	}
}
