package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/fleetreport"
)

func resetSelfReportRate(t *testing.T) {
	t.Helper()
	clear := func() {
		selfReportRate.mu.Lock()
		selfReportRate.hives = nil
		selfReportRate.mu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func selfReportIngest(s *HubServer, body, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, selfReportIngestPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	s.handleSelfReportIngest(rec, req)
	return rec
}

const selfReportValidBody = `{"hive_id":"h1","fingerprint":"abc123","class":"restart_storm","title":"[selfreport] restart_storm: agent restarted N times","body":"<!-- hive-fleet-fingerprint:abc123 -->\nbody","labels":["fleet-report","hive/selfreport"],"version":"v5.1.0"}`

// selfReportGitHubScript answers the GitHub calls the relay makes: the search
// (hit or miss), then either a comment+labels or a create.
func selfReportGitHubScript(searchHit bool) (http.HandlerFunc, *[]string) {
	var mu sync.Mutex
	var paths []string
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/search/issues":
			items := []map[string]any{}
			if searchHit {
				items = append(items, map[string]any{"number": 7, "html_url": "https://github.com/hivecommons/hive/issues/7", "body": "<!-- hive-fleet-fingerprint:abc123 -->\n" + fleetreport.RenderAffectedBlock([]string{"other-hive"}, 1)})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/hivecommons/hive/issues/7":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "html_url": "https://github.com/hivecommons/hive/issues/42"})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusTeapot)
		}
	}, &paths
}

func TestHubSelfReportIngestRejectsBadBearer(t *testing.T) {
	resetSelfReportRate(t)
	handler, _ := selfReportGitHubScript(false)
	gh := interceptFeedbackGitHub(t, handler)
	s := npsTestHub("h1")
	rec := selfReportIngest(s, selfReportValidBody, "nope")
	if rec.Code != http.StatusUnauthorized || gh.count() != 0 {
		t.Fatalf("status=%d github calls=%d", rec.Code, gh.count())
	}
	rec = selfReportIngest(s, selfReportValidBody, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer status=%d", rec.Code)
	}
}

func TestHubSelfReportIngestRejectsUnregisteredHiveAndBadPayload(t *testing.T) {
	resetSelfReportRate(t)
	handler, _ := selfReportGitHubScript(false)
	gh := interceptFeedbackGitHub(t, handler)
	s := npsTestHub("h1")
	body := strings.Replace(selfReportValidBody, `"hive_id":"h1"`, `"hive_id":"ghost"`, 1)
	if rec := selfReportIngest(s, body, s.heartbeatKeyFor("ghost")); rec.Code != http.StatusForbidden {
		t.Fatalf("unregistered hive status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := selfReportIngest(s, `{"hive_id":"h1","fingerprint":"","title":"x","body":"y"}`, s.heartbeatKeyFor("h1")); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty fingerprint status=%d", rec.Code)
	}
	if rec := selfReportIngest(s, strings.Replace(selfReportValidBody, `"version":"v5.1.0"`, `"version":"v5.1.0","repo":"not-a-repo"`, 1), s.heartbeatKeyFor("h1")); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad repo status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gh.count() != 0 {
		t.Fatalf("GitHub was called for rejected requests: %d", gh.count())
	}
}

func TestHubSelfReportIngestCreatesUnderAppIdentity(t *testing.T) {
	resetSelfReportRate(t)
	handler, paths := selfReportGitHubScript(false)
	gh := interceptFeedbackGitHub(t, handler)
	s := npsTestHub("h1")
	rec := selfReportIngest(s, selfReportValidBody, s.heartbeatKeyFor("h1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out selfReportRelayResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !out.OK || out.Number != 42 || out.Joined {
		t.Fatalf("response = %s (%v)", rec.Body.String(), err)
	}
	if strings.Join(*paths, ",") != "GET /search/issues,POST /repos/hivecommons/hive/issues" {
		t.Fatalf("github calls = %v", *paths)
	}
	for _, req := range gh.requests {
		if got := req.Header.Get("Authorization"); got != "******" && got != "Bearer hub-app-token" {
			t.Fatalf("Authorization = %q want the minted App token", got)
		}
	}
	var payload struct {
		Labels []string `json:"labels"`
		Body   string   `json:"body"`
	}
	_ = json.Unmarshal(gh.bodies[1], &payload)
	if !strings.Contains(strings.Join(payload.Labels, ","), selfReportRelayLabel) || !strings.Contains(payload.Body, "hive-fleet-fingerprint:abc123") {
		t.Fatalf("issue payload = %+v", payload)
	}
}

func TestHubSelfReportObservationCreatesThenJoins(t *testing.T) {
	resetSelfReportRate(t)
	oldSaaSHivesDir := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldSaaSHivesDir })
	decisionLabel := "owner-call"
	if err := saveSaaSHive(&SaaSHive{
		ID: "h1",
		IssueFilter: &config.IssueFilterConfig{
			HardSuppressLabels: config.HardSuppressLabelsConfig{NeedsDecision: []string{decisionLabel}},
		},
	}); err != nil {
		t.Fatalf("save hive config: %v", err)
	}

	var mu sync.Mutex
	searches := 0
	creates := 0
	comments := 0
	var createdBody struct {
		Labels []string `json:"labels"`
		Body   string   `json:"body"`
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/issues":
			mu.Lock()
			searches++
			hit := searches > 1
			mu.Unlock()
			items := []map[string]any{}
			if hit {
				items = append(items, map[string]any{
					"number":   42,
					"html_url": "https://github.com/hivecommons/hive/issues/42",
					"body":     "<!-- hive-fleet-fingerprint:h1:queue-growth -->",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues":
			mu.Lock()
			creates++
			mu.Unlock()
			if err := json.NewDecoder(r.Body).Decode(&createdBody); err != nil {
				t.Fatalf("decode create payload: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "html_url": "https://github.com/hivecommons/hive/issues/42"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			mu.Lock()
			comments++
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
		}
	}
	interceptFeedbackGitHub(t, handler)
	s := npsTestHub("h1")
	body := `{"hive_id":"h1","fingerprint":"h1:queue-growth","class":"observation","title":"Queue depth is growing","body":"<!-- hive-fleet-fingerprint:h1:queue-growth -->\nproject organization: acme\nseries: [15,18,30]","labels":["fleet-report"],"version":"v5.1.0","repo":"acme/widgets","observation":{"kind":"queue-growth","series":[15,18,30]}}`

	first := selfReportIngest(s, body, s.heartbeatKeyFor("h1"))
	if first.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	var firstOut selfReportRelayResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstOut); err != nil || !firstOut.OK || firstOut.Joined {
		t.Fatalf("first response=%s (%v)", first.Body.String(), err)
	}

	second := selfReportIngest(s, body, s.heartbeatKeyFor("h1"))
	if second.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}
	var secondOut selfReportRelayResponse
	if err := json.Unmarshal(second.Body.Bytes(), &secondOut); err != nil || !secondOut.OK || !secondOut.Joined {
		t.Fatalf("second response=%s (%v)", second.Body.String(), err)
	}
	if creates != 1 || comments != 1 {
		t.Fatalf("creates=%d comments=%d, want one create and one join comment", creates, comments)
	}
	if strings.Contains(createdBody.Body, "acme/widgets") || strings.Contains(createdBody.Body, "foo.go") {
		t.Fatalf("observation body leaked project repo identity: %q", createdBody.Body)
	}
	labels := strings.Join(createdBody.Labels, ",")
	for _, want := range []string{selfReportObservationLabel, selfReportRelayLabel, decisionLabel} {
		if !strings.Contains(labels, want) {
			t.Fatalf("create labels=%v missing %q", createdBody.Labels, want)
		}
	}
}

func TestHubSelfReportIngestJoinsExistingFingerprint(t *testing.T) {
	resetSelfReportRate(t)
	handler, paths := selfReportGitHubScript(true)
	gh := interceptFeedbackGitHub(t, handler)
	s := npsTestHub("h1")
	rec := selfReportIngest(s, selfReportValidBody, s.heartbeatKeyFor("h1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out selfReportRelayResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.OK || !out.Joined || out.Number != 7 {
		t.Fatalf("response = %s", rec.Body.String())
	}
	joined := strings.Join(*paths, ",")
	if !strings.Contains(joined, "POST /repos/hivecommons/hive/issues/7/comments") || !strings.Contains(joined, "POST /repos/hivecommons/hive/issues/7/labels") || strings.Contains(joined, "POST /repos/hivecommons/hive/issues,") || strings.HasSuffix(joined, "POST /repos/hivecommons/hive/issues") {
		t.Fatalf("github calls = %v", *paths)
	}
	var comment map[string]string
	_ = json.Unmarshal(gh.bodies[1], &comment)
	if !strings.Contains(comment["body"], "hive:selfreport-seen hive=h1 fp=abc123") || !strings.Contains(comment["body"], "Also seen on **h1**") {
		t.Fatalf("comment = %q", comment["body"])
	}
	var labels map[string][]string
	_ = json.Unmarshal(gh.bodies[2], &labels)
	if strings.Join(labels["labels"], ",") != "hive/selfreport" {
		t.Fatalf("labels = %v", labels)
	}
	// The affected block is edited in place: the joining hive gets its own
	// line (owners unknown here: the payload carried none and no SaaS record
	// exists) and seen_count is bumped.
	if !strings.Contains(joined, "PATCH /repos/hivecommons/hive/issues/7") {
		t.Fatalf("github calls = %v", *paths)
	}
	var patch map[string]string
	_ = json.Unmarshal(gh.bodies[3], &patch)
	if !strings.Contains(patch["body"], "**Affected hives (2):**\n- h1 — owner(s): unknown — seen 1, last ") || !strings.Contains(patch["body"], "\n- other-hive — owner(s): unknown — seen 1\n") || !strings.Contains(patch["body"], "seen_count: 2") {
		t.Fatalf("patched body = %q", patch["body"])
	}
}

// TestHubSelfReportIngestJoinNamesOwners: the spoke's owners field lands on
// the hive's affected line, and a spoke that predates the field is named
// from the hub's own SaaS owner record.
func TestHubSelfReportIngestJoinNamesOwners(t *testing.T) {
	resetSelfReportRate(t)
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })
	if err := os.MkdirAll(filepath.Join(saasHivesDir, "h1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(saasHivesDir, "h1", "meta.json"), []byte(`{"id":"h1","owner":"Dana"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, extra, want string
	}{
		{"spoke owners", `"instance_id":"b04ca59b7312017e","owners":["@Alice","github:bob","ibmid:123"],`, "- h1 (instance b04ca59b7312017e) — owner(s): @alice, @bob — seen 1, last "},
		{"hub fallback", ``, "- h1 — owner(s): @dana — seen 1, last "},
		{"anonymous opt-out", `"owners_hidden":true,`, "- h1 — owner(s): (anonymous) — seen 1, last "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, _ := selfReportGitHubScript(true)
			gh := interceptFeedbackGitHub(t, handler)
			s := npsTestHub("h1")
			body := strings.Replace(selfReportValidBody, `"hive_id":"h1",`, `"hive_id":"h1",`+tc.extra, 1)
			rec := selfReportIngest(s, body, s.heartbeatKeyFor("h1"))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var patch map[string]string
			_ = json.Unmarshal(gh.bodies[3], &patch)
			if !strings.Contains(patch["body"], tc.want) {
				t.Fatalf("patched body = %q, want line %q", patch["body"], tc.want)
			}
			if strings.Contains(patch["body"], "ibmid") || (tc.name == "anonymous opt-out" && strings.Contains(patch["body"], "dana")) {
				t.Fatalf("patched body leaked an identity: %q", patch["body"])
			}
		})
	}
}

// TestHubSelfReportIngestCreateFillsOwnersForLegacySpoke: a spoke whose body
// still carries the inline hive list gets its line filled from the hub's
// owner record before the issue is created.
func TestHubSelfReportIngestCreateFillsOwnersForLegacySpoke(t *testing.T) {
	resetSelfReportRate(t)
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })
	if err := os.MkdirAll(filepath.Join(saasHivesDir, "h1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(saasHivesDir, "h1", "meta.json"), []byte(`{"id":"h1","owner":"dana"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	handler, paths := selfReportGitHubScript(false)
	gh := interceptFeedbackGitHub(t, handler)
	s := npsTestHub("h1")
	legacyBlock := strings.ReplaceAll(fleetreport.AffectedBlockStart+"\n**Affected hives (1):** h1\nseen_count: 1\n"+fleetreport.AffectedBlockEnd, "\n", `\n`)
	body := strings.Replace(selfReportValidBody, `\nbody"`, `\nbody\n\n`+legacyBlock+`"`, 1)
	rec := selfReportIngest(s, body, s.heartbeatKeyFor("h1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Join(*paths, ",") != "GET /search/issues,POST /repos/hivecommons/hive/issues" {
		t.Fatalf("github calls = %v", *paths)
	}
	var payload struct {
		Body string `json:"body"`
	}
	_ = json.Unmarshal(gh.bodies[1], &payload)
	if !strings.Contains(payload.Body, "**Affected hives (1):**\n- h1 — owner(s): @dana — seen 1, last ") || !strings.Contains(payload.Body, "seen_count: 1") {
		t.Fatalf("created body = %q", payload.Body)
	}
}

func TestHubSelfReportRecoverUnknownFingerprintIsNoop(t *testing.T) {
	resetSelfReportRate(t)
	handler, paths := selfReportGitHubScript(false)
	interceptFeedbackGitHub(t, handler)
	s := npsTestHub("h1")
	body := `{"action":"recover","hive_id":"h1","fingerprint":"h1:queue-growth","class":"observation","body":"<!-- hive-fleet-fingerprint:h1:queue-growth -->\nrecovered"}`
	rec := selfReportIngest(s, body, s.heartbeatKeyFor("h1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out selfReportRelayResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.OK || out.Reason != "unknown_fingerprint" {
		t.Fatalf("response=%s err=%v", rec.Body.String(), err)
	}
	if strings.Join(*paths, ",") != "GET /search/issues" {
		t.Fatalf("github calls = %v", *paths)
	}
}

func TestHubSelfReportRecoverCommentsAndClosesOpenObservation(t *testing.T) {
	resetSelfReportRate(t)
	var paths []string
	var commentBody string
	var closePayload map[string]string
	handler := func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/search/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{
				"number": 7, "html_url": "https://github.com/hivecommons/hive/issues/7",
				"body":   "<!-- hive-fleet-fingerprint:h1:queue-growth -->\n| hive id | h1 |",
				"labels": []map[string]any{{"name": "hive/selfreport"}, {"name": "hive-observation"}},
			}}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode recovery comment: %v", err)
			}
			commentBody = payload["body"]
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/hivecommons/hive/issues/7":
			if err := json.NewDecoder(r.Body).Decode(&closePayload); err != nil {
				t.Fatalf("decode close payload: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
		}
	}
	interceptFeedbackGitHub(t, handler)
	s := npsTestHub("h1")
	body := `{"action":"recover","hive_id":"h1","fingerprint":"h1:queue-growth","class":"observation","repo":"acme/widgets","body":"<!-- hive-fleet-fingerprint:h1:queue-growth -->\n\n| before | 15 |\n| after | 30 |"}`
	rec := selfReportIngest(s, body, s.heartbeatKeyFor("h1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out selfReportRelayResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !out.OK || out.Number != 7 {
		t.Fatalf("response=%s err=%v", rec.Body.String(), err)
	}
	if !strings.Contains(commentBody, "before") || !strings.Contains(commentBody, "after") {
		t.Fatalf("recovery comment = %q", commentBody)
	}
	if closePayload["state"] != "closed" || closePayload["state_reason"] != "completed" {
		t.Fatalf("close payload = %#v", closePayload)
	}
	if strings.Join(paths, ",") != "GET /search/issues,POST /repos/hivecommons/hive/issues/7/comments,PATCH /repos/hivecommons/hive/issues/7" {
		t.Fatalf("github calls = %v", paths)
	}
}

func TestHubSelfReportRecoverRequiresMatchedIssueHive(t *testing.T) {
	for _, tt := range []struct {
		name        string
		fingerprint string
		evidence    string
		wantOK      bool
	}{
		{"other hive", "h2:queue-growth", "| hive id | h2 |", false},
		{"request prefix cannot override owner", "h1:queue-growth", "| hive id | h2 |", false},
		{"missing owner", "h1:queue-growth", "", false},
		{"empty owner", "h1:queue-growth", "| hive id | |", false},
		{"partial owner match", "h1:queue-growth", "| hive id | h10 |", false},
		{"conflicting owners", "h1:queue-growth", "| hive id | h1 |\n| hive id | h2 |", false},
		{"duplicate owners", "h1:queue-growth", "| hive id | h1 |\n| hive id | h1 |", false},
		{"malformed owner", "h1:queue-growth", "| hive id | h1 | extra |", false},
		{"custom fingerprint", "detector-custom-key", "| hive id | h1 |", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetSelfReportRate(t)
			var paths []string
			interceptFeedbackGitHub(t, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.Method+" "+r.URL.Path)
				if r.URL.Path == "/search/issues" {
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{
						"number": 7,
						"body":   "<!-- hive-fleet-fingerprint:" + tt.fingerprint + " -->\n" + tt.evidence,
						"labels": []map[string]any{{"name": "hive-observation"}},
					}}})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "id": 1})
			})
			s := npsTestHub("h1")
			// Even a caller-supplied body claiming h1 must not override the
			// ownership evidence on the matched GitHub issue.
			body, err := json.Marshal(map[string]string{
				"action": "recover", "hive_id": "h1", "class": "observation",
				"fingerprint": tt.fingerprint, "body": "recovered\n| hive id | h1 |",
			})
			if err != nil {
				t.Fatal(err)
			}
			rec := selfReportIngest(s, string(body), s.heartbeatKeyFor("h1"))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var out selfReportRelayResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.OK != tt.wantOK {
				t.Fatalf("response=%s err=%v", rec.Body.String(), err)
			}
			wantPaths := "GET /search/issues"
			if tt.wantOK {
				wantPaths += ",POST /repos/hivecommons/hive/issues/7/comments,PATCH /repos/hivecommons/hive/issues/7"
			} else if out.Reason != "hive_mismatch" || out.Number != 0 {
				t.Fatalf("response=%s", rec.Body.String())
			}
			if strings.Join(paths, ",") != wantPaths {
				t.Fatalf("github calls = %v, want %s", paths, wantPaths)
			}
		})
	}
}

