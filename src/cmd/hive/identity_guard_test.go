package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hub"
)

// prospectiveGitHubIdentity must mirror the adoption rules in
// GitHubAppConfigCallback exactly. If it drifts, the guard validates an
// identity the spoke will never hold and the real write goes unchecked.

func TestProspectiveIdentity_IncidentPushIsRejected(t *testing.T) {
	// A healthy public spoke: app_id 3568013, empty URLs — the shape ~41 of the
	// fleet runs today.
	cur := config.GitHubConfig{
		AppID:          config.PublicGitHubAppID,
		AppSlug:        config.PublicGitHubAppSlug,
		InstallationID: 149922991,
	}
	// The 2026-07-31 push: the GHE App, with no api_url (this channel has no
	// field for one).
	push := &hub.HeartbeatGitHubAppConfig{
		AppID:   config.EnterpriseGitHubAppID,
		AppSlug: config.EnterpriseGitHubAppSlug,
	}

	prospective := prospectiveGitHubIdentity(cur, push)
	if prospective == nil {
		t.Fatal("a push that changes app_id and app_slug must produce a prospective identity to validate")
	}
	// The pushed app_id MUST appear in the prospective identity. Without this
	// assertion the test passes even if app_id is dropped entirely, because the
	// slug alone trips the legacy slugIsGHE rule — a rejection for the wrong
	// reason, on a marker that is empty on most of the real fleet.
	if prospective.AppID != config.EnterpriseGitHubAppID {
		t.Fatalf("prospective app_id = %d, want the PUSHED %d — the guard must validate the app_id the spoke would actually adopt",
			prospective.AppID, config.EnterpriseGitHubAppID)
	}
	if prospective.AppSlug != config.EnterpriseGitHubAppSlug {
		t.Fatalf("prospective app_slug = %q, want the pushed %q", prospective.AppSlug, config.EnterpriseGitHubAppSlug)
	}
	if prospective.APIURL != "" {
		t.Fatalf("prospective api_url = %q, want empty — the channel cannot deliver one, which is the whole defect", prospective.APIURL)
	}
	if err := config.RejectIdentitySet(*prospective); err == nil {
		t.Fatal("the incident push was ACCEPTED at the spoke write path")
	}
}

// TestProspectiveIdentity_AppIDAloneIsEnoughToReject is the reachability half at
// the spoke boundary. The hub can push an app_id with NO slug (the cluster's
// github_app_slug is empty on several records), leaving every string marker
// blank — so only an app_id-keyed rule can catch it. If this passes while
// app_id is dropped from the prospective identity, the guard is decorative.
func TestProspectiveIdentity_AppIDAloneIsEnoughToReject(t *testing.T) {
	cur := config.GitHubConfig{AppID: config.PublicGitHubAppID} // healthy public spoke, all URLs empty
	push := &hub.HeartbeatGitHubAppConfig{AppID: config.EnterpriseGitHubAppID}

	prospective := prospectiveGitHubIdentity(cur, push)
	if prospective == nil {
		t.Fatal("an app_id-only push must still be validated")
	}
	if prospective.AppSlug != "" || prospective.APIURL != "" || prospective.BaseURL != "" {
		t.Fatalf("premise broken: expected every string marker empty, got %+v", prospective)
	}
	if err := config.RejectIdentitySet(*prospective); err == nil {
		t.Fatal("a GHE app_id pushed onto a public spoke with no other identity field was ACCEPTED — the spoke guard is unreachable on real fleet data")
	}
}

func TestProspectiveIdentity_HealthyPushIsAccepted(t *testing.T) {
	cur := config.GitHubConfig{AppID: config.PlaceholderAppID}
	push := &hub.HeartbeatGitHubAppConfig{
		AppID:   config.PublicGitHubAppID,
		AppSlug: config.PublicGitHubAppSlug,
	}
	prospective := prospectiveGitHubIdentity(cur, push)
	if prospective == nil {
		t.Fatal("a real app_id replacing the placeholder must be validated, not skipped")
	}
	if err := config.RejectIdentitySet(*prospective); err != nil {
		t.Fatalf("a healthy public App push was refused: %v", err)
	}
}

func TestProspectiveIdentity_GHEPushOntoAGHESpokeIsAccepted(t *testing.T) {
	// A GHE spoke already carrying its api_url. Receiving its own cluster's App
	// is consistent and must apply.
	cur := config.GitHubConfig{APIURL: config.EnterpriseGitHubAPIURL}
	push := &hub.HeartbeatGitHubAppConfig{
		AppID:   config.EnterpriseGitHubAppID,
		AppSlug: config.EnterpriseGitHubAppSlug,
	}
	prospective := prospectiveGitHubIdentity(cur, push)
	if prospective == nil {
		t.Fatal("want a prospective identity")
	}
	if err := config.RejectIdentitySet(*prospective); err != nil {
		t.Fatalf("a GHE App delivered to a GHE spoke was refused: %v", err)
	}
}

func TestProspectiveIdentity_NoIdentityFieldsMeansNothingToValidate(t *testing.T) {
	cur := config.GitHubConfig{AppID: config.PublicGitHubAppID, AppSlug: config.PublicGitHubAppSlug}
	// A key-only delivery: the common case, and it must not be gated.
	if got := prospectiveGitHubIdentity(cur, &hub.HeartbeatGitHubAppConfig{PrivateKey: "pem"}); got != nil {
		t.Errorf("a key-only push produced a prospective identity %+v, want nil", got)
	}
	// An echo of the values we already hold.
	if got := prospectiveGitHubIdentity(cur, &hub.HeartbeatGitHubAppConfig{AppSlug: config.PublicGitHubAppSlug}); got != nil {
		t.Errorf("a no-op slug echo produced a prospective identity %+v, want nil", got)
	}
	if got := prospectiveGitHubIdentity(cur, nil); got != nil {
		t.Errorf("a nil push produced %+v, want nil", got)
	}
}

func TestProspectiveIdentity_PlaceholderIsNeverAdopted(t *testing.T) {
	// Mirrors the callback: the placeholder sentinel must not overwrite a real
	// app_id, so it must not appear in the prospective identity either.
	cur := config.GitHubConfig{AppID: config.EnterpriseGitHubAppID, APIURL: config.EnterpriseGitHubAPIURL}
	got := prospectiveGitHubIdentity(cur, &hub.HeartbeatGitHubAppConfig{AppID: config.PlaceholderAppID})
	if got != nil {
		t.Fatalf("the placeholder sentinel was treated as an adoption: %+v", got)
	}
}
