package automerge

// Stale Dependabot PRs (hivecommons/hive#18441): behind -> App update-branch,
// conflicting with only Dependabot's commits -> left to Dependabot,
// conflicting and edited by others -> closed once. No `@dependabot` command
// is ever posted.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestDecideDependabotStale(t *testing.T) {
	cases := []struct {
		name   string
		state  string
		edited bool
		want   dependabotStaleDecision
	}{
		{name: "behind is updated", state: "behind", want: dependabotStaleUpdateBranch},
		{name: "behind and edited by others is still updated", state: "behind", edited: true, want: dependabotStaleUpdateBranch},
		{name: "conflicting with only dependabot commits is left to the bot", state: "dirty", want: dependabotStaleLeaveToBot},
		{name: "conflicting state alias", state: "Conflicting", want: dependabotStaleLeaveToBot},
		{name: "conflicting and edited by others is closed", state: "dirty", edited: true, want: dependabotStaleClose},
		{name: "clean is untouched", state: "clean", edited: true, want: dependabotStaleNone},
		{name: "unknown is untouched", state: "", want: dependabotStaleNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideDependabotStale(tc.state, tc.edited); got != tc.want {
				t.Fatalf("decideDependabotStale(%q, %v) = %v, want %v", tc.state, tc.edited, got, tc.want)
			}
		})
	}
}

