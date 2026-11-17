package releasefeed

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeFeed serves every feed kind from one test server and records the
// request headers it saw.
type fakeFeed struct {
	mu       sync.Mutex
	t        *testing.T
	srv      *httptest.Server
	version  string
	etag     string
	lastMod  string
	status   int
	body     string
	lastAuth string
	lastINM  string
	lastIMS  string
	hits     int
}

func newFakeFeed(t *testing.T) *fakeFeed {
	t.Helper()
	f := &fakeFeed{t: t, version: "1.0.0", etag: `"v1"`, lastMod: "Mon, 16 Nov 2026 10:00:00 GMT"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// set mutates the feed under its lock; the handler runs on the server's
// goroutines.
func (f *fakeFeed) set(fn func(*fakeFeed)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// seenReq is a copy of what the handler last recorded.
type seenReq struct {
	hits                       int
	auth, ifNoneMatch, ifSince string
}

func (f *fakeFeed) seen() seenReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return seenReq{hits: f.hits, auth: f.lastAuth, ifNoneMatch: f.lastINM, ifSince: f.lastIMS}
}

func (f *fakeFeed) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	f.lastAuth = r.Header.Get("Authorization")
	f.lastINM = r.Header.Get("If-None-Match")
	f.lastIMS = r.Header.Get("If-Modified-Since")
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	if f.lastINM != "" && f.lastINM == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", f.etag)
	w.Header().Set("Last-Modified", f.lastMod)
	if f.body != "" {
		_, _ = w.Write([]byte(f.body))
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/releases/latest"):
		_, _ = w.Write([]byte(`{"tag_name":"` + f.version + `","html_url":"https://github.com/psf/requests/releases/tag/` + f.version + `"}`))
	case strings.HasSuffix(r.URL.Path, "/tags"):
		if r.URL.Query().Get("per_page") != "1" {
			f.t.Errorf("tags request without per_page=1: %s", r.URL)
		}
		_, _ = w.Write([]byte(`[{"name":"` + f.version + `"}]`))
	case strings.HasPrefix(r.URL.Path, "/pypi/"):
		_, _ = w.Write([]byte(`{"info":{"version":"` + f.version + `"},"releases":{}}`))
	default:
		_, _ = w.Write([]byte(`{"version":"` + f.version + `"}`))
	}
}

func (f *fakeFeed) fetcher() *Fetcher {
	return &Fetcher{Client: f.srv.Client(), GitHubAPI: f.srv.URL, GitHubWeb: "https://github.example", PyPIBase: f.srv.URL}
}

func TestFetch_EachKind(t *testing.T) {
	f := newFakeFeed(t)
	ctx := context.Background()
	cases := []struct {
		src      Source
		wantURL  string
		wantPath string
	}{
		{Source{Kind: KindGitHubRelease, Repo: "psf/requests"}, "https://github.com/psf/requests/releases/tag/1.0.0", ""},
		{Source{Kind: KindGitHubTag, Repo: "psf/requests"}, "https://github.example/psf/requests/releases/tag/1.0.0", ""},
		{Source{Kind: KindPyPI, Package: "requests"}, "https://pypi.org/project/requests/1.0.0/", ""},
		{Source{Kind: KindIndex, URL: f.srv.URL + "/index.json"}, f.srv.URL + "/index.json", ""},
	}
	for _, tc := range cases {
		got, err := f.fetcher().Fetch(ctx, tc.src, SourceState{}, "")
		if err != nil {
			t.Fatalf("%s: %v", tc.src.Kind, err)
		}
		if got.NotModified || got.Version != "1.0.0" || got.URL != tc.wantURL || got.ETag != `"v1"` || got.LastModified == "" {
			t.Fatalf("%s: %+v", tc.src.Kind, got)
		}
		if f.seen().auth != "" {
			t.Fatalf("%s: unauthenticated source sent Authorization", tc.src.Kind)
		}
	}
}

func TestFetch_ConditionalAndAuth(t *testing.T) {
	f := newFakeFeed(t)
	src := Source{Kind: KindPyPI, Package: "requests", AuthVar: "PYPI_TOKEN"}
	prev := SourceState{ETag: `"v1"`, LastModified: f.lastMod, Version: "1.0.0"}
	got, err := f.fetcher().Fetch(context.Background(), src, prev, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if !got.NotModified || got.ETag != `"v1"` || got.LastModified != f.lastMod {
		t.Fatalf("304 result = %+v", got)
	}
	if seen := f.seen(); seen.ifNoneMatch != `"v1"` || seen.ifSince != f.lastMod {
		t.Fatalf("validators not sent: %+v", seen)
	}
	if got := f.seen().auth; got != "Bearer s3cret" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestFetch_Errors(t *testing.T) {
	f := newFakeFeed(t)
	ctx := context.Background()

	f.set(func(f *fakeFeed) { f.status = http.StatusUnauthorized })
	_, err := f.fetcher().Fetch(ctx, Source{Kind: KindPyPI, Package: "requests"}, SourceState{}, "s3cret")
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("401: %v", err)
	}
	f.set(func(f *fakeFeed) { f.status, f.body = 0, `{"info":{"version":""}}` })
	if _, err := f.fetcher().Fetch(ctx, Source{Kind: KindPyPI, Package: "requests"}, SourceState{}, ""); !errors.Is(err, ErrNoVersion) {
		t.Fatalf("empty version: %v", err)
	}
	f.set(func(f *fakeFeed) { f.body = `not json` })
	for _, src := range []Source{
		{Kind: KindGitHubRelease, Repo: "a/b"},
		{Kind: KindGitHubTag, Repo: "a/b"},
		{Kind: KindPyPI, Package: "p"},
	} {
		if _, err := f.fetcher().Fetch(ctx, src, SourceState{}, ""); err == nil || !strings.Contains(err.Error(), "parse") {
			t.Fatalf("%s bad json: %v", src.Kind, err)
		}
	}
	f.set(func(f *fakeFeed) { f.body = `[]` })
	if _, err := f.fetcher().Fetch(ctx, Source{Kind: KindGitHubTag, Repo: "a/b"}, SourceState{}, ""); !errors.Is(err, ErrNoVersion) {
		t.Fatalf("no tags: %v", err)
	}
	f.set(func(f *fakeFeed) { f.body = "" })

	for _, src := range []Source{
		{Kind: KindGitHubRelease, Repo: "nope"},
		{Kind: KindPyPI},
		{Kind: KindIndex, URL: "ftp://x"},
		{Kind: "npm"},
	} {
		if _, err := f.fetcher().Fetch(ctx, src, SourceState{}, ""); err == nil {
			t.Fatalf("%+v: want an endpoint error", src)
		}
	}

	// A transport failure names the feed but never the query string.
	dead := &Fetcher{Client: f.srv.Client()}
	_, err = dead.Fetch(ctx, Source{Kind: KindIndex, URL: "http://127.0.0.1:1/latest?token=s3cret"}, SourceState{}, "")
	if err == nil || strings.Contains(err.Error(), "token=s3cret") {
		t.Fatalf("transport error leaked the query: %v", err)
	}
}

func TestFetch_BodyTooLarge(t *testing.T) {
	f := newFakeFeed(t)
	f.set(func(f *fakeFeed) { f.body = `{"version":"1"}` + strings.Repeat(" ", maxOtherBodyBytes) })
	_, err := f.fetcher().Fetch(context.Background(), Source{Kind: KindIndex, URL: f.srv.URL + "/big"}, SourceState{}, "")
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized body: %v", err)
	}
}

func TestFetch_DefaultClientAndEndpoints(t *testing.T) {
	f := newFakeFeed(t)
	// A zero Fetcher builds the egress client itself; the index kind needs
	// no base URL, so it reaches the fake directly.
	got, err := (&Fetcher{}).Fetch(context.Background(), Source{Kind: KindIndex, URL: f.srv.URL + "/v"}, SourceState{}, "")
	if err != nil || got.Version != "1.0.0" {
		t.Fatalf("default client: %+v %v", got, err)
	}
	ep, _, err := (&Fetcher{}).endpoint(Source{Kind: KindGitHubRelease, Repo: "psf/requests"})
	if err != nil || ep != DefaultGitHubAPI+"/repos/psf/requests/releases/latest" {
		t.Fatalf("default github endpoint = %q %v", ep, err)
	}
	ep, _, err = (&Fetcher{}).endpoint(Source{Kind: KindPyPI, Package: "requests"})
	if err != nil || ep != DefaultPyPIBase+"/pypi/requests/json" {
		t.Fatalf("default pypi endpoint = %q %v", ep, err)
	}
}

func TestIndexVersion(t *testing.T) {
	cases := map[string]string{
		`{"version":"2.1"}`:  "2.1",
		`{"version":3}`:      "3",
		`{"latest":"x"}`:     "",
		`{"version":["x"]}`:  "",
		`{bad`:               "",
		"\n\n 4.5.6 \nold\n": "4.5.6",
		"":                   "",
	}
	for body, want := range cases {
		if got := indexVersion([]byte(body), "version"); got != want {
			t.Errorf("indexVersion(%q) = %q, want %q", body, got, want)
		}
	}
	if got := indexVersion([]byte(`{"latest":"9"}`), "latest"); got != "9" {
		t.Errorf("custom field = %q", got)
	}
}

func TestCleanVersionAndLink(t *testing.T) {
	if got := cleanVersion(" v1.2\n\tIGNORE ALL "); got != "v1.2IGNOREALL" {
		t.Fatalf("cleanVersion = %q", got)
	}
	if got := cleanVersion(strings.Repeat("9", maxVersionLen+10)); len(got) != maxVersionLen {
		t.Fatalf("cleanVersion length = %d", len(got))
	}
	if got := cleanLink("https://user:pw@example.com/x"); got != "https://example.com/x" {
		t.Fatalf("cleanLink userinfo = %q", got)
	}
	for _, bad := range []string{"javascript:alert(1)", "", "::"} {
		if got := cleanLink(bad); got != "" {
			t.Fatalf("cleanLink(%q) = %q", bad, got)
		}
	}
	plain := errors.New("plain")
	if redactURLError(plain) != plain {
		t.Fatal("non-url errors pass through")
	}
	ue := &url.Error{Op: "Get", URL: "https://u:p@x.example/a?k=v", Err: plain}
	if got := redactURLError(ue).Error(); strings.Contains(got, "k=v") || strings.Contains(got, "u:p") {
		t.Fatalf("redacted = %q", got)
	}
}

func TestSourceKeyAndLabel(t *testing.T) {
	cases := []struct {
		src   Source
		key   string
		label string
	}{
		{Source{Kind: KindGitHubRelease, Repo: "psf/requests"}, "github_release:psf/requests", "psf/requests"},
		{Source{Kind: KindPyPI, Package: "Requests", AuthVar: "TOK"}, "pypi:requests#TOK", "Requests"},
		{Source{Kind: KindIndex, URL: "https://i/x", Name: "internal"}, "index:https://i/x", "internal"},
		{Source{Kind: KindIndex, URL: "https://i/x"}, "index:https://i/x", "https://i/x"},
	}
	for _, tc := range cases {
		if tc.src.Key() != tc.key || tc.src.Label() != tc.label {
			t.Errorf("%+v: key=%q label=%q", tc.src, tc.src.Key(), tc.src.Label())
		}
	}
	// A credential in an index URL never reaches the key or the label, and
	// two URLs that differ only in the query keep distinct keys.
	a := Source{Kind: KindIndex, URL: "https://bot:pw@i.example/x?token=s3cret"}
	b := Source{Kind: KindIndex, URL: "https://i.example/x?token=other"}
	for _, s := range []string{a.Key(), a.Label()} {
		if strings.Contains(s, "s3cret") || strings.Contains(s, "pw") {
			t.Fatalf("credential leaked: %q", s)
		}
	}
	if a.Key() == b.Key() {
		t.Fatalf("distinct queries share key %q", a.Key())
	}
	if got := redactedURL("http://[::1"); !strings.HasPrefix(got, "invalid-url-") {
		t.Fatalf("unparseable url = %q", got)
	}
}

func TestNewEgressClient_UsesUpstreamProxyFunc(t *testing.T) {
	c := NewEgressClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatalf("egress client transport = %T without a proxy func", c.Transport)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://pypi.org/pypi/x/json", nil)
	// With no upstream proxy installed the selector reports direct.
	if u, err := tr.Proxy(req); err != nil || u != nil {
		t.Fatalf("no proxy installed: %v %v", u, err)
	}
}
