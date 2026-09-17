package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// #7451: Settings → Hub rendered DASHBOARD URL as an ordinary editable input.
// On a hub-managed spoke the hub owns hub.dashboard_url — the heartbeat's
// project-config push reverts any edit — and until the revert lands the
// value is the second-choice OAuth callback origin, so a typo locks the
// operator out. The server now says who owns the field, refuses a hub-owned
// change, and the tab renders it read-only with its provenance.

func TestHubOwnsDashboardURL(t *testing.T) {
	s, deps := apiServer(t)
	if s.hubOwnsDashboardURL() {
		t.Fatal("a self-hosted, never-pushed spoke owns its own dashboard URL")
	}
	// A hub-proxied (hosted) spoke: the hub's ingress terminates the host.
	deps.Config.Dashboard.HubProxied = true
	if !s.hubOwnsDashboardURL() {
		t.Error("a hub-proxied spoke's dashboard URL belongs to the hub")
	}
	deps.Config.Dashboard.HubProxied = false

	// A heartbeat push flips ownership for the rest of the process; a blank
	// push (the hub not speaking to the field) does not.
	s.SetHubPushedDashboardURL("   ")
	if s.hubOwnsDashboardURL() {
		t.Error("a blank push must not claim ownership")
	}
	s.SetHubPushedDashboardURL("https://bluefin.hive.hivecommons.dev")
	if !s.hubOwnsDashboardURL() {
		t.Error("a spoke that received a vanity URL on the heartbeat no longer owns the field")
	}
}

func TestGovernorHubRefusesHubOwnedDashboardURLChange(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Hub.DashboardURL = "https://bluefin.hive.hivecommons.dev"
	deps.Config.Hub.URL = "https://hive.hivecommons.dev"
	deps.Config.Hub.Enabled = true
	s.SetHubPushedDashboardURL(deps.Config.Hub.DashboardURL)

	// The whole save is refused BEFORE any field is applied: a bundled
	// hub.url change must not land either, or a 409 would leave the running
	// config half-updated.
	rec := doPut(s, "/api/config/governor/hub", map[string]any{
		"dashboard_url": "https://typo.example",
		"url":           "https://other-hub.example",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("hub-owned dashboard_url change: status = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "dashboard.public_url") {
		t.Errorf("the refusal does not point the operator at dashboard.public_url: %s", rec.Body.String())
	}
	if deps.Config.Hub.DashboardURL != "https://bluefin.hive.hivecommons.dev" {
		t.Errorf("dashboard_url was changed to %q despite the refusal — that value would have become the OAuth callback origin", deps.Config.Hub.DashboardURL)
	}
	if deps.Config.Hub.URL != "https://hive.hivecommons.dev" {
		t.Errorf("hub.url was applied by a refused save: %q", deps.Config.Hub.URL)
	}

	// Echoing the current value back (what the read-only tab does) is fine,
	// and other hub fields still save.
	rec = doPut(s, "/api/config/governor/hub", map[string]any{
		"dashboard_url": "https://bluefin.hive.hivecommons.dev",
		"is_public":     true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("unchanged dashboard_url: status = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if !deps.Config.Hub.IsPublic {
		t.Error("is_public did not save alongside an unchanged dashboard_url")
	}

	// GET names the owner and exposes the operator-owned override field.
	var resp struct {
		Hub struct {
			DashboardURL      string `json:"dashboard_url"`
			DashboardURLOwner string `json:"dashboard_url_owner"`
			PublicURL         string `json:"public_url"`
		} `json:"hub"`
	}
	deps.Config.Dashboard.PublicURL = "https://bluefin.example.org"
	got := doOwnerGet(s, "/api/config/governor")
	if got.Code != http.StatusOK {
		t.Fatalf("GET governor: %d", got.Code)
	}
	if err := json.Unmarshal(got.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Hub.DashboardURLOwner != "hub" || resp.Hub.PublicURL != "https://bluefin.example.org" {
		t.Errorf("GET hub = %+v, want owner=hub and the public_url override surfaced", resp.Hub)
	}
}

func TestGovernorHubStillAcceptsDashboardURLOnSelfHostedSpoke(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Hub.DashboardURL = "https://old.example"
	rec := doPut(s, "/api/config/governor/hub", map[string]any{"dashboard_url": "https://new.example"})
	if rec.Code != http.StatusOK {
		t.Fatalf("self-hosted change: status = %d %s, want 200 — here hub.dashboard_url is how the spoke tells the hub where it lives", rec.Code, rec.Body.String())
	}
	if deps.Config.Hub.DashboardURL != "https://new.example" {
		t.Errorf("dashboard_url = %q, want the operator's value", deps.Config.Hub.DashboardURL)
	}
	var resp struct {
		Hub struct {
			DashboardURLOwner string `json:"dashboard_url_owner"`
		} `json:"hub"`
	}
	got := doOwnerGet(s, "/api/config/governor")
	_ = json.Unmarshal(got.Body.Bytes(), &resp)
	if resp.Hub.DashboardURLOwner != "spoke" {
		t.Errorf("owner = %q, want spoke", resp.Hub.DashboardURLOwner)
	}
}

// TestHubTabRendersHubOwnedDashboardURLReadOnly pins the tab: the hub-owned
// branch is a disabled input with the provenance line and the public_url
// pointer, the spoke-owned branch keeps the editable input, and the field
// is no longer hard-wired to markDirty regardless of owner.
func TestHubTabRendersHubOwnedDashboardURLReadOnly(t *testing.T) {
	html := indexHTML(t)
	start := strings.Index(html, "function renderHubDashboardURLField(hub, dashUrl)")
	if start < 0 {
		t.Fatal("renderHubDashboardURLField not found")
	}
	end := strings.Index(html[start:], "function renderGovHub(hub)")
	if end < 0 {
		t.Fatal("renderGovHub not found after renderHubDashboardURLField")
	}
	fn := html[start : start+end]
	for _, want := range []string{
		"if (hub.dashboard_url_owner !== 'hub') {",
		`data-arg1="dashboard_url"`, // the editable branch still exists for self-hosted spokes
		`readonly disabled style="opacity:0.7"`,
		"Owned by the hub (heartbeat) — read-only here.",
		"dashboard.public_url",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("renderHubDashboardURLField is missing %q", want)
		}
	}
	// Exactly one editable input for dashboard_url on the whole page, and it
	// lives inside the owner-gated helper — not in renderGovHub unconditionally.
	if n := strings.Count(html, `data-arg1="dashboard_url"`); n != 1 {
		t.Errorf("found %d editable dashboard_url inputs, want exactly 1 (inside the owner-gated helper)", n)
	}
	if !strings.Contains(html, "${renderHubDashboardURLField(hub, dashUrl)}") {
		t.Error("renderGovHub does not route the Dashboard URL field through the owner-gated helper")
	}
}
