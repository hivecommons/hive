package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSnapshotAPIMatchesStatus(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.AutoSnapshot = true

	// Seed status data
	status := &StatusPayload{
		Timestamp: "2026-01-01T00:00:00Z",
		Agents: []FrontendAgent{
			{Name: "scanner", State: "running"},
			{Name: "guide", State: "paused"},
		},
		Governor: FrontendGovernor{Mode: "busy", Issues: 5, PRs: 3},
	}
	srv.statusMu.Lock()
	srv.status = status
	srv.statusMu.Unlock()

	// Fetch /api/snapshot
	req := httptest.NewRequest("GET", "/api/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotAPI(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var snapData StatusPayload
	if err := json.Unmarshal(w.Body.Bytes(), &snapData); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if snapData.Timestamp != status.Timestamp {
		t.Errorf("timestamp mismatch: snapshot=%q status=%q", snapData.Timestamp, status.Timestamp)
	}
	if len(snapData.Agents) != len(status.Agents) {
		t.Errorf("agent count mismatch: snapshot=%d status=%d", len(snapData.Agents), len(status.Agents))
	}
	for i, a := range snapData.Agents {
		if a.Name != status.Agents[i].Name || a.State != status.Agents[i].State {
			t.Errorf("agent %d mismatch: snapshot=%s/%s status=%s/%s",
				i, a.Name, a.State, status.Agents[i].Name, status.Agents[i].State)
		}
	}
	if snapData.Governor.Mode != status.Governor.Mode {
		t.Errorf("governor mode mismatch: snapshot=%q status=%q", snapData.Governor.Mode, status.Governor.Mode)
	}
}

func TestSnapshotAPIDisabled(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.AutoSnapshot = false

	req := httptest.NewRequest("GET", "/api/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotAPI(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when snapshots disabled, got %d", w.Code)
	}
}

func TestSnapshotAPINoData(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.AutoSnapshot = true
	srv.statusMu.Lock()
	srv.status = nil
	srv.statusMu.Unlock()

	req := httptest.NewRequest("GET", "/api/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotAPI(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when no data, got %d", w.Code)
	}
}

func TestSnapshotReadOnly(t *testing.T) {
	srv := newFullServer(t)

	methods := []string{"POST", "PUT", "PATCH", "DELETE"}
	paths := []string{"/snapshot", "/api/snapshot"}

	for _, method := range methods {
		for _, path := range paths {
			req := httptest.NewRequest(method, path, nil)
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, req)

			if w.Code == http.StatusOK {
				t.Errorf("%s %s should not return 200 (got %d) — snapshot must be read-only",
					method, path, w.Code)
			}
		}
	}
}

func TestSnapshotIsPublicPath(t *testing.T) {
	tests := []struct {
		path   string
		public bool
	}{
		{"/snapshot", true},
		{"/snapshot/light", true},
		{"/api/snapshot", true},
		{"/api/status", false},
		{"/api/agents", false},
	}
	for _, tt := range tests {
		got := isPublicPath(tt.path)
		if got != tt.public {
			t.Errorf("isPublicPath(%q) = %v, want %v", tt.path, got, tt.public)
		}
	}
}

