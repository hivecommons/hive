package apphealth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// Tests for the same-account rename case (#19915). The live shape: the hive's
// config names tuna-os/bootc-installer, the repository was renamed to
// tuna-os/installer, the installation covers it under the new name, and GitHub
// redirects the old name. The hive works, so the recheck must not tell an org
// owner to tick a repository name that no longer exists.

// renamedRepoID is an arbitrary repository id for the redirect target.
const renamedRepoID = 4242

// renameServer stubs the token mint, the installation listing, and GitHub's
// rename redirect: GET /repos/{old} answers 301 to /repositories/{id}, which
// returns the repository under its current name.
func renameServer(t *testing.T, listing http.HandlerFunc, oldFull, newFull string) *httptest.Server {
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
	var srv *httptest.Server
	mux.HandleFunc("/repos/"+oldFull, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("%s/repositories/%d", srv.URL, renamedRepoID), http.StatusMovedPermanently)
	})
	mux.HandleFunc(fmt.Sprintf("/repositories/%d", renamedRepoID), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": renamedRepoID, "full_name": newFull})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestClassifyGitHubAppRepoCoverage_RenamedRepoCoveredUnderNewNameDoesNotRaise(t *testing.T) {
	srv := renameServer(t, listingOf("tuna-os/installer", "tuna-os/tunaos"),
		"tuna-os/bootc-installer", "tuna-os/installer")

	raise, msg, state := ClassifyRepoCoverage(
		context.Background(), verdictTestAuth(t, srv.URL), "tuna-os",
		[]string{"tunaos", "bootc-installer"}, verdictTestLogger())

	if raise {
		t.Fatalf("a repo renamed within the account and covered under its new name must not raise; msg=%q", msg)
	}
	if state != github.AppStateOK {
		t.Errorf("state = %s, want ok", state)
	}
}

// A rename that is still not ticked under its new name is a real gap and must
// keep the not-covered verdict.
func TestClassifyGitHubAppRepoCoverage_RenamedRepoNotTickedStillRaises(t *testing.T) {
	srv := renameServer(t, listingOf("tuna-os/tunaos"),
		"tuna-os/bootc-installer", "tuna-os/installer")

	raise, msg, state := ClassifyRepoCoverage(
		context.Background(), verdictTestAuth(t, srv.URL), "tuna-os",
		[]string{"tunaos", "bootc-installer"}, verdictTestLogger())

	if !raise {
		t.Fatal("a renamed repo the installation does not cover under any name must still raise")
	}
	if state != github.AppStateRepoNotCovered {
		t.Errorf("state = %s, want repo-not-covered", state)
	}
	if !strings.Contains(msg, "tuna-os/bootc-installer") {
		t.Errorf("msg = %q, want it to name the uncovered repo", msg)
	}
}

// Only the renamed repo is dropped: another genuinely unticked repo in the same
// config still raises and is the only one named.
func TestClassifyGitHubAppRepoCoverage_RenameDoesNotHideOtherGaps(t *testing.T) {
	srv := renameServer(t, listingOf("tuna-os/installer"),
		"tuna-os/bootc-installer", "tuna-os/installer")

	raise, msg, state := ClassifyRepoCoverage(
		context.Background(), verdictTestAuth(t, srv.URL), "tuna-os",
		[]string{"bootc-installer", "gizmos"}, verdictTestLogger())

	if !raise || state != github.AppStateRepoNotCovered {
		t.Fatalf("got (raise=%v, state=%s), want (true, repo-not-covered)", raise, state)
	}
	if !strings.Contains(msg, "tuna-os/gizmos") {
		t.Errorf("msg = %q, want it to name the unticked repo", msg)
	}
	if strings.Contains(msg, "bootc-installer") {
		t.Errorf("msg = %q, must not name the renamed repo the installation covers", msg)
	}
}
