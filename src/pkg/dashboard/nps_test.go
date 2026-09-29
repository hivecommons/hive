package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

// npsTestBearer is the per-hive heartbeat bearer the fake hub expects.
const npsTestBearer = "nps-test-heartbeat-key"

// npsFakeHub records every request it receives and answers with status.
type npsFakeHub struct {
	mu       sync.Mutex
	status   int
	bodies   []string
	headers  []http.Header
	srv      *httptest.Server
	requests int
}

func newNPSFakeHub(t *testing.T) *npsFakeHub {
	t.Helper()
	h := &npsFakeHub{status: http.StatusCreated}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.requests++
		h.bodies = append(h.bodies, string(b))
		h.headers = append(h.headers, r.Header.Clone())
		status := h.status
		h.mu.Unlock()
		if r.URL.Path != npsHubIngestPath {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *npsFakeHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.requests
}

func (h *npsFakeHub) setStatus(code int) {
	h.mu.Lock()
	h.status = code
	h.mu.Unlock()
}

// npsServer builds a dashboard server wired to hub. hiveType "hosted" gets the
// default-on behavior; anything else defaults off.
func npsServer(t *testing.T, hub *npsFakeHub, hiveType string) *Server {
	t.Helper()
	t.Setenv(config.NPSEnabledEnvVar, "")
	t.Setenv(spoke.EnvHeartbeatKey, npsTestBearer)
	s := NewServer(0, dismissLogger())
	url := ""
	if hub != nil {
		url = hub.srv.URL
	}
	s.deps = &Dependencies{Config: &config.Config{
		HiveID: "hive-one",
		Hub:    config.HubConfig{Enabled: true, URL: url, HiveType: hiveType},
	}}
	return s
}

func npsPost(s *Server, body io.Reader, user, role string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/feedback/nps", body)
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Hive-User", user)
		req.Header.Set("X-Hive-Role", role)
	}
	rec := httptest.NewRecorder()
	s.handleNPSSubmit(rec, req)
	return rec
}

func npsPostString(s *Server, body, user string) *httptest.ResponseRecorder {
	return npsPost(s, strings.NewReader(body), user, "owner")
}

func npsStatus(t *testing.T, s *Server, user, role string) npsStatusResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/feedback/nps/status", nil)
	if user != "" {
		req.Header.Set("X-Hive-User", user)
		req.Header.Set("X-Hive-Role", role)
	}
	rec := httptest.NewRecorder()
	s.handleNPSStatus(rec, req)
	var got npsStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode status: %v (%s)", err, rec.Body.String())
	}
	return got
}

func TestNPSStatusGatesThePrompt(t *testing.T) {
	hub := newNPSFakeHub(t)

	hosted := npsServer(t, hub, config.HiveTypeHosted)
	if got := npsStatus(t, hosted, "alice", "owner"); !got.Enabled || !got.CanSubmit || got.MaxFeedbackChars != npsMaxFeedbackRunes {
		t.Errorf("hosted signed-in owner: %+v, want enabled+can_submit", got)
	}
	if got := npsStatus(t, hosted, "viewer", "read"); !got.Enabled || got.CanSubmit {
		t.Errorf("read-only user: %+v, want enabled but NOT can_submit", got)
	}
	if got := npsStatus(t, hosted, "", ""); got.CanSubmit {
		t.Errorf("anonymous: %+v, want NOT can_submit", got)
	}

	standalone := npsServer(t, hub, "")
	if got := npsStatus(t, standalone, "alice", "owner"); got.Enabled || got.CanSubmit {
		t.Errorf("standalone default: %+v, want disabled (opt-in)", got)
	}

	noHub := npsServer(t, nil, config.HiveTypeHosted)
	if got := npsStatus(t, noHub, "alice", "owner"); got.Enabled {
		t.Errorf("no hub link: %+v, want disabled", got)
	}
	if hub.count() != 0 {
		t.Fatalf("status calls reached the hub %d times", hub.count())
	}
}