func TestSnapshotCacheHeaders(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.AutoSnapshot = true
	srv.statusMu.Lock()
	srv.status = &StatusPayload{Timestamp: "2026-01-01T00:00:00Z"}
	srv.statusMu.Unlock()

	req := httptest.NewRequest("GET", "/api/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotAPI(w, req)

	cc := w.Header().Get("Cache-Control")
	if cc == "" {
		t.Error("snapshot API should set Cache-Control header")
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected application/json, got %q", ct)
	}
}

// TestSnapshotAPIEmbedsRealGitHubAppState is the regression test for the
// false-negative "GitHub App Not Installed" / "Repo Target Misconfigured"
// banners on the read-only /snapshot page: the snapshot's baked `data`
// object must carry the SAME server-computed verdict the live dashboard
// uses (UpdateStatus, shared by /api/status and /api/snapshot), not an
// undefined/empty default that the banner JS misreads as "problem".
//
// A healthy hive — a real (non-placeholder) GitHub App with a non-zero
// installation and a well-formed org/repo target — must produce a status
// payload where githubAppInstallMissing, repoTargetMisconfigured are false
// (or simply omitted, which render()'s truthy checks treat identically)
// and repoTargetIssue is empty, so neither banner's trigger condition is
// ever true.
func TestSnapshotAPIEmbedsRealGitHubAppState(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.AutoSnapshot = true
	// A real (non-placeholder) App ID with a live installation — the
	// "fully configured, nothing to fix" case.
	srv.deps.Config.GitHub.AppID = 123456
	srv.deps.Config.GitHub.InstallationID = 987654
	srv.deps.Config.Project.Org = "testorg"
	srv.deps.Config.Project.Repos = []string{"testrepo"}
	srv.deps.Config.Project.PrimaryRepo = "testrepo"

	status := &StatusPayload{Timestamp: "2026-01-01T00:00:00Z"}
	srv.UpdateStatus(status)
	srv.statusMu.Lock()
	srv.status = status
	srv.statusMu.Unlock()

	req := httptest.NewRequest("GET", "/api/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotAPI(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var snapData StatusPayload
	if err := json.Unmarshal(w.Body.Bytes(), &snapData); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// These are exactly the fields the banner JS in static/index.html reads
	// (renderGitHubAppBanner / renderRepoTargetBanner). A healthy hive must
	// report every one of them as "no problem", matching what the live
	// dashboard would show for the same config.
	if snapData.GitHubAppInstallMissing {
		t.Error("healthy hive: snapshot reports githubAppInstallMissing=true — would falsely show 'GitHub App Not Installed'")
	}
	if snapData.RepoTargetMisconfigured {
		t.Errorf("healthy hive: snapshot reports repoTargetMisconfigured=true (issue=%q) — would falsely show 'Repo Target Misconfigured'", snapData.RepoTargetIssue)
	}
	if snapData.RepoTargetIssue != "" {
		t.Errorf("healthy hive: repoTargetIssue = %q, want empty", snapData.RepoTargetIssue)
	}
}

// TestSnapshotAPISurfacesRealGitHubAppProblem is the inverse of
// TestSnapshotAPIEmbedsRealGitHubAppState: a hive that IS genuinely
// misconfigured (a real App named, but never installed anywhere) must still
// surface that truth in the snapshot payload — the fix must not paper over
// real problems, only stop inventing fake ones.
func TestSnapshotAPISurfacesRealGitHubAppProblem(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.AutoSnapshot = true
	// A real (non-placeholder) App ID with NO installation — the actual
	// "not installed yet" state that config.GitHubConfig.ConfiguredButUninstalled
	// is designed to catch.
	srv.deps.Config.GitHub.AppID = 123456
	srv.deps.Config.GitHub.InstallationID = 0

	status := &StatusPayload{Timestamp: "2026-01-01T00:00:00Z"}
	srv.UpdateStatus(status)
	srv.statusMu.Lock()
	srv.status = status
	srv.statusMu.Unlock()

	req := httptest.NewRequest("GET", "/api/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotAPI(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var snapData StatusPayload
	if err := json.Unmarshal(w.Body.Bytes(), &snapData); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !snapData.GitHubAppInstallMissing {
		t.Error("misconfigured hive: snapshot reports githubAppInstallMissing=false — a real problem would be hidden")
	}
	if !snapData.GitHubAppRequired {
		t.Error("misconfigured hive: snapshot reports githubAppRequired=false — the install banner's gate condition would never fire")
	}
}

// TestSnapshotAPINoSecretsEmbedded guards the snapshot's existing
// secret-free discipline: only the App-installed BOOLEAN verdict, the
// resolved GitHub base URL, and the repo-target verdict/issue STRING may
// ever be embedded. Nothing that looks like key material, a token, or an
// installation secret should ever appear in the payload the snapshot bakes
// into static HTML that anyone with the link can view.
func TestSnapshotAPINoSecretsEmbedded(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.AutoSnapshot = true
	srv.deps.Config.GitHub.AppID = 123456
	srv.deps.Config.GitHub.InstallationID = 987654
	srv.deps.Config.GitHub.Token = "ghp_supersecrettoken0000000000000000"
	srv.deps.Config.GitHub.KeyFile = "/etc/hive/secrets/app-key.pem"

	status := &StatusPayload{Timestamp: "2026-01-01T00:00:00Z"}
	srv.UpdateStatus(status)
	srv.statusMu.Lock()
	srv.status = status
	srv.statusMu.Unlock()

	req := httptest.NewRequest("GET", "/api/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotAPI(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if got := srv.deps.Config.GitHub.Token; got != "" && strings.Contains(body, got) {
		t.Errorf("snapshot payload leaked the GitHub token")
	}
	if strings.Contains(body, "app-key.pem") || strings.Contains(body, "BEGIN RSA PRIVATE KEY") || strings.Contains(body, "BEGIN PRIVATE KEY") {
		t.Errorf("snapshot payload leaked App key material/path")
	}
}

// TestSnapshotBuilderEnvPassesAuthToken is the regression test for the empty
// /snapshot page: the Node snapshot builder fetches /api/status unauthenticated
// and gets 401 unless DASHBOARD_AUTH_TOKEN is supplied. When the server has an
// auth token it MUST be exposed to the builder (as the trusted X-Hive-Internal
// credential); when there is no token the env must be left unchanged so open
// spokes keep working.
func TestSnapshotBuilderEnvPassesAuthToken(t *testing.T) {
	const token = "test-internal-token-1234"

	// With a token: DASHBOARD_AUTH_TOKEN must be present with that value.
	env := snapshotBuilderEnv([]string{"PATH=/usr/bin"}, token)
	var gotToken string
	var haveTLS bool
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "DASHBOARD_AUTH_TOKEN="); ok {
			gotToken = v
		}
		if kv == "NODE_TLS_REJECT_UNAUTHORIZED=0" {
			haveTLS = true
		}
	}
	if !haveTLS {
		t.Error("snapshotBuilderEnv must always set NODE_TLS_REJECT_UNAUTHORIZED=0")
	}
	if gotToken != token {
		t.Errorf("DASHBOARD_AUTH_TOKEN = %q, want %q — builder would fetch /api/status unauthenticated and bake an empty snapshot", gotToken, token)
	}

	// Without a token: DASHBOARD_AUTH_TOKEN must NOT be added (open spokes).
	env = snapshotBuilderEnv([]string{"PATH=/usr/bin"}, "")
	for _, kv := range env {
		if strings.HasPrefix(kv, "DASHBOARD_AUTH_TOKEN=") {
			t.Errorf("snapshotBuilderEnv added %q for an empty token — must leave no-auth spokes unchanged", kv)
		}
	}
}

func TestSnapshotFrameAncestorsEndpoint(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Dashboard.SnapshotFrameAncestors = []string{"https://docs.projectbluefin.io"}

	req := httptest.NewRequest(http.MethodGet, "/api/snapshot/frame-ancestors", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotFrameAncestors(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Origins []string `json:"origins"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(body.Origins) != 1 || body.Origins[0] != "https://docs.projectbluefin.io" {
		t.Fatalf("origins = %#v", body.Origins)
	}
}
