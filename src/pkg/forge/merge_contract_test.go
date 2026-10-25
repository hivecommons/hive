package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// MergeContractScenario identifies a scenario in the shared merge contract test suite.
type MergeContractScenario string

const (
	// ScenarioMergeSuccess tests a standard successful merge with default options.
	ScenarioMergeSuccess MergeContractScenario = "success"
	// ScenarioStrategySquash tests squash merge strategy mapping.
	ScenarioStrategySquash MergeContractScenario = "strategy_squash"
	// ScenarioStrategyMerge tests explicit merge commit strategy mapping.
	ScenarioStrategyMerge MergeContractScenario = "strategy_merge"
	// ScenarioStrategyRebase tests rebase merge strategy mapping.
	ScenarioStrategyRebase MergeContractScenario = "strategy_rebase"
	// ScenarioHeadSHAGuard tests that head SHA mismatch refuses the merge with ErrConflict.
	ScenarioHeadSHAGuard MergeContractScenario = "head_sha_guard"
	// ScenarioHoldLabelRefuses tests that active hold label refuses merge with ErrHoldActive.
	ScenarioHoldLabelRefuses MergeContractScenario = "hold_label_refuses"
	// ScenarioNotMergeable tests that HTTP 405 unmergeable state maps to ErrNotMergeable.
	ScenarioNotMergeable MergeContractScenario = "not_mergeable"
	// ScenarioAlreadyMerged tests that already merged change requests map to ErrAlreadyMerged.
	ScenarioAlreadyMerged MergeContractScenario = "already_merged"
	// ScenarioDeleteBranch tests requesting deletion of source branch after merge.
	ScenarioDeleteBranch MergeContractScenario = "delete_branch"
)

// MergeContractAdapter defines the operations a forge adapter provides to plug
// into the shared merge contract harness.
type MergeContractAdapter interface {
	// Name returns the forge adapter name (e.g. "gitlab", "github", "gitea").
	Name() string

	// Setup configures a fake forge server and constructs a Merger under test
	// for the given scenario.
	Setup(t *testing.T, scenario MergeContractScenario) (merger Merger, repo string, number int, cleanup func())
}

