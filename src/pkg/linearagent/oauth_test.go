package linearagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBuildAuthorizeURL(t *testing.T) {
	got := BuildAuthorizeURL("cid-1", "https://hive.example/linear/callback", "st4te")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.HasPrefix(got, AuthorizeURL+"?") {
		t.Fatalf("authorize URL base = %q", got)
	}
	q := u.Query()
	want := map[string]string{
		"client_id":     "cid-1",
		"redirect_uri":  "https://hive.example/linear/callback",
		"response_type": "code",
		"scope":         "read,write,app:assignable,app:mentionable",
		"actor":         "app",
		"prompt":        "consent",
		"state":         "st4te",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
}

func TestCredentialsFromEnv(t *testing.T) {
	t.Setenv("LINEAR_CLIENT_ID", " cid ")
	t.Setenv("LINEAR_CLIENT_SECRET", "sec")
	c := CredentialsFromEnv()
	if c.ClientID != "cid" || c.ClientSecret != "sec" {
		t.Fatalf("creds = %+v", c)
	}
	if !c.Configured() {
		t.Error("Configured() = false with both set")
	}
	if (Credentials{ClientID: "cid"}).Configured() {
		t.Error("Configured() = true with missing secret")
	}
	if (Credentials{ClientSecret: "s"}).Configured() {
		t.Error("Configured() = true with missing id")
	}
}

// fakeTokenServer records the token-endpoint form posts it receives — the
// recording-fake pattern from the TokenReview mint work (#4517).
type fakeTokenServer struct {
	srv   *httptest.Server
	forms []url.Values
	// respond overrides the next response; nil serves a default grant.
	respond func(w http.ResponseWriter, form url.Values)
}

func newFakeTokenServer(t *testing.T) *fakeTokenServer {
	t.Helper()
	f := &fakeTokenServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("token POST Content-Type = %q", ct)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		f.forms = append(f.forms, r.PostForm)
		if f.respond != nil {
			f.respond(w, r.PostForm)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "at-1",
			"refresh_token": "rt-1",
			"expires_in":    86399,
			"scope":         "read,write",
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestExchangeCode(t *testing.T) {
	f := newFakeTokenServer(t)
	creds := Credentials{ClientID: "cid", ClientSecret: "csec"}
	tok, err := ExchangeCode(context.Background(), f.srv.Client(), f.srv.URL, creds, "the-code", "https://h/cb")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok.AccessToken != "at-1" || tok.RefreshToken != "rt-1" || tok.Scope != "read,write" {
		t.Errorf("token = %+v", tok)
	}
	if tok.ExpiresAt.Before(time.Now().Add(23 * time.Hour)) {
		t.Errorf("ExpiresAt too soon: %v", tok.ExpiresAt)
	}
	form := f.forms[0]
	for k, v := range map[string]string{
		"grant_type": "authorization_code", "code": "the-code",
		"redirect_uri": "https://h/cb", "client_id": "cid", "client_secret": "csec",
	} {
		if form.Get(k) != v {
			t.Errorf("form %s = %q, want %q", k, form.Get(k), v)
		}
	}
}

func TestRefreshToken(t *testing.T) {
	f := newFakeTokenServer(t)
	creds := Credentials{ClientID: "cid", ClientSecret: "csec"}
	tok, err := RefreshToken(context.Background(), f.srv.Client(), f.srv.URL, creds, "rt-old")
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if tok.AccessToken != "at-1" {
		t.Errorf("token = %+v", tok)
	}
	form := f.forms[0]
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "rt-old" {
		t.Errorf("form = %v", form)
	}
}

func TestExchangeCode_ErrorResponse(t *testing.T) {
	f := newFakeTokenServer(t)
	f.respond = func(w http.ResponseWriter, _ url.Values) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "code expired"})
	}
	_, err := ExchangeCode(context.Background(), f.srv.Client(), f.srv.URL, Credentials{}, "c", "r")
	if err == nil || !strings.Contains(err.Error(), "invalid_grant: code expired") {
		t.Fatalf("err = %v", err)
	}
}

func TestExchangeCode_UnparseableResponse(t *testing.T) {
	f := newFakeTokenServer(t)
	f.respond = func(w http.ResponseWriter, _ url.Values) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html>oops</html>"))
	}
	_, err := ExchangeCode(context.Background(), f.srv.Client(), f.srv.URL, Credentials{}, "c", "r")
	if err == nil || !strings.Contains(err.Error(), "unparseable") {
		t.Fatalf("err = %v", err)
	}
}

func TestExchangeCode_EmptyAccessToken(t *testing.T) {
	f := newFakeTokenServer(t)
	f.respond = func(w http.ResponseWriter, _ url.Values) {
		json.NewEncoder(w).Encode(map[string]string{})
	}
	_, err := ExchangeCode(context.Background(), f.srv.Client(), f.srv.URL, Credentials{}, "c", "r")
	if err == nil || !strings.Contains(err.Error(), "no access token") {
		t.Fatalf("err = %v", err)
	}
}

func TestExchangeCode_UnreachableEndpoint(t *testing.T) {
	_, err := ExchangeCode(context.Background(), &http.Client{Timeout: time.Second},
		"http://127.0.0.1:1/token", Credentials{}, "c", "r")
	if err == nil {
		t.Fatal("expected connection error")
	}
}

// TestScopeString covers the pre-Dec-2023 array shape beside the modern string.
func TestScopeString(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`"read,write"`, "read,write"},
		{`["read","write"]`, "read,write"},
		{``, ""},
		{`42`, ""},
	}
	for _, c := range cases {
		if got := scopeString(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("scopeString(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestStateStore_SingleUse(t *testing.T) {
	s := NewStateStore(StateTTL)
	state, err := s.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(state) != 48 {
		t.Errorf("state length = %d, want 48 hex chars", len(state))
	}
	if !s.Consume(state) {
		t.Fatal("first Consume = false")
	}
	if s.Consume(state) {
		t.Fatal("replayed Consume = true — state must be single-use")
	}
	if s.Consume("never-issued") {
		t.Fatal("unknown state accepted")
	}
}

func TestStateStore_Expiry(t *testing.T) {
	s := NewStateStore(10 * time.Minute)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	state, err := s.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	now = now.Add(10*time.Minute + time.Second)
	if s.Consume(state) {
		t.Fatal("expired state accepted")
	}

	// Expired states are also evicted on the next Create, not retained.
	old, _ := s.Create()
	now = now.Add(11 * time.Minute)
	if _, err := s.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.mu.Lock()
	_, stillThere := s.pending[old]
	s.mu.Unlock()
	if stillThere {
		t.Error("expired state not evicted by Create")
	}
}
