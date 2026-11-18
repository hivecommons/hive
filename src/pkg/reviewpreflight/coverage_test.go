package reviewpreflight

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func stubServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func stubSettings(srv *httptest.Server, over config.ReviewPreflightConfig) config.ReviewPreflightResolved {
	base := config.ReviewPreflightConfig{
		RuleDocs:     boolPtr(false),
		Dependencies: boolPtr(true),
		OSVURL:       srv.URL,
		RegistryURLs: map[string]string{"go": srv.URL, "npm": srv.URL},
	}
	return config.ResolveReviewPreflight(base, over)
}

func stubClient(srv *httptest.Server) *Client {
	return &Client{HTTP: srv.Client(), Now: func() time.Time { return testNow }}
}

func TestOSVErrorShapesAreReportedNotHidden(t *testing.T) {
	noAge := config.ReviewPreflightConfig{ReleaseAge: boolPtr(false)}
	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		want    string
	}{
		"unreadable": {func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not json")) }, "unreadable OSV answer"},
		"short":      {func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"results":[]}`)) }, "OSV answered 0 results for 2 queries"},
		"throttled":  {func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }, "rate limited by"},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := stubServer(t, tc.handler)
			out := Build(context.Background(), stubClient(srv), stubSettings(srv, noAge), depInput())
			if !strings.Contains(out, "could not check vulnerabilities: ") || !strings.Contains(out, tc.want) {
				t.Fatalf("want %q in:\n%s", tc.want, out)
			}
			if strings.Contains(out, "no known advisories") {
				t.Fatalf("failure must not read as clean:\n%s", out)
			}
		})
	}
}

func TestAdvisoryWithoutSeverityWithdrawnAndBadIDs(t *testing.T) {
	srv, _ := stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/querybatch"):
			_, _ = w.Write([]byte(`{"results":[{"vulns":[{"id":"GHSA-nosev"},{"id":"GHSA-gone"},{"id":"bad id!"}]},{"vulns":[]}]}`))
		case strings.HasSuffix(r.URL.Path, "/GHSA-nosev"):
			_, _ = w.Write([]byte(`{"id":"GHSA-nosev","aliases":["CVE-2024-9","bad alias!"]}`))
		case strings.HasSuffix(r.URL.Path, "/GHSA-gone"):
			_, _ = w.Write([]byte(`{"id":"GHSA-gone","withdrawn":"2024-01-01T00:00:00Z"}`))
		default:
			http.NotFound(w, r)
		}
	})
	set := stubSettings(srv, config.ReviewPreflightConfig{ReleaseAge: boolPtr(false)})
	out := Build(context.Background(), stubClient(srv), set, depInput())
	if !strings.Contains(out, "GHSA-nosev (severity not stated)") {
		t.Fatalf("advisory with no severity must be listed as such:\n%s", out)
	}
	for _, bad := range []string{"GHSA-gone", "bad id", "bad alias"} {
		if strings.Contains(out, bad) {
			t.Fatalf("%q must not reach the prompt:\n%s", bad, out)
		}
	}
	// An ignore entry can match the alias.
	set = stubSettings(srv, config.ReviewPreflightConfig{ReleaseAge: boolPtr(false), Ignore: []config.ReviewPreflightIgnore{{ID: "cve-2024-9", Reason: "triaged"}}})
	out = Build(context.Background(), stubClient(srv), set, depInput())
	if !strings.Contains(out, "GHSA-nosev accepted by this hive (triaged)") {
		t.Fatalf("alias ignore not applied:\n%s", out)
	}
}

func TestReleaseAgeFailureAndOptOut(t *testing.T) {
	srv, hits := stubServer(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) })
	novuln := config.ReviewPreflightConfig{Vulnerabilities: boolPtr(false)}
	out := Build(context.Background(), stubClient(srv), stubSettings(srv, novuln), depInput())
	if !strings.Contains(out, "could not check release age for 2 of 2 dependencies (first error: ") || !strings.Contains(out, "HTTP 500") {
		t.Fatalf("age failure not reported:\n%s", out)
	}
	before := hits.Load()
	off := config.ReviewPreflightConfig{Vulnerabilities: boolPtr(false), ReleaseAge: boolPtr(false)}
	out = Build(context.Background(), stubClient(srv), stubSettings(srv, off), depInput())
	if hits.Load() != before || strings.Contains(out, "published") {
		t.Fatalf("a disabled age check must make no request:\n%s", out)
	}
	if !strings.Contains(out, "no data") {
		t.Fatalf("nothing checked must say no data:\n%s", out)
	}
}

func TestEcosystemsWithoutSourcesAreListedNotChecked(t *testing.T) {
	srv, hits := stubServer(t, func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected request %s", r.URL) })
	in := Input{Repo: "o/r", FilesKnown: true, Files: []ChangedFile{
		{Path: "Dockerfile", Patch: "+FROM golang:1.22\n"},
		{Path: ".github/workflows/ci.yml", Patch: "+      - uses: actions/setup-go@v5\n"},
	}}
	out := Build(context.Background(), stubClient(srv), stubSettings(srv, config.ReviewPreflightConfig{}), in)
	for _, want := range []string{
		"container golang 1.22 (Dockerfile): not checked: no vulnerability source for container images",
		"actions actions/setup-go - (.github/workflows/ci.yml): not checked: floating ref v5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("no request expected, got %d", hits.Load())
	}
}

func TestBuildWithoutClientFallsBackOffline(t *testing.T) {
	off := true
	set := config.ResolveReviewPreflight(config.ReviewPreflightConfig{RuleDocs: boolPtr(false), Dependencies: boolPtr(true), Offline: &off}, config.ReviewPreflightConfig{})
	if out := Build(context.Background(), nil, set, depInput()); !strings.Contains(out, "could not check: offline") {
		t.Fatalf("unexpected:\n%s", out)
	}
	if NewClient().HTTP == nil || (&Client{}).now().IsZero() {
		t.Fatal("client defaults missing")
	}
}

func TestClientStatusMappingAndCache(t *testing.T) {
	srv, _ := stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/boom":
			w.WriteHeader(http.StatusBadGateway)
		case "/post":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("posted"))
		default:
			_, _ = w.Write([]byte("ok"))
		}
	})
	c := stubClient(srv)
	get := c.getter(time.Second)
	if b, err := get(context.Background(), srv.URL+"/x"); err != nil || string(b) != "ok" {
		t.Fatalf("got %q %v", b, err)
	}
	if _, err := get(context.Background(), srv.URL+"/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404 must be ErrNotFound, got %v", err)
	}
	if _, err := get(context.Background(), srv.URL+"/boom"); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("5xx must be an error naming the status, got %v", err)
	}
	if _, err := get(context.Background(), "http://127.0.0.1:1/unreachable"); err == nil {
		t.Fatal("a connection failure must be an error")
	}
	if _, err := get(context.Background(), "::bad url"); err == nil {
		t.Fatal("a malformed URL must be an error")
	}
	if b, err := c.do(context.Background(), http.MethodPost, srv.URL+"/post", strings.NewReader("{}"), time.Second); err != nil || string(b) != "posted" {
		t.Fatalf("POST: %q %v", b, err)
	}
	if got := (RateLimitError{Host: "h"}).Error(); got != "rate limited by h" {
		t.Fatalf("error text %q", got)
	}

	calls := 0
	compute := func() (any, error) { calls++; return calls, nil }
	if v, _ := c.remember("k", time.Hour, compute); v != 1 {
		t.Fatalf("first compute = %v", v)
	}
	if v, _ := c.remember("k", time.Hour, compute); v != 1 {
		t.Fatalf("cached value expected, got %v", v)
	}
	clock := testNow.Add(2 * time.Hour)
	c.Now = func() time.Time { return clock }
	if v, _ := c.remember("k", time.Hour, compute); v != 2 {
		t.Fatalf("expired entry must be recomputed, got %v", v)
	}
	if _, err := c.remember("bad", time.Hour, func() (any, error) { return nil, errors.New("x") }); err == nil {
		t.Fatal("compute error must propagate")
	}
	if _, ok := c.lookup("bad"); ok {
		t.Fatal("errors must not be cached")
	}
}

func TestAgeLookupErrorPaths(t *testing.T) {
	bodies := map[string]string{"empty": `{}`, "garbage": `not json`}
	for _, e := range Ecosystems {
		if e.Age == nil {
			continue
		}
		d := Dep{Ecosystem: e.ID, Name: "a/b", Version: "1.0.0"}
		if e.ID == "maven" {
			d.Name = "g:a"
		}
		for kind, body := range bodies {
			b := body
			_, err := e.Age(context.Background(), func(context.Context, string) ([]byte, error) { return []byte(b), nil }, "https://r", d)
			if err == nil {
				t.Errorf("%s/%s: expected an error", e.ID, kind)
			}
		}
		boom := errors.New("network down")
		if _, err := e.Age(context.Background(), func(context.Context, string) ([]byte, error) { return nil, boom }, "https://r", d); !errors.Is(err, boom) {
			t.Errorf("%s: transport error must propagate, got %v", e.ID, err)
		}
	}
	maven, _ := ecosystemByID("maven")
	if _, err := maven.Age(context.Background(), nil, "https://r", Dep{Name: "no-colon", Version: "1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed maven name must be ErrNotFound, got %v", err)
	}
}

func TestNpmAndMavenAgeSuccess(t *testing.T) {
	q := url.QueryEscape(`g:"org.x" AND a:"lib" AND v:"2.0.0"`)
	bodies := map[string]string{
		"https://r/@s%2fp": `{"time":{"1.0.0":"2021-02-03T04:05:06Z"}}`,
		"https://r/solrsearch/select?q=" + q + "&core=gav&rows=1&wt=json": `{"response":{"docs":[{"timestamp":1700000000000}]}}`,
	}
	get := func(_ context.Context, u string) ([]byte, error) {
		if b, ok := bodies[u]; ok {
			return []byte(b), nil
		}
		return nil, fmt.Errorf("unexpected url %s", u)
	}
	npm, _ := ecosystemByID("npm")
	got, err := npm.Age(context.Background(), get, "https://r", Dep{Name: "@s/p", Version: "1.0.0"})
	if err != nil || got.Format(time.RFC3339) != "2021-02-03T04:05:06Z" {
		t.Fatalf("npm: %v %v", got, err)
	}
	if _, err := npm.Age(context.Background(), get, "https://r", Dep{Name: "@s/p", Version: "9.9.9"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown npm version must be ErrNotFound, got %v", err)
	}
	maven, _ := ecosystemByID("maven")
	got, err = maven.Age(context.Background(), get, "https://r", Dep{Name: "org.x:lib", Version: "2.0.0"})
	if err != nil || got.Unix() != 1700000000 {
		t.Fatalf("maven: %v %v", got, err)
	}
}

func TestParseTimeAndSeverityNormalization(t *testing.T) {
	for _, s := range []string{"2024-01-02T03:04:05Z", "2024-01-02T03:04:05", "2024-01-02 03:04:05", "2024-01-02T03:04:05.123+02:00"} {
		if _, err := parseTime(s); err != nil {
			t.Errorf("parseTime(%q): %v", s, err)
		}
	}
	if _, err := parseTime("yesterday"); err == nil || !strings.Contains(err.Error(), "unrecognized timestamp") {
		t.Fatalf("bad timestamp must be an error, got %v", err)
	}
	if got := truncate("abcdef", 3); got != "abc" {
		t.Fatalf("truncate = %q", got)
	}
	for in, want := range map[string]string{"LOW": "low", "Moderate": "medium", "medium": "medium", "HIGH": "high", "critical": "critical", "": "", "weird": ""} {
		if got := normalizeSeverity(in); got != want {
			t.Errorf("normalizeSeverity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIgnoreMatchingEdges(t *testing.T) {
	adv := Advisory{ID: "GHSA-1", Aliases: []string{"CVE-1"}}
	ig := []config.ReviewPreflightIgnore{
		{ID: "GHSA-1", Package: "other", Reason: "wrong package"},
		{ID: "GHSA-2", Reason: "wrong id"},
	}
	if _, hit, note := matchIgnore(ig, adv, "pkg", testNow); hit || note != "" {
		t.Fatal("non-matching entries must not suppress")
	}
	if ageIgnored(ig, "pkg", testNow) {
		t.Fatal("no release-age entry")
	}
	age := []config.ReviewPreflightIgnore{
		{ID: "release-age", Package: "pkg", Reason: "r", Expires: "2026-01-01"},
		{ID: "release-age", Package: "other", Reason: "r"},
	}
	if ageIgnored(age, "pkg", testNow) {
		t.Fatal("an expired release-age ignore must not apply")
	}
	if !ageIgnored(append(age, config.ReviewPreflightIgnore{ID: "RELEASE-AGE", Reason: "all"}), "pkg", testNow) {
		t.Fatal("a package-less release-age ignore applies everywhere")
	}
}

func TestRuleDocLimits(t *testing.T) {
	var tree []TreeEntry
	for i := 0; i < maxRuleDocs+5; i++ {
		tree = append(tree, TreeEntry{Path: fmt.Sprintf("docs/adr/%03d.md", i), Size: 10})
	}
	tree = append(tree, TreeEntry{Path: strings.Repeat("a", 301) + "/AGENTS.md", Size: 10}, TreeEntry{Path: "line\nbreak/AGENTS.md", Size: 10})
	sel := SelectRuleDocs(tree, []string{"docs/adr/**"}, nil, 0)
	if len(sel.Docs) != maxRuleDocs || len(sel.Omitted) != 5 {
		t.Fatalf("docs=%d omitted=%d, want %d and 5", len(sel.Docs), len(sel.Omitted), maxRuleDocs)
	}
	for _, p := range []string{"", strings.Repeat("a", 301), "x`y", "x\ny"} {
		if safePath(p) {
			t.Errorf("safePath(%q) must be false", p)
		}
	}
	set := config.ResolveReviewPreflight(config.ReviewPreflightConfig{}, config.ReviewPreflightConfig{})
	out := renderRules("o/r", "", set, sel, true)
	if !strings.Contains(out, "Not listed (over the read budget): 5 more") {
		t.Fatalf("omitted documents must be counted:\n%s", out)
	}
	if !strings.Contains(renderRules("o/r", "", set, RuleDocSelection{}, true), "No rule documents were found") {
		t.Fatal("an empty selection must say so")
	}
}

func TestFileMatchesAndUnsupportedHelpers(t *testing.T) {
	if !FileMatches("package.json", "web/PACKAGE.JSON") || FileMatches(".github/workflows/*.yml", "x/.github/workflows/a.yml") {
		t.Fatal("FileMatches basename or full path rules broken")
	}
	if FileMatches("[", "x") {
		t.Fatal("a malformed pattern never matches")
	}
}
