package dashboard

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/compliance"
	"github.com/hivecommons/hive/pkg/config"
)

// isolateComplianceEvidence points posture history, attestations and the
// audit-slice export at a temp dir so tests never touch the host's /data.
func isolateComplianceEvidence(t *testing.T) string {
	t.Helper()
	dir := isolatePosture(t)
	origAttest, origAudit, origNow := complianceAttestationsPath, complianceAuditLogPath, complianceEvidenceNow
	t.Cleanup(func() {
		complianceAttestationsPath, complianceAuditLogPath, complianceEvidenceNow = origAttest, origAudit, origNow
	})
	complianceAttestationsPath = filepath.Join(dir, "compliance-attestations.jsonl")
	complianceAuditLogPath = filepath.Join(dir, "audit.jsonl")
	return dir
}

func complianceEvidenceRequest(t *testing.T, s *Server, method, target, body string, owner bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if owner {
		req.Header.Set("X-Hive-Role", config.RoleOwner)
		req.Header.Set(ownerRoleVerifiedHeader, "true")
		req.Header.Set("X-Hive-User", "alice")
	}
	rr := httptest.NewRecorder()
	path := req.URL.Path
	switch {
	case path == "/api/compliance/posture/history":
		s.handleCompliancePostureHistory(rr, req)
	case path == "/api/compliance/export":
		s.handleComplianceExport(rr, req)
	case path == "/api/compliance/attestations" && method == http.MethodPost:
		s.handleComplianceAttestationCreate(rr, req)
	default:
		s.handleComplianceAttestations(rr, req)
	}
	return rr
}

func seedPostureRuns(t *testing.T, s *Server, ats ...time.Time) {
	t.Helper()
	id := compliance.PostureCatalogue()[0].ID
	for i, at := range ats {
		st := compliance.PosturePass
		if i%2 == 1 {
			st = compliance.PostureFail
		}
		run := compliance.PostureRun{At: at, Trigger: compliance.TriggerSchedule, TriggeredBy: "hive:scheduler", Results: []compliance.Result{{CheckID: id, Title: "t", At: at, Status: st, Pass: st == compliance.PosturePass, Detail: "d"}}}
		if err := s.postureRunner().History().Append(run); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
}

func TestComplianceEvidenceAuth(t *testing.T) {
	isolateComplianceEvidence(t)
	targets := []struct {
		method, target, body string
		ok                   int
	}{
		{http.MethodGet, "/api/compliance/posture/history", "", http.StatusOK},
		{http.MethodGet, "/api/compliance/export?kind=controls", "", http.StatusOK},
		{http.MethodGet, "/api/compliance/attestations", "", http.StatusOK},
		{http.MethodPost, "/api/compliance/attestations", `{"framework":"soc2-type2","reviewed_on":"2026-01-02"}`, http.StatusCreated},
	}
	roles := []struct {
		name     string
		role     string
		verified bool
	}{
		{name: "anonymous"},
		{name: "read-write", role: config.RoleReadWrite},
		{name: "merger", role: config.RoleMerger},
		{name: "unverified owner", role: config.RoleOwner},
	}
	for _, tg := range targets {
		for _, rl := range roles {
			t.Run(tg.method+" "+tg.target+" "+rl.name, func(t *testing.T) {
				req := httptest.NewRequest(tg.method, tg.target, strings.NewReader(tg.body))
				if rl.role != "" {
					req.Header.Set("X-Hive-Role", rl.role)
				}
				rr := httptest.NewRecorder()
				s := complianceTestServer()
				switch {
				case strings.HasPrefix(tg.target, "/api/compliance/posture/history"):
					s.handleCompliancePostureHistory(rr, req)
				case strings.HasPrefix(tg.target, "/api/compliance/export"):
					s.handleComplianceExport(rr, req)
				case tg.method == http.MethodPost:
					s.handleComplianceAttestationCreate(rr, req)
				default:
					s.handleComplianceAttestations(rr, req)
				}
				if rr.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", rr.Code)
				}
			})
		}
		t.Run(tg.method+" "+tg.target+" verified owner", func(t *testing.T) {
			rr := complianceEvidenceRequest(t, complianceTestServer(), tg.method, tg.target, tg.body, true)
			if rr.Code != tg.ok {
				t.Fatalf("status = %d, want %d (body=%s)", rr.Code, tg.ok, rr.Body.String())
			}
		})
	}
}

