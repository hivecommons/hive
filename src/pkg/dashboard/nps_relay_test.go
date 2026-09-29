package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

// Spoke side of the standalone NPS relay (issue #9619).

// npsTestRelayToken is the per-install relay token the fake relay expects.
const npsTestRelayToken = "nps-test-relay-install-token"

// npsTestRelayPath is where the fake relay accepts submissions.
const npsTestRelayPath = "/api/nps"

// npsClearRelayEnv makes a test independent of any relay env in the runner.
func npsClearRelayEnv(t *testing.T) {
	t.Helper()
	t.Setenv(config.NPSRelayURLEnvVar, "")
	t.Setenv(config.NPSRelayTokenEnvVar, "")
	t.Setenv(config.NPSRelayPullSecretEnvVar, "")
}

// npsFakeRelay records every request it receives and answers with status.
type npsFakeRelay struct {
	mu       sync.Mutex
	status   int
	location string
	paths    []string
	bodies   []string
	headers  []http.Header
	srv      *httptest.Server
}

func newNPSFakeRelay(t *testing.T) *npsFakeRelay {
	t.Helper()
	f := &npsFakeRelay{status: http.StatusCreated}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		f.bodies = append(f.bodies, string(b))
		f.headers = append(f.headers, r.Header.Clone())
		status, location := f.status, f.location
		f.mu.Unlock()
		if location != "" {
			w.Header().Set("Location", location)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *npsFakeRelay) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.paths)
}

func (f *npsFakeRelay) set(status int, location string) {
	f.mu.Lock()
	f.status, f.location = status, location
	f.mu.Unlock()
}

// npsRelayServer builds a STANDALONE dashboard server (no hub link) wired to
// relay. optIn nil means "unset" (the default, which is off for a standalone
// hive). An empty token leaves the install token unset.
func npsRelayServer(t *testing.T, relay *npsFakeRelay, token string, optIn *bool) *Server {
	t.Helper()
	t.Setenv(config.NPSEnabledEnvVar, "")
	t.Setenv(spoke.EnvHeartbeatKey, npsTestBearer)
	npsClearRelayEnv(t)
	s := NewServer(0, dismissLogger())
	relayURL := ""
	if relay != nil {
		relayURL = relay.srv.URL + npsTestRelayPath
	}
	s.deps = &Dependencies{Config: &config.Config{
		HiveID: "hive-solo",
		Hub: config.HubConfig{
			NPSEnabled:    optIn,
			NPSRelayURL:   relayURL,
			NPSRelayToken: token,
		},
	}}
	return s
}

func npsBool(b bool) *bool { return &b }

// TestNPSRelayOptInOffSendsNothing is the telemetry.md rule for the relay: a
// standalone hive that has a relay URL and token configured but has NOT opted
// in never prompts and never sends a request off the box.
func TestNPSRelayOptInOffSendsNothing(t *testing.T) {
	relay := newNPSFakeRelay(t)

	defaultOff := npsRelayServer(t, relay, npsTestRelayToken, nil)
	if got := npsStatus(t, defaultOff, "alice", "owner"); got.Enabled || got.CanSubmit {
		t.Errorf("standalone default: %+v, want disabled", got)
	}
	if rec := npsPostString(defaultOff, `{"score":4}`, "alice"); rec.Code != http.StatusForbidden {
		t.Errorf("standalone default: code = %d, want 403", rec.Code)
	}

	explicitOff := npsRelayServer(t, relay, npsTestRelayToken, npsBool(false))
	if rec := npsPostString(explicitOff, `{"score":4}`, "bob"); rec.Code != http.StatusForbidden {
		t.Errorf("explicit opt-out: code = %d, want 403", rec.Code)
	}

	envOff := npsRelayServer(t, relay, npsTestRelayToken, npsBool(true))
	t.Setenv(config.NPSEnabledEnvVar, "false")
	if rec := npsPostString(envOff, `{"score":4}`, "carol"); rec.Code != http.StatusForbidden {
		t.Errorf("HIVE_NPS_ENABLED=false: code = %d, want 403", rec.Code)
	}

	if relay.count() != 0 {
		t.Fatalf("opt-in off still reached the relay %d times", relay.count())
	}

	// Positive control: the same hive with the opt-in on does reach it.
	t.Setenv(config.NPSEnabledEnvVar, "true")
	if rec := npsPostString(envOff, `{"score":4}`, "dave"); rec.Code != http.StatusOK {
		t.Fatalf("opt-in on: code = %d; body=%s", rec.Code, rec.Body.String())
	}
	if relay.count() != 1 {
		t.Fatalf("opt-in on: relay requests = %d, want 1", relay.count())
	}
}

