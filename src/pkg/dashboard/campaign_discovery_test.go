package dashboard

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
	"github.com/hivecommons/hive/pkg/knowledge"
)

func TestDiscoverySnapshotsAndFailures(t *testing.T) {
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
	for _, path := range []string{"/release", "/large", "/redirect", "/error"} {
		sources = append(sources, config.SpektacularDiscoverySource{Name: path, Kind: "release", URL: server.URL + path})
	}
	evidence := collectDiscovery(context.Background(), sources, client)
	if requests.Load() != 4 || len(evidence) != 4 {
		t.Fatalf("requests=%d evidence=%v", requests.Load(), evidence)
	}
	if evidence[0].Evidence != "v2 adds a capability" || len(evidence[0].SHA256) != 64 || evidence[0].URL != sources[0].URL {
		t.Fatalf("lost source evidence: %+v", evidence[0])
	}
	for _, item := range evidence[1:] {
		if item.Error == "" || item.Evidence != "" {
			t.Fatalf("failure treated as evidence: %+v", item)
		}
	}
	// Round-trip the evidence through the revision store before projecting JSON.
	s := newMinimalServer(t)
	s.deps.Inception = knowledge.NewInceptionEngine(t.TempDir(), nil, s.logger)
	revision, err := s.deps.Inception.ReviseExternalCampaign("prior", "Research", "prior", "Spektacular", "spektacular", "owner", nil, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.deps.Inception.SetCampaignDrift(revision.ID, &knowledge.CampaignDrift{DriftSource: evidence}); err != nil {
		t.Fatal(err)
	}
	archive, err := s.deps.Inception.LoadCampaignArchive(revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	drift := campaignDriftFromArchive(*archive)
	body, err := json.Marshal(Campaign{Drift: drift})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"drift_source"`) || !strings.Contains(string(body), "v2 adds a capability") {
		t.Fatalf("missing evidence: %s", body)
	}
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
	s := &Server{deps: &Dependencies{Config: &config.Config{}}}
	cfg := &s.deps.Config.Runs.Spektacular.Recheck
	cfg.DiscoveryProxy = relay.URL
	cfg.EgressAllowlist = []string{"source.invalid"}
	if got := s.discoverRecheckSources(context.Background()); len(got) != 0 || requests.Load() != 0 {
		t.Fatal("empty sources performed discovery")
	}
	cfg.Sources = []config.SpektacularDiscoverySource{{Name: "source", Kind: "standards", URL: "https://source.invalid/news"}}
	got := s.discoverRecheckSources(context.Background())
	if requests.Load() != 1 || len(got) != 1 || got[0].Error == "" {
		t.Fatalf("relay not enforced: requests=%d evidence=%+v", requests.Load(), got)
	}
	cfg.Sources[0].URL = "https://outside.invalid/news"
	got = s.discoverRecheckSources(context.Background())
	if requests.Load() != 1 || len(got) != 1 || !strings.Contains(got[0].Error, "allow-list") {
		t.Fatalf("invalid source reached network: %+v", got)
	}
}

func TestRecheckAttachesDiscoveryToRevision(t *testing.T) {
	t.Skip("Spek continuous convergence recheck is disabled on v6 pending #10734")
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer relay.Close()
	hub, s := covK2Hub(t)
	s.contributeHub = hub
	s.deps.Inception = knowledge.NewInceptionEngine(t.TempDir(), nil, s.logger)
	s.deps.Config.Runs.Spektacular.Recheck = config.SpektacularRecheckConfig{
		DiscoveryProxy: relay.URL, EgressAllowlist: []string{"source.invalid"},
		Sources: []config.SpektacularDiscoverySource{{Name: "upstream", Kind: "release", URL: "https://source.invalid/news"}},
	}
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	base, err := s.deps.Inception.UpsertExternalCampaign("discovery-base", "Research", "discovery-base", "Spektacular", "spektacular", []string{"acme/tool"}, now)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := s.triggerCampaignRecheck(context.Background(), base.ID, "owner", recheckReasonManual, true, now)
	if err != nil {
		t.Fatal(err)
	}
	if revision.RevisionOf != base.ID || revision.Drift == nil || len(revision.Drift.DriftSource) != 1 || revision.Drift.DriftSource[0].Name != "upstream" || revision.Drift.DriftSource[0].Error == "" {
		t.Fatalf("missing revision evidence: %+v", revision)
	}
	stored, err := s.deps.Inception.LoadCampaignArchive(revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Drift == nil || len(stored.Drift.DriftSource) != 1 {
		t.Fatalf("evidence not retained: %+v", stored)
	}
	original, err := s.deps.Inception.LoadCampaignArchive(base.ID)
	if err != nil {
		t.Fatal(err)
	}
	if original.Drift != nil {
		t.Fatal("discovery overwrote prior campaign")
	}
}
