package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// The "Request a Hive" wizard captures WHO is asking, not just which repo they
// want: a first/last name so an operator can map a GitHub login to a person,
// and an optional Slack ID so the hub can reach them.
//
// Both REUSE the existing SaaSUser contact fields (FullName / SlackID) rather
// than introducing a parallel key. That is the property most worth pinning: a
// second Slack key would split one fact across two names, with this form
// writing one and the admin users panel plus the Slack sender reading the
// other. The tests below assert the reuse end-to-end — captured on the request,
// then carried onto the user record at approval, where those readers look.

// helperContactRequestBody builds a request body that is valid apart from the
// contact fields under test.
func helperContactRequestBody(t *testing.T, fullName, slackID string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"org":         "z-innersource",
		"repos":       "repo1",
		"github_host": "github.com",
		"acmm_level":  3,
		"full_name":   fullName,
		"slack_id":    slackID,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(body)
}

// TestRequestProvisionRequiresName pins the required half. Client-side
// validation is not enforcement: a curl or a stale page bypasses it, and a name
// that is usually absent is one nobody can rely on.
func TestRequestProvisionRequiresName(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	authAsForgeTester(t, "ghp_contact_noname", "contact-noname-user")

	for _, name := range []string{"", "   ", "\t\n"} {
		w := postProvisionRequest(t, srv, "ghp_contact_noname", helperContactRequestBody(t, name, ""))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("name %q: expected 400, got %d (body %s)", name, w.Code, w.Body.String())
		}
		if !strings.Contains(strings.ToLower(w.Body.String()), "name") {
			t.Errorf("name %q: rejection should say which field is missing, got %s", name, w.Body.String())
		}
	}
}

// TestRequestProvisionSlackIDIsOptional pins the optional half. The owner asked
// for the Slack ID explicitly as optional, so a request without one must be a
// clean 200 — not a 400, and not a stored placeholder string.
func TestRequestProvisionSlackIDIsOptional(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	const user = "contact-noslack-user"
	authAsForgeTester(t, "ghp_contact_noslack", user)

	w := postProvisionRequest(t, srv, "ghp_contact_noslack", helperContactRequestBody(t, "Ada Lovelace", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with no slack_id, got %d (body %s)", w.Code, w.Body.String())
	}
	pr := loadProvisionRequest(user)
	if pr == nil {
		t.Fatal("request was not persisted")
	}
	if pr.FullName != "Ada Lovelace" {
		t.Errorf("full_name = %q, want %q", pr.FullName, "Ada Lovelace")
	}
	if pr.SlackID != "" {
		t.Errorf("slack_id should stay empty when not supplied, got %q", pr.SlackID)
	}
}

// TestRequestProvisionPersistsContactFields — the values must land on the
// stored request, trimmed. Captured-then-dropped would be worse than not
// asking at all.
func TestRequestProvisionPersistsContactFields(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	const user = "contact-persist-user"
	authAsForgeTester(t, "ghp_contact_persist", user)

	w := postProvisionRequest(t, srv, "ghp_contact_persist",
		helperContactRequestBody(t, "  Ada Lovelace  ", "  U01ABCDEF23  "))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body %s)", w.Code, w.Body.String())
	}
	pr := loadProvisionRequest(user)
	if pr == nil {
		t.Fatal("request was not persisted")
	}
	if pr.FullName != "Ada Lovelace" {
		t.Errorf("full_name = %q, want it trimmed to %q", pr.FullName, "Ada Lovelace")
	}
	if pr.SlackID != "U01ABCDEF23" {
		t.Errorf("slack_id = %q, want it trimmed to %q", pr.SlackID, "U01ABCDEF23")
	}
}

// TestRequestProvisionCapsContactFields pins the length bounds to the SAME
// constants the admin contact editor uses. Truncation, not rejection, matches
// handleAdminUpdateUser's existing treatment of these two fields.
func TestRequestProvisionCapsContactFields(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	const user = "contact-cap-user"
	authAsForgeTester(t, "ghp_contact_cap", user)

	longName := strings.Repeat("a", maxContactNameLen*2)
	longSlack := strings.Repeat("b", maxContactSlackIDLen*2)
	w := postProvisionRequest(t, srv, "ghp_contact_cap", helperContactRequestBody(t, longName, longSlack))
	if w.Code != http.StatusOK {
		t.Fatalf("over-long values should truncate, not reject; got %d (%s)", w.Code, w.Body.String())
	}
	pr := loadProvisionRequest(user)
	if pr == nil {
		t.Fatal("request was not persisted")
	}
	if len([]rune(pr.FullName)) != maxContactNameLen {
		t.Errorf("full_name kept %d runes, want the maxContactNameLen cap of %d",
			len([]rune(pr.FullName)), maxContactNameLen)
	}
	if len([]rune(pr.SlackID)) != maxContactSlackIDLen {
		t.Errorf("slack_id kept %d runes, want the maxContactSlackIDLen cap of %d",
			len([]rune(pr.SlackID)), maxContactSlackIDLen)
	}
}