// TestNPSRelayTokenMissingSendsNothing: an opted-in standalone hive with no
// install token (or no usable relay URL) has nowhere to send, so the prompt
// stays off and nothing leaves the box.
func TestNPSRelayTokenMissingSendsNothing(t *testing.T) {
	relay := newNPSFakeRelay(t)

	noToken := npsRelayServer(t, relay, "", npsBool(true))
	if got := npsStatus(t, noToken, "alice", "owner"); got.Enabled || got.CanSubmit {
		t.Errorf("no token: %+v, want disabled", got)
	}
	if rec := npsPostString(noToken, `{"score":4}`, "alice"); rec.Code != http.StatusForbidden {
		t.Errorf("no token: code = %d, want 403", rec.Code)
	}

	noURL := npsRelayServer(t, nil, npsTestRelayToken, npsBool(true))
	if rec := npsPostString(noURL, `{"score":4}`, "bob"); rec.Code != http.StatusForbidden {
		t.Errorf("no relay URL: code = %d, want 403", rec.Code)
	}

	// A plain-http, non-loopback relay URL is refused outright (the token
	// would travel in clear text), which leaves the hive with no relay.
	insecure := npsRelayServer(t, relay, npsTestRelayToken, npsBool(true))
	insecure.deps.Config.Hub.NPSRelayURL = "http://relay.example/api/nps"
	if rec := npsPostString(insecure, `{"score":4}`, "carol"); rec.Code != http.StatusForbidden {
		t.Errorf("insecure relay URL: code = %d, want 403", rec.Code)
	}

	if relay.count() != 0 {
		t.Fatalf("an unconfigured relay was contacted %d times", relay.count())
	}
}

