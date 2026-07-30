package dashboard

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// spokeIndexHTML loads the real spoke dashboard file the server serves.
func spokeIndexHTML(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading static/index.html: %v", err)
	}
	return string(b)
}

// TestGitHubAppBannerHandlesOperatorSideStates locks the banner contract: the
// spoke dashboard must branch on the classified state and must not send a user
// to an install page when the failure is the operator's to fix.
func TestGitHubAppBannerHandlesOperatorSideStates(t *testing.T) {
	html := spokeIndexHTML(t)

	required := []struct {
		snippet string
		why     string
	}{
		{"ghAppIsOperatorSide", "the operator-side predicate must exist"},
		{"GH_APP_STATE_KEY_MISSING = 'key-missing'", "the key-missing wire token must match github.AppStateKeyMissing.String()"},
		{"GH_APP_STATE_KEY_INVALID = 'key-invalid'", "the key-invalid wire token must match github.AppStateKeyInvalid.String()"},
		{"data.githubAppState", "the banner must read the classified state from the status payload"},
		{"operator-issue", "operator-side failures need their own non-alarming styling"},
		{"no action needed from you", "the operator-side banner must tell the user they need do nothing"},
	}
	for _, r := range required {
		if !strings.Contains(html, r.snippet) {
			t.Errorf("static/index.html is missing %q — %s", r.snippet, r.why)
		}
	}
}

// TestGitHubAppBannerOperatorBranchOffersNoInstallLink is the specific
// regression for the reported bug: a user whose App is installed correctly but
// whose spoke holds the wrong key was shown "Install the GitHub App". The
// operator-side branch must hide the action link instead.
func TestGitHubAppBannerOperatorBranchOffersNoInstallLink(t *testing.T) {
	html := spokeIndexHTML(t)

	const branchStart = "if (isOperatorSide) {"
	i := strings.Index(html, branchStart)
	if i < 0 {
		t.Fatal("static/index.html has no operator-side banner branch")
	}
	// Bound the branch at its early return, which is where it ends.
	rest := html[i:]
	end := strings.Index(rest, "return;")
	if end < 0 {
		t.Fatal("operator-side branch does not return early")
	}
	branch := rest[:end]

	if !strings.Contains(branch, "oLink.removeAttribute('href')") {
		t.Error("the operator-side branch must remove the install link href")
	}
	// The forbidden strings are the exact misdirection the fix removes.
	for _, forbidden := range []string{"githubAppInstallURL", "Install GitHub App"} {
		if strings.Contains(branch, forbidden) {
			t.Errorf("the operator-side branch must not reference %q — the user cannot fix this by installing", forbidden)
		}
	}
	if !strings.Contains(branch, "hub administrator") && !strings.Contains(branch, "githubAppPermIssue") {
		t.Error("the operator-side branch must point the reader at the hub administrator")
	}
}

// TestWelcomeChecklistDoesNotBlameUserForOperatorFailure guards the journey
// nudge on the spoke: an operator-side block is not an unfinished user step.
func TestWelcomeChecklistDoesNotBlameUserForOperatorFailure(t *testing.T) {
	html := spokeIndexHTML(t)

	const marker = "if (ghAppIsOperatorSide(data.githubAppState)) return null;"
	if !strings.Contains(html, marker) {
		t.Error("the github-app welcome step must return null (unknown), not false, for an operator-side block")
	}
	if !strings.Contains(html, "installing the App again will not help") {
		t.Error("welcomeInstallGitHubApp must refuse to open an install page for an operator-side block")
	}
}

// TestGitHubAppStatusFieldIsExposed proves the classified state actually
// reaches the browser — the banner logic above is dead code without it.
func TestGitHubAppStatusFieldIsExposed(t *testing.T) {
	f, ok := reflect.TypeOf(StatusPayload{}).FieldByName("GitHubAppState")
	if !ok {
		t.Fatal("StatusPayload has no GitHubAppState field — the banner logic would be dead code")
	}
	// The JSON tag is the name the dashboard actually reads.
	if got := f.Tag.Get("json"); !strings.HasPrefix(got, "githubAppState") {
		t.Errorf("GitHubAppState json tag = %q, want it to serialise as githubAppState", got)
	}
}

// TestSetGitHubAppStateRoundTrips covers the setter/getter and the clearing
// contract: turning the requirement off must also clear the classified state,
// or a stale operator-side token would suppress future genuine nudges.
func TestSetGitHubAppStateRoundTrips(t *testing.T) {
	s := &Server{}
	s.SetGitHubAppRequired(true)
	s.SetGitHubAppState("key-invalid")
	if got := s.GetGitHubAppState(); got != "key-invalid" {
		t.Fatalf("GetGitHubAppState() = %q, want key-invalid", got)
	}
	s.SetGitHubAppRequired(false)
	if got := s.GetGitHubAppState(); got != "" {
		t.Errorf("GetGitHubAppState() = %q after clearing the requirement, want empty", got)
	}
}
