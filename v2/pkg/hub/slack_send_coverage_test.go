package hub

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// withFakeSlackEndpoint points the Slack sender at a local test server and
// collapses the send pacing, restoring both afterwards. Every test in this file
// goes through it: sendSlackDM must never be exercised against the real
// slack.com from CI.
func withFakeSlackEndpoint(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	oldURL, oldInterval := slackPostMessageURL, slackSendInterval
	slackPostMessageURL = srv.URL
	slackSendInterval = time.Millisecond
	t.Cleanup(func() {
		slackPostMessageURL = oldURL
		slackSendInterval = oldInterval
		srv.Close()
	})
	return srv
}

// The whole reason sendSlackDM decodes the body: Slack answers HTTP 200 with
// {"ok": false} for an invalid user, a missing scope or a revoked token.
// Treating those as success is the bug this test pins — a broadcast would
// report "88 sent" while delivering nothing.
func TestSendSlackDMTreats200WithOKFalseAsFailure(t *testing.T) {
	withFakeSlackEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":false,"error":"user_not_found"}`)
	})

	err := sendSlackDM("xoxb-test", "U123", "hello")
	if err == nil {
		t.Fatal("a 200 carrying ok:false must be an error, not a successful send")
	}
	if !strings.Contains(err.Error(), "user_not_found") {
		t.Errorf("error %q does not carry Slack's own error code", err)
	}
}

// The token must never reach a log or an error string. A previous leak in this
// repo required scrubbing git history.
func TestSendSlackDMNeverLeaksTokenInError(t *testing.T) {
	const token = "xoxb-super-secret-value"
	withFakeSlackEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":false,"error":"invalid_auth"}`)
	})

	err := sendSlackDM(token, "U123", "hello")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("token leaked into the error: %q", err)
	}
}

// A 429 is called out separately because it means the pacing is wrong, which
// is a different fix from a bad token or a bad user ID.
func TestSendSlackDMReportsRateLimitDistinctly(t *testing.T) {
	withFakeSlackEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	err := sendSlackDM("xoxb-test", "U123", "hello")
	if err == nil {
		t.Fatal("HTTP 429 must be an error")
	}
	if !strings.Contains(err.Error(), "429") || !strings.Contains(strings.ToLower(err.Error()), "rate limited") {
		t.Errorf("429 not surfaced distinctly: %q", err)
	}
}

func TestSendSlackDMReportsHTTPErrorStatus(t *testing.T) {
	withFakeSlackEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	err := sendSlackDM("xoxb-test", "U123", "hello")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected an error naming HTTP 500, got %v", err)
	}
}

// A body that is not JSON must be an error rather than a silent success.
func TestSendSlackDMRejectsUndecodableBody(t *testing.T) {
	withFakeSlackEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `not json at all`)
	})

	err := sendSlackDM("xoxb-test", "U123", "hello")
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("expected a decode error, got %v", err)
	}
}

// The happy path, plus the request shape Slack requires: a bearer token, a
// JSON content type, and channel/text in the body.
func TestSendSlackDMSendsWellFormedRequest(t *testing.T) {
	var (
		gotAuth, gotCT, gotMethod string
		gotBody                   map[string]string
	)
	withFakeSlackEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&gotBody)
		io.WriteString(w, `{"ok":true}`)
	})

	if err := sendSlackDM("xoxb-test", "U123", "hello there"); err != nil {
		t.Fatalf("well-formed send failed: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotAuth != "Bearer xoxb-test" {
		t.Errorf("Authorization = %q, want a bearer token", gotAuth)
	}
	if !strings.Contains(gotCT, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if gotBody["channel"] != "U123" || gotBody["text"] != "hello there" {
		t.Errorf("body = %v, want channel U123 / text 'hello there'", gotBody)
	}
}

// A transport-level failure (connection refused) must be an error, not a
// nil-deref on the response.
func TestSendSlackDMHandlesTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	old := slackPostMessageURL
	slackPostMessageURL = url
	t.Cleanup(func() { slackPostMessageURL = old })

	if err := sendSlackDM("xoxb-test", "U123", "hello"); err == nil {
		t.Fatal("a dead endpoint must produce an error")
	}
}

func testHubServer() *HubServer {
	return &HubServer{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// deliverSlackMessages runs on a background goroutine long after the HTTP
// response went out, so a partial failure is only ever visible in the log. It
// must attempt EVERY recipient — an early return on the first bad Slack ID
// would silently drop everyone after it.
func TestDeliverSlackMessagesContinuesPastAFailure(t *testing.T) {
	var mu sync.Mutex
	var got []string
	withFakeSlackEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, body["channel"])
		mu.Unlock()
		if body["channel"] == "U-BAD" {
			io.WriteString(w, `{"ok":false,"error":"user_not_found"}`)
			return
		}
		io.WriteString(w, `{"ok":true}`)
	})

	testHubServer().deliverSlackMessages("xoxb-test", "all", "msg", []SlackRecipient{
		{Username: "alice", SlackID: "U-A"},
		{Username: "bob", SlackID: "U-BAD"},
		{Username: "carol", SlackID: "U-C"},
	}, "operator")

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("attempted %d sends (%v), want all 3 — a failure must not stop the broadcast", len(got), got)
	}
	if got[2] != "U-C" {
		t.Errorf("recipient after the failure was %q, want U-C", got[2])
	}
}

