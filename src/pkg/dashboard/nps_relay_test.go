package dashboard

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

// Spoke side of the standalone NPS relay (issue #9619): self-registered
// Ed25519 install keys, signed submissions, re-registration after a relay
// store reset.

// npsTestRelayPath is where the fake relay accepts submissions.
const npsTestRelayPath = "/api/nps"

// npsTestRelaySkew is the fake relay's timestamp window (the real relay's
// window is a named constant in relay.ts).
const npsTestRelaySkew = 5 * time.Minute

// useTempNPSRelayIdentity points the identity file at a fresh temp dir and
// returns its path.
func useTempNPSRelayIdentity(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets", npsRelayIdentityFileName)
	old := npsRelayIdentityPath
	npsRelayIdentityPath = path
	t.Cleanup(func() { npsRelayIdentityPath = old })
	return path
}

// npsClearRelayEnv makes a test independent of any relay env in the runner
// and of any identity file outside the test's temp dir.
func npsClearRelayEnv(t *testing.T) {
	t.Helper()
	t.Setenv(config.NPSRelayURLEnvVar, "")
	t.Setenv(config.NPSRelayPullSecretEnvVar, "")
	useTempNPSRelayIdentity(t)
}

// npsFakeRelayRequest is one request the fake relay received.
type npsFakeRelayRequest struct {
	path   string
	header http.Header
	body   string
}

// npsFakeRelay implements the relay's wire contract: registration with proof
// of possession, signed submissions, a timestamp window and nonce replay
// protection. Its knobs simulate a store reset and upstream failures.
type npsFakeRelay struct {
	mu             sync.Mutex
	keys           map[string]ed25519.PublicKey
	nonces         map[string]bool
	registrations  int
	accepted       []string
	requests       []npsFakeRelayRequest
	rejectedSig    int
	submitStatus   int
	registerStatus int
	alwaysUnknown  bool
	location       string
	srv            *httptest.Server
}

