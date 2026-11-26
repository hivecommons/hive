package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	hubspoke "github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/spoke"
)

// #20102. End to end: after a master-key rotation, every hub signing path
// (SSO handoff, session cookie, heartbeat response) must sign with the ACTIVE
// generation, a spoke that the sweep has moved onto the new generation must
// accept what the hub signs, and retiring the previous generation must not
// break that spoke.
//
// Every master here is generated at runtime (generateMasterSecret, or the one
// rotateMasterSecret generates itself). No test fixes a key.

const rotationSigningHiveID = "hive-rotation-signing"

// rotationSpoke is what a spoke that the reconcile sweep has reached holds: the
// public keys from desiredPerHiveEnv and nothing else. The heartbeat verifier
// is per-spoke state (trust-on-first-signed plus the replay floor), so one
// spoke keeps one verifier across the whole test.
type rotationSpoke struct {
	ssoKeys     []string
	sessionKeys []string
	hb          *spoke.Verifier
}

// spokeFromDesiredEnv builds the spoke the sweep would leave behind right now.
// newOnly drops the _PREV keys, giving a spoke that holds ONLY the current
// generation's public keys.
func spokeFromDesiredEnv(t *testing.T, hb *spoke.Verifier, newOnly bool) rotationSpoke {
	t.Helper()
	env := desiredPerHiveEnv(rotationSigningHiveID)
	if env == nil {
		t.Fatal("desiredPerHiveEnv returned nil")
	}
	sp := rotationSpoke{
		ssoKeys:     []string{env[EnvSSOPublicKey]},
		sessionKeys: []string{env[envSessionPublicKey]},
		hb:          hb,
	}
	if !newOnly {
		if v := env[EnvSSOPublicKeyPrevious]; v != "" {
			sp.ssoKeys = append(sp.ssoKeys, v)
		}
		if v := env[envSessionPublicKeyPrevious]; v != "" {
			sp.sessionKeys = append(sp.sessionKeys, v)
		}
	}
	return sp
}

// spokeAcceptsSSO reports whether the spoke accepts an SSO handoff token, using
// the verifier the spoke dashboard runs.
func (sp rotationSpoke) acceptsSSO(token string) bool {
	_, _, _, err := hubspoke.VerifySSOTokenAcrossKeys(sp.ssoKeys, token, rotationSigningHiveID, time.Now())
	return err == nil
}

// acceptsSession mirrors the spoke proxy: the cookie is checked against each
// session public key the spoke holds, with no legacy symmetric lane.
func (sp rotationSpoke) acceptsSession(value string) bool {
	for _, k := range sp.sessionKeys {
		if _, ok := verifyHubUserCookieEitherAt(k, "", value, time.Now(), nil); ok {
			return true
		}
	}
	return false
}

// acceptsHeartbeat runs the spoke's enforcing heartbeat verifier on a response
// the hub signed through its real signing path.
func (sp rotationSpoke) acceptsHeartbeat(t *testing.T, s *HubServer) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	resp := &HeartbeatResponse{OK: true}
	s.signHeartbeatResponse(rec, resp, rotationSigningHiveID)
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal heartbeat response: %v", err)
	}
	sig := rec.Header().Get(spoke.SigHeader)
	if sig == "" {
		t.Fatal("hub left the heartbeat response unsigned")
	}
	got := sp.hb.Verify(sp.ssoKeys, rotationSigningHiveID, body, sig, time.Now())
	return got.Accepted && got.Signed
}

// mintSSO mints an SSO handoff token the way the hub's handoff route does.
func mintSSO(t *testing.T, s *HubServer) string {
	t.Helper()
	tok := MintSSOToken(s.ssoSigningSeed(), "alice", "admin", rotationSigningHiveID, time.Now())
	if tok == "" {
		t.Fatal("hub minted no SSO token")
	}
	return tok
}

// mintSession mints a session cookie through the hub's login path and returns
// the hive_hub_user value it set.
func mintSession(t *testing.T, s *HubServer) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://hive.example.test/auth/callback", nil)
	if !s.mintSessionCookies(rec, req, "alice") {
		t.Fatal("hub could not mint a session cookie")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hive_hub_user" && c.Value != "" {
			return c.Value
		}
	}
	t.Fatal("hub set no hive_hub_user cookie")
	return ""
}

// allowRotation declares a fully observed fleet so rotateMasterSecret's
// stranding interlock (which this test does not exercise) lets it proceed.
func allowRotation(s *HubServer) {
	s.perHiveEnvMu.Lock()
	defer s.perHiveEnvMu.Unlock()
	s.perHiveEnvConsidered = 1
	s.perHiveEnvUnreachable = 0
	s.perHiveEnvUnreachableClusters = nil
}

