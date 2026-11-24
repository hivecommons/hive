package reviewpreflight

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

var testNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

const (
	goModPatch = "@@ -1 +1 @@\n+\tgithub.com/a/b v1.1.0\n"
	pkgPatch   = "@@ -1 +1 @@\n+    \"left-pad\": \"^1.3.0\",\n"
)

type fakeServices struct {
	srv   *httptest.Server
	hits  atomic.Int32
	osvOK bool
}

func newFakeServices(t *testing.T) *fakeServices {
	t.Helper()
	f := &fakeServices{osvOK: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/querybatch", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if !f.osvOK {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Queries []struct {
				Package struct{ Name, Ecosystem string }
				Version string
			}
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type vuln struct {
			ID string `json:"id"`
		}
		type result struct {
			Vulns []vuln `json:"vulns"`
		}
		var out struct {
			Results []result `json:"results"`
		}
		for _, q := range req.Queries {
			res := result{}
			if q.Package.Ecosystem == "Go" && q.Version == "1.1.0" {
				res.Vulns = []vuln{{ID: "GHSA-aaaa-bbbb-cccc"}}
			}
			out.Results = append(out.Results, res)
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/v1/vulns/GHSA-aaaa-bbbb-cccc", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		_, _ = w.Write([]byte(`{"id":"GHSA-aaaa-bbbb-cccc","aliases":["CVE-2024-0001"],"database_specific":{"severity":"HIGH"}}`))
	})
	mux.HandleFunc("/github.com/a/b/@v/v1.1.0.info", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		_, _ = w.Write([]byte(`{"Version":"v1.1.0","Time":"2026-11-16T12:00:00Z"}`))
	})
	mux.HandleFunc("/left-pad", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		_, _ = w.Write([]byte(`{"time":{"1.3.0":"2020-01-01T00:00:00Z"}}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServices) settings(over config.ReviewPreflightConfig) config.ReviewPreflightResolved {
	base := config.ReviewPreflightConfig{
		RuleDocs:     boolPtr(false),
		Dependencies: boolPtr(true),
		OSVURL:       f.srv.URL,
		RegistryURLs: map[string]string{"go": f.srv.URL, "npm": f.srv.URL},
	}
	return config.ResolveReviewPreflight(base, over)
}

func (f *fakeServices) client() *Client {
	return &Client{HTTP: f.srv.Client(), Now: func() time.Time { return testNow }}
}

func depInput() Input {
	return Input{Repo: "o/r", FilesKnown: true, Files: []ChangedFile{
		{Path: "go.mod", Patch: goModPatch},
		{Path: "web/package.json", Patch: pkgPatch},
	}}
}

func TestBuildDependencyEvidenceAcrossEcosystems(t *testing.T) {
	f := newFakeServices(t)
	out := Build(context.Background(), f.client(), f.settings(config.ReviewPreflightConfig{}), depInput())
	for _, want := range []string{
		"DEPENDENCY EVIDENCE",
		"go github.com/a/b v1.1.0 (go.mod):",
		"GHSA-aaaa-bbbb-cccc (high, at or above block_severity)",
		"published 2026-11-16 (2 days ago)",
		"npm left-pad 1.3.0 (web/package.json): no known advisories; published 2020-01-01",
		"Publish dates are facts, not verdicts: this hive declares no release-age policy",
		"If the repository's own documents state a release-age policy, apply it and quote it",
		"means no data, not a clean result",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, banned := range []string{"younger than", "release-age window", "minimum release age of"} {
		if strings.Contains(out, banned) {
			t.Errorf("with no declared policy a publish date carries no verdict, found %q in:\n%s", banned, out)
		}
	}
}

func TestBuildReleaseAgePolicyHonoredWhenDeclared(t *testing.T) {
	f := newFakeServices(t)
	out := Build(context.Background(), f.client(), f.settings(config.ReviewPreflightConfig{MinReleaseAgeDays: 14}), depInput())
	for _, want := range []string{
		"published 2026-11-16 (2 days ago), younger than the 14 day minimum release age declared in this hive's config",
		"declares a minimum release age of 14 days for this repository",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Publish dates are facts, not verdicts") {
		t.Errorf("a declared policy replaces the no-policy wording:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "left-pad") && strings.Contains(line, "younger than") {
			t.Errorf("an old release must not be reported against the policy: %s", line)
		}
	}
}

func TestBuildUsesCacheAcrossReviews(t *testing.T) {
	f := newFakeServices(t)
	c := f.client()
	set := f.settings(config.ReviewPreflightConfig{})
	first := Build(context.Background(), c, set, depInput())
	hits := f.hits.Load()
	second := Build(context.Background(), c, set, depInput())
	if f.hits.Load() != hits {
		t.Fatalf("second review made %d new requests, want 0", f.hits.Load()-hits)
	}
	if first != second {
		t.Fatal("cached evidence differs from fresh evidence")
	}
}

func TestBuildOfflineDegradesAndSendsNothing(t *testing.T) {
	f := newFakeServices(t)
	offline := true
	out := Build(context.Background(), f.client(), f.settings(config.ReviewPreflightConfig{Offline: &offline}), depInput())
	if !strings.Contains(out, "could not check: offline") || !strings.Contains(out, "no data") {
		t.Fatalf("offline must be stated explicitly:\n%s", out)
	}
	if strings.Contains(out, "no known advisories") {
		t.Fatalf("offline must not claim a clean result:\n%s", out)
	}
	if f.hits.Load() != 0 {
		t.Fatalf("offline mode made %d requests", f.hits.Load())
	}
}

func TestOfflineEnvOverridesConfig(t *testing.T) {
	t.Setenv(config.ReviewPreflightOfflineEnv, "true")
	f := newFakeServices(t)
	out := Build(context.Background(), f.client(), f.settings(config.ReviewPreflightConfig{}), depInput())
	if !strings.Contains(out, "could not check: offline") || f.hits.Load() != 0 {
		t.Fatalf("env offline switch ignored:\n%s", out)
	}
}

func TestBuildVulnerabilitySourceDownIsNotACleanResult(t *testing.T) {
	f := newFakeServices(t)
	f.osvOK = false
	out := Build(context.Background(), f.client(), f.settings(config.ReviewPreflightConfig{}), depInput())
	if !strings.Contains(out, "could not check vulnerabilities:") || !strings.Contains(out, "HTTP 503") {
		t.Fatalf("outage not reported:\n%s", out)
	}
	if strings.Contains(out, "no known advisories") {
		t.Fatalf("an outage must not read as clean:\n%s", out)
	}
	if !strings.Contains(out, "published 2026-11-16") {
		t.Fatalf("release age must still be reported when only OSV is down:\n%s", out)
	}
}

func TestBuildIgnoreListWithExpiry(t *testing.T) {
	f := newFakeServices(t)
	live := f.settings(config.ReviewPreflightConfig{MinReleaseAgeDays: 14, Ignore: []config.ReviewPreflightIgnore{
		{ID: "CVE-2024-0001", Package: "github.com/a/b", Reason: "not reachable from our code", Expires: "2027-01-01"},
		{ID: "release-age", Package: "github.com/a/b", Reason: "we vendored it ourselves"},
	}})
	out := Build(context.Background(), f.client(), live, depInput())
	if !strings.Contains(out, "GHSA-aaaa-bbbb-cccc accepted by this hive (not reachable from our code)") {
		t.Fatalf("ignore by alias not applied:\n%s", out)
	}
	if strings.Contains(out, "(high, at or above block_severity)") || strings.Contains(out, "younger than the 14 day") {
		t.Fatalf("ignored findings still shown:\n%s", out)
	}
	expired := f.settings(config.ReviewPreflightConfig{Ignore: []config.ReviewPreflightIgnore{
		{ID: "GHSA-aaaa-bbbb-cccc", Reason: "old decision", Expires: "2026-11-17"},
	}})
	out = Build(context.Background(), f.client(), expired, depInput())
	if !strings.Contains(out, "expired on 2026-11-17") || !strings.Contains(out, "GHSA-aaaa-bbbb-cccc (high") {
		t.Fatalf("an expired ignore must stop suppressing and say so:\n%s", out)
	}
}

func TestBuildSeverityFloorAndBlockThreshold(t *testing.T) {
	f := newFakeServices(t)
	out := Build(context.Background(), f.client(), f.settings(config.ReviewPreflightConfig{SeverityFloor: "critical"}), depInput())
	if strings.Contains(out, "GHSA-aaaa-bbbb-cccc") {
		t.Fatalf("advisory below the floor must not be listed:\n%s", out)
	}
	out = Build(context.Background(), f.client(), f.settings(config.ReviewPreflightConfig{BlockSeverity: "critical"}), depInput())
	if !strings.Contains(out, "GHSA-aaaa-bbbb-cccc (high)") || strings.Contains(out, "at or above block_severity") {
		t.Fatalf("block threshold not honoured:\n%s", out)
	}
}

func TestBuildNotCheckedIsExplicit(t *testing.T) {
	f := newFakeServices(t)
	offline := true
	in := Input{Repo: "o/r", FilesKnown: true, Files: []ChangedFile{
		{Path: "mix.lock", Patch: "+x"},
		{Path: "Cargo.lock", Patch: ""},
		{Path: "README.md", Patch: "+docs"},
	}}
	out := Build(context.Background(), f.client(), f.settings(config.ReviewPreflightConfig{Offline: &offline}), in)
	for _, want := range []string{"Not checked: mix.lock (Hex (Elixir)): no support", "Not checked: Cargo.lock (Rust): no patch available", "No added or changed dependency versions"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestBuildSilentWhenNoManifestChanged(t *testing.T) {
	f := newFakeServices(t)
	in := Input{Repo: "o/r", FilesKnown: true, Files: []ChangedFile{{Path: "main.go", Patch: "+x"}}}
	if out := Build(context.Background(), f.client(), f.settings(config.ReviewPreflightConfig{}), in); out != "" {
		t.Fatalf("expected no section, got:\n%s", out)
	}
	if out := Build(context.Background(), f.client(), f.settings(config.ReviewPreflightConfig{}), Input{Repo: "o/r"}); !strings.Contains(out, "could not check") {
		t.Fatalf("unknown file list must be said:\n%s", out)
	}
}

func TestBuildMaxDependencies(t *testing.T) {
	f := newFakeServices(t)
	offline := true
	out := Build(context.Background(), f.client(), f.settings(config.ReviewPreflightConfig{Offline: &offline, MaxDependencies: 1}), depInput())
	if !strings.Contains(out, "over review.preflight.max_dependencies (1)") {
		t.Fatalf("over-limit dependencies must be listed as not checked:\n%s", out)
	}
}

func TestBuildDependenciesOffByDefault(t *testing.T) {
	set := config.ResolveReviewPreflight(config.ReviewPreflightConfig{RuleDocs: boolPtr(false)}, config.ReviewPreflightConfig{})
	if out := Build(context.Background(), nil, set, depInput()); out != "" {
		t.Fatalf("dependency checks must be off by default:\n%s", out)
	}
}

func TestBuildRulesWithTree(t *testing.T) {
	set := config.ResolveReviewPreflight(config.ReviewPreflightConfig{}, config.ReviewPreflightConfig{})
	out := Build(context.Background(), nil, set, Input{Repo: "o/r", Ref: "base1", TreeKnown: true, Tree: []TreeEntry{{"AGENTS.md", 50}, {"x.go", 5}}})
	if !strings.Contains(out, "AGENTS.md") || strings.Contains(out, "x.go") {
		t.Fatalf("unexpected rules section:\n%s", out)
	}
}

func TestRegistryAgeLookups(t *testing.T) {
	bodies := map[string]string{
		"https://r/pypi/requests/2.31.0/json":                            `{"urls":[{"upload_time_iso_8601":"2023-05-22T15:12:44.175Z"},{"upload_time_iso_8601":"2023-05-22T15:12:42Z"}]}`,
		"https://r/api/v1/crates/serde/1.0.200":                          `{"version":{"created_at":"2024-04-14T10:00:00Z"}}`,
		"https://r/api/v2/rubygems/rack/versions/3.0.9.json":             `{"created_at":"2023-10-01T00:00:00.000Z"}`,
		"https://r/p2/monolog/monolog.json":                              `{"packages":{"monolog/monolog":[{"version":"v3.5.0","time":"2023-10-27T15:32:31+00:00"}]}}`,
		"https://r/v3/registration5-semver1/newtonsoft.json/13.0.3.json": `{"published":"2023-03-08T00:00:00Z"}`,
		"https://r/!azure/!b!c/@v/v1.0.0.info":                           `{"Time":"2021-01-01T00:00:00Z"}`,
	}
	get := func(_ context.Context, u string) ([]byte, error) {
		if b, ok := bodies[u]; ok {
			return []byte(b), nil
		}
		return nil, ErrNotFound
	}
	for _, tc := range []struct {
		eco  string
		dep  Dep
		want string
	}{
		{"pypi", Dep{Name: "requests", Version: "2.31.0"}, "2023-05-22T15:12:42Z"},
		{"crates", Dep{Name: "serde", Version: "1.0.200"}, "2024-04-14T10:00:00Z"},
		{"rubygems", Dep{Name: "rack", Version: "3.0.9"}, "2023-10-01T00:00:00Z"},
		{"packagist", Dep{Name: "monolog/monolog", Version: "3.5.0"}, "2023-10-27T15:32:31Z"},
		{"nuget", Dep{Name: "Newtonsoft.Json", Version: "13.0.3"}, "2023-03-08T00:00:00Z"},
		{"go", Dep{Name: "Azure/BC", Version: "v1.0.0"}, "2021-01-01T00:00:00Z"},
	} {
		e, _ := ecosystemByID(tc.eco)
		got, err := e.Age(context.Background(), get, "https://r", tc.dep)
		if err != nil {
			t.Fatalf("%s: %v", tc.eco, err)
		}
		if got.Format(time.RFC3339) != tc.want {
			t.Errorf("%s: got %s, want %s", tc.eco, got.Format(time.RFC3339), tc.want)
		}
	}
	e, _ := ecosystemByID("go")
	if _, err := e.Age(context.Background(), get, "https://r", Dep{Name: "nope/x", Version: "v1.0.0"}); err != ErrNotFound {
		t.Fatalf("missing version must be ErrNotFound, got %v", err)
	}
}

func TestClientMapsRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("a credential was sent: %q", got)
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client()}
	_, err := c.getter(time.Second)(context.Background(), srv.URL+"/x")
	if _, ok := err.(RateLimitError); !ok {
		t.Fatalf("expected RateLimitError, got %v", err)
	}
}
