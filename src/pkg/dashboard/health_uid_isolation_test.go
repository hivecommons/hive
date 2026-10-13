package dashboard

// Operator advisory for a hive booted on the shared dev UID (#11713): the
// uid_isolation check must fail — with the remedy and runbook link — in both
// HealthSummary (what the hub alerts on) and /api/health/deep when isolation
// was expected but every agent shares the dev UID, and must be omitted when
// the deployment never expected isolation.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func stubUIDIsolation(t *testing.T, expected, shared bool) {
	t.Helper()
	uidIsolationExpectedFn = func() bool { return expected }
	uidIsolationSharedFn = func() bool { return shared }
	t.Cleanup(func() {
		uidIsolationExpectedFn = func() bool { return false }
		uidIsolationSharedFn = nil
	})
}

// clearUIDIsolationEnv makes uidIsolationExpected hermetic: no ambient HIVE_*
// env, no Kubernetes or Podman signal from the host.
func clearUIDIsolationEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HIVE_UID_ISOLATION_REVISION", "")
	t.Setenv("AGENT_NAMES", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("container", "")
	t.Cleanup(config.SetSATokenFileForTest(filepath.Join(t.TempDir(), "no-sa-token")))
}

func TestUIDIsolationExpected(t *testing.T) {
	cases := []struct {
		name     string
		revision string
		agents   string
		k8sHost  string
		ctr      string
		want     bool
	}{
		{"kubernetes with roster", "2", "scanner\nfixer", "10.0.0.1", "", true},
		{"podman with roster", "2", "scanner", "", "podman", true},
		{"no revision", "", "scanner", "10.0.0.1", "", false},
		{"no agents", "2", "  \n", "10.0.0.1", "", false},
		{"plain docker", "2", "scanner", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearUIDIsolationEnv(t)
			t.Setenv("HIVE_UID_ISOLATION_REVISION", tc.revision)
			t.Setenv("AGENT_NAMES", tc.agents)
			t.Setenv("KUBERNETES_SERVICE_HOST", tc.k8sHost)
			t.Setenv("container", tc.ctr)
			if got := uidIsolationExpected(); got != tc.want {
				t.Fatalf("uidIsolationExpected() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUIDIsolationHealth(t *testing.T) {
	st, detail := uidIsolationHealth(false)
	if st != "pass" {
		t.Fatalf("isolated: status = %q (%s), want pass", st, detail)
	}
	st, detail = uidIsolationHealth(true)
	if st != "fail" {
		t.Fatalf("shared: status = %q, want fail", st)
	}
	for _, want := range []string{
		"dev UID",
		"#11706",
		"/data/.hive/uid-map.json",
		"troubleshooting.md#hive_agent_token_cache-unset-no-read-credential-available",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("shared detail = %q, want it to mention %q", detail, want)
		}
	}
}

func TestHealthSummary_UIDIsolationSharedFails(t *testing.T) {
	stubUIDIsolation(t, true, true)
	deps := testDeps(t)
	s := NewServer(0, deps.Logger)
	s.RegisterAPI(deps)
	status, detail := healthCheckOf(t, s, healthCheckUIDIsolation)
	if status != "fail" || !strings.Contains(detail, uidIsolationTroubleshootingURL) {
		t.Fatalf("uid_isolation = %q %q, want fail linking the runbook", status, detail)
	}
	summary := s.HealthSummary()
	if f, _ := summary["fails"].(int); f < 1 {
		t.Fatalf("fails = %v, want ≥1 on the shared dev UID", summary["fails"])
	}
	if overall, _ := summary["status"].(string); overall == "ok" || overall == "warning" {
		t.Fatalf("overall = %q, want degraded or critical on the shared dev UID", overall)
	}
}

func TestHealthSummary_UIDIsolationPassWhenIsolated(t *testing.T) {
	stubUIDIsolation(t, true, false)
	deps := testDeps(t)
	s := NewServer(0, deps.Logger)
	s.RegisterAPI(deps)
	if status, detail := healthCheckOf(t, s, healthCheckUIDIsolation); status != "pass" {
		t.Fatalf("uid_isolation = %q %q, want pass", status, detail)
	}
}

func TestHealthSummary_UIDIsolationOmittedWhenNotExpected(t *testing.T) {
	stubUIDIsolation(t, false, true)
	deps := testDeps(t)
	s := NewServer(0, deps.Logger)
	s.RegisterAPI(deps)
	for _, c := range healthChecksOf(t, s) {
		if c.Name == healthCheckUIDIsolation {
			t.Fatalf("uid_isolation reported %q when isolation was not expected; want omitted", c.Status)
		}
	}
}

func TestHandleHealthDeep_UIDIsolationCheck(t *testing.T) {
	stubUIDIsolation(t, true, true)
	srv := newFullServer(t)
	srv.MarkReady()
	srv.UpdateStatus(minimalPayload())

	w := httptest.NewRecorder()
	srv.handleHealthDeep(w, httptest.NewRequest(http.MethodGet, "/api/health/deep", nil))
	var result struct {
		Status string `json:"status"`
		Checks map[string]struct {
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	c, ok := result.Checks[healthCheckUIDIsolation]
	if !ok {
		t.Fatalf("no uid_isolation check in /api/health/deep: %+v", result.Checks)
	}
	if c.Status != "fail" || !strings.Contains(c.Detail, uidIsolationTroubleshootingURL) {
		t.Fatalf("uid_isolation = %+v, want fail linking the runbook", c)
	}
	if result.Status == "ok" {
		t.Fatalf("overall = %q, want degraded on the shared dev UID", result.Status)
	}
}