// RunMergeContract runs the forge-neutral merge contract test suite against the
// provided adapter. All forge adapters implementing Merger must satisfy this
// suite.
func RunMergeContract(t *testing.T, adapter MergeContractAdapter) {
	t.Helper()

	t.Run("MergeSuccess", func(t *testing.T) {
		m, repo, num, cleanup := adapter.Setup(t, ScenarioMergeSuccess)
		defer cleanup()

		if err := m.Merge(context.Background(), repo, num, MergeOptions{}); err != nil {
			t.Fatalf("expected nil error on successful merge, got: %v", err)
		}
	})

	t.Run("StrategySquash", func(t *testing.T) {
		m, repo, num, cleanup := adapter.Setup(t, ScenarioStrategySquash)
		defer cleanup()

		if err := m.Merge(context.Background(), repo, num, MergeOptions{Strategy: string(MergeStrategySquash)}); err != nil {
			t.Fatalf("expected nil error on squash merge, got: %v", err)
		}
	})

	t.Run("StrategyMerge", func(t *testing.T) {
		m, repo, num, cleanup := adapter.Setup(t, ScenarioStrategyMerge)
		defer cleanup()

		if err := m.Merge(context.Background(), repo, num, MergeOptions{Strategy: string(MergeStrategyMerge)}); err != nil {
			t.Fatalf("expected nil error on merge strategy, got: %v", err)
		}
	})

	t.Run("StrategyRebase", func(t *testing.T) {
		m, repo, num, cleanup := adapter.Setup(t, ScenarioStrategyRebase)
		defer cleanup()

		if err := m.Merge(context.Background(), repo, num, MergeOptions{Strategy: string(MergeStrategyRebase)}); err != nil {
			t.Fatalf("expected nil error on rebase strategy, got: %v", err)
		}
	})

	t.Run("HeadSHAGuard", func(t *testing.T) {
		m, repo, num, cleanup := adapter.Setup(t, ScenarioHeadSHAGuard)
		defer cleanup()

		err := m.Merge(context.Background(), repo, num, MergeOptions{HeadSHA: "mismatched-sha-12345"})
		if err == nil {
			t.Fatalf("expected error on head SHA mismatch, got nil")
		}
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("expected ErrConflict on head SHA mismatch, got: %v", err)
		}
	})

	t.Run("HoldLabelRefuses", func(t *testing.T) {
		m, repo, num, cleanup := adapter.Setup(t, ScenarioHoldLabelRefuses)
		defer cleanup()

		err := m.Merge(context.Background(), repo, num, MergeOptions{})
		if err == nil {
			t.Fatalf("expected error when hold label is active, got nil")
		}
		if !errors.Is(err, ErrHoldActive) {
			t.Fatalf("expected ErrHoldActive, got: %v", err)
		}
	})

	t.Run("NotMergeable", func(t *testing.T) {
		m, repo, num, cleanup := adapter.Setup(t, ScenarioNotMergeable)
		defer cleanup()

		err := m.Merge(context.Background(), repo, num, MergeOptions{})
		if err == nil {
			t.Fatalf("expected error when not mergeable, got nil")
		}
		if !errors.Is(err, ErrNotMergeable) {
			t.Fatalf("expected ErrNotMergeable, got: %v", err)
		}
	})

	t.Run("AlreadyMerged", func(t *testing.T) {
		m, repo, num, cleanup := adapter.Setup(t, ScenarioAlreadyMerged)
		defer cleanup()

		err := m.Merge(context.Background(), repo, num, MergeOptions{})
		if err == nil {
			t.Fatalf("expected error when already merged, got nil")
		}
		if !errors.Is(err, ErrAlreadyMerged) {
			t.Fatalf("expected ErrAlreadyMerged, got: %v", err)
		}
	})

	t.Run("DeleteBranch", func(t *testing.T) {
		m, repo, num, cleanup := adapter.Setup(t, ScenarioDeleteBranch)
		defer cleanup()

		if err := m.Merge(context.Background(), repo, num, MergeOptions{DeleteBranch: true}); err != nil {
			t.Fatalf("expected nil error on delete branch merge, got: %v", err)
		}
	})
}

// gitLabContractAdapter adapts gitLabForge to the shared contract harness.
type gitLabContractAdapter struct{}

func (a *gitLabContractAdapter) Name() string { return "gitlab" }

