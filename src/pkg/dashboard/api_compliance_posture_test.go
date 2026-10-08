package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/compliance"
	"github.com/hivecommons/hive/pkg/config"
)

// isolatePosture points the posture history and config scan at a temp dir
// so tests never touch the host's /data.
func isolatePosture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origPath, origFiles := compliancePostureHistoryPath, compliancePostureConfigFiles
	t.Cleanup(func() {
		compliancePostureHistoryPath, compliancePostureConfigFiles = origPath, origFiles
	})
	compliancePostureHistoryPath = filepath.Join(dir, "compliance-posture.jsonl")
	compliancePostureConfigFiles = func(*config.Config) []string { return nil }
	return dir
}

func compliancePostureRequest(t *testing.T, s *Server, method, target, role string, verified bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	rr := httptest.NewRecorder()
	if method == http.MethodPost {
		s.handleCompliancePostureRun(rr, req)
	} else {
		s.handleCompliancePosture(rr, req)
	}
	return rr
}

func decodePosture(t *testing.T, rr *httptest.ResponseRecorder) compliancePostureResponse {
	t.Helper()
	var resp compliancePostureResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	return resp
}

func TestHandleCompliancePostureAuth(t *testing.T) {
	isolatePosture(t)
	cases := []struct {
		name     string
		method   string
		role     string
		verified bool
		want     int
	}{
		{name: "get anonymous", method: http.MethodGet, want: http.StatusForbidden},
		{name: "get read", method: http.MethodGet, role: config.RoleRead, want: http.StatusForbidden},
		{name: "get read-write", method: http.MethodGet, role: config.RoleReadWrite, want: http.StatusForbidden},
		{name: "get unverified owner", method: http.MethodGet, role: config.RoleOwner, want: http.StatusForbidden},
		{name: "get verified owner", method: http.MethodGet, role: config.RoleOwner, verified: true, want: http.StatusOK},
		{name: "run anonymous", method: http.MethodPost, want: http.StatusForbidden},
		{name: "run read-write", method: http.MethodPost, role: config.RoleReadWrite, want: http.StatusForbidden},
		{name: "run unverified owner", method: http.MethodPost, role: config.RoleOwner, want: http.StatusForbidden},
		{name: "run verified owner", method: http.MethodPost, role: config.RoleOwner, verified: true, want: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := compliancePostureRequest(t, complianceTestServer(), tc.method, "/api/compliance/posture", tc.role, tc.verified)
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestHandleCompliancePostureNoConfig(t *testing.T) {
	isolatePosture(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rr := compliancePostureRequest(t, newTestServer(), method, "/api/compliance/posture", config.RoleOwner, true)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d, want 503", method, rr.Code)
		}
	}
}

func TestHandleCompliancePostureBodyAndHistory(t *testing.T) {
	isolatePosture(t)
	s := complianceTestServer()

	rr := compliancePostureRequest(t, s, http.MethodGet, "/api/compliance/posture", config.RoleOwner, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rr.Code, rr.Body.String())
	}
	resp := decodePosture(t, rr)
	if resp.Disclaimer != compliance.Disclaimer || !resp.Enabled {
		t.Fatalf("disclaimer/enabled = %q/%v", resp.Disclaimer, resp.Enabled)
	}
	if resp.Interval != config.DefaultCompliancePostureInterval.String() || resp.WindowDays != 30 || resp.HistoryDays != 365 {
		t.Fatalf("interval/window/history = %s/%d/%d", resp.Interval, resp.WindowDays, resp.HistoryDays)
	}
	if len(resp.Checks) != len(compliance.PostureChecks()) {
		t.Fatalf("checks = %d, want %d", len(resp.Checks), len(compliance.PostureChecks()))
	}
	if resp.Latest != nil || resp.History != nil {
		t.Fatalf("expected no runs yet, got latest=%v history=%v", resp.Latest, resp.History)
	}

	run := compliancePostureRequest(t, s, http.MethodPost, "/api/compliance/posture/run", config.RoleOwner, true)
	if run.Code != http.StatusOK {
		t.Fatalf("run status = %d (body=%s)", run.Code, run.Body.String())
	}
	var got compliance.PostureRun
	if err := json.Unmarshal(run.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if got.Trigger != compliance.TriggerManual || len(got.Results) != len(compliance.PostureChecks()) {
		t.Fatalf("run = trigger %q, %d results", got.Trigger, len(got.Results))
	}
	if got.Summary.Pass+got.Summary.Fail+got.Summary.Skip+got.Summary.Error != len(got.Results) {
		t.Fatalf("summary %+v does not add up to %d", got.Summary, len(got.Results))
	}
	if auditCount(s, "compliance_posture_run") == 0 {
		t.Fatal("manual run was not audited")
	}

	rr = compliancePostureRequest(t, s, http.MethodGet, "/api/compliance/posture?since=24h", config.RoleOwner, true)
	resp = decodePosture(t, rr)
	if resp.Latest == nil || !resp.Latest.At.Equal(got.At) {
		t.Fatalf("latest = %v, want run at %s", resp.Latest, got.At)
	}
	if len(resp.History) != 1 || resp.Truncated {
		t.Fatalf("history = %d runs (truncated=%v), want 1", len(resp.History), resp.Truncated)
	}

	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rr = compliancePostureRequest(t, s, http.MethodGet, "/api/compliance/posture?since="+future, config.RoleOwner, true)
	resp = decodePosture(t, rr)
	if resp.History == nil || len(resp.History) != 0 {
		t.Fatalf("future since: history = %v, want empty slice", resp.History)
	}

	rr = compliancePostureRequest(t, s, http.MethodGet, "/api/compliance/posture?since=yesterday", config.RoleOwner, true)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad since status = %d, want 400", rr.Code)
	}
}

func TestHandleCompliancePostureHistorySurvivesRestart(t *testing.T) {
	isolatePosture(t)
	s := complianceTestServer()
	if rr := compliancePostureRequest(t, s, http.MethodPost, "/api/compliance/posture/run", config.RoleOwner, true); rr.Code != http.StatusOK {
		t.Fatalf("run status = %d", rr.Code)
	}
	restarted := complianceTestServer()
	resp := decodePosture(t, compliancePostureRequest(t, restarted, http.MethodGet, "/api/compliance/posture", config.RoleOwner, true))
	if resp.Latest == nil {
		t.Fatal("latest run lost across restart")
	}
}

func TestHandleCompliancePostureRunConflicts(t *testing.T) {
	isolatePosture(t)
	s := complianceTestServer()
	s.deps.Config.Compliance.Frameworks = nil
	rr := compliancePostureRequest(t, s, http.MethodPost, "/api/compliance/posture/run", config.RoleOwner, true)
	if rr.Code != http.StatusConflict {
		t.Fatalf("no frameworks: status = %d, want 409", rr.Code)
	}
	resp := decodePosture(t, compliancePostureRequest(t, s, http.MethodGet, "/api/compliance/posture", config.RoleOwner, true))
	if resp.Enabled {
		t.Fatal("enabled = true with no frameworks")
	}

	s = complianceTestServer()
	block := make(chan struct{})
	entered := make(chan struct{})
	r := s.postureRunner()
	slow := compliance.PostureCheck{ID: "slow", ControlIDs: []string{"X"}, Title: "slow", Run: func(context.Context, compliance.PostureDeps) compliance.Result {
		close(entered)
		<-block
		return compliance.Result{Status: compliance.PosturePass}
	}}
	s.posture = compliance.NewPostureRunner([]compliance.PostureCheck{slow}, r.History(), s.postureDeps)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.posture.Run(context.Background(), compliance.TriggerSchedule)
	}()
	<-entered
	rr = compliancePostureRequest(t, s, http.MethodPost, "/api/compliance/posture/run", config.RoleOwner, true)
	close(block)
	<-done
	if rr.Code != http.StatusConflict {
		t.Fatalf("overlapping run: status = %d, want 409", rr.Code)
	}
}

