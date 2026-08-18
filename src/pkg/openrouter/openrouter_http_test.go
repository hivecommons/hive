package openrouter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withURLs temporarily overrides the package endpoint vars for a test and
// restores them afterward.
func withURLs(t *testing.T, base, exchange, keyInfo string) {
	t.Helper()
	ob, oe, ok := BaseURL, KeyExchangeURL, KeyInfoURL
	BaseURL, KeyExchangeURL, KeyInfoURL = base, exchange, keyInfo
	t.Cleanup(func() { BaseURL, KeyExchangeURL, KeyInfoURL = ob, oe, ok })
}

func TestExchangeCode_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.Write([]byte(`{"key":"  sk-or-test-key  "}`))
	}))
	defer srv.Close()
	withURLs(t, BaseURL, srv.URL, KeyInfoURL)

	key, err := ExchangeCode("the-code", "the-verifier")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "sk-or-test-key" { // trimmed
		t.Fatalf("got key %q, want trimmed sk-or-test-key", key)
	}
}

func TestExchangeCode_MissingArgs(t *testing.T) {
	if _, err := ExchangeCode("", "v"); err == nil {
		t.Fatal("empty code must error")
	}
	if _, err := ExchangeCode("c", ""); err == nil {
		t.Fatal("empty verifier must error")
	}
}

func TestExchangeCode_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad code", http.StatusBadRequest)
	}))
	defer srv.Close()
	withURLs(t, BaseURL, srv.URL, KeyInfoURL)
	if _, err := ExchangeCode("c", "v"); err == nil {
		t.Fatal("non-200 must error")
	}
}

func TestExchangeCode_EmptyKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"key":""}`))
	}))
	defer srv.Close()
	withURLs(t, BaseURL, srv.URL, KeyInfoURL)
	if _, err := ExchangeCode("c", "v"); err == nil {
		t.Fatal("empty key in response must error")
	}
}

func TestExchangeCode_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()
	withURLs(t, BaseURL, srv.URL, KeyInfoURL)
	if _, err := ExchangeCode("c", "v"); err == nil {
		t.Fatal("undecodable response must error")
	}
}

func TestFetchCredit_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-or-x" {
			t.Errorf("missing/wrong bearer: %q", got)
		}
		w.Write([]byte(`{"data":{"label":"my key","limit":15,"limit_remaining":11.75,"usage":3.25}}`))
	}))
	defer srv.Close()
	withURLs(t, BaseURL, KeyExchangeURL, srv.URL)

	c, err := FetchCredit("sk-or-x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Label != "my key" || c.Usage != 3.25 || c.Limit == nil || *c.Limit != 15 || c.LimitRemaining == nil || *c.LimitRemaining != 11.75 {
		t.Fatalf("unexpected credit: %+v", c)
	}
}

func TestFetchCredit_NoKey(t *testing.T) {
	if _, err := FetchCredit(""); err == nil {
		t.Fatal("empty key must error")
	}
}

func TestFetchCredit_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()
	withURLs(t, BaseURL, KeyExchangeURL, srv.URL)
	if _, err := FetchCredit("k"); err == nil {
		t.Fatal("401 must error")
	}
}

func TestFetchCredit_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{oops`))
	}))
	defer srv.Close()
	withURLs(t, BaseURL, KeyExchangeURL, srv.URL)
	if _, err := FetchCredit("k"); err == nil {
		t.Fatal("bad json must error")
	}
}

