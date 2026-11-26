package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	return mintSSOFor(t, s, rotationSigningHiveID)
}

func mintSSOFor(t *testing.T, s *HubServer, hiveID string) string {
	t.Helper()
	tok := MintSSOToken(s.ssoSigningSeedForHive(hiveID, time.Now()), "alice", "admin", hiveID, time.Now())
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

func mustGenerateMaster(t *testing.T) string {
	t.Helper()
	m, err := generateMasterSecret()
	if err != nil {
		t.Fatalf("generateMasterSecret: %v", err)
	}
	return m
}

// TestMintingSecretFollowsActiveGeneration pins the accessor every hub signing
// path derives from: the active generation after a rotation, and s.hubSecret
// only when there is no generation set.
func TestMintingSecretFollowsActiveGeneration(t *testing.T) {
	masterA, masterB := mustGenerateMaster(t), mustGenerateMaster(t)
	s := &HubServer{hubSecret: masterA, keyGenerations: legacyGenerationSet(masterA)}
	before := s.ssoSigningSeed()
	if before != SSOSigningSeedFromMaster(masterA) {
		t.Fatal("un-rotated hub must sign exactly as the single-master hub did")
	}

	s.keyGenerations = s.keyGenerations.rotate(masterB, time.Now(), defaultVerifyWindow)
	checks := map[string][2]string{
		"sso seed":        {s.ssoSigningSeed(), deriveDomainKey(masterB, infoSSOEd25519Seed)},
		"session seed":    {s.sessionSigningSeed(), deriveDomainKey(masterB, infoSessionEd25519Seed)},
		"impersonate":     {s.impersonateKey(), deriveDomainKey(masterB, infoImpersonateKey)},
		"heartbeat":       {s.heartbeatKey(), deriveDomainKey(masterB, infoHeartbeatKey)},
		"session hmac":    {s.sessionKey(), deriveDomainKey(masterB, infoSessionKey)},
		"per-hive bearer": {s.heartbeatKeyFor("hive-x"), derivePerHiveKey(masterB, infoHeartbeatKey, "hive-x")},
	}
	for name, c := range checks {
		if c[0] == "" || c[0] != c[1] {
			t.Errorf("%s does not derive from the active generation after a rotation", name)
		}
	}

	bare := &HubServer{hubSecret: masterA}
	if bare.mintingSecret() != masterA {
		t.Error("a hub with no generation set must fall back to s.hubSecret")
	}
	var nilHub *HubServer
	if nilHub.mintingSecret() != "" {
		t.Error("a nil hub must have no minting secret")
	}
}

// TestLiveGenerationsRootLifecycle covers the lineage root that keeps the
// derivation set authoritative after retirement, and that it does not make a
// set from a DIFFERENT master authoritative.
func TestLiveGenerationsRootLifecycle(t *testing.T) {
	withCleanLiveGenerations(t)
	masterA, masterB, other := mustGenerateMaster(t), mustGenerateMaster(t), mustGenerateMaster(t)
	withTestMaster(t, masterA)

	// After retirement the set holds only B; the ambient master is still A.
	retiredSet := newGenerationSet(2, []keyGeneration{{ID: 2, Secret: masterB}})

	installLiveGenerations(retiredSet, masterA)
	if provisionGenerationSet() != retiredSet {
		t.Fatal("a set loaded against the ambient master must stay authoritative after its root generation retires")
	}
	advanceLiveGenerations(retiredSet)
	if provisionGenerationSet() != retiredSet {
		t.Fatal("advancing the live set must keep the recorded root")
	}

	// A set with no recorded root and without the ambient master is stale.
	setLiveGenerations(retiredSet, false)
	if provisionGenerationSet() == retiredSet {
		t.Fatal("setLiveGenerations must not carry a root over to a set it did not load")
	}

	// A root that is not the ambient master grants nothing.
	installLiveGenerations(retiredSet, other)
	if provisionGenerationSet() == retiredSet {
		t.Fatal("a set loaded against a different master must not be trusted")
	}

	// Installing or advancing to nil clears the root.
	installLiveGenerations(nil, masterA)
	advanceLiveGenerations(nil)
	liveGenerationsMu.RLock()
	root := liveGenerationsRoot
	liveGenerationsMu.RUnlock()
	if root != "" {
		t.Error("a nil live set must not keep a root")
	}
	installLiveGenerations(retiredSet, masterA)
	advanceLiveGenerations(nil)
	liveGenerationsMu.RLock()
	root = liveGenerationsRoot
	liveGenerationsMu.RUnlock()
	if root != "" {
		t.Error("advancing to a nil set must clear the root")
	}
}

// TestUnsweptSpokeKeepsWorkingDuringOverlap: a spoke the sweep has not patched
// yet holds only the previous generation's public key. Per-hive artifacts for
// it (SSO handoff, heartbeat signature) are signed with the generation the
// sweep last observed it on while that generation is still acceptable, and
// with the current generation once it is not.
func TestUnsweptSpokeKeepsWorkingDuringOverlap(t *testing.T) {
	withTempGenerationsPath(t)
	withCleanLiveGenerations(t)
	masterA := mustGenerateMaster(t)
	s := newRotationTestHub(t, masterA)
	const hiveID = "hive-unswept"
	prevPub := ssoPublicKeyFromSeed(SSOSigningSeedFromMaster(masterA))
	s.recordPerHiveEnvObservation(hiveID, perHiveEnvObservation{Generation: legacyGenerationID, Observed: time.Now()})

	if _, _, err := s.rotateMasterSecret(time.Now(), false); err != nil {
		t.Fatalf("rotateMasterSecret: %v", err)
	}
	gs := s.currentGenerations()
	curPub := ssoPublicKeyFromSeed(deriveDomainKey(gs.currentSecret(), infoSSOEd25519Seed))

	unswept := []string{prevPub}
	swept := []string{curPub, prevPub}
	accepts := func(keys []string, tok string) bool {
		_, _, _, err := hubspoke.VerifySSOTokenAcrossKeys(keys, tok, hiveID, time.Now())
		return err == nil
	}

	tok := mintSSOFor(t, s, hiveID)
	if !accepts(unswept, tok) {
		t.Error("overlap: an unswept spoke rejected the hub's SSO handoff")
	}
	if !accepts(swept, tok) {
		t.Error("overlap: a just-swept spoke (current + _PREV) rejected the hub's SSO handoff")
	}
	sp := rotationSpoke{ssoKeys: unswept, hb: spoke.NewVerifier(spoke.ModeEnforce, nil)}
	rec := httptest.NewRecorder()
	resp := &HeartbeatResponse{OK: true}
	s.signHeartbeatResponse(rec, resp, hiveID)
	body, _ := json.Marshal(resp)
	if got := sp.hb.Verify(sp.ssoKeys, hiveID, body, rec.Header().Get(spoke.SigHeader), time.Now()); !got.Accepted || !got.Signed {
		t.Errorf("overlap: an unswept spoke rejected the hub's heartbeat signature: %+v", got)
	}

	// Once the sweep observes the spoke on the current generation, the hub
	// signs with it, and a spoke still holding only the old key would fail:
	// the selection really moves.
	s.recordPerHiveEnvObservation(hiveID, perHiveEnvObservation{Generation: gs.Current, Observed: time.Now()})
	tok = mintSSOFor(t, s, hiveID)
	if accepts(unswept, tok) || !accepts([]string{curPub}, tok) {
		t.Error("a spoke observed on the current generation must get a current-generation signature")
	}

	// Past verify_until the previous generation is never used, even for a
	// spoke still observed on it.
	s.recordPerHiveEnvObservation(hiveID, perHiveEnvObservation{Generation: legacyGenerationID, Observed: time.Now()})
	afterWindow := time.Now().Add(defaultVerifyWindow + time.Hour)
	if got, want := s.ssoSigningSeedForHive(hiveID, afterWindow), deriveDomainKey(gs.currentSecret(), infoSSOEd25519Seed); got != want {
		t.Error("after verify_until the hub must sign with the current generation")
	}
	if _, err := s.retireExpiredGenerations(afterWindow); err != nil {
		t.Fatalf("retireExpiredGenerations: %v", err)
	}
	tok = mintSSOFor(t, s, hiveID)
	if accepts(unswept, tok) {
		t.Error("after retirement the hub signed with the retired generation")
	}

	// Hand-built hubs with no generation set keep the single-master seed.
	bare := &HubServer{hubSecret: masterA}
	if bare.ssoSigningSeedForHive(hiveID, time.Now()) != SSOSigningSeedFromMaster(masterA) {
		t.Error("a hub with no generation set must sign with s.hubSecret")
	}
	var nilHub *HubServer
	if nilHub.ssoSigningSeedForHive(hiveID, time.Now()) != "" || nilHub.observedSpokeGeneration(hiveID) != 0 {
		t.Error("a nil hub must sign nothing")
	}
}

// writeSaaSHiveRecord seeds a hive's meta.json with only a creation time.
func writeSaaSHiveRecord(t *testing.T, hiveID string, created time.Time) {
	t.Helper()
	dir := filepath.Join(saasHivesDir, hiveID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(SaaSHive{CreatedAt: created.UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestUnobservedHiveDuringOverlap covers the window after a hub restart,
// before the first sweep has attributed each hive: a hive provisioned before
// the rotation gets the previous generation (the key an unswept spoke holds,
// and a swept one holds as _PREV), a hive provisioned after it gets the
// current one, and a hive with no record gets the current one.
func TestUnobservedHiveDuringOverlap(t *testing.T) {
	withTempGenerationsPath(t)
	withCleanLiveGenerations(t)
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })

	masterA := mustGenerateMaster(t)
	s := newRotationTestHub(t, masterA)
	rotatedAt := time.Now()
	if _, _, err := s.rotateMasterSecret(rotatedAt, false); err != nil {
		t.Fatalf("rotateMasterSecret: %v", err)
	}
	gs := s.currentGenerations()
	prevSeed := SSOSigningSeedFromMaster(masterA)
	curSeed := deriveDomainKey(gs.currentSecret(), infoSSOEd25519Seed)

	writeSaaSHiveRecord(t, "hive-old", rotatedAt.Add(-24*time.Hour))
	writeSaaSHiveRecord(t, "hive-new", rotatedAt.Add(time.Hour))
	now := time.Now()
	if s.ssoSigningSeedForHive("hive-old", now) != prevSeed {
		t.Error("an unobserved hive provisioned before the rotation must get the previous generation during the overlap")
	}
	if s.ssoSigningSeedForHive("hive-new", now) != curSeed {
		t.Error("a hive provisioned after the rotation must get the current generation")
	}
	if s.ssoSigningSeedForHive("hive-missing", now) != curSeed {
		t.Error("a hive with no record must get the current generation")
	}
	afterWindow := now.Add(defaultVerifyWindow + time.Hour)
	if s.ssoSigningSeedForHive("hive-old", afterWindow) != curSeed {
		t.Error("after verify_until no hive may get the previous generation")
	}

	// Unknown creation times keep the default.
	writeSaaSHiveRecord(t, "hive-bad", rotatedAt)
	if err := os.WriteFile(filepath.Join(saasHivesDir, "hive-bad", "meta.json"), []byte(`{"created_at":"not-a-time"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if hiveProvisionedBefore("hive-bad", gs) {
		t.Error("an unparseable creation time must not count as provisioned before")
	}
	noCreated := newGenerationSet(gs.Current, []keyGeneration{{ID: gs.Current, Secret: gs.currentSecret()}})
	if hiveProvisionedBefore("hive-old", noCreated) {
		t.Error("a current generation with no Created stamp must not select the previous one")
	}
}