func (a *gitLabContractAdapter) Setup(t *testing.T, scenario MergeContractScenario) (Merger, string, int, func()) {
	t.Helper()
	const (
		org    = "contract-org"
		repo   = "contract-repo"
		mrIID  = 42
		reqURL = "contract-org/contract-repo"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Route: GET MR details
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && strings.Contains(r.URL.Path, fmt.Sprintf("/merge_requests/%d", mrIID)) {
			switch scenario {
			case ScenarioHoldLabelRefuses:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"iid":    mrIID,
					"state":  "opened",
					"labels": []string{"kind/bug", "hold"},
				})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"iid":    mrIID,
					"state":  "opened",
					"labels": []string{"kind/bug"},
				})
			}
			return
		}

		// Route: PUT Rebase
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, fmt.Sprintf("/merge_requests/%d/rebase", mrIID)) {
			if scenario == ScenarioStrategyRebase {
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{"rebase_in_progress": false})
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "unexpected rebase call"})
			return
		}

		// Route: PUT Merge
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, fmt.Sprintf("/merge_requests/%d/merge", mrIID)) {
			if scenario == ScenarioHoldLabelRefuses {
				t.Errorf("merge endpoint called unexpectedly during hold label test")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
				t.Errorf("failed to decode merge request body: %v", err)
			}

			switch scenario {
			case ScenarioHeadSHAGuard:
				sha, _ := body["sha"].(string)
				if sha != "mismatched-sha-12345" {
					t.Errorf("expected sha mismatched-sha-12345, got %v", sha)
				}
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "409 Conflict: SHA does not match HEAD of source branch"})
				return

			case ScenarioNotMergeable:
				w.WriteHeader(http.StatusMethodNotAllowed)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "405 Method Not Allowed: Branch cannot be merged"})
				return

			case ScenarioAlreadyMerged:
				w.WriteHeader(http.StatusNotAcceptable)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "406 Not Acceptable: Merge request already merged"})
				return

			case ScenarioStrategySquash:
				sq, ok := body["squash"].(bool)
				if !ok || !sq {
					t.Errorf("expected squash=true in payload, got %v", body["squash"])
				}
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{"iid": mrIID, "state": "merged"})
				return

			case ScenarioStrategyMerge:
				sq, ok := body["squash"].(bool)
				if !ok || sq {
					t.Errorf("expected squash=false in payload, got %v", body["squash"])
				}
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{"iid": mrIID, "state": "merged"})
				return

			case ScenarioDeleteBranch:
				del, ok := body["should_remove_source_branch"].(bool)
				if !ok || !del {
					t.Errorf("expected should_remove_source_branch=true, got %v", body["should_remove_source_branch"])
				}
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{"iid": mrIID, "state": "merged"})
				return

			case ScenarioMergeSuccess, ScenarioStrategyRebase:
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{"iid": mrIID, "state": "merged"})
				return

			default:
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{"iid": mrIID, "state": "merged"})
				return
			}
		}

		http.NotFound(w, r)
	}))

	f, err := newGitLabForge("contract-token", Options{BaseURL: srv.URL, Org: org})
	if err != nil {
		srv.Close()
		t.Fatalf("newGitLabForge: %v", err)
	}

	return f, reqURL, mrIID, srv.Close
}

func TestMergeContract_GitLab(t *testing.T) {
	RunMergeContract(t, &gitLabContractAdapter{})
}

// gitHubContractAdapter adapts gitHubForge to the shared contract harness.
//
// The fake serves only what the adapter actually calls: the PR read that
// answers the gates, the merge endpoint, and the ref delete DeleteBranch
// needs. Anything else (the forward-merge guard's history lookups) falls
// through to 404, which that guard treats as "could not tell" and leaves the
// requested strategy alone — so the strategy cases still assert the method
// this adapter put on the wire.
type gitHubContractAdapter struct{}

func (a *gitHubContractAdapter) Name() string { return "github" }

func (a *gitHubContractAdapter) Setup(t *testing.T, scenario MergeContractScenario) (Merger, string, int, func()) {
	t.Helper()
	const (
		org     = "contract-org"
		name    = "contract-repo"
		slug    = org + "/" + name
		prNum   = 42
		headSHA = "feedfacefeedfacefeedfacefeedfacefeedface"
		headRef = "contract-head"
	)
	prPath := fmt.Sprintf("/repos/%s/%s/pulls/%d", org, name, prNum)

	var deletedRef string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Route: GET the pull request (gate read + forward-merge guard).
		if r.Method == http.MethodGet && r.URL.Path == prPath {
			labels := []map[string]any{{"name": "kind/bug"}}
			if scenario == ScenarioHoldLabelRefuses {
				labels = append(labels, map[string]any{"name": "hold"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": prNum,
				"state":  "open",
				"merged": scenario == ScenarioAlreadyMerged,
				"labels": labels,
				"head":   map[string]any{"sha": headSHA, "ref": headRef},
				"base":   map[string]any{"ref": "main"},
			})
			return
		}

		// Route: PUT the merge.
		if r.Method == http.MethodPut && r.URL.Path == prPath+"/merge" {
			switch scenario {
			case ScenarioHoldLabelRefuses, ScenarioHeadSHAGuard, ScenarioAlreadyMerged:
				t.Errorf("merge endpoint called in scenario %q — the gate must refuse before the merge", scenario)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
				t.Errorf("decode merge body: %v", err)
			}
			if want, ok := map[MergeContractScenario]string{
				ScenarioStrategySquash: "squash",
				ScenarioStrategyMerge:  "merge",
				ScenarioStrategyRebase: "rebase",
			}[scenario]; ok {
				if got, _ := body["merge_method"].(string); got != want {
					t.Errorf("merge_method = %q, want %q", got, want)
				}
			}
			if scenario == ScenarioNotMergeable {
				w.WriteHeader(http.StatusMethodNotAllowed)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "Pull Request is not mergeable"})
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"merged": true, "sha": "merge-commit-sha"})
			return
		}

		// Route: DELETE the head ref (MergeOptions.DeleteBranch).
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/git/refs/", org, name)) {
			deletedRef = strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/git/refs/", org, name))
			w.WriteHeader(http.StatusNoContent)
			return
		}

		http.NotFound(w, r)
	}))

	// go-github requires the base URL to end in a slash.
	f := newGitHubForge("contract-token", Options{BaseURL: srv.URL + "/", Org: org})
	cleanup := func() {
		if scenario == ScenarioDeleteBranch && deletedRef != "heads/"+headRef {
			t.Errorf("deleted ref = %q, want %q — DeleteBranch must remove the merged head branch", deletedRef, "heads/"+headRef)
		}
		srv.Close()
	}
	return f, slug, prNum, cleanup
}

