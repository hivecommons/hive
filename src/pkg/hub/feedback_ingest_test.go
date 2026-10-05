package hub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// feedbackGitHubCapture is the GitHub side of a feedback-ingest test. It
// stands in for api.github.com at the TRANSPORT, not at a listening socket:
// handleFeedbackIngest calls GitHub through http.DefaultClient with the
// production feedbackGitHubAPIBase, and that client's nil Transport resolves
// to http.DefaultTransport. Swapping the transport means no test ever dials
// out, the handler's URL construction is exercised unchanged, and the test
// can read the outbound request exactly as the handler built it — headers,
// method, path and body — without the httptest.Server indirection.
type feedbackGitHubCapture struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
	handler  http.HandlerFunc
}

func (c *feedbackGitHubCapture) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	c.mu.Lock()
	c.requests = append(c.requests, req.Clone(req.Context()))
	c.bodies = append(c.bodies, body)
	c.mu.Unlock()
	rec := httptest.NewRecorder()
	req.Body = io.NopCloser(bytes.NewReader(body))
	c.handler(rec, req)
	return rec.Result(), nil
}

func (c *feedbackGitHubCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

// interceptFeedbackGitHub routes http.DefaultTransport through handler for the
// rest of the test and returns the capture so the test can inspect what the
// handler under test sent.
func interceptFeedbackGitHub(t *testing.T, handler http.HandlerFunc) *feedbackGitHubCapture {
	t.Helper()
	capture := &feedbackGitHubCapture{handler: handler}
	prevTransport := http.DefaultTransport
	prevClientTransport := http.DefaultClient.Transport
	prevLogin := lookupHubFeedbackTokenLogin
	http.DefaultTransport = capture
	http.DefaultClient.Transport = nil
	lookupHubFeedbackTokenLogin = func(string) string { return "" }
	t.Cleanup(func() {
		http.DefaultTransport = prevTransport
		http.DefaultClient.Transport = prevClientTransport
		lookupHubFeedbackTokenLogin = prevLogin
	})
	return capture
}

// resetHubFeedbackRate empties the process-global per-hive rate limiter so one
// test's reservations cannot 429 the next.
func resetHubFeedbackRate(t *testing.T) {
	t.Helper()
	clear := func() {
		hubFeedbackRate.mu.Lock()
		hubFeedbackRate.hives = nil
		hubFeedbackRate.mu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func feedbackIngestReq(body, bearer string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, feedbackIngestPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return req
}

func feedbackIngest(s *HubServer, body, bearer string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.handleFeedbackIngest(rec, feedbackIngestReq(body, bearer))
	return rec
}

func feedbackIngestError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("error body %q is not JSON: %v", rec.Body.String(), err)
	}
	return out["error"]
}

const feedbackValidBody = `{"title":"Dashboard crashed","description":"Clicking save blanked the page","request_type":"bug","hive_id":"h1"}`

func feedbackCreatedHandler(number int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": number, "html_url": "https://github.com/hivecommons/hive/issues/" + strconv.Itoa(number), "id": 1000 + number})
	}
}

