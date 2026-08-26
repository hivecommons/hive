package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// AUDIT F10 (mint flip) and F15 (unauthenticated logout grows the revocation
// store without bound).
//
// F10: production minted V2 cookies, which carry only a username. Their lifetime
// lives entirely in the client-side MaxAge attribute — which a replaying
// attacker simply ignores — and they carry no session id, so /api/auth/logout
// had nothing to revoke and was a no-op against a captured cookie. V3 puts a
// signed exp and a random sid inside the signature. The spoke-side verifier
// (src/proxy/server.js, verifyHubUserCookieEither) already tries v3 → v2 →
// legacy, so the verifier shipped ahead of the minter and this is a cutover, not
// a flag day.
//
// F15: the logout route is deliberately unauthenticated (logging out must work
// when the session is already broken). It passed the RAW cookie value to
// revokeHubSessionCookie, so an anonymous attacker could POST arbitrary values
// and grow a PERSISTED store without bound, pinning entries with a forged
// far-future expiry.

// f10f15Server builds a hub whose revocation store points at a temp path.
func f10f15Server(t *testing.T) *HubServer {
	t.Helper()
	cleanup := helperSetupTempDirs(t)
	t.Cleanup(cleanup)
	s := newHandlerHub()
	if s.revokedSessions == nil {
		s.revokedSessions = newRevokedSessions()
	}
	prev := revokedSessionsPath
	revokedSessionsPath = filepath.Join(t.TempDir(), "revoked.json")
	t.Cleanup(func() { revokedSessionsPath = prev })
	return s
}

// TestF10ProductionMintsV3 is the regression: the cookie the login path actually
// sets must be v3, i.e. must carry a signed expiry and a revocable session id.
func TestF10ProductionMintsV3(t *testing.T) {
	s := f10f15Server(t)
	mkUser(t, "alice")

	rec := httptest.NewRecorder()
	if !s.mintSessionCookies(rec, httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/login", nil), "github:alice") {
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
		t.Errorf("production minted a non-v3 session cookie %q — no signed expiry and "+
			"no session id, so logout cannot revoke it (F10)", value)
	}
	// The two properties that make v3 worth minting, asserted directly rather
	// than inferred from the version marker.
	if sid := hubCookieSessionID(value); sid == "" {
		t.Error("minted cookie carries no session id — nothing for logout to revoke")
	}
	if exp := hubCookieExpiry(value); exp <= time.Now().Unix() {
		t.Errorf("minted cookie has no future signed expiry (exp=%d)", exp)
	}

	// POSITIVE CONTROL: it must still be a WORKING session. A mint that produced
	// an unverifiable cookie would satisfy every assertion above while locking
	// everyone out.
	if user, ok := s.verifyHubUserCookie(value); !ok || user != "github:alice" {
		t.Errorf("minted v3 cookie does not verify: (%q, %v)", user, ok)
	}
}

// TestF10SignedExpiryIsEnforcedNotAdvisory: the whole point of v3's exp is that
// it is checked by the verifier, unlike MaxAge which only the browser honours.
func TestF10SignedExpiryIsEnforcedNotAdvisory(t *testing.T) {
	s := f10f15Server(t)
	mkUser(t, "alice")
	rec := httptest.NewRecorder()
	if !s.mintSessionCookies(rec, httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/login", nil), "github:alice") {
		t.Fatal("mint failed")
	}
	var value string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hive_hub_user" {
			value = c.Value
		}
	}

	pub := s.sessionPublicKey()
	// Valid now (positive control)...
	if _, ok := verifyHubUserCookieEitherAt(pub, s.sessionKey(), value, time.Now(), nil); !ok {
		t.Fatal("freshly minted cookie did not verify")
	}
	// ...and refused past its signed expiry, even though the value is byte
	// identical and its signature is still perfectly valid. This is what a
	// replaying attacker cannot get around.
	future := time.Now().Add(cookieSessionTTL + time.Hour)
	if _, ok := verifyHubUserCookieEitherAt(pub, s.sessionKey(), value, future, nil); ok {
		t.Error("cookie still verified past its signed expiry — exp is not enforced (F10)")
	}
}

// TestF10LogoutActuallyRevokes ties the flip to the outcome F10 is about: after
// logout, the SAME cookie value stops working. Under v2 this was a no-op.
func TestF10LogoutActuallyRevokes(t *testing.T) {
	s := f10f15Server(t)
	mkUser(t, "alice")

	rec := httptest.NewRecorder()
	if !s.mintSessionCookies(rec, httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/login", nil), "github:alice") {
		t.Fatal("mint failed")
	}
	var value string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hive_hub_user" {
			value = c.Value
		}
	}

	// POSITIVE CONTROL: live before logout.
	if _, ok := s.verifyHubUserCookie(value); !ok {
		t.Fatal("cookie not valid before logout — setup is wrong")
	}

	out := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/auth/logout", nil)
	req.Header.Set("Origin", f4TrustedOrigin)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: value})
	s.handleLogout(out, req)

	if _, ok := s.verifyHubUserCookie(value); ok {
		t.Error("the captured cookie value STILL works after logout — logout is a no-op (F10)")
	}
}