// TestHubSelfReportRecoverIgnoresNonObservationIssue: every hive-filed fleet
// issue carries the fingerprint marker, and fingerprints are deterministic and
// fleet-wide, so a spoke's recover must not comment on or close a matched
// issue unless the hub filed it as an observation (hive-observation label).
func TestHubSelfReportRecoverIgnoresNonObservationIssue(t *testing.T) {
	resetSelfReportRate(t)
	var paths []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/search/issues" {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{
				"number": 9, "html_url": "https://github.com/hivecommons/hive/issues/9",
				"body":   "<!-- hive-fleet-fingerprint:runtime:panic:v5.1 -->\n" + fleetreport.AffectedBlockStart + "\n**Affected hives (2):** h1, h2\n" + fleetreport.AffectedBlockEnd,
				"labels": []map[string]any{{"name": "hive/selfreport"}, {"name": "bug"}},
			}}})
			return
		}
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
	}
	interceptFeedbackGitHub(t, handler)
	s := npsTestHub("h1")
	body := `{"action":"recover","hive_id":"h1","fingerprint":"runtime:panic:v5.1","class":"observation","body":"<!-- hive-fleet-fingerprint:runtime:panic:v5.1 -->\nrecovered"}`
	rec := selfReportIngest(s, body, s.heartbeatKeyFor("h1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out selfReportRelayResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.OK || out.Number != 0 || out.Reason != "not_observation" {
		t.Fatalf("response=%s err=%v", rec.Body.String(), err)
	}
	if strings.Join(paths, ",") != "GET /search/issues" {
		t.Fatalf("github calls = %v, want search only (no comment, no close)", paths)
	}
}