// TestNPSRelayForwardsWithInstallToken proves an opted-in standalone hive
// sends the same payload the hub path sends, to the relay URL, authenticated
// with the install token, and nothing that identifies the user.
func TestNPSRelayForwardsWithInstallToken(t *testing.T) {
	relay := newNPSFakeRelay(t)
	s := npsRelayServer(t, relay, npsTestRelayToken, npsBool(true))

	if got := npsStatus(t, s, "alice", "owner"); !got.Enabled || !got.CanSubmit {
		t.Fatalf("configured relay: status %+v, want enabled+can_submit", got)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/feedback/nps", strings.NewReader(`{"score":2,"feedback":"  needs work  "}`))
	req.Header.Set("X-Hive-User", "alice-secret-login")
	req.Header.Set("X-Hive-Role", "owner")
	rec := httptest.NewRecorder()
	s.handleNPSSubmit(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if relay.count() != 1 {
		t.Fatalf("relay requests = %d, want 1", relay.count())
	}
	if relay.paths[0] != http.MethodPost+" "+npsTestRelayPath {
		t.Errorf("relay request = %q, want POST %s", relay.paths[0], npsTestRelayPath)
	}
	if auth := relay.headers[0].Get("Authorization"); auth != "Bearer "+npsTestRelayToken {
		t.Errorf("Authorization = %q, want the install token", auth)
	}
	var got npsHubPayload
	if err := json.Unmarshal([]byte(relay.bodies[0]), &got); err != nil {
		t.Fatalf("decode relay body: %v", err)
	}
	if got.HiveID != "hive-solo" || got.Score != 2 || got.Feedback != "needs work" || got.DashboardVersion != versionShort {
		t.Errorf("relay payload = %+v", got)
	}
	if strings.Contains(relay.bodies[0], "alice") {
		t.Errorf("relay body leaked the user: %s", relay.bodies[0])
	}
	if strings.Contains(relay.bodies[0], npsTestRelayToken) || strings.Contains(rec.Body.String(), npsTestRelayToken) {
		t.Error("the install token leaked outside the Authorization header")
	}

	// The spoke's own rate limits apply on the relay path too.
	if rec := npsPostString(s, `{"score":4}`, "alice-secret-login"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("second response from the same user: code = %d, want 429", rec.Code)
	}
	// And so do the size cap and validation, before anything is sent.
	if rec := npsPostString(s, `{"score":9}`, "bob"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad score: code = %d, want 400", rec.Code)
	}
	big := `{"score":4,"feedback":"` + strings.Repeat("x", npsMaxBodyBytes) + `"}`
	if rec := npsPostString(s, big, "carol"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: code = %d, want 413", rec.Code)
	}
	if relay.count() != 1 {
		t.Fatalf("rejected submissions reached the relay: %d requests", relay.count())
	}
}

// TestNPSRelayFailureNeverLogsToken: a relay error is logged without the
// install token, surfaces as 502 (or 429 for the relay's rate limit), and
// does not burn the user's quota.
func TestNPSRelayFailureNeverLogsToken(t *testing.T) {
	relay := newNPSFakeRelay(t)
	s := npsRelayServer(t, relay, npsTestRelayToken, npsBool(true))
	var logs bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	relay.set(http.StatusUnauthorized, "")
	if rec := npsPostString(s, `{"score":4}`, "alice"); rec.Code != http.StatusBadGateway {
		t.Fatalf("relay 401: code = %d, want 502", rec.Code)
	}
	relay.set(http.StatusTooManyRequests, "")
	if rec := npsPostString(s, `{"score":4}`, "alice"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("relay 429: code = %d, want 429", rec.Code)
	}
	if !strings.Contains(logs.String(), "via="+npsRouteRelay) {
		t.Errorf("relay failure was not logged with its route: %s", logs.String())
	}
	if strings.Contains(logs.String(), npsTestRelayToken) {
		t.Fatalf("the install token was logged: %s", logs.String())
	}

	relay.set(http.StatusCreated, "")
	if rec := npsPostString(s, `{"score":4}`, "alice"); rec.Code != http.StatusOK {
		t.Fatalf("retry after recovery: code = %d, want 200", rec.Code)
	}
}

// TestNPSRelayRedirectNotFollowed: a relay that redirects must not get the
// install token re-sent to the redirect target.
func TestNPSRelayRedirectNotFollowed(t *testing.T) {
	relay := newNPSFakeRelay(t)
	elsewhere := newNPSFakeRelay(t)
	relay.set(http.StatusTemporaryRedirect, elsewhere.srv.URL+npsTestRelayPath)
	s := npsRelayServer(t, relay, npsTestRelayToken, npsBool(true))
	if rec := npsPostString(s, `{"score":4}`, "alice"); rec.Code != http.StatusBadGateway {
		t.Fatalf("redirecting relay: code = %d, want 502", rec.Code)
	}
	if elsewhere.count() != 0 {
		t.Fatalf("the redirect was followed: target got %d requests", elsewhere.count())
	}
}

// TestNPSHubLinkWinsOverRelay: a hive with a hub link forwards to the hub
// even when a relay is also configured; the relay sees nothing.
func TestNPSHubLinkWinsOverRelay(t *testing.T) {
	hub := newNPSFakeHub(t)
	relay := newNPSFakeRelay(t)
	s := npsServer(t, hub, config.HiveTypeHosted)
	s.deps.Config.Hub.NPSRelayURL = relay.srv.URL + npsTestRelayPath
	s.deps.Config.Hub.NPSRelayToken = npsTestRelayToken
	if rec := npsPostString(s, `{"score":3}`, "alice"); rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	if hub.count() != 1 || relay.count() != 0 {
		t.Fatalf("hub requests = %d (want 1), relay requests = %d (want 0)", hub.count(), relay.count())
	}
}