// A nil/empty recipient slice must be a no-op. This runs on a goroutine where
// nothing would recover a panic.
func TestDeliverSlackMessagesHandlesNoRecipients(t *testing.T) {
	called := false
	withFakeSlackEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		io.WriteString(w, `{"ok":true}`)
	})

	testHubServer().deliverSlackMessages("xoxb-test", "all", "msg", nil, "operator")

	if called {
		t.Error("no recipients must mean no Slack calls")
	}
}

// Deliveries are serialized: two concurrent broadcasts would each pace
// themselves correctly and still collectively exceed Slack's limit.
func TestDeliverSlackMessagesSerializesConcurrentBroadcasts(t *testing.T) {
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	withFakeSlackEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		io.WriteString(w, `{"ok":true}`)
	})

	s := testHubServer()
	rcpts := []SlackRecipient{{Username: "a", SlackID: "U-A"}, {Username: "b", SlackID: "U-B"}}

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.deliverSlackMessages("xoxb-test", "all", "msg", rcpts, "operator")
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if maxInFlight > 1 {
		t.Errorf("observed %d concurrent Slack calls; broadcasts must be serialized", maxInFlight)
	}
}

// slackPreflight is the CORS gate in front of every Slack endpoint. An
// untrusted Origin must NOT be echoed back: reflecting an arbitrary origin
// with Allow-Credentials true would let any site make credentialed calls to
// the hub on a signed-in operator's behalf.
func TestSlackPreflightDoesNotReflectUntrustedOrigin(t *testing.T) {
	for _, origin := range []string{
		"https://evil.example.com",
		"https://hive.kubestellar.io.evil.com",
		"://not a url",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		req.Header.Set("Origin", origin)

		slackPreflight(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("origin %q was reflected as %q; untrusted origins must not be allowed", origin, got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
			t.Errorf("origin %q got Allow-Credentials %q", origin, got)
		}
	}
}

func TestSlackPreflightAllowsTrustedOrigins(t *testing.T) {
	for _, origin := range []string{
		"https://hive.kubestellar.io",
		// F4: "https://my-hive.hive.kubestellar.io" removed — a sibling tenant
		// must NOT receive a credentialed CORS reflection.
		"http://localhost:5174",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		req.Header.Set("Origin", origin)

		slackPreflight(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("trusted origin %q not allowed (got %q)", origin, got)
		}
	}
}

// An OPTIONS request must terminate in the preflight (returning true) with 204
// and never fall through to the handler body.
func TestSlackPreflightShortCircuitsOPTIONS(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/x", nil)
	req.Header.Set("Origin", "https://hive.kubestellar.io")

	if handled := slackPreflight(rec, req); !handled {
		t.Fatal("OPTIONS must be handled by the preflight")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

// A POST must fall through (returning false) and be set up to answer JSON.
func TestSlackPreflightPassesThroughPOST(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x", nil)

	if handled := slackPreflight(rec, req); handled {
		t.Fatal("POST must fall through to the handler")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// A hub with no Slack token must fail loudly with 503. Reporting a cheerful
// "sending" for a send that can never happen would leave the operator
// believing the fleet had been notified.
func TestRequireSlackTokenFailsLoudlyWhenUnset(t *testing.T) {
	t.Setenv(slackTokenEnvVar, "")

	rec := httptest.NewRecorder()
	if token := requireSlackToken(rec); token != "" {
		t.Fatalf("expected no token, got %q", token)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), slackTokenEnvVar) {
		t.Errorf("body %q does not name the missing variable", rec.Body.String())
	}
}

// Whitespace is not a token. A Secret populated with a stray newline must be
// treated as unset rather than sent to Slack as a bearer credential.
func TestRequireSlackTokenTreatsWhitespaceAsUnset(t *testing.T) {
	t.Setenv(slackTokenEnvVar, "   \n\t ")

	rec := httptest.NewRecorder()
	if token := requireSlackToken(rec); token != "" {
		t.Fatalf("whitespace must not count as a token, got %q", token)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestRequireSlackTokenReturnsTrimmedToken(t *testing.T) {
	t.Setenv(slackTokenEnvVar, "  xoxb-real-token\n")

	rec := httptest.NewRecorder()
	if got := requireSlackToken(rec); got != "xoxb-real-token" {
		t.Fatalf("token = %q, want the trimmed value", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("a configured hub must not write an error status, got %d", rec.Code)
	}
}

// F4 (CWE-942): a sibling tenant origin must be REFUSED the credentialed CORS
// reflection. Reflecting it with Allow-Credentials gave a hostile tenant a
// scripted cross-tenant READ of the hub API.
func TestF4_SlackPreflightRefusesSiblingOrigin(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("Origin", "https://evil.hive.kubestellar.io")

	slackPreflight(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("F4: sibling origin was reflected for credentialed CORS: %q", got)
	}
}