func TestComplianceEvidenceNoConfig(t *testing.T) {
	isolateComplianceEvidence(t)
	for _, target := range []string{"/api/compliance/posture/history", "/api/compliance/export?kind=controls"} {
		rr := complianceEvidenceRequest(t, newTestServer(), http.MethodGet, target, "", true)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d, want 503", target, rr.Code)
		}
	}
}

func TestParseComplianceUntil(t *testing.T) {
	day := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Time
		err  bool
	}{
		{in: "2026-10-08T05:00:00Z", want: day.Add(5 * time.Hour)},
		{in: " 2026-10-08 ", want: day.Add(24 * time.Hour)},
		{in: "168h", err: true},
		{in: "soon", err: true},
	}
	for _, tc := range cases {
		got, err := parseComplianceUntil(tc.in)
		if (err != nil) != tc.err || (!tc.err && !got.Equal(tc.want)) {
			t.Fatalf("parseComplianceUntil(%q) = %s, %v", tc.in, got, err)
		}
	}
}

func TestComplianceRange(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name         string
		query        string
		since, until time.Time
		err          string
	}{
		{name: "defaults", since: now.Add(-complianceDefaultExportWindow), until: now},
		{name: "since duration", query: "since=24h", since: now.Add(-24 * time.Hour), until: now},
		{name: "date range", query: "since=2026-10-01&until=2026-10-02", since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), until: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)},
		{name: "until only", query: "until=2026-10-02", since: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC).Add(-complianceDefaultExportWindow), until: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)},
		{name: "bad since", query: "since=x", err: "since"},
		{name: "bad until", query: "until=x", err: "until"},
		{name: "inverted", query: "since=2026-10-05&until=2026-10-01", err: "before"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x?"+tc.query, nil)
			since, until, err := complianceRange(req, now)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || !since.Equal(tc.since) || !until.Equal(tc.until) {
				t.Fatalf("got %s..%s (%v), want %s..%s", since, until, err, tc.since, tc.until)
			}
		})
	}
}

func TestComplianceExportFormat(t *testing.T) {
	cases := []struct {
		kind, format, want string
		err                bool
	}{
		{kind: "controls", want: "json"},
		{kind: "controls", format: "md", want: "md"},
		{kind: "posture", format: "csv", want: "csv"},
		{kind: "audit", format: "csv", err: true},
		{kind: "bundle", want: "json"},
		{kind: "zip", err: true},
		{kind: "", err: true},
	}
	for _, tc := range cases {
		got, err := complianceExportFormat(tc.kind, tc.format)
		if (err != nil) != tc.err || got != tc.want {
			t.Fatalf("complianceExportFormat(%q,%q) = %q, %v", tc.kind, tc.format, got, err)
		}
	}
}