func TestFetchCredit_UnlimitedNullFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"limit":null,"limit_remaining":null,"usage":0}}`))
	}))
	defer srv.Close()
	withURLs(t, BaseURL, KeyExchangeURL, srv.URL)
	c, err := FetchCredit("k")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Limit != nil || c.LimitRemaining != nil {
		t.Fatalf("null limit fields should be nil pointers: %+v", c)
	}
}

func TestPublicModelIDs_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			t.Errorf("expected /models path, got %s", r.URL.Path)
		}
		w.Write([]byte(`{"data":[{"id":"anthropic/claude-opus-4.8"},{"id":""},{"id":"deepseek/deepseek-v3"}]}`))
	}))
	defer srv.Close()
	withURLs(t, srv.URL, KeyExchangeURL, KeyInfoURL)

	ids := PublicModelIDs()
	if len(ids) != 2 { // the empty id is skipped
		t.Fatalf("got %v, want 2 non-empty ids", ids)
	}
	if ids[0] != "anthropic/claude-opus-4.8" || ids[1] != "deepseek/deepseek-v3" {
		t.Fatalf("unexpected ids: %v", ids)
	}
}

func TestPublicModelIDs_HTTPErrorReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	withURLs(t, srv.URL, KeyExchangeURL, KeyInfoURL)
	if ids := PublicModelIDs(); ids != nil {
		t.Fatalf("non-200 must return nil, got %v", ids)
	}
}

func TestPublicModelIDs_BadJSONReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`nope`))
	}))
	defer srv.Close()
	withURLs(t, srv.URL, KeyExchangeURL, KeyInfoURL)
	if ids := PublicModelIDs(); ids != nil {
		t.Fatalf("bad json must return nil, got %v", ids)
	}
}

func TestStateStore_CreateEvictsExpired(t *testing.T) {
	ss := NewStateStore(10 * time.Minute)
	base := time.Unix(1_700_000_000, 0)
	ss.SetClock(func() time.Time { return base })

	// Seed an already-expired flow directly, then Create should evict it.
	ss.mu.Lock()
	ss.flows["stale"] = PendingFlow{HiveID: "old", CreatedAt: base.Add(-time.Hour)}
	ss.mu.Unlock()

	if _, err := ss.Create("v", "hiveA", "model-x"); err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	ss.mu.Lock()
	_, staleStillThere := ss.flows["stale"]
	n := len(ss.flows)
	ss.mu.Unlock()
	if staleStillThere {
		t.Fatal("Create should have evicted the expired 'stale' flow")
	}
	if n != 1 {
		t.Fatalf("expected exactly the fresh flow, got %d flows", n)
	}
}

func TestStateStore_NowDefaultClock(t *testing.T) {
	ss := NewStateStore(time.Minute)
	ss.nowFunc = nil // force the default-clock branch
	if got := ss.now(); got.IsZero() {
		t.Fatal("default clock returned zero time")
	}
}

func TestQRPNG_EmptyErrors(t *testing.T) {
	if _, err := QRPNG("   "); err == nil {
		t.Fatal("blank text must error")
	}
}

func TestBuildAuthorizeURL_Success(t *testing.T) {
	u, err := BuildAuthorizeURL("https://spoke.example/or/callback", "chal", "st8")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(u, AuthURL+"?") {
		t.Fatalf("url missing authorize prefix: %s", u)
	}
	// state must round-trip both on the top-level authorize param and inside callback_url
	if !strings.Contains(u, "state=st8") {
		t.Fatalf("state not present: %s", u)
	}
	if !strings.Contains(u, "code_challenge_method=S256") {
		t.Fatalf("challenge method missing: %s", u)
	}
}

func TestBuildAuthorizeURL_InvalidCallback(t *testing.T) {
	// A control character makes url.Parse fail.
	if _, err := BuildAuthorizeURL("http://\x7f\x00/bad", "chal", "st"); err == nil {
		t.Fatal("invalid callback url must error")
	}
}

// NOTE: the crypto/rand failure branches in GeneratePKCE/generateState are not
// unit-testable on Go 1.25+ — rand.Read panics fatally on a reader error
// (go.dev/issue/66821) rather than returning it, so those `if err != nil`
// branches are unreachable defensive code and intentionally left uncovered.

func TestGeneratePKCE_Success(t *testing.T) {
	v, c, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v == "" || c == "" {
		t.Fatal("verifier and challenge must be non-empty")
	}
	if c != ChallengeFor(v) {
		t.Fatal("challenge must equal ChallengeFor(verifier)")
	}
}

// badURL contains a control char that makes http.NewRequest fail to parse,
// exercising the request-build error branches.
const badURL = "http://\x7f\x00-invalid"

// closedURL points at a port nothing listens on so client.Do fails fast,
// exercising the network-error branches.
const closedURL = "http://127.0.0.1:1"

func TestExchangeCode_RequestBuildError(t *testing.T) {
	withURLs(t, BaseURL, badURL, KeyInfoURL)
	if _, err := ExchangeCode("c", "v"); err == nil {
		t.Fatal("unparseable KeyExchangeURL must error")
	}
}

func TestExchangeCode_NetworkError(t *testing.T) {
	withURLs(t, BaseURL, closedURL, KeyInfoURL)
	if _, err := ExchangeCode("c", "v"); err == nil {
		t.Fatal("connection failure must error")
	}
}

func TestFetchCredit_RequestBuildError(t *testing.T) {
	withURLs(t, BaseURL, KeyExchangeURL, badURL)
	if _, err := FetchCredit("k"); err == nil {
		t.Fatal("unparseable KeyInfoURL must error")
	}
}

func TestFetchCredit_NetworkError(t *testing.T) {
	withURLs(t, BaseURL, KeyExchangeURL, closedURL)
	if _, err := FetchCredit("k"); err == nil {
		t.Fatal("connection failure must error")
	}
}

func TestPublicModelIDs_RequestBuildErrorReturnsNil(t *testing.T) {
	withURLs(t, badURL, KeyExchangeURL, KeyInfoURL)
	if ids := PublicModelIDs(); ids != nil {
		t.Fatalf("unparseable BaseURL must return nil, got %v", ids)
	}
}

func TestPublicModelIDs_NetworkErrorReturnsNil(t *testing.T) {
	withURLs(t, closedURL, KeyExchangeURL, KeyInfoURL)
	if ids := PublicModelIDs(); ids != nil {
		t.Fatalf("connection failure must return nil, got %v", ids)
	}
}
