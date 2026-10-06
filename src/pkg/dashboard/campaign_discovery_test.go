package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestDiscoveryDocumentsSnapshotsAndFailures(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		switch r.URL.Path {
		case "/release":
			_, _ = w.Write([]byte("v2 adds a capability"))
		case "/large":
			_, _ = w.Write([]byte(strings.Repeat("x", discoveryMaxBytes+1)))
		case "/binary":
			_, _ = w.Write([]byte{0xff, 0xfe, 0xfd})
		case "/redirect":
			http.Redirect(w, r, "/release", http.StatusFound)
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var sources []config.SpektacularDiscoverySource
	for _, path := range []string{"/release", "/large", "/binary", "/redirect", "/error"} {
		sources = append(sources, config.SpektacularDiscoverySource{Name: path, Kind: "release", URL: server.URL + path})
	}
	evidence, failures := collectDiscoveryDocuments(context.Background(), sources, client)
	if requests.Load() != 5 || len(evidence) != 1 || len(failures) != 4 {
		t.Fatalf("requests=%d evidence=%+v failures=%+v", requests.Load(), evidence, failures)
	}
	got := evidence[0]
	if got.Summary != "v2 adds a capability" || len(got.SHA256) != 64 || got.URL != sources[0].URL || got.Source != "/release" || got.Kind != "release" {
		t.Fatalf("lost source evidence: %+v", got)
	}
	for i, failure := range failures {
		if failure.Name != sources[i+1].Name || failure.Reason == "" {
			t.Fatalf("failure %d = %+v, want named typed reason", i, failure)
		}
	}
	if !strings.Contains(failures[0].Reason, "exceeds") || !strings.Contains(failures[1].Reason, "UTF-8") || !strings.Contains(failures[2].Reason, "302") {
		t.Fatalf("failure reasons = %+v", failures)
	}

	_, failures = collectDiscoveryDocuments(context.Background(), sources[:1], nil)
	if len(failures) != 1 || !strings.Contains(failures[0].Reason, "proxy") {
		t.Fatalf("nil client failures = %+v, want proxy unavailable", failures)
	}
	_, failures = collectDiscoveryDocuments(context.Background(), []config.SpektacularDiscoverySource{{Name: "bad", Kind: "release", URL: "://bad"}}, client)
	if len(failures) != 1 || !strings.Contains(failures[0].Reason, "invalid") {
		t.Fatalf("bad url failures = %+v", failures)
	}
}

func TestDiscoveryProxyClientRejectsMissingProxy(t *testing.T) {
	if _, _, err := discoveryProxyClient(""); err == nil {
		t.Fatal("empty proxy produced a client")
	}
	client, closeIdle, err := discoveryProxyClient("http://relay.invalid:18443")
	if err != nil || client == nil {
		t.Fatalf("discoveryProxyClient = %v, %v", client, err)
	}
	closeIdle()
}

func discoveryTestServer(relayURL string) *Server {
	s := &Server{deps: &Dependencies{Config: &config.Config{}}}
	cfg := &s.deps.Config.Runs.Spektacular.Recheck
	cfg.DiscoveryProxy = relayURL
	cfg.EgressAllowlist = []string{"source.invalid"}
	return s
}

func TestDiscoveryUsesOnlyConfiguredProxy(t *testing.T) {
	var requests atomic.Int32
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodConnect || r.Host != "source.invalid:443" {
			t.Errorf("unexpected relay request %s %s", r.Method, r.Host)
		}
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer relay.Close()
	// NO_PROXY must never let a declaration bypass the relay.
	t.Setenv("NO_PROXY", "*")
	s := discoveryTestServer(relay.URL)
	if external, failures := s.collectRecheckDiscovery(context.Background(), Campaign{}, time.Time{}); len(external) != 0 || len(failures) != 0 || requests.Load() != 0 {
		t.Fatal("empty sources performed discovery")
	}
	cfg := &s.deps.Config.Runs.Spektacular.Recheck
	cfg.Sources = []config.SpektacularDiscoverySource{{Name: "source", Kind: "standards", URL: "https://source.invalid/news"}}
	external, failures := s.collectRecheckDiscovery(context.Background(), Campaign{}, time.Time{})
	if requests.Load() != 1 || len(external) != 0 || len(failures) != 1 || failures[0].Name != "source" {
		t.Fatalf("relay not enforced: requests=%d external=%+v failures=%+v", requests.Load(), external, failures)
	}
}

func TestDiscoveryRejectedAtUse(t *testing.T) {
	var requests atomic.Int32
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer relay.Close()
	for name, mutate := range map[string]func(*config.SpektacularRecheckConfig){
		"overlay-injected destination": func(c *config.SpektacularRecheckConfig) { c.Sources[0].URL = "https://outside.invalid/news" },
		"missing proxy":                func(c *config.SpektacularRecheckConfig) { c.DiscoveryProxy = "" },
		"credentialed proxy":           func(c *config.SpektacularRecheckConfig) { c.DiscoveryProxy = "http://user:pass@relay.invalid:18443" },
	} {
		t.Run(name, func(t *testing.T) {
			s := discoveryTestServer(relay.URL)
			cfg := &s.deps.Config.Runs.Spektacular.Recheck
			cfg.Sources = []config.SpektacularDiscoverySource{{Name: "source", Kind: "standards", URL: "https://source.invalid/news"}}
			mutate(cfg)
			external, failures := s.collectRecheckDiscovery(context.Background(), Campaign{}, time.Time{})
			if len(external) != 0 || len(failures) != 1 || failures[0].Name != "config" || !strings.Contains(failures[0].Reason, "runs.spektacular.recheck.sources") {
				t.Fatalf("external=%+v failures=%+v, want config rejection", external, failures)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("rejected discovery reached the relay %d times", requests.Load())
	}
}

func TestDiscoveryRoutesTypedFeedsThroughProxy(t *testing.T) {
	var connects, feeds atomic.Int32
	published := time.Now().UTC().Format(time.RFC3339)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			connects.Add(1)
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		if r.URL.Host != "feed.invalid" {
			t.Errorf("relay asked for %s, want feed.invalid", r.URL.Host)
		}
		feeds.Add(1)
		_, _ = w.Write([]byte(`<feed><entry><title>signal</title><updated>` + published + `</updated><summary>new standard</summary><link href="https://feed.invalid/signal"/></entry></feed>`))
	}))
	defer relay.Close()
	t.Setenv("NO_PROXY", "*")
	s := discoveryTestServer(relay.URL)
	s.deps.Config.Variables.Security.HTTPAllowlist = []string{"feed.invalid"}
	s.deps.Config.Runs.Spektacular.Recheck.Discovery = config.SpektacularRecheckDiscoveryConfig{
		Enabled: true,
		Sources: []config.SpektacularRecheckDiscoverySource{{Kind: "standards_feed", Name: "standards", URLOrRepo: "http://feed.invalid/atom"}},
	}
	external, failures := s.collectRecheckDiscovery(context.Background(), Campaign{}, time.Time{})
	if feeds.Load() != 1 || connects.Load() != 0 || len(failures) != 0 || len(external) != 1 || external[0].Title != "signal" {
		t.Fatalf("feeds=%d connects=%d external=%+v failures=%+v", feeds.Load(), connects.Load(), external, failures)
	}

	s.deps.Config.Variables.Security.HTTPAllowlist = nil
	external, failures = s.collectRecheckDiscovery(context.Background(), Campaign{}, time.Time{})
	if feeds.Load() != 1 || len(external) != 0 || len(failures) != 1 || failures[0].Name != "config" {
		t.Fatalf("overlay-removed allowlist still fetched: external=%+v failures=%+v", external, failures)
	}
}

func TestRecheckRecordsDiscoveryOnRewoundGeneration(t *testing.T) {
	published := time.Now().UTC().Format(time.RFC3339)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`<feed><entry><title>signal</title><updated>` + published + `</updated><summary>new standard</summary><link href="https://feed.invalid/signal"/></entry></feed>`))
	}))
	defer relay.Close()
	t.Setenv("NO_PROXY", "*")
	s := recheckTestServer(t)
	s.deps.Config.Variables.Security.HTTPAllowlist = []string{"feed.invalid"}
	s.deps.Config.Runs.Spektacular.Recheck = config.SpektacularRecheckConfig{
		DiscoveryProxy:  relay.URL,
		EgressAllowlist: []string{"source.invalid"},
		Sources:         []config.SpektacularDiscoverySource{{Name: "upstream", Kind: "release", URL: "https://source.invalid/news"}},
		Discovery: config.SpektacularRecheckDiscoveryConfig{
			Enabled: true,
			Sources: []config.SpektacularRecheckDiscoverySource{{Kind: "standards_feed", Name: "standards", URLOrRepo: "http://feed.invalid/atom"}},
		},
	}
	recordCompletedRecheckRun(s, 3, time.Now().Add(-time.Hour))
	path := "/api/campaigns/" + url.PathEscape(recheckTestRunKey) + "/recheck?force=true"
	forced := doOwnerPostAsUser(s, path, "bob", map[string]string{})
	if forced.Code != http.StatusOK {
		t.Fatalf("forced recheck = %d body=%s", forced.Code, forced.Body.String())
	}
	var resp campaignRecheckResponse
	if err := json.Unmarshal(forced.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode forced recheck: %v", err)
	}
	drift := resp.Campaign.Drift
	if drift == nil || drift.ExternalCount != 1 || len(drift.External) != 1 || drift.External[0].Title != "signal" ||
		len(drift.SourcesFailed) != 1 || drift.SourcesFailed[0].Name != "upstream" {
		t.Fatalf("rewound generation drift = %+v", drift)
	}
	archive, err := s.deps.Inception.LoadCampaignArchive(recheckTestRunKey)
	if err != nil {
		t.Fatalf("load archive: %v", err)
	}
	if archive.Drift == nil || archive.Drift.ExternalCount != 1 || len(archive.Drift.External) != 1 || len(archive.Drift.SourcesFailed) != 1 {
		t.Fatalf("stored drift = %+v", archive.Drift)
	}
	if resp.Campaign.Recheck == nil || resp.Campaign.Recheck.ExternalCount != 1 || len(resp.Campaign.Recheck.SourcesFailed) != 1 {
		t.Fatalf("recheck summary = %+v", resp.Campaign.Recheck)
	}
}