// TestRequestProvisionStoresHostileNameVerbatim — these are display strings,
// so they are stored as typed and escaped at every RENDER site. Storing a
// pre-escaped or stripped value would corrupt the legitimate half of this set:
// "O'Brien" is a real name, and mangling it to serve an escaping concern is the
// bug, not the fix.
func TestRequestProvisionStoresHostileNameVerbatim(t *testing.T) {
	cases := []struct {
		user  string
		token string
		name  string
	}{
		{"contact-apos-user", "ghp_contact_apos", "Máire O'Brien"},
		{"contact-html-user", "ghp_contact_html", `<script>alert(1)</script>`},
		{"contact-quote-user", "ghp_contact_quote", `Ada "Countess" Lovelace`},
	}
	for _, tc := range cases {
		t.Run(tc.user, func(t *testing.T) {
			cleanup := helperSetupTempDirs(t)
			defer cleanup()
			srv := NewHubServer(0, slog.Default(), "test", "v2")
			authAsForgeTester(t, tc.token, tc.user)

			w := postProvisionRequest(t, srv, tc.token, helperContactRequestBody(t, tc.name, ""))
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d (body %s)", w.Code, w.Body.String())
			}
			pr := loadProvisionRequest(tc.user)
			if pr == nil {
				t.Fatal("request was not persisted")
			}
			if pr.FullName != tc.name {
				t.Errorf("full_name = %q, want it stored verbatim as %q", pr.FullName, tc.name)
			}
		})
	}
}

// TestApprovalCarriesContactFieldsOntoUser is the reason this feature is worth
// anything. The admin users panel and the Slack sender read SaaSUser, NOT
// ProvisionRequest — so a value that stops at the request file is invisible to
// every consumer of it. Slack messaging shipped with no user having a slack_id;
// this is the path that finally populates one.
func TestApprovalCarriesContactFieldsOntoUser(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const user = "contact-approve-user"
	pr := &ProvisionRequest{
		Username:    user,
		Org:         "z-innersource",
		Repos:       "repo1",
		PrimaryRepo: "repo1",
		GitHubHost:  "github.com",
		ACMMLevel:   3,
		Status:      provisionStatusPending,
		FullName:    "Ada Lovelace",
		SlackID:     "U01ABCDEF23",
	}
	if err := saveProvisionRequest(pr); err != nil {
		t.Fatalf("seed request: %v", err)
	}

	u := ensureSaaSUser(user)
	if u.FullName != "" || u.SlackID != "" {
		t.Fatalf("precondition: a fresh user must have no contact details, got %q/%q", u.FullName, u.SlackID)
	}
	applyRequestContactToUser(u, pr)
	if err := saveSaaSUser(u); err != nil {
		t.Fatalf("save user: %v", err)
	}

	got := loadSaaSUser(user)
	if got == nil {
		t.Fatal("user was not persisted")
	}
	if got.FullName != "Ada Lovelace" {
		t.Errorf("user.FullName = %q, want the requested %q", got.FullName, "Ada Lovelace")
	}
	if got.SlackID != "U01ABCDEF23" {
		t.Errorf("user.SlackID = %q, want the requested %q — this is the field the "+
			"Slack sender reads", got.SlackID, "U01ABCDEF23")
	}
}

// TestApprovalNeverOverwritesCuratedContactFields — an admin who has already
// curated a user's contact details (or a user who corrected them) outranks
// whatever was typed into a request form, and a re-approval must not silently
// revert that. The carry-over fills a blank; it does not overwrite.
func TestApprovalNeverOverwritesCuratedContactFields(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const user = "contact-curated-user"
	u := ensureSaaSUser(user)
	u.FullName = "Augusta Ada King"
	u.SlackID = "U99CURATED"
	if err := saveSaaSUser(u); err != nil {
		t.Fatalf("seed curated user: %v", err)
	}

	pr := &ProvisionRequest{
		Username: user,
		Status:   provisionStatusPending,
		FullName: "typo mcTypoface",
		SlackID:  "U00WRONG",
	}
	applyRequestContactToUser(u, pr)

	if u.FullName != "Augusta Ada King" {
		t.Errorf("curated FullName was overwritten to %q", u.FullName)
	}
	if u.SlackID != "U99CURATED" {
		t.Errorf("curated SlackID was overwritten to %q", u.SlackID)
	}
}

// TestApprovalContactCarryIsNilSafe — the carry-over runs on the approval path
// next to other work that can legitimately leave either side absent. Neither a
// nil user nor a nil request may panic there.
func TestApprovalContactCarryIsNilSafe(t *testing.T) {
	applyRequestContactToUser(nil, &ProvisionRequest{FullName: "Ada"})
	applyRequestContactToUser(&SaaSUser{}, nil)
	applyRequestContactToUser(nil, nil)
}