func TestHubFeedbackIngestRejectsBeforeReachingGitHub(t *testing.T) {
	resetHubFeedbackRate(t)
	gh := interceptFeedbackGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub must not be called for a rejected request: %s %s", r.Method, r.URL)
	})
	s := npsTestHub("h1")
	s.envGitHubToken = "hub-token"
	good := s.heartbeatKeyFor("h1")

	oversize := strings.Repeat("a", feedbackMaxRequestBytes+1)
	cases := []struct {
		name    string
		mutate  func(*HubServer)
		body    string
		bearer  string
		status  int
		message string
	}{
		{name: "no hub secret", mutate: func(s *HubServer) { s.hubSecret = "" }, body: feedbackValidBody, bearer: good, status: http.StatusServiceUnavailable, message: "hub secret"},
		{name: "missing bearer", body: feedbackValidBody, status: http.StatusUnauthorized, message: "unauthorized"},
		{name: "payload too large", body: oversize, bearer: good, status: http.StatusRequestEntityTooLarge, message: "payload too large"},
		{name: "invalid json", body: `{"title":`, bearer: good, status: http.StatusBadRequest, message: "invalid payload"},
		{name: "validation failure", body: `{"title":"","description":"x","request_type":"bug","hive_id":"h1"}`, bearer: good, status: http.StatusBadRequest, message: "title is required"},
		{name: "missing hive id", body: `{"title":"t","description":"d","request_type":"bug"}`, bearer: good, status: http.StatusBadRequest, message: "invalid hive_id"},
		{name: "hive id fails name check", body: `{"title":"t","description":"d","request_type":"bug","hive_id":"a/b"}`, bearer: good, status: http.StatusBadRequest, message: "invalid hive_id"},
		{name: "bearer for another hive", body: feedbackValidBody, bearer: s.heartbeatKeyFor("h2"), status: http.StatusUnauthorized, message: "unauthorized"},
		{name: "unregistered hive", body: `{"title":"t","description":"d","request_type":"bug","hive_id":"h9"}`, bearer: s.heartbeatKeyFor("h9"), status: http.StatusForbidden, message: "heartbeat first"},
		{name: "no github token", mutate: func(s *HubServer) { s.envGitHubToken = "" }, body: feedbackValidBody, bearer: good, status: http.StatusServiceUnavailable, message: "GitHub token is not configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(hubGitHubTokenEnv, "")
			srv := npsTestHub("h1")
			srv.envGitHubToken = "hub-token"
			if tc.mutate != nil {
				tc.mutate(srv)
			}
			rec := feedbackIngest(srv, tc.body, tc.bearer)
			if rec.Code != tc.status {
				t.Fatalf("status = %d want %d body=%s", rec.Code, tc.status, rec.Body.String())
			}
			if msg := feedbackIngestError(t, rec); !strings.Contains(msg, tc.message) {
				t.Fatalf("error = %q want substring %q", msg, tc.message)
			}
		})
	}
	if gh.count() != 0 {
		t.Fatalf("GitHub was called %d times for rejected requests", gh.count())
	}
}

