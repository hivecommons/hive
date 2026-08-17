package hub

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// AUDIT F10 regressions.
//
// The finding: the v2 signature covers only the username, so the cookie's
// lifetime is whatever the BROWSER chooses to honour, and logout only deletes
// the browser's copy. A captured cookie therefore outlives both its MaxAge and
// its own logout.
//
// Every test here that asserts a REJECTION is paired with a POSITIVE CONTROL
// that asserts acceptance. A rejection-only suite passes just as green against a
// verifier that rejects everything, which is precisely how the original finding
// stayed invisible.

// f10TestSeed is a deterministic 32-byte Ed25519 seed. Test-only key material.
const f10TestSeed = "4f2c9a1b7e3d5068a4c2e6f81937bd50a1c3e5f7092b4d6688aa11cc33ee5577"

func f10PubKey(t *testing.T) string {
	t.Helper()
	pub := ssoPublicKeyFromSeed(f10TestSeed)
	if pub == "" {
		t.Fatal("could not derive public key from test seed")
	}
	return pub
}

// TestF10_V3CookieCarriesSignedLifetimeAndSession is the core of the finding:
// the signed bytes must actually contain iat/exp/sid. Against unfixed code there
// is no v3 minter at all, so this cannot compile — which is the strongest form
// of "fails against the unfixed code".
func TestF10_V3CookieCarriesSignedLifetimeAndSession(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	value, sid := mintHubUserCookieValueV3(f10TestSeed, "clubanderson", now, time.Hour)
	if value == "" || sid == "" {
		t.Fatal("mint returned empty value/sid")
	}
	if !strings.Contains(value, hubCookieV3Marker) {
		t.Fatalf("v3 cookie missing %q marker: %q", hubCookieV3Marker, value)
	}
	body := value[:strings.LastIndex(value, hubCookieV3Marker)]
	raw, err := hubCookieB64.DecodeString(body)
	if err != nil {
		t.Fatalf("payload is not base64url: %v", err)
	}
	var claims hubCookieClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("payload is not the frozen JSON shape: %v", err)
	}
	if claims.U != "clubanderson" {
		t.Errorf("u = %q, want clubanderson", claims.U)
	}
	if claims.IAT != now.Unix() {
		t.Errorf("iat = %d, want %d", claims.IAT, now.Unix())
	}
	if claims.EXP != now.Add(time.Hour).Unix() {
		t.Errorf("exp = %d, want %d", claims.EXP, now.Add(time.Hour).Unix())
	}
	if claims.SID != sid {
		t.Errorf("sid in payload = %q, want %q", claims.SID, sid)
	}
	// Frozen wire contract shared with the Node proxy. A rename here without the
	// matching rename there 401s every hosted login at deploy, production-only.
	for _, key := range []string{`"u"`, `"iat"`, `"exp"`, `"sid"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("payload JSON missing frozen key %s: %s", key, raw)
		}
	}
}

// TestF10_V3VerificationBoundaries table-tests every way a v3 cookie can be
// bad, and — critically — the ways it must still be GOOD.
func TestF10_V3VerificationBoundaries(t *testing.T) {
	pub := f10PubKey(t)
	now := time.Unix(1_700_000_000, 0)

	valid, validSID := mintHubUserCookieValueV3(f10TestSeed, "alice", now, time.Hour)
	expired, _ := mintHubUserCookieValueV3(f10TestSeed, "alice", now.Add(-2*time.Hour), time.Hour)
	future, _ := mintHubUserCookieValueV3(f10TestSeed, "alice", now.Add(time.Hour), time.Hour)

	// Tampered payload: re-encode claims with a different username but keep the
	// original signature. This is the impersonation attempt the signature exists
	// to stop.
	tamperedPayload := func() string {
		idx := strings.LastIndex(valid, hubCookieV3Marker)
		raw, _ := hubCookieB64.DecodeString(valid[:idx])
		var c hubCookieClaims
		_ = json.Unmarshal(raw, &c)
		c.U = "clubanderson" // the hub admin
		forged, _ := json.Marshal(c)
		return hubCookieB64.EncodeToString(forged) + valid[idx:]
	}()

	// Tampered expiry only: extend exp far into the future, keep everything else.
	// Without a signature over the payload this would be a free session extension.
	tamperedExpiry := func() string {
		idx := strings.LastIndex(valid, hubCookieV3Marker)
		raw, _ := hubCookieB64.DecodeString(valid[:idx])
		var c hubCookieClaims
		_ = json.Unmarshal(raw, &c)
		c.EXP = now.Add(100 * 365 * 24 * time.Hour).Unix()
		forged, _ := json.Marshal(c)
		return hubCookieB64.EncodeToString(forged) + valid[idx:]
	}()

	tamperedSig := func() string {
		idx := strings.LastIndex(valid, hubCookieV3Marker)
		sig := []byte(valid[idx+len(hubCookieV3Marker):])
		if sig[0] == 'A' {
			sig[0] = 'B'
		} else {
			sig[0] = 'A'
		}
		return valid[:idx+len(hubCookieV3Marker)] + string(sig)
	}()

	revokeSID := func(target string) hubSessionRevokedFunc {
		return func(sid string) bool { return sid == target }
	}

	tests := []struct {
		name     string
		value    string
		now      time.Time
		revoked  hubSessionRevokedFunc
		wantUser string
		wantOK   bool
	}{
		// --- POSITIVE CONTROLS. Without these the suite would pass against a
		// verifier that simply returns false for everything.
		{"valid unexpired unrevoked verifies", valid, now, nil, "alice", true},
		{"valid with a revocation set that does not contain it", valid, now, revokeSID("someone-else"), "alice", true},
		{"valid just inside expiry", valid, now.Add(59 * time.Minute), nil, "alice", true},
		{"expired but within clock skew still verifies", valid, now.Add(time.Hour + 20*time.Second), nil, "alice", true},
		{"iat slightly ahead but within clock skew verifies", future, now.Add(time.Hour - 20*time.Second), nil, "alice", true},

		// --- REJECTIONS. Each is a distinct arm of the finding.
		{"expired cookie is rejected", expired, now, nil, "", false},
		{"expired well past skew is rejected", valid, now.Add(2 * time.Hour), nil, "", false},
		{"not-yet-valid (iat in the future) is rejected", future, now, nil, "", false},
		{"revoked sid is rejected despite a good signature", valid, now, revokeSID(validSID), "", false},
		{"tampered payload (username swap) is rejected", tamperedPayload, now, nil, "", false},
		{"tampered expiry is rejected", tamperedExpiry, now, nil, "", false},
		{"tampered signature is rejected", tamperedSig, now, nil, "", false},
		{"wrong marker (.v2.) is not parsed as v3", strings.Replace(valid, ".v3.", ".v2.", 1), now, nil, "", false},
		{"empty value", "", now, nil, "", false},
		{"marker only", ".v3.", now, nil, "", false},
		{"garbage payload", "!!!not-base64!!!.v3.sig", now, nil, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			user, ok := verifyHubUserCookieValueV3(pub, tc.value, tc.now, tc.revoked)
			if ok != tc.wantOK || user != tc.wantUser {
				t.Errorf("verify = (%q, %v), want (%q, %v)", user, ok, tc.wantUser, tc.wantOK)
			}
		})
	}
}

// TestF10_V3RejectsWrongKey proves the spoke-side split still holds: a cookie
// minted under a different seed does not verify. Without this, a "verifier" that
// ignored the key entirely would pass everything above.
func TestF10_V3RejectsWrongKey(t *testing.T) {
	const otherSeed = "1111111111111111111111111111111111111111111111111111111111111111"
	now := time.Unix(1_700_000_000, 0)
	value, _ := mintHubUserCookieValueV3(otherSeed, "alice", now, time.Hour)
	if value == "" {
		t.Fatal("mint failed")
	}
	if _, ok := verifyHubUserCookieValueV3(f10PubKey(t), value, now, nil); ok {
		t.Fatal("cookie minted under a foreign seed verified — key is not being checked")
	}
	// Positive control: it DOES verify under its own key.
	if _, ok := verifyHubUserCookieValueV3(ssoPublicKeyFromSeed(otherSeed), value, now, nil); !ok {
		t.Fatal("cookie failed to verify under its own key — test setup is wrong")
	}
}

// TestF10_MintFailsClosed: bad inputs must yield "" rather than a junk-signed
// cookie, matching mintHubUserCookieValueV2.
func TestF10_MintFailsClosed(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name string
		seed string
		user string
		ttl  time.Duration
	}{
		{"empty seed", "", "alice", time.Hour},
		{"empty username", f10TestSeed, "", time.Hour},
		{"non-hex seed", "zzzz", "alice", time.Hour},
		{"short seed", "abcd", "alice", time.Hour},
		{"zero ttl", f10TestSeed, "alice", 0},
		{"negative ttl", f10TestSeed, "alice", -time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if v, sid := mintHubUserCookieValueV3(tc.seed, tc.user, now, tc.ttl); v != "" || sid != "" {
				t.Errorf("minted %q/%q on bad input, want empty", v, sid)
			}
		})
	}
}

// TestF10_EitherLaneIsAdditive is the outage guard. Removing or shadowing the v2
// or legacy lane 401s every spoke that has not rolled — F1/F2 compatibility.
func TestF10_EitherLaneIsAdditive(t *testing.T) {
	pub := f10PubKey(t)
	const legacySecret = "legacy-hmac-secret"
	now := time.Unix(1_700_000_000, 0)

	v3, _ := mintHubUserCookieValueV3(f10TestSeed, "v3user", now, time.Hour)
	v2 := mintHubUserCookieValueV2(f10TestSeed, "v2user")
	legacy := mintHubUserCookieValue(legacySecret, "legacyuser")
	if v3 == "" || v2 == "" || legacy == "" {
		t.Fatal("test setup: a mint returned empty")
	}

	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{"v3 lane", v3, "v3user"},
		{"v2 lane still verifies", v2, "v2user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, ok := verifyHubUserCookieEitherAt(pub, legacySecret, tc.value, now, nil)
			if !ok || user != tc.want {
				t.Errorf("verify = (%q, %v), want (%q, true) — a compatibility lane was broken", user, ok, tc.want)
			}
		})
	}

	// AUDIT F1: this case previously asserted "legacy HMAC lane still verifies".
	// That ENCODED THE VULNERABILITY — the legacy lane is keyed on the fleet-wide
	// HIVE_SESSION_KEY, so accepting it means any spoke can forge hub admin. The
	// lane is deleted; the assertion is inverted rather than removed, so the file
	// still documents that the legacy format is deliberately refused.
	t.Run("legacy HMAC lane is REJECTED", func(t *testing.T) {
		if user, ok := verifyHubUserCookieEitherAt(pub, legacySecret, legacy, now, nil); ok {
			t.Errorf("legacy cookie accepted as %q — F1 lane is back", user)
		}
	})

	// An expired v3 must NOT silently fall through to the v2 or legacy lane and
	// get accepted there. That fallthrough would re-open the finding completely.
	expired, _ := mintHubUserCookieValueV3(f10TestSeed, "v3user", now.Add(-48*time.Hour), time.Hour)
	if user, ok := verifyHubUserCookieEitherAt(pub, legacySecret, expired, now, nil); ok {
		t.Errorf("expired v3 cookie was accepted as %q by a fallback lane", user)
	}

	// Likewise a revoked v3 must not fall through.
	revoked, sid := mintHubUserCookieValueV3(f10TestSeed, "v3user", now, time.Hour)
	rf := func(s string) bool { return s == sid }
	if user, ok := verifyHubUserCookieEitherAt(pub, legacySecret, revoked, now, rf); ok {
		t.Errorf("revoked v3 cookie was accepted as %q by a fallback lane", user)
	}
}

// TestF10_MintingHasFlippedToV3 — INVERTED, by this test's own instructions.
//
// It was TestF10_MintingHasNotFlipped, the anti-outage tripwire for the PR that
// shipped the v3 VERIFIER only: flipping the minter before the whole fleet
// verified v3 would have broken /terminal fleet-wide (incident #2773). Its
// comment said "when minting legitimately flips, this test is updated in the
// SAME PR, which forces the change to be seen." This is that PR. The fleet is
// v4 and src/proxy/server.js verifies v3 → v2 → legacy, so the precondition is
// met and the tripwire becomes its opposite: a guard that minting does not
// silently fall BACK to the non-revocable format.
//
// Note it also had a real defect worth not reproducing: it called
// mintHubUserCookieValueV2 directly, so it asserted what that function does,
// not what PRODUCTION mints — it could never have detected the flip it existed
// to catch. It now goes through mintSessionCookies, the actual login path.
func TestF10_MintingHasFlippedToV3(t *testing.T) {
	s := f10f15Server(t)
	mkUser(t, "alice")

	rec := httptest.NewRecorder()
	if !s.mintSessionCookies(rec, "github:alice") {
		t.Fatal("mintSessionCookies failed")
	}
	var value string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hive_hub_user" {
			value = c.Value
		}
	}
	if value == "" {
		t.Fatal("no session cookie minted")
	}
	if !hubCookieIsV3(value) {
		t.Fatal("the production minter no longer emits v3 — sessions would again have " +
			"no signed expiry and no revocable id (F10)")
	}
	if hubCookieIsV2(value) {
		t.Fatal("the production minter fell back to v2")
	}
}

// --- revocation store ------------------------------------------------------

func f10TestServer(t *testing.T) *HubServer {
	t.Helper()
	dir := t.TempDir()
	orig := revokedSessionsPath
	revokedSessionsPath = filepath.Join(dir, "saas", "hub-revoked-sessions.json")
	t.Cleanup(func() { revokedSessionsPath = orig })
	return &HubServer{
		logger:          slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		revokedSessions: newRevokedSessions(),
	}
}

// TestF10_RevocationSurvivesRestart is the second half of the finding. The hub
// rolls several times a day; an in-memory-only revocation set would un-revoke
// every logged-out session on the next roll, on a schedule an attacker can wait
// for.
func TestF10_RevocationSurvivesRestart(t *testing.T) {
	s := f10TestServer(t)
	// Anchored to the REAL clock, not a fixed timestamp: loadRevokedSessions
	// prunes against time.Now() on startup (that pruning is what bounds the
	// file), so a cookie dated 2023 would be correctly evicted on load and the
	// test would be asserting the wrong thing.
	now := time.Now()
	value, sid := mintHubUserCookieValueV3(f10TestSeed, "alice", now, 24*time.Hour)

	// Positive control BEFORE logout: the session is live.
	if _, ok := verifyHubUserCookieValueV3(f10PubKey(t), value, now, s.hubSessionRevokedLookup(now)); !ok {
		t.Fatal("freshly minted cookie did not verify — setup is wrong")
	}

	if !s.revokeHubSessionCookieAt(value, now) {
		t.Fatal("revokeHubSessionCookie reported no revocation")
	}
	if !s.revokedSessions.isRevoked(sid, now) {
		t.Fatal("session not revoked in memory")
	}

	// Simulate a hub restart: brand-new server object, same PVC path.
	restarted := &HubServer{logger: s.logger, revokedSessions: newRevokedSessions()}
	restarted.loadRevokedSessions()
	if !restarted.revokedSessions.isRevoked(sid, now) {
		t.Fatal("revocation did NOT survive restart — a logged-out cookie works again after a roll")
	}
	if _, ok := verifyHubUserCookieValueV3(f10PubKey(t), value, now, restarted.hubSessionRevokedLookup(now)); ok {
		t.Fatal("revoked cookie verified after restart")
	}

	// Positive control AFTER restart: an unrelated live session still works, so
	// the restart did not simply break all verification.
	other, _ := mintHubUserCookieValueV3(f10TestSeed, "bob", now, time.Hour)
	if _, ok := verifyHubUserCookieValueV3(f10PubKey(t), other, now, restarted.hubSessionRevokedLookup(now)); !ok {
		t.Fatal("an unrevoked session was rejected after restart")
	}
}

// TestF10_RevocationSetStaysBounded: entries evict at their own expiry, so the
// file cannot grow without limit on a long-lived hub.
func TestF10_RevocationSetStaysBounded(t *testing.T) {
	s := f10TestServer(t)
	now := time.Now()

	short, shortSID := mintHubUserCookieValueV3(f10TestSeed, "alice", now, time.Minute)
	long, longSID := mintHubUserCookieValueV3(f10TestSeed, "bob", now, 24*time.Hour)
	s.revokeHubSessionCookieAt(short, now)
	s.revokeHubSessionCookieAt(long, now)

	later := now.Add(time.Hour)
	if !s.revokedSessions.pruneExpired(later) {
		t.Fatal("prune reported no change though one entry was long expired")
	}
	if _, present := s.revokedSessions.snapshot()[shortSID]; present {
		t.Error("expired revocation entry was not evicted — the file grows unbounded")
	}
	if _, present := s.revokedSessions.snapshot()[longSID]; !present {
		t.Error("a still-live revocation entry was evicted — a revoked session came back")
	}
	// The evicted entry is safe to drop only because the cookie itself is now
	// expired. Confirm that, rather than assuming it.
	if _, ok := verifyHubUserCookieValueV3(f10PubKey(t), short, later, nil); ok {
		t.Error("evicted entry's cookie still verifies on expiry alone — eviction is unsafe")
	}

	// Expired entries are dropped at load time too, not just by prune.
	reloaded := newRevokedSessions()
	reloaded.load(map[string]int64{shortSID: now.Unix() - 1, longSID: now.Add(24 * time.Hour).Unix()}, later)
	if _, present := reloaded.snapshot()[shortSID]; present {
		t.Error("load kept an already-expired entry")
	}
	if _, present := reloaded.snapshot()[longSID]; !present {
		t.Error("load dropped a live entry")
	}
}

// TestF10_CorruptRevocationFileFailsClosed: a revocation set the hub cannot read
// must not be treated as empty. "Empty" means "nothing is revoked", which is the
// finding restored by an I/O error.
func TestF10_CorruptRevocationFileFailsClosed(t *testing.T) {
	s := f10TestServer(t)
	if err := os.MkdirAll(filepath.Dir(revokedSessionsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(revokedSessionsPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.loadRevokedSessions()

	if !s.revokedSessions.isRevoked("any-session-at-all", time.Now()) {
		t.Fatal("corrupt revocation file was treated as an empty set — revoked sessions silently work again")
	}
	if _, err := os.Stat(revokedSessionsPath + revokedSessionsQuarantineSuffix); err != nil {
		t.Errorf("corrupt file was not quarantined for inspection: %v", err)
	}

	// Positive control: a HEALTHY store does not fail closed. Without this the
	// test above would pass against a store that rejects everything always.
	healthy := f10TestServer(t)
	healthy.loadRevokedSessions() // missing file — normal
	if healthy.revokedSessions.isRevoked("any-session-at-all", time.Now()) {
		t.Fatal("a healthy (missing-file) store rejected an unrevoked session")
	}
}

// TestF10_RevokeIsNoOpForV2 documents the residual honestly: a v2 cookie carries
// no session ID, so logout cannot revoke it. This is WHY the finding is not
// closed until minting flips to v3, and the test exists so that fact cannot be
// quietly forgotten.
func TestF10_RevokeIsNoOpForV2(t *testing.T) {
	s := f10TestServer(t)
	v2 := mintHubUserCookieValueV2(f10TestSeed, "alice")
	if s.revokeHubSessionCookie(v2) {
		t.Fatal("claimed to revoke a v2 cookie, which carries no session ID")
	}
	// And the v3 case DOES revoke — positive control.
	v3, _ := mintHubUserCookieValueV3(f10TestSeed, "alice", time.Now(), time.Hour)
	if !s.revokeHubSessionCookie(v3) {
		t.Fatal("failed to revoke a v3 cookie")
	}
}