func TestCompliancePostureTransitionAudited(t *testing.T) {
	isolatePosture(t)
	s := complianceTestServer()
	s.deps.Config.Dashboard.HubProxied = true
	r := s.postureRunner()
	before := auditCount(s, "compliance_posture_failed")
	if _, err := r.Run(context.Background(), compliance.TriggerSchedule); err != nil {
		t.Fatal(err)
	}
	if auditCount(s, "compliance_posture_failed") != before {
		t.Fatal("transition audited before any failure")
	}
	s.deps.Config.Dashboard.HubProxied = false
	s.deps.Config.Dashboard.AuthorizedUsers = nil
	if _, err := r.Run(context.Background(), compliance.TriggerSchedule); err != nil {
		t.Fatal(err)
	}
	if auditCount(s, "compliance_posture_failed") != before+1 {
		t.Fatal("pass→fail transition was not audited")
	}
}

func TestPostureScheduleAndDeps(t *testing.T) {
	isolatePosture(t)
	s := newTestServer()
	if d, ok := s.postureSchedule(); ok || d != config.DefaultCompliancePostureInterval {
		t.Fatalf("no deps: schedule = %s/%v", d, ok)
	}
	if d := s.postureDeps(); d.Config == nil || d.GitHub != nil {
		t.Fatalf("no deps: postureDeps = %+v", d)
	}
	s = complianceTestServer()
	s.deps.Config.Compliance.PostureChecks.Interval = 15 * time.Minute
	s.deps.Config.HiveID = "h1"
	s.deps.Config.Project.Repos = []string{"o/r"}
	if d, ok := s.postureSchedule(); !ok || d != 15*time.Minute {
		t.Fatalf("schedule = %s/%v, want 15m/true", d, ok)
	}
	d := s.postureDeps()
	if len(d.Repos) != 1 || d.Repos[0] != "o/r" || d.HoldLabel == "" || d.GitHub != nil {
		t.Fatalf("postureDeps = %+v", d)
	}
}

func TestStartCompliancePostureStops(t *testing.T) {
	isolatePosture(t)
	s := complianceTestServer()
	ctx, cancel := context.WithCancel(context.Background())
	s.StartCompliancePosture(ctx)
	cancel()
}

func TestParsePostureSince(t *testing.T) {
	now := time.Date(2025, 6, 10, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Time
		err  bool
	}{
		{in: "2025-06-01T00:00:00Z", want: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)},
		{in: "2025-06-01", want: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)},
		{in: "48h", want: now.Add(-48 * time.Hour)},
		{in: "-1h", err: true},
		{in: "soon", err: true},
	}
	for _, tc := range cases {
		got, err := parsePostureSince(tc.in, now)
		if (err != nil) != tc.err || (!tc.err && !got.Equal(tc.want)) {
			t.Fatalf("parsePostureSince(%q) = %s, %v", tc.in, got, err)
		}
	}
}

func auditCount(s *Server, action string) int {
	n := 0
	for _, e := range s.GetAudit().Recent(0) {
		if e.Action == action {
			n++
		}
	}
	return n
}
