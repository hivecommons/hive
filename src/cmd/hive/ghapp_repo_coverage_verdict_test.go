package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/github"
)

// Tests for classifyGitHubAppRepoCoverage (#4360), the boot-time check that
// asks whether the App installation actually covers the configured repos.
// The stakes are asymmetric: a false "not covered" verdict sends an operator
// to change an installation setting that was already correct, while a missed
// one leaves the misleading "key never arrived" story in place. These tests
// pin both directions.

// repoCoverageServer stubs the two endpoints classifyGitHubAppRepoCoverage
// exercises through a real AppAuth: the installation-token mint and the
// installation repository listing. verdictTestAuth (ghapp_banner_verdict_test.go)
// builds the AppAuth with a fresh key, so unlike the pkg/github tests we
// cannot pre-seed a cached token — the mint endpoint must answer for real.
func repoCoverageServer(t *testing.T, listing http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/app/installations/%d/access_tokens", verdictTestInstallationID),
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "test-installation-token",
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
		})
	mux.HandleFunc("/installation/repositories", listing)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// listingOf serves a single-page repository listing with the given full names.
func listingOf(full ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repos := make([]map[string]any, 0, len(full))
		for _, f := range full {
			repos = append(repos, map[string]any{"full_name": f})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count":  len(full),
			"repositories": repos,
		})
	}
}

// A hive with no App auth at all has nothing to check; the verdict must be
// silence, not an accusation.
func TestClassifyGitHubAppRepoCoverage_NilAuthStaysSilent(t *testing.T) {
	raise, msg, state := classifyGitHubAppRepoCoverage(
		context.Background(), nil, "acme", []string{"widgets"}, verdictTestLogger())
	if raise {
		t.Error("nil AppAuth must not raise the banner")
	}
	if msg != "" {
		t.Errorf("msg = %q, want empty", msg)
	}
	if state != github.AppStateUnknown {
		t.Errorf("state = %s, want unknown", state)
	}
}

// No configured repos means there is nothing the installation could fail to
// cover. No API call should be needed to conclude that — a nil auth plus empty
// repos both short-circuit, so pass a live-looking auth and an empty list.
func TestClassifyGitHubAppRepoCoverage_NoConfiguredReposStaysSilent(t *testing.T) {
	srv := repoCoverageServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no configured repos must not trigger a coverage listing")
		w.WriteHeader(http.StatusInternalServerError)
	})

	raise, _, state := classifyGitHubAppRepoCoverage(
		context.Background(), verdictTestAuth(t, srv.URL), "acme", nil, verdictTestLogger())
	if raise {
		t.Error("an empty repo list must not raise the banner")
	}
	if state != github.AppStateUnknown {
		t.Errorf("state = %s, want unknown", state)
	}
}

// An error fetching the listing is NOT a verdict. The credential checks tell
// that story better; this classifier must defer with raise=false rather than
// accuse every configured repo at once.
func TestClassifyGitHubAppRepoCoverage_ListingErrorDefersToCredentialChecks(t *testing.T) {
	srv := repoCoverageServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "boom"})
	})

	raise, msg, state := classifyGitHubAppRepoCoverage(
		context.Background(), verdictTestAuth(t, srv.URL), "acme", []string{"widgets"}, verdictTestLogger())
	if raise {
		t.Error("a failed listing must not raise the coverage banner")
	}
	if msg != "" {
		t.Errorf("msg = %q, want empty when the listing could not be fetched", msg)
	}
	if state != github.AppStateUnknown {
		t.Errorf("state = %s, want unknown so the credential checks still run", state)
	}
}

// The healthy case: every configured repo is covered, including bare names
// that need the org prefix and case differences GitHub treats as equal.
func TestClassifyGitHubAppRepoCoverage_FullCoverageIsOK(t *testing.T) {
	srv := repoCoverageServer(t, listingOf("acme/widgets", "acme/Gadgets"))

	raise, msg, state := classifyGitHubAppRepoCoverage(
		context.Background(), verdictTestAuth(t, srv.URL), "acme",
		[]string{"widgets", "acme/gadgets"}, verdictTestLogger())
	if raise {
		t.Errorf("full coverage must not raise the banner (msg=%q)", msg)
	}
	if msg != "" {
		t.Errorf("msg = %q, want empty for full coverage", msg)
	}
	if state != github.AppStateOK {
		t.Errorf("state = %s, want ok", state)
	}
}

// The live #4360 shape: right org, right installation, one configured repo
// simply not ticked. The banner must raise, carry the repo-not-covered state,
// and name the missing repo — this is the case that used to be misreported as
// an undelivered private key.
func TestClassifyGitHubAppRepoCoverage_MissingRepoRaisesWithAccurateCopy(t *testing.T) {
	srv := repoCoverageServer(t, listingOf("acme/widgets"))

	raise, msg, state := classifyGitHubAppRepoCoverage(
		context.Background(), verdictTestAuth(t, srv.URL), "acme",
		[]string{"widgets", "gizmos"}, verdictTestLogger())
	if !raise {
		t.Fatal("an uncovered configured repo must raise the banner")
	}
	if state != github.AppStateRepoNotCovered {
		t.Errorf("state = %s, want repo-not-covered", state)
	}
	if !state.UserActionable() {
		t.Error("repo-not-covered is fixed in the App's repository selection — it is the user's to fix")
	}
	if !strings.Contains(msg, "acme/gizmos") {
		t.Errorf("message must name the missing repo; got %q", msg)
	}
	if strings.Contains(msg, "acme/widgets") {
		t.Errorf("message must not accuse the covered repo; got %q", msg)
	}
	if strings.Contains(strings.ToLower(msg), "private key") && !strings.Contains(strings.ToLower(msg), "repo") {
		t.Errorf("message must tell the repo-selection story, not the key story; got %q", msg)
	}
}