// TestF15UnauthenticatedLogoutCannotGrowTheStore is the F15 regression. An
// anonymous attacker POSTing garbage (or forged) cookie values must not add a
// single entry to the persisted revocation store.
func TestF15UnauthenticatedLogoutCannotGrowTheStore(t *testing.T) {
	s := f10f15Server(t)

	before := len(s.revokedSessions.snapshot())

	// A spread of attacker-controlled shapes: junk, a well-formed-but-unsigned
	// v3 body, and a v3 cookie signed with the WRONG key.
	forged, _ := mintHubUserCookieValueV3(
		"00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		"attacker", time.Now(), 100000*time.Hour)

	for i, value := range []string{
		"garbage",
		"not.a.cookie",
		"eyJ1IjoiYXR0YWNrZXIifQ.v3.bm90YXNpZw",
		forged,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/auth/logout", nil)
		req.Header.Set("Origin", f4TrustedOrigin)
		req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: value})
		s.handleLogout(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("case %d: logout status = %d, want 200 (logout must always clear the browser copy)", i, rec.Code)
		}
	}

	if got := len(s.revokedSessions.snapshot()); got != before {
		t.Errorf("revocation store grew from %d to %d entries on UNVERIFIED cookies — "+
			"an anonymous attacker can grow the persisted store without bound (F15)", before, got)
	}

	// And nothing was persisted either.
	if data, err := os.ReadFile(revokedSessionsPath); err == nil && len(data) > 0 {
		var stored map[string]int64
		_ = json.Unmarshal(data, &stored)
		if len(stored) != 0 {
			t.Errorf("forged logouts wrote %d entries to the PVC (F15)", len(stored))
		}
	}
}

// TestF15LegitimateLogoutStillRevokes is the POSITIVE CONTROL for F15. Verifying
// before revoking must not break real logout — otherwise "revoke nothing" would
// pass the regression above and silently re-open F10.
func TestF15LegitimateLogoutStillRevokes(t *testing.T) {
	s := f10f15Server(t)
	mkUser(t, "alice")

	rec := httptest.NewRecorder()
	if !s.mintSessionCookies(rec, httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/login", nil), "github:alice") {
		t.Fatal("mint failed")
	}
	var value string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hive_hub_user" {
			value = c.Value
		}
	}
	sid := hubCookieSessionID(value)

	out := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/auth/logout", nil)
	req.Header.Set("Origin", f4TrustedOrigin)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: value})
	s.handleLogout(out, req)

	if !s.revokedSessions.isRevoked(sid, time.Now()) {
		t.Fatal("a LEGITIMATE logout did not revoke the session — verify-before-revoke " +
			"is too strict and has re-opened F10")
	}
	// Durability: the revocation must survive a hub roll, which is the entire
	// reason the store is on disk. A deferred-only flush would fail here.
	restarted := &HubServer{logger: s.logger, revokedSessions: newRevokedSessions()}
	restarted.loadRevokedSessions()
	if !restarted.revokedSessions.isRevoked(sid, time.Now()) {
		t.Error("revocation did not survive restart — write coalescing dropped durability")
	}
}

// TestF15ExpiryIsClampedToTheMintHorizon: an entry may not be stored further out
// than the longest session the hub will ever mint. Defence in depth behind
// verify-before-revoke, so the store's bound does not depend on its caller.
func TestF15ExpiryIsClampedToTheMintHorizon(t *testing.T) {
	r := newRevokedSessions()
	now := time.Now()

	absurd := now.Add(1000 * 24 * time.Hour).Unix()
	if !r.revoke("sid-far-future", absurd, now) {
		t.Fatal("revoke refused a new entry")
	}
	got := r.snapshot()["sid-far-future"]
	maxAllowed := now.Add(maxRevokedSessionExpiry).Unix()
	if got > maxAllowed {
		t.Errorf("stored expiry %d exceeds the mint horizon %d — a forged far-future exp "+
			"pins an entry for as long as the attacker likes (F15)", got, maxAllowed)
	}

	// POSITIVE CONTROL: a NORMAL expiry is stored unchanged, not clamped down to
	// something uselessly short (which would silently un-revoke sessions early).
	normal := now.Add(cookieSessionTTL / 2).Unix()
	if !r.revoke("sid-normal", normal, now) {
		t.Fatal("revoke refused a normal entry")
	}
	if got := r.snapshot()["sid-normal"]; got != normal {
		t.Errorf("normal expiry was altered: stored %d, want %d", got, normal)
	}
}

// TestF15StoreIsHardCapped: the entry count is bounded no matter what.
func TestF15StoreIsHardCapped(t *testing.T) {
	r := newRevokedSessions()
	now := time.Now()
	exp := now.Add(time.Hour).Unix()

	// Fill to the cap directly (cheaper than driving maxRevokedSessions HTTP
	// requests, and it is the store's invariant under test).
	r.mu.Lock()
	for i := 0; i < maxRevokedSessions; i++ {
		r.sid["sid-"+strconv.Itoa(i)] = exp
	}
	r.mu.Unlock()

	if r.revoke("one-too-many", exp, now) {
		t.Error("store accepted an entry beyond its hard cap (F15)")
	}
	if got := len(r.snapshot()); got > maxRevokedSessions {
		t.Errorf("store holds %d entries, cap is %d", got, maxRevokedSessions)
	}

	// POSITIVE CONTROL: at capacity, EXTENDING an existing entry must still
	// work. Refusing it would leave a session un-revoked, which is the unsafe
	// direction and exactly the wrong way to enforce a cap.
	if !r.revoke("sid-0", exp+3600, now) {
		t.Error("at capacity, extending an EXISTING revocation was refused — that leaves " +
			"a session live and is the unsafe direction")
	}
}