func newNPSFakeRelay(t *testing.T) *npsFakeRelay {
	t.Helper()
	f := &npsFakeRelay{keys: map[string]ed25519.PublicKey{}, nonces: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func npsFakeReply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// verify checks the signed-request headers against pub for purpose.
func (f *npsFakeRelay) verify(r *http.Request, purpose string, body []byte, pub ed25519.PublicKey) bool {
	ts := r.Header.Get(npsRelayHeaderTimestamp)
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if d := time.Since(time.Unix(secs, 0)); d > npsTestRelaySkew || d < -npsTestRelaySkew {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get(npsRelayHeaderSignature))
	if err != nil {
		return false
	}
	input := npsRelaySigningInput(purpose, r.Header.Get(npsRelayHeaderInstallID), ts, r.Header.Get(npsRelayHeaderNonce), body)
	return ed25519.Verify(pub, input, sig)
}

func (f *npsFakeRelay) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, npsFakeRelayRequest{path: r.Method + " " + r.URL.Path, header: r.Header.Clone(), body: string(body)})
	if f.location != "" {
		w.Header().Set("Location", f.location)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	switch r.URL.Path {
	case npsTestRelayPath + npsRelayRegisterPath:
		if f.registerStatus != 0 {
			npsFakeReply(w, f.registerStatus, `{"error":"refused"}`)
			return
		}
		var reg npsRelayRegisterRequest
		if json.Unmarshal(body, &reg) != nil {
			npsFakeReply(w, http.StatusBadRequest, `{"error":"bad body"}`)
			return
		}
		raw, err := base64.StdEncoding.DecodeString(reg.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize || reg.InstallID != r.Header.Get(npsRelayHeaderInstallID) ||
			!npsRelayInstallIDPattern.MatchString(reg.InstallID) {
			npsFakeReply(w, http.StatusBadRequest, `{"error":"bad registration"}`)
			return
		}
		pub := ed25519.PublicKey(raw)
		if !f.verify(r, npsRelayPurposeRegister, body, pub) {
			f.rejectedSig++
			npsFakeReply(w, http.StatusUnauthorized, `{"error":"unauthorized"}`)
			return
		}
		if existing, ok := f.keys[reg.InstallID]; ok {
			if !existing.Equal(pub) {
				npsFakeReply(w, http.StatusConflict, `{"error":"install_id is registered to a different key"}`)
				return
			}
			npsFakeReply(w, http.StatusOK, `{"ok":true}`)
			return
		}
		f.keys[reg.InstallID] = pub
		f.registrations++
		npsFakeReply(w, http.StatusCreated, `{"ok":true}`)
	case npsTestRelayPath:
		pub, ok := f.keys[r.Header.Get(npsRelayHeaderInstallID)]
		if !ok || f.alwaysUnknown {
			npsFakeReply(w, http.StatusUnauthorized, `{"error":"unknown install","code":"`+npsRelayUnknownInstallCode+`"}`)
			return
		}
		nonce := r.Header.Get(npsRelayHeaderNonce)
		if !f.verify(r, npsRelayPurposeSubmit, body, pub) || f.nonces[nonce] {
			f.rejectedSig++
			npsFakeReply(w, http.StatusUnauthorized, `{"error":"unauthorized"}`)
			return
		}
		f.nonces[nonce] = true
		if f.submitStatus != 0 {
			npsFakeReply(w, f.submitStatus, `{"error":"upstream"}`)
			return
		}
		f.accepted = append(f.accepted, string(body))
		npsFakeReply(w, http.StatusCreated, `{"ok":true}`)
	default:
		http.NotFound(w, r)
	}
}

func (f *npsFakeRelay) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// snapshot returns locked copies of the fake's counters and logs.
func (f *npsFakeRelay) snapshot() (registrations int, accepted []string, requests []npsFakeRelayRequest, rejectedSig int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registrations, append([]string(nil), f.accepted...), append([]npsFakeRelayRequest(nil), f.requests...), f.rejectedSig
}

func (f *npsFakeRelay) update(fn func(*npsFakeRelay)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// npsRelayServer builds a STANDALONE dashboard server (no hub link) wired to
// relay. optIn nil means "unset" (the default, which is off for a standalone
// hive). It returns the server and the identity file path.
func npsRelayServer(t *testing.T, relay *npsFakeRelay, optIn *bool) (*Server, string) {
	t.Helper()
	t.Setenv(config.NPSEnabledEnvVar, "")
	t.Setenv(spoke.EnvHeartbeatKey, npsTestBearer)
	npsClearRelayEnv(t)
	idPath := npsRelayIdentityPath
	s := NewServer(0, dismissLogger())
	relayURL := ""
	if relay != nil {
		relayURL = relay.srv.URL + npsTestRelayPath
	}
	s.deps = &Dependencies{Config: &config.Config{
		HiveID: "hive-solo",
		Hub: config.HubConfig{
			NPSEnabled:  optIn,
			NPSRelayURL: relayURL,
		},
	}}
	return s, idPath
}

func npsBool(b bool) *bool { return &b }

func npsAssertNoIdentity(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("identity file exists (or stat failed: %v); nothing may be generated without opt-in and a relay", err)
	}
}

// npsReadIdentity loads the persisted identity file the spoke wrote.
func npsReadIdentity(t *testing.T, path string) *npsRelayIdentity {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	id, err := parseNPSRelayIdentity(raw)
	if err != nil {
		t.Fatalf("parse identity: %v", err)
	}
	return id
}

// TestNPSRelayOptInOffSendsNothing is the telemetry.md rule for the relay: a
// standalone hive that has a relay URL configured but has NOT opted in never
// prompts, never generates a key, never registers and never sends a request.
func TestNPSRelayOptInOffSendsNothing(t *testing.T) {
	relay := newNPSFakeRelay(t)

	defaultOff, idPath := npsRelayServer(t, relay, nil)
	if got := npsStatus(t, defaultOff, "alice", "owner"); got.Enabled || got.CanSubmit {
		t.Errorf("standalone default: %+v, want disabled", got)
	}
	if rec := npsPostString(defaultOff, `{"score":4}`, "alice"); rec.Code != http.StatusForbidden {
		t.Errorf("standalone default: code = %d, want 403", rec.Code)
	}
	npsAssertNoIdentity(t, idPath)

	explicitOff, idPath := npsRelayServer(t, relay, npsBool(false))
	if rec := npsPostString(explicitOff, `{"score":4}`, "bob"); rec.Code != http.StatusForbidden {
		t.Errorf("explicit opt-out: code = %d, want 403", rec.Code)
	}
	npsAssertNoIdentity(t, idPath)

	envOff, idPath := npsRelayServer(t, relay, npsBool(true))
	t.Setenv(config.NPSEnabledEnvVar, "false")
	if rec := npsPostString(envOff, `{"score":4}`, "carol"); rec.Code != http.StatusForbidden {
		t.Errorf("HIVE_NPS_ENABLED=false: code = %d, want 403", rec.Code)
	}
	npsAssertNoIdentity(t, idPath)

	if relay.count() != 0 {
		t.Fatalf("opt-in off still reached the relay %d times", relay.count())
	}

	// Positive control: the same hive with the opt-in on does register and
	// submit.
	t.Setenv(config.NPSEnabledEnvVar, "true")
	if rec := npsPostString(envOff, `{"score":4}`, "dave"); rec.Code != http.StatusOK {
		t.Fatalf("opt-in on: code = %d; body=%s", rec.Code, rec.Body.String())
	}
	if regs, accepted, _, _ := relay.snapshot(); regs != 1 || len(accepted) != 1 {
		t.Fatalf("opt-in on: registrations = %d, accepted = %d; want 1 and 1", regs, len(accepted))
	}
}

// TestNPSRelayURLMissingSendsNothing: an opted-in standalone hive with no
// usable relay URL has nowhere to send, so the prompt stays off, no key is
// generated and nothing leaves the box.
func TestNPSRelayURLMissingSendsNothing(t *testing.T) {
	relay := newNPSFakeRelay(t)

	noURL, idPath := npsRelayServer(t, nil, npsBool(true))
	if got := npsStatus(t, noURL, "alice", "owner"); got.Enabled || got.CanSubmit {
		t.Errorf("no relay URL: %+v, want disabled", got)
	}
	if rec := npsPostString(noURL, `{"score":4}`, "alice"); rec.Code != http.StatusForbidden {
		t.Errorf("no relay URL: code = %d, want 403", rec.Code)
	}
	npsAssertNoIdentity(t, idPath)

	// A plain-http, non-loopback relay URL is refused outright, which leaves
	// the hive with no relay.
	insecure, idPath := npsRelayServer(t, relay, npsBool(true))
	insecure.deps.Config.Hub.NPSRelayURL = "http://relay.example/api/nps"
	if rec := npsPostString(insecure, `{"score":4}`, "carol"); rec.Code != http.StatusForbidden {
		t.Errorf("insecure relay URL: code = %d, want 403", rec.Code)
	}
	npsAssertNoIdentity(t, idPath)

	if relay.count() != 0 {
		t.Fatalf("an unconfigured relay was contacted %d times", relay.count())
	}
}

// TestNPSRelayRegistersAndSigns is the end-to-end spoke flow: the first
// submission generates a keypair, persists it 0600, registers the public key
// (with proof of possession), then sends a signed payload that verifies
// against the registered key. Later submissions reuse the identity without
// re-registering, and the private key never appears on the wire.
func TestNPSRelayRegistersAndSigns(t *testing.T) {
	relay := newNPSFakeRelay(t)
	s, idPath := npsRelayServer(t, relay, npsBool(true))

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

	info, err := os.Stat(idPath)
	if err != nil {
		t.Fatalf("identity file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != npsRelayIdentityFileMode {
		t.Fatalf("identity file mode = %o, want %o", perm, npsRelayIdentityFileMode)
	}
	id := npsReadIdentity(t, idPath)
	if !npsRelayInstallIDPattern.MatchString(id.InstallID) {
		t.Fatalf("install id %q is not a lowercase UUID", id.InstallID)
	}
	if id.RegisteredWith != relay.srv.URL+npsTestRelayPath {
		t.Errorf("registered_with = %q, want the relay URL", id.RegisteredWith)
	}

	regs, accepted, requests, rejected := relay.snapshot()
	if regs != 1 || len(accepted) != 1 || rejected != 0 {
		t.Fatalf("registrations=%d accepted=%d rejected=%d; want 1, 1, 0", regs, len(accepted), rejected)
	}
	if requests[0].path != http.MethodPost+" "+npsTestRelayPath+npsRelayRegisterPath ||
		requests[1].path != http.MethodPost+" "+npsTestRelayPath {
		t.Fatalf("request order = %q, %q; want register then submit", requests[0].path, requests[1].path)
	}
	var reg npsRelayRegisterRequest
	if err := json.Unmarshal([]byte(requests[0].body), &reg); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if reg.InstallID != id.InstallID || reg.PublicKey != id.publicKey() || reg.HiveVersion != versionShort {
		t.Errorf("registration = %+v, want install %s and its public key", reg, id.InstallID)
	}
	for _, r := range requests {
		if r.header.Get("Authorization") != "" {
			t.Errorf("%s carried an Authorization header; the relay path has no bearer", r.path)
		}
	}

	var got npsHubPayload
	if err := json.Unmarshal([]byte(accepted[0]), &got); err != nil {
		t.Fatalf("decode relay body: %v", err)
	}
	if got.HiveID != "hive-solo" || got.Score != 2 || got.Feedback != "needs work" || got.DashboardVersion != versionShort {
		t.Errorf("relay payload = %+v", got)
	}
	if strings.Contains(accepted[0], "alice") {
		t.Errorf("relay body leaked the user: %s", accepted[0])
	}

	// A second submission (another user) reuses the identity: no new
	// registration, same install id, a fresh nonce.
	if rec := npsPostString(s, `{"score":4}`, "bob"); rec.Code != http.StatusOK {
		t.Fatalf("second submission: code = %d; body=%s", rec.Code, rec.Body.String())
	}
	regs, accepted, requests, _ = relay.snapshot()
	if regs != 1 || len(accepted) != 2 || len(requests) != 3 {
		t.Fatalf("after reuse: registrations=%d accepted=%d requests=%d; want 1, 2, 3", regs, len(accepted), len(requests))
	}
	if requests[2].header.Get(npsRelayHeaderInstallID) != id.InstallID {
		t.Error("second submission used a different install id")
	}
	if requests[1].header.Get(npsRelayHeaderNonce) == requests[2].header.Get(npsRelayHeaderNonce) {
		t.Error("two submissions reused a nonce")
	}

	// The private key (seed or expanded) is never on the wire.
	seed := id.PrivateKey
	expanded := base64.StdEncoding.EncodeToString(id.key)
	for _, r := range requests {
		wire := r.body + " " + strings.Join(npsHeaderValues(r.header), " ")
		if strings.Contains(wire, seed) || strings.Contains(wire, expanded) {
			t.Fatalf("%s leaked the private key", r.path)
		}
	}
	if strings.Contains(rec.Body.String(), seed) {
		t.Fatal("the dashboard response leaked the private key")
	}

	// The spoke's own rate limits and validation apply before anything is
	// sent.
	if rec := npsPostString(s, `{"score":4}`, "alice-secret-login"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("second response from the same user: code = %d, want 429", rec.Code)
	}
	if rec := npsPostString(s, `{"score":9}`, "carol"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad score: code = %d, want 400", rec.Code)
	}
	big := `{"score":4,"feedback":"` + strings.Repeat("x", npsMaxBodyBytes) + `"}`
	if rec := npsPostString(s, big, "dave"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: code = %d, want 413", rec.Code)
	}
	if relay.count() != 3 {
		t.Fatalf("rejected submissions reached the relay: %d requests", relay.count())
	}
}

func npsHeaderValues(h http.Header) []string {
	var out []string
	for _, vs := range h {
		out = append(out, vs...)
	}
	return out
}

// TestNPSRelayReRegistersOnUnknownInstall: when the relay has lost the
// install's key (its store was reset), the spoke re-registers the SAME
// identity once and the retried submission succeeds. A relay that keeps
// answering "unknown" gets exactly one re-registration, then a 502.
func TestNPSRelayReRegistersOnUnknownInstall(t *testing.T) {
	relay := newNPSFakeRelay(t)
	s, idPath := npsRelayServer(t, relay, npsBool(true))
	if rec := npsPostString(s, `{"score":4}`, "alice"); rec.Code != http.StatusOK {
		t.Fatalf("first submission: code = %d", rec.Code)
	}
	before := npsReadIdentity(t, idPath)

	relay.update(func(f *npsFakeRelay) { f.keys = map[string]ed25519.PublicKey{} })
	if rec := npsPostString(s, `{"score":3}`, "bob"); rec.Code != http.StatusOK {
		t.Fatalf("after relay reset: code = %d, want 200 (re-register and retry); body=%s", rec.Code, rec.Body.String())
	}
	regs, accepted, _, _ := relay.snapshot()
	if regs != 2 || len(accepted) != 2 {
		t.Fatalf("after reset: registrations=%d accepted=%d; want 2 and 2", regs, len(accepted))
	}
	after := npsReadIdentity(t, idPath)
	if after.InstallID != before.InstallID || after.PrivateKey != before.PrivateKey {
		t.Fatal("re-registration must reuse the persisted identity, not mint a new one")
	}

	relay.update(func(f *npsFakeRelay) { f.alwaysUnknown = true })
	regsBefore, _, reqsBefore, _ := relay.snapshot()
	if rec := npsPostString(s, `{"score":3}`, "carol"); rec.Code != http.StatusBadGateway {
		t.Fatalf("relay stuck on unknown: code = %d, want 502", rec.Code)
	}
	_, _, reqsAfter, _ := relay.snapshot()
	// submit (unknown) -> register -> submit (unknown) -> give up.
	if got := len(reqsAfter) - len(reqsBefore); got != 3 {
		t.Fatalf("relay stuck on unknown: %d requests, want exactly 3 (one re-registration)", got)
	}
	if regs, _, _, _ := relay.snapshot(); regs != regsBefore {
		t.Fatalf("an idempotent re-registration must not count as new: %d -> %d", regsBefore, regs)
	}
}

// TestNPSRelayRegistrationRefused: when the relay refuses registration (rate
// limit, key conflict) nothing is submitted, the error surfaces as 502 or
// 429, the identity stays unregistered for the next attempt, and the user's
// quota is not burned.
func TestNPSRelayRegistrationRefused(t *testing.T) {
	relay := newNPSFakeRelay(t)
	relay.update(func(f *npsFakeRelay) { f.registerStatus = http.StatusConflict })
	s, idPath := npsRelayServer(t, relay, npsBool(true))
	if rec := npsPostString(s, `{"score":4}`, "alice"); rec.Code != http.StatusBadGateway {
		t.Fatalf("registration conflict: code = %d, want 502", rec.Code)
	}
	relay.update(func(f *npsFakeRelay) { f.registerStatus = http.StatusTooManyRequests })
	if rec := npsPostString(s, `{"score":4}`, "alice"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("registration rate limited: code = %d, want 429", rec.Code)
	}
	if _, accepted, requests, _ := relay.snapshot(); len(accepted) != 0 || len(requests) != 2 {
		t.Fatalf("a refused registration still submitted: accepted=%d requests=%d", len(accepted), len(requests))
	}
	if id := npsReadIdentity(t, idPath); id.RegisteredWith != "" {
		t.Fatalf("identity marked registered after a refusal: %q", id.RegisteredWith)
	}

	relay.update(func(f *npsFakeRelay) { f.registerStatus = 0 })
	if rec := npsPostString(s, `{"score":4}`, "alice"); rec.Code != http.StatusOK {
		t.Fatalf("after the relay recovers: code = %d, want 200", rec.Code)
	}
}

// TestNPSRelayFailureNeverLogsKey: relay errors are logged with their route
// and never with key material, map to 502 (or 429 for the relay's rate
// limit), and do not burn the user's quota.
func TestNPSRelayFailureNeverLogsKey(t *testing.T) {
	relay := newNPSFakeRelay(t)
	s, idPath := npsRelayServer(t, relay, npsBool(true))
	var logs bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	relay.update(func(f *npsFakeRelay) { f.submitStatus = http.StatusInternalServerError })
	if rec := npsPostString(s, `{"score":4}`, "alice"); rec.Code != http.StatusBadGateway {
		t.Fatalf("relay 500: code = %d, want 502", rec.Code)
	}
	relay.update(func(f *npsFakeRelay) { f.submitStatus = http.StatusTooManyRequests })
	if rec := npsPostString(s, `{"score":4}`, "alice"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("relay 429: code = %d, want 429", rec.Code)
	}
	if !strings.Contains(logs.String(), "via="+npsRouteRelay) {
		t.Errorf("relay failure was not logged with its route: %s", logs.String())
	}
	id := npsReadIdentity(t, idPath)
	for _, secret := range []string{id.PrivateKey, base64.StdEncoding.EncodeToString(id.key)} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("key material was logged: %s", logs.String())
		}
	}

	relay.update(func(f *npsFakeRelay) { f.submitStatus = 0 })
	if rec := npsPostString(s, `{"score":4}`, "alice"); rec.Code != http.StatusOK {
		t.Fatalf("retry after recovery: code = %d, want 200", rec.Code)
	}
}

// TestNPSRelayRedirectNotFollowed: a relay that redirects must not get the
// signed request re-sent to the redirect target.
func TestNPSRelayRedirectNotFollowed(t *testing.T) {
	relay := newNPSFakeRelay(t)
	elsewhere := newNPSFakeRelay(t)
	relay.update(func(f *npsFakeRelay) { f.location = elsewhere.srv.URL + npsTestRelayPath })
	s, _ := npsRelayServer(t, relay, npsBool(true))
	if rec := npsPostString(s, `{"score":4}`, "alice"); rec.Code != http.StatusBadGateway {
		t.Fatalf("redirecting relay: code = %d, want 502", rec.Code)
	}
	if elsewhere.count() != 0 {
		t.Fatalf("the redirect was followed: target got %d requests", elsewhere.count())
	}
}

// TestNPSHubLinkWinsOverRelay: a hive with a hub link forwards to the hub
// even when a relay is also configured; the relay sees nothing and no relay
// identity is generated.
func TestNPSHubLinkWinsOverRelay(t *testing.T) {
	hub := newNPSFakeHub(t)
	relay := newNPSFakeRelay(t)
	s := npsServer(t, hub, config.HiveTypeHosted)
	idPath := npsRelayIdentityPath
	s.deps.Config.Hub.NPSRelayURL = relay.srv.URL + npsTestRelayPath
	if rec := npsPostString(s, `{"score":3}`, "alice"); rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	if hub.count() != 1 || relay.count() != 0 {
		t.Fatalf("hub requests = %d (want 1), relay requests = %d (want 0)", hub.count(), relay.count())
	}
	npsAssertNoIdentity(t, idPath)
}

// TestNPSRelaySigningInputContract pins the exact bytes the signature covers.
// The relay (hivecommons/docs netlify/nps-relay/relay.ts) rebuilds the same
// string; changing either side alone breaks every submission.
func TestNPSRelaySigningInputContract(t *testing.T) {
	got := string(npsRelaySigningInput(npsRelayPurposeSubmit, "11111111-2222-4333-8444-555555555555", "1790000000", "00112233445566778899aabbccddeeff", []byte("{}")))
	want := "hive-nps-relay-v1\nsubmit\n11111111-2222-4333-8444-555555555555\n1790000000\n00112233445566778899aabbccddeeff\n" +
		"44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
	if got != want {
		t.Fatalf("signing input =\n%q\nwant\n%q", got, want)
	}
}

// TestNPSRelayIdentityPersistence: the identity is created once and reused, a
// corrupt or oversized file is replaced with a fresh identity rather than
// wedging the relay path, and errors never echo the file's content.
func TestNPSRelayIdentityPersistence(t *testing.T) {
	path := useTempNPSRelayIdentity(t)
	first, err := loadOrCreateNPSRelayIdentity(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != npsRelayIdentityFileMode {
		t.Fatalf("identity file stat = (%v, %v), want mode %o", info, err, npsRelayIdentityFileMode)
	}
	if dir, err := os.Stat(filepath.Dir(path)); err != nil || dir.Mode().Perm() != npsRelayIdentityDirMode {
		t.Fatalf("identity dir stat = (%v, %v), want mode %o", dir, err, npsRelayIdentityDirMode)
	}
	again, err := loadOrCreateNPSRelayIdentity(path)
	if err != nil || again.InstallID != first.InstallID || again.PrivateKey != first.PrivateKey {
		t.Fatalf("reload = (%+v, %v), want the same identity", again, err)
	}
	if again.publicKey() != first.publicKey() || again.publicKey() == "" {
		t.Fatal("reloaded identity derives a different public key")
	}

	secretish := `{"install_id":"not-a-uuid","private_key":"c2VjcmV0LWtleS1tYXRlcmlhbA=="}`
	if _, err := parseNPSRelayIdentity([]byte(secretish)); err == nil || strings.Contains(err.Error(), "c2VjcmV0") {
		t.Fatalf("parse of an invalid identity = %v; want an error that does not echo content", err)
	}
	if _, err := parseNPSRelayIdentity([]byte("{not json")); err == nil {
		t.Fatal("parse of malformed JSON must fail")
	}

	for name, content := range map[string]string{
		"corrupt":   "{not json",
		"oversized": strings.Repeat("x", npsRelayMaxIdentityFileBytes+1),
	} {
		if err := os.WriteFile(path, []byte(content), npsRelayIdentityFileMode); err != nil {
			t.Fatal(err)
		}
		fresh, err := loadOrCreateNPSRelayIdentity(path)
		if err != nil {
			t.Fatalf("%s file: %v", name, err)
		}
		if fresh.InstallID == first.InstallID {
			t.Fatalf("%s file: expected a fresh identity", name)
		}
		if reread := npsReadIdentity(t, path); reread.InstallID != fresh.InstallID {
			t.Fatalf("%s file: the fresh identity was not persisted", name)
		}
	}
}