// TestNPSSubmitForwardsWithoutUserIdentity proves the hub gets the hive, the
// score, the text and the version, authenticated by the per-hive bearer, and
// nothing that identifies the user.
func TestNPSSubmitForwardsWithoutUserIdentity(t *testing.T) {
	hub := newNPSFakeHub(t)
	s := npsServer(t, hub, config.HiveTypeHosted)
	req := httptest.NewRequest(http.MethodPost, "/api/feedback/nps", strings.NewReader(`{"score":1,"feedback":"  too slow  "}`))
	req.Header.Set("X-Hive-User", "alice-secret-login")
	req.Header.Set("X-Hive-Role", "owner")
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "alice-cookie"})
	rec := httptest.NewRecorder()
	s.handleNPSSubmit(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"category":"detractor"`) {
		t.Errorf("response missing category: %s", rec.Body.String())
	}
	if hub.count() != 1 {
		t.Fatalf("hub requests = %d, want 1", hub.count())
	}
	var got npsHubPayload
	if err := json.Unmarshal([]byte(hub.bodies[0]), &got); err != nil {
		t.Fatalf("decode hub body: %v", err)
	}
	if got.HiveID != "hive-one" || got.Score != 1 || got.Feedback != "too slow" || got.DashboardVersion != versionShort {
		t.Errorf("hub payload = %+v", got)
	}
	if auth := hub.headers[0].Get("Authorization"); auth != "Bearer "+npsTestBearer {
		t.Errorf("Authorization = %q, want the per-hive heartbeat bearer", auth)
	}
	for name, vals := range hub.headers[0] {
		for _, v := range vals {
			if strings.Contains(v, "alice") {
				t.Errorf("hub request header %s leaked the user: %q", name, v)
			}
		}
	}
	if strings.Contains(hub.bodies[0], "alice") {
		t.Errorf("hub body leaked the user: %s", hub.bodies[0])
	}
}

// TestNPSOptInOffSendsNothing: a self-hosted/standalone hive (default off) and
// a hosted hive with HIVE_NPS_ENABLED=false both refuse, and the hub never sees
// a request.
func TestNPSOptInOffSendsNothing(t *testing.T) {
	hub := newNPSFakeHub(t)
	standalone := npsServer(t, hub, "")
	if rec := npsPostString(standalone, `{"score":4}`, "alice"); rec.Code != http.StatusForbidden {
		t.Errorf("standalone default: code = %d, want 403", rec.Code)
	}

	hosted := npsServer(t, hub, config.HiveTypeHosted)
	t.Setenv(config.NPSEnabledEnvVar, "false")
	if rec := npsPostString(hosted, `{"score":4}`, "bob"); rec.Code != http.StatusForbidden {
		t.Errorf("hosted opted out: code = %d, want 403", rec.Code)
	}
	if hub.count() != 0 {
		t.Fatalf("opt-in off still reached the hub %d times", hub.count())
	}

	// Positive control: an explicit opt-in on a standalone-typed hive works.
	t.Setenv(config.NPSEnabledEnvVar, "true")
	if rec := npsPostString(standalone, `{"score":4}`, "carol"); rec.Code != http.StatusOK {
		t.Fatalf("explicit opt-in: code = %d; body=%s", rec.Code, rec.Body.String())
	}
	if hub.count() != 1 {
		t.Fatalf("explicit opt-in: hub requests = %d, want 1", hub.count())
	}
}

func TestNPSSubmitRequiresWritableSignedInUser(t *testing.T) {
	hub := newNPSFakeHub(t)
	s := npsServer(t, hub, config.HiveTypeHosted)
	if rec := npsPost(s, strings.NewReader(`{"score":4}`), "", ""); rec.Code != http.StatusForbidden {
		t.Errorf("anonymous: code = %d, want 403", rec.Code)
	}
	if rec := npsPost(s, strings.NewReader(`{"score":4}`), "viewer", "read"); rec.Code != http.StatusForbidden {
		t.Errorf("read-only: code = %d, want 403", rec.Code)
	}
	if hub.count() != 0 {
		t.Fatalf("unauthorized submissions reached the hub %d times", hub.count())
	}
}

func TestNPSSubmitValidatesScore(t *testing.T) {
	hub := newNPSFakeHub(t)
	s := npsServer(t, hub, config.HiveTypeHosted)
	for i, body := range []string{
		`{"score":0}`, `{"score":5}`, `{"score":-3}`, `{"score":"4"}`, `{"score":2.5}`,
		`{"feedback":"no score"}`, `not json`, ``,
	} {
		if rec := npsPostString(s, body, fmt.Sprintf("user%d", i)); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: code = %d, want 400", body, rec.Code)
		}
	}
	if hub.count() != 0 {
		t.Fatalf("invalid submissions reached the hub %d times", hub.count())
	}
}

// npsChunkedBody hides its length, the shape of a chunked request.
type npsChunkedBody struct{ r io.Reader }

func (c npsChunkedBody) Read(p []byte) (int, error) { return c.r.Read(p) }

// TestNPSSubmitSizeCapIgnoresContentLength: the cap applies to bytes read, for
// a chunked body and a lying Content-Length alike (console #16666).
func TestNPSSubmitSizeCapIgnoresContentLength(t *testing.T) {
	hub := newNPSFakeHub(t)
	s := npsServer(t, hub, config.HiveTypeHosted)
	big := `{"score":4,"feedback":"` + strings.Repeat("x", npsMaxBodyBytes) + `"}`

	if rec := npsPost(s, npsChunkedBody{strings.NewReader(big)}, "alice", "owner"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("chunked oversize: code = %d, want 413", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/feedback/nps", strings.NewReader(big))
	req.ContentLength = 12
	req.Header.Set("X-Hive-User", "bob")
	req.Header.Set("X-Hive-Role", "owner")
	rec := httptest.NewRecorder()
	s.handleNPSSubmit(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("lying Content-Length: code = %d, want 413", rec.Code)
	}
	if hub.count() != 0 {
		t.Fatalf("oversize bodies reached the hub %d times", hub.count())
	}

	// Positive control + feedback cap: in-cap body with over-long text is
	// accepted and forwarded truncated.
	long := `{"score":2,"feedback":"` + strings.Repeat("é", npsMaxFeedbackRunes+100) + `"}`
	if rec := npsPostString(s, long, "carol"); rec.Code != http.StatusOK {
		t.Fatalf("in-cap body: code = %d; body=%s", rec.Code, rec.Body.String())
	}
	if hub.count() != 1 {
		t.Fatalf("hub requests = %d, want 1", hub.count())
	}
	var got npsHubPayload
	if err := json.Unmarshal([]byte(hub.bodies[0]), &got); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(got.Feedback)); n != npsMaxFeedbackRunes {
		t.Errorf("forwarded feedback = %d runes, want %d", n, npsMaxFeedbackRunes)
	}
}

func TestNPSSubmitRateLimitsPerUser(t *testing.T) {
	hub := newNPSFakeHub(t)
	s := npsServer(t, hub, config.HiveTypeHosted)
	if rec := npsPostString(s, `{"score":3}`, "alice"); rec.Code != http.StatusOK {
		t.Fatalf("first: code = %d", rec.Code)
	}
	// Same user, different case: still the same user.
	if rec := npsPostString(s, `{"score":3}`, "ALICE"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("second from same user: code = %d, want 429", rec.Code)
	}
	if rec := npsPostString(s, `{"score":3}`, "bob"); rec.Code != http.StatusOK {
		t.Errorf("another user: code = %d, want 200", rec.Code)
	}
	if hub.count() != 2 {
		t.Fatalf("hub requests = %d, want 2", hub.count())
	}
}

func TestNPSSubmitRateLimitsPerHive(t *testing.T) {
	hub := newNPSFakeHub(t)
	s := npsServer(t, hub, config.HiveTypeHosted)
	for i := 0; i < npsMaxPerHivePerWindow; i++ {
		if rec := npsPostString(s, `{"score":4}`, fmt.Sprintf("user%d", i)); rec.Code != http.StatusOK {
			t.Fatalf("user%d: code = %d", i, rec.Code)
		}
	}
	if rec := npsPostString(s, `{"score":4}`, "one-too-many"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over hive cap: code = %d, want 429", rec.Code)
	}
	if hub.count() != npsMaxPerHivePerWindow {
		t.Fatalf("hub requests = %d, want %d", hub.count(), npsMaxPerHivePerWindow)
	}
}

// TestNPSSubmitHubFailureDoesNotBurnQuota: a failed forward releases the
// user's slot so a retry after the hub recovers is accepted.
func TestNPSSubmitHubFailureDoesNotBurnQuota(t *testing.T) {
	hub := newNPSFakeHub(t)
	s := npsServer(t, hub, config.HiveTypeHosted)
	hub.setStatus(http.StatusInternalServerError)
	if rec := npsPostString(s, `{"score":4}`, "alice"); rec.Code != http.StatusBadGateway {
		t.Fatalf("hub 500: code = %d, want 502", rec.Code)
	}
	hub.setStatus(http.StatusTooManyRequests)
	if rec := npsPostString(s, `{"score":4}`, "alice"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("hub 429: code = %d, want 429", rec.Code)
	}
	hub.setStatus(http.StatusCreated)
	if rec := npsPostString(s, `{"score":4}`, "alice"); rec.Code != http.StatusOK {
		t.Fatalf("retry after recovery: code = %d, want 200", rec.Code)
	}
}

func TestNPSSubmitWithoutHubCredentialFails(t *testing.T) {
	hub := newNPSFakeHub(t)
	s := npsServer(t, hub, config.HiveTypeHosted)
	t.Setenv(spoke.EnvHeartbeatKey, "")
	t.Setenv("HIVE_HUB_SECRET", "")
	if rec := npsPostString(s, `{"score":4}`, "alice"); rec.Code != http.StatusBadGateway {
		t.Fatalf("no credential: code = %d, want 502", rec.Code)
	}
	if hub.count() != 0 {
		t.Fatalf("an unauthenticated forward reached the hub")
	}
}

func TestNPSRateLimiterWindowSlides(t *testing.T) {
	var l npsRateLimiter
	start := time.Now()
	if _, err := l.reserve("alice", start); err != nil {
		t.Fatal(err)
	}
	if _, err := l.reserve("alice", start.Add(time.Hour)); err != errNPSUserLimited {
		t.Fatalf("inside window: err = %v, want errNPSUserLimited", err)
	}
	if _, err := l.reserve("alice", start.Add(npsRateWindow+time.Second)); err != nil {
		t.Fatalf("after window: err = %v, want nil", err)
	}
}

func TestNPSCategoryDashboard(t *testing.T) {
	for score, want := range map[int]string{1: "detractor", 2: "passive", 3: "passive", 4: "promoter"} {
		if got := npsCategory(score); got != want {
			t.Errorf("npsCategory(%d) = %q, want %q", score, got, want)
		}
	}
}

func TestNPSRoutesRegistered(t *testing.T) {
	b, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{
		`s.mux.HandleFunc("GET /api/feedback/nps/status", s.handleNPSStatus)`,
		`s.mux.HandleFunc("POST /api/feedback/nps", s.handleNPSSubmit)`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("api.go does not register %s", want)
		}
	}
}