func TestMergeContract_GitHub(t *testing.T) {
	RunMergeContract(t, &gitHubContractAdapter{})
}

// giteaContractAdapter adapts giteaForge to the shared contract harness.
//
// Gitea carries the strategy, the head pin and the branch deletion in ONE
// merge payload (Do / head_commit_id / delete_branch_after_merge), so the fake
// asserts on that body rather than on a sequence of calls.
type giteaContractAdapter struct{}

func (a *giteaContractAdapter) Name() string { return "gitea" }

func (a *giteaContractAdapter) Setup(t *testing.T, scenario MergeContractScenario) (Merger, string, int, func()) {
	t.Helper()
	const (
		org   = "contract-org"
		name  = "contract-repo"
		slug  = org + "/" + name
		prNum = 42
		head  = "feedfacefeedfacefeedfacefeedfacefeedface"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, fmt.Sprintf("/pulls/%d", prNum)) {
			pr := map[string]any{
				"number": prNum,
				"state":  "open",
				"merged": scenario == ScenarioAlreadyMerged,
				"head":   map[string]any{"ref": "contract-head", "sha": head},
				"base":   map[string]any{"ref": "main"},
			}
			if scenario == ScenarioHoldLabelRefuses {
				pr["labels"] = []map[string]any{{"name": "kind/bug"}, {"name": "hold"}}
			}
			_ = json.NewEncoder(w).Encode(pr)
			return
		}

		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, fmt.Sprintf("/pulls/%d/merge", prNum)) {
			switch scenario {
			case ScenarioHoldLabelRefuses, ScenarioHeadSHAGuard, ScenarioAlreadyMerged:
				t.Errorf("merge endpoint called in scenario %q — the gate must refuse before the merge", scenario)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var p giteaMergePayload
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil && err != io.EOF {
				t.Errorf("decode merge body: %v", err)
			}
			if want, ok := map[MergeContractScenario]string{
				ScenarioStrategySquash: "squash",
				ScenarioStrategyMerge:  "merge",
				ScenarioStrategyRebase: "rebase",
			}[scenario]; ok && p.Do != want {
				t.Errorf("Do = %q, want %q", p.Do, want)
			}
			if scenario == ScenarioDeleteBranch && !p.DeleteBranchAfterMerge {
				t.Errorf("delete_branch_after_merge = false, want true")
			}
			if scenario == ScenarioNotMergeable {
				w.WriteHeader(http.StatusMethodNotAllowed)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "Please check your email address"})
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
			return
		}

		http.NotFound(w, r)
	}))

	f, err := newGiteaForge("contract-token", Options{BaseURL: srv.URL, Org: org})
	if err != nil {
		srv.Close()
		t.Fatalf("newGiteaForge: %v", err)
	}
	return f, slug, prNum, srv.Close
}

func TestMergeContract_Gitea(t *testing.T) {
	RunMergeContract(t, &giteaContractAdapter{})
}
