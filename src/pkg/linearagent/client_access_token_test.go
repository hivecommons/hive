package linearagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests cover Client.AccessToken — the path that exports the workspace
// OAuth token for injection into ISSUES_ONLY+ agents as LINEAR_ACCESS_TOKEN —
// plus the SetClock seam and the liveToken refresh-failure branches that
// CreateActivity-based tests do not reach.

func TestClient_AccessToken_ReturnsStoredLiveToken(t *testing.T) {
	api := newFakeLinearAPI(t)
	c, _ := newTestClient(t, api, "", Token{AccessToken: "at-live", ExpiresAt: time.Now().Add(time.Hour)})

	tok, err := c.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if tok != "at-live" {
		t.Errorf("token = %q, want at-live", tok)
	}
	if len(api.requests) != 0 {
		t.Errorf("live token must not hit GraphQL, got %d requests", len(api.requests))
	}
}

func TestClient_AccessToken_NotInstalled(t *testing.T) {
	api := newFakeLinearAPI(t)
	store, err := NewStore(filepath.Join(t.TempDir(), "l.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	c := NewClient(store, Credentials{}, api.srv.Client(), "", api.srv.URL, quietLogger())

	tok, err := c.AccessToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("err = %v", err)
	}
	if tok != "" {
		t.Errorf("token = %q, want empty on error", tok)
	}
}

func TestClient_AccessToken_EmptyStoredToken(t *testing.T) {
	api := newFakeLinearAPI(t)
	c, _ := newTestClient(t, api, "", Token{})

	_, err := c.AccessToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no access token") {
		t.Fatalf("err = %v", err)
	}
}

func TestClient_AccessToken_RefreshesAndPersists(t *testing.T) {
	api := newFakeLinearAPI(t)
	tokens := newFakeTokenServer(t)
	c, store := newTestClient(t, api, tokens.srv.URL,
		Token{AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: time.Now().Add(-time.Minute)})

	tok, err := c.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if tok != "at-1" {
		t.Errorf("token = %q, want refreshed at-1", tok)
	}
	if len(tokens.forms) != 1 || tokens.forms[0].Get("refresh_token") != "rt-old" {
		t.Fatalf("refresh forms = %v", tokens.forms)
	}
	inst, _ := store.Get()
	if inst.Token.AccessToken != "at-1" || inst.Token.RefreshToken != "rt-1" {
		t.Errorf("persisted token = %+v, want refreshed grant stored", inst.Token)
	}
}

func TestClient_AccessToken_RefreshFailureIsError(t *testing.T) {
	api := newFakeLinearAPI(t)
	tokens := newFakeTokenServer(t)
	tokens.respond = func(w http.ResponseWriter, _ url.Values) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}
	c, store := newTestClient(t, api, tokens.srv.URL,
		Token{AccessToken: "at-old", RefreshToken: "rt-bad", ExpiresAt: time.Now().Add(-time.Minute)})

	tok, err := c.AccessToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refresh linear token") {
		t.Fatalf("err = %v, want refresh failure", err)
	}
	if tok != "" {
		t.Errorf("token = %q, want empty on refresh failure", tok)
	}
	// The stale grant must not be clobbered by a failed refresh.
	inst, _ := store.Get()
	if inst.Token.RefreshToken != "rt-bad" {
		t.Errorf("stored refresh token = %q, want rt-bad kept", inst.Token.RefreshToken)
	}
}

func TestClient_SetClock_DrivesRefreshDecision(t *testing.T) {
	api := newFakeLinearAPI(t)
	tokens := newFakeTokenServer(t)
	expiry := time.Now().Add(time.Hour)
	c, _ := newTestClient(t, api, tokens.srv.URL,
		Token{AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: expiry})

	// Clock well before expiry: no refresh.
	c.SetClock(func() time.Time { return expiry.Add(-30 * time.Minute) })
	tok, err := c.AccessToken(context.Background())
	if err != nil || tok != "at-old" {
		t.Fatalf("before expiry: tok=%q err=%v", tok, err)
	}
	if len(tokens.forms) != 0 {
		t.Fatalf("no refresh expected, got %d", len(tokens.forms))
	}

	// Clock inside the refresh skew (default one minute): refresh fires.
	c.SetClock(func() time.Time { return expiry.Add(-time.Second) })
	tok, err = c.AccessToken(context.Background())
	if err != nil || tok != "at-1" {
		t.Fatalf("inside skew: tok=%q err=%v", tok, err)
	}
	if len(tokens.forms) != 1 {
		t.Fatalf("refresh expected, got %d", len(tokens.forms))
	}
}

func TestClient_AccessToken_PersistFailureStillReturnsFreshToken(t *testing.T) {
	api := newFakeLinearAPI(t)
	tokens := newFakeTokenServer(t)
	tokens.respond = func(w http.ResponseWriter, _ url.Values) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "at-fresh", "refresh_token": "rt-fresh", "expires_in": 86399,
		})
	}
	path := filepath.Join(t.TempDir(), "l.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	inst := testInstall()
	inst.Token = Token{AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: time.Now().Add(-time.Minute)}
	if err := store.Set(inst); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Break persistence: saveLocked writes path+".tmp" first; a directory
	// squatting there makes the write fail while the in-memory refresh works.
	if err := os.Mkdir(path+".tmp", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	c := NewClient(store, Credentials{ClientID: "cid", ClientSecret: "cs"}, api.srv.Client(), tokens.srv.URL, api.srv.URL, quietLogger())

	tok, err := c.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v — a persist failure must not drop the fresh token", err)
	}
	if tok != "at-fresh" {
		t.Errorf("token = %q, want at-fresh", tok)
	}
}
