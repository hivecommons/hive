package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
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
				items = append(items, map[string]any{"number": 7, "html_url": "https://github.com/hivecommons/hive/issues/7", "body": "<!-- hive-fleet-fingerprint:abc123 -->"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			_ = json.NewEncoder(w).Encode([]any{})
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