func TestDependabotUpdateBranchAllowed(t *testing.T) {
	dependabot := currentBaseTestPR(false, "dependabot/go_modules/src/golang.org/x/image-0.48.0", "dependabot[bot]", "behind")
	renovateRef := currentBaseTestPR(false, "renovate/x", "dependabot[bot]", "behind")
	renovate := currentBaseTestPR(false, "renovate/x", "renovate[bot]", "behind")
	for _, tc := range []struct {
		name string
		pr   *gh.PullRequest
		step string
		want bool
	}{
		{name: "dependabot update-branch", pr: dependabot, step: "update-branch", want: true},
		{name: "dependabot current-base", pr: dependabot, step: "current-base", want: true},
		{name: "dependabot merge-forward stays with the bot", pr: dependabot, step: "merge-forward"},
		{name: "dependabot merge-train stays with the bot", pr: dependabot, step: "merge-train"},
		{name: "renovate head ref", pr: renovateRef, step: "update-branch"},
		{name: "renovate", pr: renovate, step: "update-branch"},
		{name: "nil", step: "update-branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dependabotUpdateBranchAllowed(tc.pr, tc.step); got != tc.want {
				t.Fatalf("dependabotUpdateBranchAllowed = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUpdateBranchAndSkipUpdatesBehindDependabotPR: a behind Dependabot PR is
// brought up to date with the App's update-branch call and no comment.
func TestUpdateBranchAndSkipUpdatesBehindDependabotPR(t *testing.T) {
	const head = "sha-head"
	var mu sync.Mutex
	updates, comments := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if servePRCommitsSigned(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "open", "head": map[string]string{"sha": head}, "base": map[string]string{"ref": "main", "sha": "base1"}})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/update-branch":
			updates++
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"message":"Updating pull request branch."}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			comments++
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newAutoMergeSweepClient(srv.URL)
	pr := currentBaseTestPR(false, "dependabot/npm_and_yarn/x", "dependabot[bot]", "behind")
	event, _, err := c.updateBranchAndSkip(context.Background(), "acme/widget", "acme", "widget", 7, head, "behind", pr)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if updates != 1 || !event.BranchUpdated {
		t.Fatalf("update-branch calls = %d (BranchUpdated %v), want 1", updates, event.BranchUpdated)
	}
	if comments != 0 {
		t.Fatalf("comments = %d, want 0", comments)
	}
}

type dependabotStaleAPI struct {
	mu       sync.Mutex
	commits  []map[string]any
	comments []string
	closes   int
}

func (a *dependabotStaleAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/commits":
			_ = json.NewEncoder(w).Encode(a.commits)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			a.comments = append(a.comments, body.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"html_url":"https://example.test/c/1"}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/acme/widget/pulls/7":
			a.closes++
			_, _ = w.Write([]byte(`{"number":7,"state":"closed"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func dependabotCommit(sha, login, name string) map[string]any {
	out := map[string]any{
		"sha":    sha,
		"commit": map[string]any{"message": "bump", "author": map[string]any{"name": name}},
	}
	if login != "" {
		out["author"] = map[string]any{"login": login}
	}
	return out
}

// TestResolveConflictingDependabotPR pins the conflicting half of the
// decision: only Dependabot's commits -> hold for Dependabot's own rebase;
// edited by github-actions[bot] (the NOTICE autofix) -> closed once per head
// with a note, never a `@dependabot` command; a non-Dependabot PR is not
// handled.
func TestResolveConflictingDependabotPR(t *testing.T) {
	onlyDependabot := []map[string]any{dependabotCommit("c1", "dependabot[bot]", "dependabot[bot]")}
	editedByActions := []map[string]any{
		dependabotCommit("c1", "dependabot[bot]", "dependabot[bot]"),
		dependabotCommit("c2", githubActionsBotLogin, githubActionsBotLogin),
	}
	// The lane's own update-branch merge is a non-Dependabot commit too:
	// Dependabot stops rebasing a branch once anyone else has committed to it,
	// so a PR the App merged the base into and that later conflicts is stuck
	// and is closed like any other edited PR (owner decision on #18446).
	editedByAppMerge := []map[string]any{
		dependabotCommit("c1", "dependabot[bot]", "dependabot[bot]"),
		dependabotCommit("c2", "hivecommons-hive[bot]", "hivecommons-hive[bot]"),
	}
	cases := []struct {
		name       string
		author     string
		commits    []map[string]any
		wantOK     bool
		wantReason string
		wantClose  int
	}{
		{name: "conflicting, only dependabot commits", author: "dependabot[bot]", commits: onlyDependabot, wantOK: true, wantReason: "conflicting"},
		{name: "conflicting, edited by others", author: "dependabot[bot]", commits: editedByActions, wantOK: true, wantReason: dependabotStaleClosedReason, wantClose: 1},
		{name: "conflicting after the App's own update-branch merge", author: "dependabot[bot]", commits: editedByAppMerge, wantOK: true, wantReason: dependabotStaleClosedReason, wantClose: 1},
		{name: "not a dependabot PR", author: "renovate[bot]", commits: editedByActions},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &dependabotStaleAPI{commits: tc.commits}
			srv := api.server(t)
			defer srv.Close()
			c := newAutoMergeSweepClient(srv.URL)
			pr := currentBaseTestPR(false, "dependabot/go_modules/x", tc.author, "dirty")

			reason, ok := c.resolveConflictingDependabotPR(context.Background(), "acme/widget", "acme", "widget", pr, 7, "sha-head")
			if ok != tc.wantOK || reason != tc.wantReason {
				t.Fatalf("resolve = %q, %v; want %q, %v", reason, ok, tc.wantReason, tc.wantOK)
			}
			// A second pass on the same head never repeats the note or close.
			if again, _ := c.resolveConflictingDependabotPR(context.Background(), "acme/widget", "acme", "widget", pr, 7, "sha-head"); tc.wantClose > 0 && again != "conflicting" {
				t.Fatalf("second pass reason = %q, want conflicting", again)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.closes != tc.wantClose {
				t.Fatalf("closes = %d, want %d", api.closes, tc.wantClose)
			}
			if len(api.comments) != tc.wantClose {
				t.Fatalf("comments = %q, want %d", api.comments, tc.wantClose)
			}
			for _, body := range api.comments {
				if !strings.Contains(body, dependabotStaleCloseMarker) {
					t.Fatalf("close note %q lacks marker", body)
				}
				for _, line := range strings.Split(body, "\n") {
					if strings.HasPrefix(strings.TrimSpace(line), "@dependabot") {
						t.Fatalf("close note line %q is a Dependabot command", line)
					}
				}
			}
		})
	}
}