func TestHubSelfReportIngestRateLimitsCreatesNotJoins(t *testing.T) {
	resetSelfReportRate(t)
	handler, _ := selfReportGitHubScript(false)
	interceptFeedbackGitHub(t, handler)
	s := npsTestHub("h1")
	for i := 0; i < feedbackHubMaxPerHivePerWindow; i++ {
		if rec := selfReportIngest(s, selfReportValidBody, s.heartbeatKeyFor("h1")); rec.Code != http.StatusCreated {
			t.Fatalf("create %d status=%d", i, rec.Code)
		}
	}
	if rec := selfReportIngest(s, selfReportValidBody, s.heartbeatKeyFor("h1")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-cap status=%d", rec.Code)
	}
}

func TestHubSelfReportIngestErrorPaths(t *testing.T) {
	resetSelfReportRate(t)
	// No hub secret → 503 before any auth work.
	bare := newTestHubServer("")
	if rec := selfReportIngest(bare, selfReportValidBody, "x"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no secret status=%d", rec.Code)
	}
	// Oversized / malformed payloads.
	s := npsTestHub("h1")
	if rec := selfReportIngest(s, "{nope", s.heartbeatKeyFor("h1")); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed status=%d", rec.Code)
	}
	if rec := selfReportIngest(s, `{"hive_id":"h1","fingerprint":"f","title":"t","body":"`+strings.Repeat("x", selfReportMaxRequestBytes)+`"}`, s.heartbeatKeyFor("h1")); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status=%d", rec.Code)
	}
	// Missing App token → 503 after auth.
	handler, _ := selfReportGitHubScript(false)
	gh := interceptFeedbackGitHub(t, handler)
	prevMint := mintHubFeedbackAppToken
	mintHubFeedbackAppToken = func(_ context.Context, _ *HubServer, _ string) (string, string) { return "", "" }
	t.Cleanup(func() { mintHubFeedbackAppToken = prevMint })
	if rec := selfReportIngest(s, selfReportValidBody, s.heartbeatKeyFor("h1")); rec.Code != http.StatusServiceUnavailable || gh.count() != 0 {
		t.Fatalf("no app token status=%d calls=%d", rec.Code, gh.count())
	}
}