func TestRotationSigningPathsSurviveRetirement(t *testing.T) {
	withTempGenerationsPath(t)
	withCleanLiveGenerations(t)
	masterA, err := generateMasterSecret()
	if err != nil {
		t.Fatalf("generateMasterSecret: %v", err)
	}
	// The ambient master stays the PRE-rotation one for the whole test, as in
	// production: a rotation never rewrites HIVE_HUB_SECRET or hub-secret.key.
	withTestMaster(t, masterA)

	s := newHubServerForTest(t)
	allowRotation(s)

	// A session minted before the rotation; it must keep working through the
	// overlap on the hub and on a swept spoke.
	preRotationCookie := mintSession(t, s)
	preRotationSSOKey := ssoPublicKeyFromSeed(s.ssoSigningSeed())

	if _, _, err := s.rotateMasterSecret(time.Now(), false); err != nil {
		t.Fatalf("rotateMasterSecret: %v", err)
	}
	gs := s.currentGenerations()
	if gs == nil || len(gs.acceptableGenerations(time.Now())) != maxLiveGenerations {
		t.Fatal("fixture: rotation did not leave a current and a previous generation")
	}

	// --- Overlap ------------------------------------------------------------
	hb := spoke.NewVerifier(spoke.ModeEnforce, nil)
	newOnly := spokeFromDesiredEnv(t, hb, true)
	if newOnly.ssoKeys[0] == preRotationSSOKey {
		t.Fatal("fixture: the sweep would still hand spokes the pre-rotation SSO key")
	}
	if !newOnly.acceptsSSO(mintSSO(t, s)) {
		t.Error("overlap: a spoke holding only the new generation rejected the hub's SSO handoff")
	}
	if !newOnly.acceptsSession(mintSession(t, s)) {
		t.Error("overlap: a spoke holding only the new generation rejected a new session cookie")
	}
	if !newOnly.acceptsHeartbeat(t, s) {
		t.Error("overlap: a spoke holding only the new generation rejected the hub's heartbeat signature")
	}

	// A swept spoke during the overlap holds current + _PREV and must accept
	// the pre-rotation cookie; so must the hub.
	swept := spokeFromDesiredEnv(t, spoke.NewVerifier(spoke.ModeEnforce, nil), false)
	if !swept.acceptsSession(preRotationCookie) {
		t.Error("overlap: a swept spoke rejected a session cookie minted before the rotation")
	}
	if _, ok := s.verifyHubUserCookie(preRotationCookie); !ok {
		t.Error("overlap: the hub rejected a session cookie minted before the rotation")
	}

	// --- Retirement ---------------------------------------------------------
	afterWindow := time.Now().Add(defaultVerifyWindow + time.Hour)
	retired, err := s.retireExpiredGenerations(afterWindow)
	if err != nil {
		t.Fatalf("retireExpiredGenerations: %v", err)
	}
	if len(retired) != 1 {
		t.Fatalf("fixture: retired %v, want exactly the previous generation", retired)
	}

	// What the sweep hands spokes after retirement must still be the ACTIVE
	// generation, not the retired one rebuilt from the untouched master file.
	afterRetire := spokeFromDesiredEnv(t, hb, false)
	if afterRetire.ssoKeys[0] != newOnly.ssoKeys[0] {
		t.Fatal("after retirement the sweep would push a different SSO key than the active generation's")
	}
	if len(afterRetire.ssoKeys) != 1 || len(afterRetire.sessionKeys) != 1 {
		t.Error("after retirement the sweep still advertises a _PREV key for a retired generation")
	}
	if !afterRetire.acceptsSSO(mintSSO(t, s)) {
		t.Error("after retirement: the spoke rejected the hub's SSO handoff")
	}
	if !afterRetire.acceptsSession(mintSession(t, s)) {
		t.Error("after retirement: the spoke rejected a new session cookie")
	}
	if !afterRetire.acceptsHeartbeat(t, s) {
		t.Error("after retirement: the spoke rejected the hub's heartbeat signature")
	}

	// NEGATIVE CONTROL: a token signed by the retired generation must not
	// verify on the spoke, or the positive checks above prove nothing.
	retiredTok := MintSSOToken(SSOSigningSeedFromMaster(masterA), "alice", "admin", rotationSigningHiveID, time.Now())
	if afterRetire.acceptsSSO(retiredTok) {
		t.Error("after retirement: the spoke accepted an SSO token signed by the retired generation")
	}

	// --- Restart after retirement --------------------------------------------
	// A hub roll reloads the generations file while HIVE_HUB_SECRET still holds
	// the retired master. Signing and the sweep must stay on the active one.
	s2 := newHubServerForTest(t)
	restarted := spokeFromDesiredEnv(t, hb, false)
	if restarted.ssoKeys[0] != newOnly.ssoKeys[0] {
		t.Fatal("after a hub restart the sweep would push the retired generation's SSO key")
	}
	if !restarted.acceptsSSO(mintSSO(t, s2)) {
		t.Error("after restart: the spoke rejected the hub's SSO handoff")
	}
	if !restarted.acceptsSession(mintSession(t, s2)) {
		t.Error("after restart: the spoke rejected a new session cookie")
	}
	if !restarted.acceptsHeartbeat(t, s2) {
		t.Error("after restart: the spoke rejected the hub's heartbeat signature")
	}
}