func TestHandleCompliancePostureHistorySeries(t *testing.T) {
	isolateComplianceEvidence(t)
	s := complianceTestServer()
	now := time.Now().UTC()
	seedPostureRuns(t, s, now.Add(-72*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour))

	cases := []struct {
		name   string
		query  string
		code   int
		runs   int
		points int
	}{
		{name: "default window", code: http.StatusOK, runs: 3, points: 3},
		{name: "since 24h", query: "?since=24h", code: http.StatusOK, runs: 2, points: 2},
		{name: "until excludes newest", query: "?since=24h&until=" + now.Add(-90*time.Minute).Format(time.RFC3339), code: http.StatusOK, runs: 1, points: 1},
		{name: "bad since", query: "?since=nope", code: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := complianceEvidenceRequest(t, s, http.MethodGet, "/api/compliance/posture/history"+tc.query, "", true)
			if rr.Code != tc.code {
				t.Fatalf("status = %d, want %d (%s)", rr.Code, tc.code, rr.Body.String())
			}
			if tc.code != http.StatusOK {
				return
			}
			var resp compliancePostureHistoryResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Disclaimer != compliance.Disclaimer || resp.Runs != tc.runs || len(resp.Checks) != len(compliance.PostureCatalogue()) {
				t.Fatalf("resp = runs %d checks %d", resp.Runs, len(resp.Checks))
			}
			if got := len(resp.Checks[0].Points); got != tc.points {
				t.Fatalf("points = %d, want %d", got, tc.points)
			}
		})
	}
}

func TestHandleComplianceAttestations(t *testing.T) {
	dir := isolateComplianceEvidence(t)
	s := complianceTestServer()
	today := time.Now().UTC().Format("2006-01-02")
	cases := []struct {
		name string
		body string
		code int
	}{
		{name: "valid", body: `{"framework":"SOC2-Type2","reviewed_on":"` + today + `","note":"quarterly review"}`, code: http.StatusCreated},
		{name: "second framework", body: `{"framework":"fedramp-moderate","reviewed_on":"` + today + `"}`, code: http.StatusCreated},
		{name: "invalid json", body: `{`, code: http.StatusBadRequest},
		{name: "unknown framework", body: `{"framework":"pci","reviewed_on":"` + today + `"}`, code: http.StatusBadRequest},
		{name: "bad date", body: `{"framework":"soc2-type2","reviewed_on":"yesterday"}`, code: http.StatusBadRequest},
		{name: "oversized body", body: `{"framework":"soc2-type2","note":"` + strings.Repeat("x", complianceAttestationBodyLimit) + `"}`, code: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := complianceEvidenceRequest(t, s, http.MethodPost, "/api/compliance/attestations", tc.body, true)
			if rr.Code != tc.code {
				t.Fatalf("status = %d, want %d (%s)", rr.Code, tc.code, rr.Body.String())
			}
		})
	}
	if n := auditCount(s, "compliance_attestation"); n != 2 {
		t.Fatalf("audited %d attestations, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "compliance-attestations.jsonl")); err != nil {
		t.Fatalf("attestations not persisted: %v", err)
	}

	listCases := []struct {
		query string
		want  int
	}{
		{query: "", want: 2},
		{query: "?framework=soc2-type2", want: 1},
		{query: "?framework=iso27001-annex-a", want: 0},
	}
	for _, tc := range listCases {
		rr := complianceEvidenceRequest(t, s, http.MethodGet, "/api/compliance/attestations"+tc.query, "", true)
		var resp complianceAttestationsResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Attestations) != tc.want || resp.Disclaimer == "" {
			t.Fatalf("list%s = %d, want %d", tc.query, len(resp.Attestations), tc.want)
		}
		if tc.want == 1 && (resp.Attestations[0].By != "alice" || resp.Attestations[0].Note != "quarterly review") {
			t.Fatalf("attestation = %+v", resp.Attestations[0])
		}
	}

	// A fresh server reloads the persisted attestations.
	if got := complianceTestServer().attestationStore().List("", time.Time{}, time.Time{}); len(got) != 2 {
		t.Fatalf("reloaded %d attestations, want 2", len(got))
	}
}