func TestHubFeedbackIngestCreatesIssueWithHubToken(t *testing.T) {
	resetHubFeedbackRate(t)
	gh := interceptFeedbackGitHub(t, feedbackCreatedHandler(77))
	lookupHubFeedbackTokenLogin = func(token string) string {
		if token != "hub-token" {
			t.Fatalf("lookup token = %q want hub-token", token)
		}
		return "hub-bot"
	}
	s := npsTestHub("h1")
	s.envGitHubToken = "hub-token"

	body := `{"title":"Docs page 404s","description":"The install guide link is dead","request_type":"feature","target_repo":"docs","hive_id":"h1","credential_login":"spoke-claimed","hub_name":"https://hub.example","submitter":{"github_login":"alice"},"include_diagnostics":true,"diagnostics":{"version":"v5.1.0","hive_id":"h1"}}`
	rec := feedbackIngest(s, body, s.heartbeatKeyFor("h1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out feedbackReportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.IssueNumber != 77 || out.Warning != "" || !strings.HasSuffix(out.IssueURL, "/issues/77") {
		t.Fatalf("response = %+v", out)
	}
	if gh.count() != 1 {
		t.Fatalf("GitHub calls = %d want 1", gh.count())
	}
	req := gh.requests[0]
	if req.Method != http.MethodPost || req.URL.Path != "/repos/hivecommons/docs/issues" {
		t.Fatalf("GitHub request = %s %s", req.Method, req.URL)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer hub-token" {
		t.Fatalf("Authorization = %q want the configured hub token", got)
	}
	var payload struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(gh.bodies[0], &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Title != "Docs page 404s" || !strings.Contains(payload.Body, "Target: Documentation") || !strings.Contains(payload.Body, "| Version | v5.1.0 |") {
		t.Fatalf("issue payload = %+v", payload)
	}
	if !strings.HasPrefix(payload.Body, "Opened by @hub-bot on behalf of @alice from hive h1 (https://hub.example)") || strings.Contains(payload.Body, "@spoke-claimed") {
		t.Fatalf("issue body attribution used the wrong credential:\n%s", payload.Body)
	}
	if strings.Join(payload.Labels, ",") != "enhancement,user-feedback" {
		t.Fatalf("labels = %v", payload.Labels)
	}
}

func TestHubFeedbackIngestFallsBackToEnvToken(t *testing.T) {
	resetHubFeedbackRate(t)
	t.Setenv(hubGitHubTokenEnv, "  env-token  ")
	gh := interceptFeedbackGitHub(t, feedbackCreatedHandler(5))
	s := npsTestHub("h1")
	s.envGitHubToken = "   "

	rec := feedbackIngest(s, feedbackValidBody, s.heartbeatKeyFor("h1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := gh.requests[0].Header.Get("Authorization"); got != "Bearer env-token" {
		t.Fatalf("Authorization = %q want trimmed env token", got)
	}
	if gh.requests[0].URL.Path != "/repos/hivecommons/hive/issues" {
		t.Fatalf("default target repo path = %s", gh.requests[0].URL.Path)
	}
}

func TestHubFeedbackIngestDiagnosticsHiveIDOverridesBody(t *testing.T) {
	resetHubFeedbackRate(t)
	interceptFeedbackGitHub(t, feedbackCreatedHandler(9))
	s := npsTestHub("h2")
	s.envGitHubToken = "hub-token"

	// The body names h1 but the diagnostics block names h2: the hub binds the
	// bearer to the diagnostics hive, so h1's key must be refused and h2's
	// accepted.
	body := `{"title":"t","description":"d","request_type":"bug","hive_id":"h1","include_diagnostics":true,"diagnostics":{"hive_id":"h2"}}`
	if rec := feedbackIngest(s, body, s.heartbeatKeyFor("h1")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("h1 bearer status = %d want 401", rec.Code)
	}
	if rec := feedbackIngest(s, body, s.heartbeatKeyFor("h2")); rec.Code != http.StatusCreated {
		t.Fatalf("h2 bearer status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHubFeedbackIngestGitHubFailureReleasesRateSlot(t *testing.T) {
	resetHubFeedbackRate(t)
	var fail bool
	gh := interceptFeedbackGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		feedbackCreatedHandler(1)(w, r)
	})
	s := npsTestHub("h1")
	s.envGitHubToken = "hub-token"
	bearer := s.heartbeatKeyFor("h1")

	fail = true
	for i := 0; i < feedbackHubMaxPerHivePerWindow+2; i++ {
		rec := feedbackIngest(s, feedbackValidBody, bearer)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("attempt %d: status = %d want 502 body=%s", i, rec.Code, rec.Body.String())
		}
		if msg := feedbackIngestError(t, rec); msg != "failed to create issue" {
			t.Fatalf("error = %q", msg)
		}
	}
	// Every failed attempt handed its slot back, so a success is still possible.
	fail = false
	if rec := feedbackIngest(s, feedbackValidBody, bearer); rec.Code != http.StatusCreated {
		t.Fatalf("after failures: status = %d body=%s", rec.Code, rec.Body.String())
	}
	if gh.count() != feedbackHubMaxPerHivePerWindow+3 {
		t.Fatalf("GitHub calls = %d", gh.count())
	}
}

func TestHubFeedbackIngestRateLimitsPerHive(t *testing.T) {
	resetHubFeedbackRate(t)
	gh := interceptFeedbackGitHub(t, feedbackCreatedHandler(1))
	s := npsTestHub("h1", "h2")
	s.envGitHubToken = "hub-token"

	for i := 0; i < feedbackHubMaxPerHivePerWindow; i++ {
		if rec := feedbackIngest(s, feedbackValidBody, s.heartbeatKeyFor("h1")); rec.Code != http.StatusCreated {
			t.Fatalf("report %d: status = %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	rec := feedbackIngest(s, feedbackValidBody, s.heartbeatKeyFor("h1"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit status = %d body=%s", rec.Code, rec.Body.String())
	}
	if msg := feedbackIngestError(t, rec); msg != "rate limit exceeded" {
		t.Fatalf("error = %q", msg)
	}
	if gh.count() != feedbackHubMaxPerHivePerWindow {
		t.Fatalf("rate-limited request reached GitHub: calls = %d", gh.count())
	}
	// The limit is per hive: another registered hive still gets through.
	other := strings.Replace(feedbackValidBody, `"hive_id":"h1"`, `"hive_id":"h2"`, 1)
	if rec := feedbackIngest(s, other, s.heartbeatKeyFor("h2")); rec.Code != http.StatusCreated {
		t.Fatalf("other hive status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestValidateHubFeedbackRequest(t *testing.T) {
	png := pngDataURI(t)
	base := func() feedbackReportRequest {
		return feedbackReportRequest{Title: "  A title  ", Description: "  A description  ", RequestType: feedbackTypeBug}
	}
	cases := []struct {
		name   string
		mutate func(*feedbackReportRequest)
		want   string
	}{
		{"blank title", func(r *feedbackReportRequest) { r.Title = "   " }, "title is required"},
		{"title over 200 runes", func(r *feedbackReportRequest) { r.Title = strings.Repeat("é", 201) }, "200 characters or fewer"},
		{"blank description", func(r *feedbackReportRequest) { r.Description = " " }, "description is required"},
		{"unknown request type", func(r *feedbackReportRequest) { r.RequestType = "question" }, "request_type must be bug or feature"},
		{"unknown target repo", func(r *feedbackReportRequest) { r.TargetRepo = "pluk" }, "target_repo must be hive or docs"},
		{"too many screenshots", func(r *feedbackReportRequest) {
			r.Screenshots = make([]string, feedbackMaxScreenshots+1)
			for i := range r.Screenshots {
				r.Screenshots[i] = png
			}
		}, "at most 5 screenshots"},
		{"text too large", func(r *feedbackReportRequest) { r.Description = strings.Repeat("x", feedbackMaxTextBytes) }, "too large"},
		{"screenshot not an image data uri", func(r *feedbackReportRequest) { r.Screenshots = []string{"data:text/plain;base64,aGk="} }, "image data URIs"},
		{"screenshot bad base64", func(r *feedbackReportRequest) { r.Screenshots = []string{"data:image/png;base64,%%%"} }, "invalid screenshot data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base()
			tc.mutate(&r)
			err := validateHubFeedbackRequest(&r)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v want substring %q", err, tc.want)
			}
		})
	}

	t.Run("valid request is trimmed, defaulted and redacted", func(t *testing.T) {
		r := base()
		r.Title = "  Token ghp_abcdefghijklmnop leaked  "
		r.Description = "  mail me at someone@example.com, password: hunter2  "
		r.Screenshots = []string{png}
		r.Diagnostics = &feedbackDiagnostics{HiveID: "h1"}
		if err := validateHubFeedbackRequest(&r); err != nil {
			t.Fatal(err)
		}
		if r.TargetRepo != feedbackTargetHive {
			t.Fatalf("target_repo = %q want default %q", r.TargetRepo, feedbackTargetHive)
		}
		if r.Title != "Token [REDACTED_TOKEN] leaked" {
			t.Fatalf("title = %q", r.Title)
		}
		if strings.Contains(r.Description, "example.com") || strings.Contains(r.Description, "hunter2") {
			t.Fatalf("description not redacted: %q", r.Description)
		}
		if r.Diagnostics != nil {
			t.Fatal("diagnostics must be dropped when include_diagnostics is false")
		}
	})

	t.Run("feature request to docs is accepted", func(t *testing.T) {
		r := base()
		r.RequestType = feedbackTypeFeature
		r.TargetRepo = feedbackTargetDocs
		if err := validateHubFeedbackRequest(&r); err != nil {
			t.Fatal(err)
		}
		if r.Title != "A title" || r.Description != "A description" {
			t.Fatalf("trimmed fields = %q / %q", r.Title, r.Description)
		}
	})
}
