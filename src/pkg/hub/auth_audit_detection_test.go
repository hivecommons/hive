package hub

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// These tests exercise the hub-fronted probe path of runAuthAudit — the code
// that actually detects a wide-open spoke. The pre-existing runAuthAudit tests
// never set a hub domain, so hubFrontedDashboardURL rejected every entry and
// the whole detection loop (probe, urlHealth bookkeeping, unreachable tally,
// WIDE-OPEN alert) ran zero times under test. Setting HIVE_HUB_PUBLIC_URL to
// the httptest loopback host is the seam that lets entries through the
// hub-fronted gate without any production change.

// hubFrontedAuditServer returns a HubServer whose hub domain matches httptest
// loopback URLs (127.0.0.1), so registry entries pointing at httptest servers
// pass the hubFrontedDashboardURL gate inside runAuthAudit.
func hubFrontedAuditServer(t *testing.T) *HubServer {
	t.Helper()
	t.Setenv("HIVE_HUB_PUBLIC_URL", "http://127.0.0.1")
	return &HubServer{
		logger:    slog.Default(),
		urlHealth: newURLHealthState(),
	}
}

func TestRunAuthAuditFlagsWideOpenHubFrontedSpoke(t *testing.T) {
	openSpoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != authAuditPath {
			t.Errorf("probe hit %q, want %q", r.URL.Path, authAuditPath)
		}
		w.WriteHeader(http.StatusOK) // unauthenticated 200 => WIDE OPEN
	}))
	defer openSpoke.Close()

	alerted := make(chan string, 1)
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alerted <- r.Header.Get("Priority")
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfy.Close()

	s := hubFrontedAuditServer(t)
	t.Setenv("HIVE_NTFY_SERVER", ntfy.URL)
	t.Setenv("HIVE_NTFY_TOPIC", "hive-alerts")
	s.registry.Hives = []RegistryEntry{
		{ID: "wide-open", ClusterID: "c1", DashboardURL: openSpoke.URL},
	}

	s.runAuthAudit(context.Background(), newAuditClient())

	select {
	case prio := <-alerted:
		if prio != "high" {
			t.Errorf("ntfy priority = %q, want %q", prio, "high")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wide-open spoke did not trigger an ntfy alert")
	}

	// A 200 is healthy for reachability, so the consecutive-failure count must
	// have been reset (observe recorded a healthy probe), and the last status
	// must be the 200 we served.
	failures, statuses := s.urlHealth.snapshot()
	if n := failures["wide-open"]; n != 0 {
		t.Errorf("consecutive failures = %d, want 0 (200 is healthy)", n)
	}
	if got := statuses["wide-open"]; got != http.StatusOK {
		t.Errorf("last status = %d, want %d", got, http.StatusOK)
	}
}

func TestRunAuthAuditProtectedSpokeIsNotFlagged(t *testing.T) {
	protected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound) // login redirect => protected
	}))
	defer protected.Close()

	alerted := make(chan struct{}, 1)
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alerted <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfy.Close()

	s := hubFrontedAuditServer(t)
	t.Setenv("HIVE_NTFY_SERVER", ntfy.URL)
	t.Setenv("HIVE_NTFY_TOPIC", "hive-alerts")
	s.registry.Hives = []RegistryEntry{
		{ID: "protected", ClusterID: "c1", DashboardURL: protected.URL},
	}

	s.runAuthAudit(context.Background(), newAuditClient())

	select {
	case <-alerted:
		t.Fatal("protected spoke (302) must not trigger a wide-open alert")
	case <-time.After(200 * time.Millisecond):
	}

	// 302 is a healthy reachability answer: no failure streak, status recorded.
	failures, statuses := s.urlHealth.snapshot()
	if n := failures["protected"]; n != 0 {
		t.Errorf("consecutive failures = %d, want 0 (302 is healthy)", n)
	}
	if got := statuses["protected"]; got != http.StatusFound {
		t.Errorf("last status = %d, want %d", got, http.StatusFound)
	}
}

func TestRunAuthAuditUnreachableHubFrontedSpokeCountsAsFailure(t *testing.T) {
	s := hubFrontedAuditServer(t)
	t.Setenv("HIVE_NTFY_SERVER", "")
	t.Setenv("HIVE_NTFY_TOPIC", "")
	s.registry.Hives = []RegistryEntry{
		// Closed port on the hub-fronted host: passes the domain gate, then the
		// probe's transport fails => unreachable, never wide open.
		{ID: "dark", ClusterID: "c1", DashboardURL: "http://127.0.0.1:1"},
	}

	s.runAuthAudit(context.Background(), newAuditClient())

	failures, statuses := s.urlHealth.snapshot()
	if n := failures["dark"]; n != 1 {
		t.Errorf("consecutive failures = %d, want 1 (transport error is unhealthy)", n)
	}
	if got := statuses["dark"]; got != 0 {
		t.Errorf("last status = %d, want 0 (request never completed)", got)
	}
}

func TestRunAuthAuditServingErrorPageIsUnhealthyButNotOpen(t *testing.T) {
	// A routed hostname whose backend is gone: the ingress answers 503, so the
	// HTTP exchange completes (reachable) but the URL is NOT healthy and must
	// never be flagged wide open.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer broken.Close()

	s := hubFrontedAuditServer(t)
	t.Setenv("HIVE_NTFY_SERVER", "")
	t.Setenv("HIVE_NTFY_TOPIC", "")
	s.registry.Hives = []RegistryEntry{
		{ID: "no-backend", ClusterID: "c1", DashboardURL: broken.URL},
	}

	s.runAuthAudit(context.Background(), newAuditClient())

	failures, statuses := s.urlHealth.snapshot()
	if n := failures["no-backend"]; n != 1 {
		t.Errorf("consecutive failures = %d, want 1 (503 is unhealthy)", n)
	}
	if got := statuses["no-backend"]; got != http.StatusServiceUnavailable {
		t.Errorf("last status = %d, want %d", got, http.StatusServiceUnavailable)
	}
}

func TestRunAuthAuditForgetsRecycledHives(t *testing.T) {
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer spoke.Close()

	s := hubFrontedAuditServer(t)
	t.Setenv("HIVE_NTFY_SERVER", "")
	t.Setenv("HIVE_NTFY_TOPIC", "")

	// Seed state for a hive that is no longer registered (recycled pool slot).
	s.urlHealth.observe("gone", urlProbeResult{Status: 503, Healthy: false})

	s.registry.Hives = []RegistryEntry{
		{ID: "alive", ClusterID: "c1", DashboardURL: spoke.URL},
	}
	s.runAuthAudit(context.Background(), newAuditClient())

	failures, statuses := s.urlHealth.snapshot()
	if _, ok := failures["gone"]; ok {
		t.Error("recycled hive's failure count was not forgotten")
	}
	if _, ok := statuses["gone"]; ok {
		t.Error("recycled hive's last status was not forgotten")
	}
	if _, ok := statuses["alive"]; !ok {
		t.Error("registered hive's status missing after audit")
	}
}