func TestHandleComplianceExportKinds(t *testing.T) {
	isolateComplianceEvidence(t)
	s := complianceTestServer()
	s.deps.Config.HiveID = "hive-a"
	now := time.Now().UTC()
	seedPostureRuns(t, s, now.Add(-2*time.Hour), now.Add(-time.Hour))
	// Record the attestation one tick before the export clock: the export
	// window is [since, until) and until is pinned to now, so an attestation
	// stamped exactly at now would fall outside it.
	complianceEvidenceNow = func() time.Time { return now.Add(-time.Minute) }
	// The CRLF inside the note exercises csv.Reader's quoted-cell newline
	// normalisation; the manifest must hash the delivered (LF) bytes. The <
	// and & check the JSON manifest hashes unescaped bytes a verifier's
	// compact re-encoding reproduces.
	rr := complianceEvidenceRequest(t, s, http.MethodPost, "/api/compliance/attestations", `{"framework":"soc2-type2","reviewed_on":"`+now.Format("2006-01-02")+`","note":"a\r\n<b> & c"}`, true)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed attestation: %d %s", rr.Code, rr.Body.String())
	}
	complianceEvidenceNow = func() time.Time { return now }

	cases := []struct {
		name        string
		query       string
		contentType string
		ext         string
		check       func(t *testing.T, body []byte)
	}{
		{name: "controls json", query: "kind=controls", contentType: "application/json", ext: "json", check: func(t *testing.T, body []byte) {
			var got struct {
				Meta     complianceExportMeta `json:"meta"`
				Controls compliance.Report    `json:"controls"`
			}
			mustDecodeComplianceJSON(t, body, &got)
			if got.Meta.Kind != "controls" || got.Meta.HiveID != "hive-a" || got.Meta.SourceSystem != "Hive" || got.Meta.GeneratedBy != "alice" || len(got.Controls.Controls) == 0 {
				t.Fatalf("controls export = %+v", got.Meta)
			}
			if len(got.Meta.Profiles) != 1 || got.Meta.Profiles[0].ID != "soc2-type2" || got.Meta.Profiles[0].Version == "" {
				t.Fatalf("profile versions = %+v", got.Meta.Profiles)
			}
			if len(got.Meta.Attesters) != 1 || got.Meta.Attesters[0] != "alice" || len(got.Meta.Retention) < 2 {
				t.Fatalf("attesters/retention = %v / %v", got.Meta.Attesters, got.Meta.Retention)
			}
			if !got.Meta.GeneratedAt.Equal(now) || !got.Meta.Since.Equal(now.Add(-complianceDefaultExportWindow)) || !got.Meta.Until.Equal(now) {
				t.Fatalf("export period = %s [%s, %s)", got.Meta.GeneratedAt, got.Meta.Since, got.Meta.Until)
			}
		}},
		{name: "controls md", query: "kind=controls&format=md", contentType: "text/markdown; charset=utf-8", ext: "md", check: func(t *testing.T, body []byte) {
			assertComplianceMarkdownManifest(t, body)
		}},
		{name: "posture json", query: "kind=posture&since=24h", contentType: "application/json", ext: "json", check: func(t *testing.T, body []byte) {
			var got struct {
				Posture struct {
					Runs []compliance.PostureRun `json:"runs"`
				} `json:"posture"`
			}
			mustDecodeComplianceJSON(t, body, &got)
			if len(got.Posture.Runs) != 2 {
				t.Fatalf("runs = %d", len(got.Posture.Runs))
			}
		}},
		{name: "posture json empty range", query: "kind=posture&since=2020-01-01&until=2020-01-02", contentType: "application/json", ext: "json", check: func(t *testing.T, body []byte) {
			if !strings.Contains(string(body), `"runs": []`) {
				t.Fatalf("empty runs not an array: %s", body)
			}
		}},
		{name: "posture csv", query: "kind=posture&format=csv", contentType: "text/csv; charset=utf-8", ext: "csv", check: func(t *testing.T, body []byte) {
			assertComplianceCSVManifest(t, body, "posture", 2, 1)
		}},
		{name: "posture csv empty range", query: "kind=posture&format=csv&since=2020-01-01&until=2020-01-02", contentType: "text/csv; charset=utf-8", ext: "csv", check: func(t *testing.T, body []byte) {
			// Attesters are scoped to the export window, and no attestation
			// falls inside the 2020 range.
			assertComplianceCSVManifest(t, body, "posture", 0, 0)
		}},
		// The audit entry is stamped with the real clock at one-second
		// resolution, which can land after the pinned now; widen until.
		{name: "audit json", query: "kind=audit&since=1h&until=" + now.Add(time.Hour).Format(time.RFC3339), contentType: "application/json", ext: "json", check: func(t *testing.T, body []byte) {
			var got struct {
				Audit complianceAuditExport `json:"audit"`
			}
			mustDecodeComplianceJSON(t, body, &got)
			found := false
			for _, e := range got.Audit.Entries {
				found = found || e.Action == "compliance_attestation"
			}
			if !found || got.Audit.CoveredFrom == "" {
				t.Fatalf("audit slice = %+v", got.Audit)
			}
		}},
		{name: "config json", query: "kind=config", contentType: "application/json", ext: "json", check: func(t *testing.T, body []byte) {
			var got struct {
				Config complianceConfigSnapshot `json:"config"`
			}
			mustDecodeComplianceJSON(t, body, &got)
			snap, err := s.complianceConfigSnapshot()
			if err != nil || got.Config.SHA256 == "" || got.Config.SHA256 != snap.SHA256 {
				t.Fatalf("config sha = %q, want %q (%v)", got.Config.SHA256, snap.SHA256, err)
			}
		}},
		{name: "attestations json", query: "kind=attestations", contentType: "application/json", ext: "json", check: func(t *testing.T, body []byte) {
			var got struct {
				Attestations []compliance.Attestation `json:"attestations"`
			}
			mustDecodeComplianceJSON(t, body, &got)
			if len(got.Attestations) != 1 {
				t.Fatalf("attestations = %d", len(got.Attestations))
			}
		}},
		{name: "attestations csv", query: "kind=attestations&format=csv", contentType: "text/csv; charset=utf-8", ext: "csv", check: func(t *testing.T, body []byte) {
			assertComplianceCSVManifest(t, body, "attestations", 1, 1)
		}},
		{name: "bundle", query: "kind=bundle", contentType: "application/json", ext: "json", check: func(t *testing.T, body []byte) {
			var got map[string]json.RawMessage
			mustDecodeComplianceJSON(t, body, &got)
			for _, k := range []string{"meta", "manifest", "controls", "posture", "audit", "config", "attestations"} {
				if _, ok := got[k]; !ok {
					t.Fatalf("bundle missing %q", k)
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := auditCount(s, "compliance_export")
			rr := complianceEvidenceRequest(t, s, http.MethodGet, "/api/compliance/export?"+tc.query, "", true)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); ct != tc.contentType {
				t.Fatalf("content-type = %q, want %q", ct, tc.contentType)
			}
			cd := rr.Header().Get("Content-Disposition")
			if !strings.HasPrefix(cd, `inline; filename="hive-compliance-`) || !strings.Contains(cd, "-hive-a-") || !strings.HasSuffix(cd, "."+tc.ext+`"`) {
				t.Fatalf("content-disposition = %q", cd)
			}
			if rr.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("export must not be cached")
			}
			if auditCount(s, "compliance_export") != before+1 {
				t.Fatal("export not audited")
			}
			if detail := lastAuditDetail(s, "compliance_export"); !strings.Contains(detail, "sha256="+sha256Hex(rr.Body.String())) {
				t.Fatalf("compliance_export audit detail %q lacks the delivered body digest", detail)
			}
			tc.check(t, rr.Body.Bytes())
			if tc.contentType == "application/json" {
				assertComplianceJSONManifest(t, rr.Body.Bytes())
			}
		})
	}
}

