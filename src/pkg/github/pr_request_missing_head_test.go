package github

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #5343. An agent commits a branch, cannot push it because the credential
// helper is unreachable from its UID, and the only thing anyone sees is the
// downstream 404 on compare — which reads as "the branch doesn't exist on the
// remote repository" and sends an operator to investigate branch creation.
//
// These tests pin the DIAGNOSIS, not the wording: the failure must name the
// push/credential cause when the branch is genuinely absent, must name the
// installation when the whole repo is invisible, and must NOT claim a push
// failure in any case where it cannot prove one.

// missingHeadServer serves a repo whose compare 404s and whose head ref is
// absent, i.e. the exact shape of an unpushed branch.
func missingHeadServer(t *testing.T, repoStatus, refStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/compare/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		case strings.Contains(r.URL.Path, "/git/ref/"):
			w.WriteHeader(refStatus)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		default: // GET /repos/o/r
			w.WriteHeader(repoStatus)
			if repoStatus == http.StatusOK {
				_, _ = io.WriteString(w, `{"name":"r","default_branch":"main"}`)
			} else {
				_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			}
		}
	}))
}

func TestContentGateReportsUnpushedBranchAsAuthFailure(t *testing.T) {
	srv := missingHeadServer(t, http.StatusOK, http.StatusNotFound)
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.Default())

	err := c.validatePRRequestContent(context.Background(),
		PRRequest{Repo: "o/r", Base: "main", Head: "fix/x"})
	if err == nil {
		t.Fatal("a missing head ref must be an error")
	}

	// The whole point: the operator must be pointed at credentials.
	got := err.Error()
	for _, want := range []string{"never pushed", "PUSH AUTHENTICATION", "git-credential-hive.sh", "HIVE_AGENT_TOKEN_CACHE"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnosis does not mention %q:\n%s", want, got)
		}
	}
	// And explicitly NOT at branch creation.
	if strings.Contains(strings.ToLower(got), "branch-creation problem") &&
		!strings.Contains(got, "not a branch-creation problem") {
		t.Errorf("diagnosis blames branch creation:\n%s", got)
	}

	// It must remain a retryable error, not a policy rejection: the agent
	// pushing its branch makes the same request valid.
	if _, isPolicy := prContentMetadataReason(err); isPolicy {
		t.Error("an unpushed branch must not be quarantined as a permanent policy rejection")
	}
	if _, ok := missingHeadReason(err); !ok {
		t.Error("missingHeadReason must recognise the error so the watcher can log it")
	}
}

func TestClaimsGateReportsUnpushedBranchAsAuthFailure(t *testing.T) {
	srv := missingHeadServer(t, http.StatusOK, http.StatusNotFound)
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.Default())

	// The claims gate only compares when the title makes an artifact claim.
	// "test" trips titleArtifactRules; without such a word the gate returns
	// before any API call and this test would pass vacuously.
	_, _, err := c.validatePRRequestClaims(context.Background(), PRRequest{
		Repo: "o/r", Base: "main", Head: "fix/x",
		Title: "docs: add a test for the thing",
	})
	if err == nil {
		t.Fatal("the claims gate must surface the unpushed branch; it compared and got a 404")
	}
	if reason, ok := missingHeadReason(err); !ok {
		t.Errorf("claims gate did not diagnose the unpushed branch: %v", err)
	} else if !strings.Contains(reason, "PUSH AUTHENTICATION") {
		t.Errorf("claims-gate diagnosis is not the push-auth one: %s", reason)
	}
}

func TestInvisibleRepoIsNotReportedAsAPushFailure(t *testing.T) {
	// The repo itself 404s — the App is not installed on it, or the
	// installation cannot see it. Blaming the branch here would be wrong.
	srv := missingHeadServer(t, http.StatusNotFound, http.StatusNotFound)
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.Default())

	err := c.validatePRRequestContent(context.Background(),
		PRRequest{Repo: "o/r", Base: "main", Head: "fix/x"})
	if err == nil {
		t.Fatal("an invisible repository must be an error")
	}
	got := err.Error()
	if !strings.Contains(got, "cannot see that repository") {
		t.Errorf("invisible repo not diagnosed:\n%s", got)
	}
	if strings.Contains(got, "PUSH AUTHENTICATION") {
		t.Errorf("invisible repo was mislabelled as a push failure:\n%s", got)
	}
}

func TestExistingHeadRefDoesNotClaimAPushFailure(t *testing.T) {
	// Compare 404s but the head ref IS present, so the 404 was about
	// something else (most likely the base). Asserting a push failure here
	// would be exactly the kind of confident-but-wrong message #5343 is about.
	srv := missingHeadServer(t, http.StatusOK, http.StatusOK)
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.Default())

	err := c.validatePRRequestContent(context.Background(),
		PRRequest{Repo: "o/r", Base: "nonexistent-base", Head: "fix/x"})
	if err == nil {
		t.Fatal("a 404 comparison must be an error")
	}
	got := err.Error()
	if strings.Contains(got, "PUSH AUTHENTICATION") {
		t.Errorf("a present head ref was blamed on push auth:\n%s", got)
	}
	if !strings.Contains(got, "base branch") {
		t.Errorf("diagnosis does not point at the base branch:\n%s", got)
	}
	if _, ok := missingHeadReason(err); ok {
		t.Error("a present head ref must not be reported as a missing head")
	}
}

func TestNon404ComparisonKeepsItsOwnIdentity(t *testing.T) {
	// A 403 (rate limit, revoked installation, forced-proxy denial) must NOT
	// be rewritten into a push-auth story. Existing retry and rate-limit
	// handling keys on these errors.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.Default())

	err := c.validatePRRequestContent(context.Background(),
		PRRequest{Repo: "o/r", Base: "main", Head: "fix/x"})
	if err == nil {
		t.Fatal("a 403 comparison must be an error")
	}
	if _, ok := missingHeadReason(err); ok {
		t.Errorf("a 403 was misdiagnosed as a missing head branch: %v", err)
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("the underlying 403 lost its identity: %v", err)
	}
}