func TestHubSelfReportIngestCreateFailureIs502AndReleasesBudget(t *testing.T) {
	resetSelfReportRate(t)
	interceptFeedbackGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search/issues" {
			http.Error(w, "search down", http.StatusBadGateway)
			return
		}
		http.Error(w, "nope", http.StatusInternalServerError)
	})
	s := npsTestHub("h1")
	if rec := selfReportIngest(s, selfReportValidBody, s.heartbeatKeyFor("h1")); rec.Code != http.StatusBadGateway {
		t.Fatalf("create failure status=%d body=%s", rec.Code, rec.Body.String())
	}
	selfReportRate.mu.Lock()
	n := len(selfReportRate.hives["h1"])
	selfReportRate.mu.Unlock()
	if n != 0 {
		t.Fatalf("failed create kept a rate-limit reservation: %d", n)
	}
}

func TestHubSelfReportIngestCommentFailureIs502(t *testing.T) {
	resetSelfReportRate(t)
	interceptFeedbackGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search/issues" {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"number": 7, "html_url": "u", "body": "<!-- hive-fleet-fingerprint:abc123 -->"}}})
			return
		}
		http.Error(w, "nope", http.StatusForbidden)
	})
	s := npsTestHub("h1")
	if rec := selfReportIngest(s, selfReportValidBody, s.heartbeatKeyFor("h1")); rec.Code != http.StatusBadGateway {
		t.Fatalf("comment failure status=%d", rec.Code)
	}
}