func TestHandleComplianceExportDownloadOptIn(t *testing.T) {
	isolateComplianceEvidence(t)
	s := complianceTestServer()
	rr := complianceEvidenceRequest(t, s, http.MethodGet, "/api/compliance/export?kind=controls&download=1", "", true)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="hive-compliance-controls-`) {
		t.Fatalf("content-disposition = %q", cd)
	}
	if auditCount(s, "compliance_export") != 1 {
		t.Fatal("download opt-in export not audited")
	}
}

func TestHandleComplianceExportErrors(t *testing.T) {
	isolateComplianceEvidence(t)
	s := complianceTestServer()
	cases := []struct {
		query string
		want  string
	}{
		{query: "", want: "kind must be one of"},
		{query: "kind=zip", want: "kind must be one of"},
		{query: "kind=audit&format=csv", want: "format for kind audit"},
		{query: "kind=posture&since=bad", want: "since"},
		{query: "kind=posture&since=2026-10-05&until=2026-10-01", want: "before"},
	}
	for _, tc := range cases {
		rr := complianceEvidenceRequest(t, s, http.MethodGet, "/api/compliance/export?"+tc.query, "", true)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), tc.want) {
			t.Fatalf("%q: status %d body %s", tc.query, rr.Code, rr.Body.String())
		}
	}
	if auditCount(s, "compliance_export") != 0 {
		t.Fatal("rejected exports must not be audited")
	}
	// No hive id falls back to "hive" in the filename.
	rr := complianceEvidenceRequest(t, s, http.MethodGet, "/api/compliance/export?kind=controls", "", true)
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "-controls-hive-") {
		t.Fatalf("content-disposition = %q", cd)
	}
}

func TestComplianceAuditSliceFromDisk(t *testing.T) {
	dir := isolateComplianceEvidence(t)
	s := complianceTestServer()
	now := time.Now().UTC()
	lines := []AuditEntry{
		{Timestamp: now.Add(-48 * time.Hour).Format(time.RFC3339), User: "a", Action: "old"},
		{Timestamp: now.Add(-2 * time.Hour).Format(time.RFC3339), User: "a", Action: "in_range"},
		{Timestamp: now.Add(time.Hour).Format(time.RFC3339), User: "a", Action: "after_until"},
	}
	var b strings.Builder
	for _, e := range lines {
		raw, _ := json.Marshal(e)
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "audit.jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got := s.complianceAuditSlice(now.Add(-24*time.Hour), now)
	if len(got) != 1 || got[0].Action != "in_range" {
		t.Fatalf("slice = %+v", got)
	}
	exp := newComplianceAuditExport(nil)
	if exp.CoveredFrom != "" || exp.Entries != nil {
		t.Fatalf("empty export = %+v", exp)
	}
}

func mustDecodeComplianceJSON(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
}

func lastAuditDetail(s *Server, action string) string {
	for _, e := range s.GetAudit().Recent(0) { // newest first
		if e.Action == action {
			return e.Detail
		}
	}
	return ""
}

func assertComplianceJSONManifest(t *testing.T, body []byte) {
	t.Helper()
	var envelope map[string]json.RawMessage
	mustDecodeComplianceJSON(t, body, &envelope)
	var manifest complianceExportManifest
	mustDecodeComplianceJSON(t, envelope["manifest"], &manifest)
	if manifest.Algorithm != "sha256" || len(manifest.Entries) < 2 {
		t.Fatalf("manifest = %+v", manifest)
	}
	for _, entry := range manifest.Entries {
		raw, ok := envelope[entry.Name]
		if !ok {
			t.Fatalf("manifest entry %q has no payload", entry.Name)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			t.Fatalf("compact %s: %v", entry.Name, err)
		}
		if got := sha256Hex(compact.String()); got != entry.SHA256 || compact.Len() != entry.Bytes {
			t.Fatalf("%s manifest = %s/%d, want %s/%d", entry.Name, entry.SHA256, entry.Bytes, got, compact.Len())
		}
	}
}

func assertComplianceCSVManifest(t *testing.T, body []byte, kind string, evidenceRows, attesters int) {
	t.Helper()
	rows, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	if len(rows) != evidenceRows+2 || rows[0][0] != "record_type" || rows[1][0] != "metadata" {
		t.Fatalf("CSV envelope rows = %v", rows)
	}
	const metadataColumns = 12
	var meta complianceExportMeta
	mustDecodeComplianceJSON(t, []byte(rows[1][10]), &meta)
	if meta.HiveID != "hive-a" || meta.SourceSystem != "Hive" || meta.GeneratedBy != "alice" || len(meta.Profiles) != 1 || len(meta.Attesters) != attesters {
		t.Fatalf("CSV metadata = %+v", meta)
	}
	var manifest complianceExportManifest
	mustDecodeComplianceJSON(t, []byte(rows[1][11]), &manifest)
	domainRows := make([][]string, 0, evidenceRows+1)
	domainRows = append(domainRows, rows[0][metadataColumns:])
	for _, row := range rows[2:] {
		if row[0] != "evidence" {
			t.Fatalf("CSV record type = %q", row[0])
		}
		domainRows = append(domainRows, row[metadataColumns:])
	}
	var evidence bytes.Buffer
	writer := csv.NewWriter(&evidence)
	writer.WriteAll(domainRows)
	want := map[string]string{
		"meta": sha256Hex(rows[1][10]),
		kind:   sha256Hex(evidence.String()),
	}
	for _, entry := range manifest.Entries {
		if entry.SHA256 != want[entry.Name] {
			t.Fatalf("%s CSV manifest hash = %q, want %q", entry.Name, entry.SHA256, want[entry.Name])
		}
		delete(want, entry.Name)
	}
	if len(want) != 0 {
		t.Fatalf("CSV manifest missing entries: %v", want)
	}
}

func assertComplianceMarkdownManifest(t *testing.T, body []byte) {
	t.Helper()
	const marker = "\n## Evidence export metadata\n\n```json\n"
	start := bytes.Index(body, []byte(marker))
	if start < 0 || !bytes.HasSuffix(body, []byte("\n```\n")) {
		t.Fatalf("Markdown manifest missing: %s", body)
	}
	report := body[:start]
	raw := body[start+len(marker) : len(body)-len("\n```\n")]
	var envelope struct {
		Meta     complianceExportMeta     `json:"meta"`
		Manifest complianceExportManifest `json:"manifest"`
	}
	mustDecodeComplianceJSON(t, raw, &envelope)
	metaRaw, err := complianceJSON(envelope.Meta, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"meta":     sha256Hex(string(metaRaw)),
		"controls": sha256Hex(string(report)),
	}
	for _, entry := range envelope.Manifest.Entries {
		if entry.SHA256 != want[entry.Name] {
			t.Fatalf("%s Markdown manifest hash = %q, want %q", entry.Name, entry.SHA256, want[entry.Name])
		}
		delete(want, entry.Name)
	}
	if len(want) != 0 {
		t.Fatalf("Markdown manifest missing entries: %v", want)
	}
}

// TestComplianceEvidenceUIContract pins the Compliance tab's posture-history,
// export and attestation panels to their owner-only endpoints and the
// in-app modal/toast helpers.
func TestComplianceEvidenceUIContract(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	start := strings.Index(html, "// ── Compliance posture history, evidence exports and attestations (#11081) ──")
	end := strings.Index(html, "function addSensingItem(")
	if start < 0 || end < start {
		t.Fatal("compliance evidence block not found before addSensingItem")
	}
	block := html[start:end]
	for _, want := range []string{
		"fetch('/api/compliance/posture/history?since='",
		"fetch('/api/compliance/attestations')",
		"fetch('/api/compliance/attestations', { method: 'POST'",
		"fetch('/api/compliance/posture/run', { method: 'POST'",
		"fetch('/api/compliance/export?'",
		`data-action="complianceDownload"`,
		`data-action="complianceRecordAttestation"`,
		"await hiveConfirm(",
		"showToast(",
		"viewerIsOwner()",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("compliance evidence panels missing %q", want)
		}
	}
	for _, want := range []string{
		"renderCompliancePosture(owner) + renderComplianceEvidence(st.report || {}, owner)",
		"loadComplianceEvidence();",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
