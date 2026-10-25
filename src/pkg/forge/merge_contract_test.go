package forge

// Shared Forge.Merge contract (#14107).
//
// One table of forge-NEUTRAL cases, run once per adapter against that forge's
// own httptest fake. The cases assert what MergeOptions/MergeResult promise —
// the strategy map, the head pin, the label gate, already-merged idempotency —
// and nothing about a single forge's wire format, so an adapter that passes
// here behaves identically to every other one from a caller's seat.
//
// Adding a forge is one function: write a mergeFake for it (translate the
// neutral mergeFixture into that forge's JSON, record what the adapter put on
// the wire) and register it in mergeContractAdapters below with Pending
// cleared. Nothing else in this file changes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// mergeFixture is the forge-neutral state a case seeds its fake forge with,
// plus the facts that fake recorded. Fakes read the exported fields and report
// what the adapter sent through recordMerge; cases assert on the accessors.
type mergeFixture struct {
	// Repo is the "owner/name" slug under test.
	Repo string
	// Number is the change request number under test.
	Number int
	// HeadSHA is the change's current head commit.
	HeadSHA string
	// MergeSHA is the commit the forge reports for the merge — returned by a
	// successful merge, and the recorded merge commit of an already-merged
	// change.
	MergeSHA string
	// Labels are the labels on the change.
	Labels []string
	// AlreadyMerged seeds the change as merged before the call.
	AlreadyMerged bool
	// HeadMovedAtMerge makes the fake answer the merge call itself with its
	// forge's "the head is not the commit you pinned" refusal, modelling a
	// push that lands between the adapter's read and its merge.
	HeadMovedAtMerge bool

	mu           sync.Mutex
	mergeCalls   int
	gotStrategy  string
	gotExpectSHA string
}

// recordMerge is how a fake reports one merge attempt: the forge-native
// strategy string and the expected-head pin the adapter put on the wire.
func (fx *mergeFixture) recordMerge(strategy, expectSHA string) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.mergeCalls++
	fx.gotStrategy = strategy
	fx.gotExpectSHA = expectSHA
}

func (fx *mergeFixture) observed() (calls int, strategy, expectSHA string) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return fx.mergeCalls, fx.gotStrategy, fx.gotExpectSHA
}

// newMergeFixture is the baseline every case starts from: one open, unlabelled
// change on a repo whose default branch is not a release line.
func newMergeFixture() *mergeFixture {
	return &mergeFixture{
		Repo:     "hivecommons/hive",
		Number:   7,
		HeadSHA:  "1111111111111111111111111111111111111111",
		MergeSHA: "2222222222222222222222222222222222222222",
	}
}

// mergeContractAdapter is the single entry point a forge plugs into the shared
// contract. GitLab and Gitea register here with Pending set until their
// adapters implement Merger.
type mergeContractAdapter struct {
	// Name labels the subtest tree.
	Name string
	// Kind is the forge this adapter speaks for.
	Kind Kind
	// Pending, when non-empty, says why this forge does not implement Merger
	// yet; the whole contract runs as a skip. Clearing it is all the child
	// that lands the adapter has to do here.
	Pending string
	// Native maps each neutral strategy onto the exact string this forge must
	// see on the wire. A forge that expresses a strategy differently (a flag
	// rather than a name) records that flag's value through recordMerge.
	Native map[MergeStrategy]string
	// Start serves fx over an httptest server bound to t's lifetime and
	// returns a Merger wired to it.
	Start func(t *testing.T, fx *mergeFixture) Merger
}

// mergeContractAdapters is the registry the contract runs over.
var mergeContractAdapters = []mergeContractAdapter{
	{
		Name: "github",
		Kind: KindGitHub,
		Native: map[MergeStrategy]string{
			MergeStrategyDefault: "squash",
			MergeStrategySquash:  "squash",
			MergeStrategyMerge:   "merge",
			MergeStrategyRebase:  "rebase",
		},
		Start: startGitHubMergeFake,
	},
	{
		Name:    "gitlab",
		Kind:    KindGitLab,
		Pending: "gitlabForge does not implement Merger yet (child of #14096)",
	},
	{
		Name: "gitea",
		Kind: KindGitea,
		Native: map[MergeStrategy]string{
			MergeStrategyDefault: "merge",
			MergeStrategySquash:  "squash",
			MergeStrategyMerge:   "merge",
			MergeStrategyRebase:  "rebase",
		},
		Start: startGiteaMergeFake,
	},
}

// mergeContractCase is one forge-neutral expectation.
type mergeContractCase struct {
	name string
	// seed mutates the baseline fixture before the fake is started.
	seed func(fx *mergeFixture)
	// opts builds the call's options from the (already seeded) fixture.
	opts func(fx *mergeFixture) MergeOptions
	// wantErr is the sentinel the call must report; nil means success.
	wantErr error
	// want is the result expected on the success path.
	want func(fx *mergeFixture) MergeResult
	// wantMergeCalls is how many merge attempts the fake must have seen.
	wantMergeCalls int
	// wantStrategy is the neutral strategy whose native spelling must have
	// reached the wire; only checked when wantMergeCalls > 0.
	wantStrategy MergeStrategy
	// wantExpectSHA returns the head pin the adapter must have sent.
	wantExpectSHA func(fx *mergeFixture) string
}

func mergeContractCases() []mergeContractCase {
	merged := func(fx *mergeFixture) MergeResult {
		return MergeResult{Merged: true, SHA: fx.MergeSHA}
	}
	strategyCase := func(name string, s MergeStrategy) mergeContractCase {
		return mergeContractCase{
			name:           name,
			opts:           func(*mergeFixture) MergeOptions { return MergeOptions{Strategy: s} },
			want:           merged,
			wantMergeCalls: 1,
			wantStrategy:   s,
		}
	}
	return []mergeContractCase{
		{
			name:           "zero options merge with the forge default strategy",
			opts:           func(*mergeFixture) MergeOptions { return MergeOptions{} },
			want:           merged,
			wantMergeCalls: 1,
			wantStrategy:   MergeStrategyDefault,
		},
		strategyCase("squash maps to the forge's squash", MergeStrategySquash),
		strategyCase("merge maps to the forge's merge commit", MergeStrategyMerge),
		strategyCase("rebase maps to the forge's rebase", MergeStrategyRebase),
		{
			name: "an unknown strategy is refused, never defaulted",
			opts: func(*mergeFixture) MergeOptions {
				return MergeOptions{Strategy: MergeStrategy("fast-forward")}
			},
			wantErr:        ErrMergeStrategyUnsupported,
			wantMergeCalls: 0,
		},
		{
			name: "the expected head reaches the forge on the happy path",
			opts: func(fx *mergeFixture) MergeOptions {
				return MergeOptions{ExpectedHeadSHA: fx.HeadSHA}
			},
			want:           merged,
			wantMergeCalls: 1,
			wantStrategy:   MergeStrategyDefault,
			wantExpectSHA:  func(fx *mergeFixture) string { return fx.HeadSHA },
		},
		{
			name: "a head that already moved refuses without merging",
			opts: func(*mergeFixture) MergeOptions {
				return MergeOptions{ExpectedHeadSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}
			},
			wantErr:        ErrMergeHeadMismatch,
			wantMergeCalls: 0,
		},
		{
			name: "a head that moves during the merge refuses too",
			seed: func(fx *mergeFixture) { fx.HeadMovedAtMerge = true },
			opts: func(fx *mergeFixture) MergeOptions {
				return MergeOptions{ExpectedHeadSHA: fx.HeadSHA}
			},
			wantErr:        ErrMergeHeadMismatch,
			wantMergeCalls: 1,
			wantStrategy:   MergeStrategyDefault,
			wantExpectSHA:  func(fx *mergeFixture) string { return fx.HeadSHA },
		},
		{
			name:           "the hold gate refuses without merging",
			seed:           func(fx *mergeFixture) { fx.Labels = []string{"kind/bug", holdLabel} },
			opts:           func(*mergeFixture) MergeOptions { return MergeOptions{} },
			wantErr:        ErrMergeGated,
			wantMergeCalls: 0,
		},
		{
			name: "a caller-supplied gate label refuses without merging",
			seed: func(fx *mergeFixture) { fx.Labels = []string{"Do-Not-Merge/Work-In-Progress"} },
			opts: func(*mergeFixture) MergeOptions {
				return MergeOptions{BlockingLabels: []string{" do-not-merge/work-in-progress "}}
			},
			wantErr:        ErrMergeGated,
			wantMergeCalls: 0,
		},
		{
			name: "an unrelated label does not gate the merge",
			seed: func(fx *mergeFixture) { fx.Labels = []string{"on-hold-discussion", "kind/bug"} },
			opts: func(*mergeFixture) MergeOptions {
				return MergeOptions{BlockingLabels: []string{"", "  "}}
			},
			want:           merged,
			wantMergeCalls: 1,
			wantStrategy:   MergeStrategyDefault,
		},
		{
			name: "an already-merged change is idempotent success",
			seed: func(fx *mergeFixture) { fx.AlreadyMerged = true },
			opts: func(*mergeFixture) MergeOptions { return MergeOptions{} },
			want: func(fx *mergeFixture) MergeResult {
				return MergeResult{Merged: true, AlreadyMerged: true, SHA: fx.MergeSHA}
			},
			wantMergeCalls: 0,
		},
	}
}

// runMergeContract is the harness entry point: it runs every neutral case
// against one adapter.
func runMergeContract(t *testing.T, a mergeContractAdapter) {
	t.Helper()
	if a.Pending != "" {
		t.Skipf("merge contract pending for %s: %s", a.Kind, a.Pending)
	}
	if a.Start == nil {
		t.Fatalf("merge contract for %s: no Start function registered", a.Kind)
	}
	for _, tc := range mergeContractCases() {
		t.Run(tc.name, func(t *testing.T) {
			fx := newMergeFixture()
			if tc.seed != nil {
				tc.seed(fx)
			}
			m := a.Start(t, fx)
			got, err := m.Merge(context.Background(), fx.Repo, fx.Number, tc.opts(fx))

			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Merge err = %v, want %v", err, tc.wantErr)
				}
				if got != (MergeResult{}) {
					t.Fatalf("Merge result = %+v on refusal, want the zero result", got)
				}
			default:
				if err != nil {
					t.Fatalf("Merge: %v", err)
				}
				if want := tc.want(fx); got != want {
					t.Fatalf("Merge result = %+v, want %+v", got, want)
				}
			}

			calls, strategy, expectSHA := fx.observed()
			if calls != tc.wantMergeCalls {
				t.Fatalf("forge saw %d merge call(s), want %d", calls, tc.wantMergeCalls)
			}
			if tc.wantMergeCalls == 0 {
				return
			}
			if want := a.Native[tc.wantStrategy]; strategy != want {
				t.Fatalf("forge saw strategy %q for %q, want %q", strategy, tc.wantStrategy, want)
			}
			want := ""
			if tc.wantExpectSHA != nil {
				want = tc.wantExpectSHA(fx)
			}
			if expectSHA != want {
				t.Fatalf("forge saw expected-head pin %q, want %q", expectSHA, want)
			}
		})
	}
}

// TestForgeMergeContract runs the shared contract over every registered forge.
func TestForgeMergeContract(t *testing.T) {
	for _, a := range mergeContractAdapters {
		t.Run(a.Name, func(t *testing.T) { runMergeContract(t, a) })
	}
}

// TestForgeMergeContractCoversEveryForge keeps the registry honest: a new Kind
// must arrive with a contract entry (even a Pending one), so no forge can ship
// a Merger that was never held to the shared behaviour.
func TestForgeMergeContractCoversEveryForge(t *testing.T) {
	registered := map[Kind]bool{}
	for _, a := range mergeContractAdapters {
		registered[a.Kind] = true
	}
	for _, kind := range []Kind{KindGitHub, KindGitLab, KindGitea} {
		if !registered[kind] {
			t.Errorf("forge kind %q has no merge contract entry", kind)
		}
	}
}

// TestForgeMergeContractPendingForgesDoNotImplementMerger pins the other half
// of a Pending entry: a forge is only allowed to be skipped while it genuinely
// cannot merge. The day GitLab or Gitea implements Merger, this fails and the
// child clears Pending instead of leaving the contract silently skipped.
func TestForgeMergeContractPendingForgesDoNotImplementMerger(t *testing.T) {
	built := map[Kind]Forge{}
	gitlab, err := NewForge(KindGitLab, "t", Options{BaseURL: "https://gitlab.example.com", Org: "o"})
	if err != nil {
		t.Fatalf("NewForge(gitlab): %v", err)
	}
	built[KindGitLab] = gitlab
	gitea, err := NewForge(KindGitea, "t", Options{BaseURL: "https://gitea.example.com", Org: "o"})
	if err != nil {
		t.Fatalf("NewForge(gitea): %v", err)
	}
	built[KindGitea] = gitea

	for _, a := range mergeContractAdapters {
		f, ok := built[a.Kind]
		if !ok {
			continue
		}
		_, implements := f.(Merger)
		if a.Pending != "" && implements {
			t.Errorf("%s implements Merger but is still Pending in the contract registry: clear Pending and give it a Start fake", a.Kind)
		}
		if a.Pending == "" && !implements {
			t.Errorf("%s is registered as implemented but does not satisfy Merger", a.Kind)
		}
	}
}

// --- GitHub fake ---

// startGitHubMergeFake serves fx as GitHub's REST API: the PR read the adapter
// gates on, and the merge endpoint, which records the merge_method and sha it
// was sent. Any other path is a test failure, so an adapter that starts making
// extra calls is caught here rather than against real GitHub.
func startGitHubMergeFake(t *testing.T, fx *mergeFixture) Merger {
	t.Helper()
	owner, name := splitRepo(fx.Repo, "")
	prPath := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, name, fx.Number)

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+prPath, func(w http.ResponseWriter, _ *http.Request) {
		labels := make([]map[string]any, 0, len(fx.Labels))
		for _, l := range fx.Labels {
			labels = append(labels, map[string]any{"name": l})
		}
		mergeCommit := ""
		if fx.AlreadyMerged {
			mergeCommit = fx.MergeSHA
		}
		state := "open"
		if fx.AlreadyMerged {
			state = "closed"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number":           fx.Number,
			"title":            "fix: a change",
			"state":            state,
			"merged":           fx.AlreadyMerged,
			"merge_commit_sha": mergeCommit,
			"labels":           labels,
			"head":             map[string]any{"ref": "feature/x", "sha": fx.HeadSHA},
			// A non-release-line base keeps the forward-merge guard in
			// pkg/github.MergePR from rewriting the requested method, so the
			// strategy the contract asserts is the strategy on the wire.
			"base": map[string]any{"ref": "main"},
		})
	})
	mux.HandleFunc("PUT "+prPath+"/merge", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MergeMethod string `json:"merge_method"`
			SHA         string `json:"sha"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fx.recordMerge(body.MergeMethod, body.SHA)
		if fx.HeadMovedAtMerge {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Head branch was modified. Review and try the merge again."})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"merged":  true,
			"sha":     fx.MergeSHA,
			"message": "Pull Request successfully merged",
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to the GitHub merge fake: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	f, err := NewForge(KindGitHub, "test-token", Options{BaseURL: srv.URL, Org: owner})
	if err != nil {
		t.Fatalf("NewForge(github): %v", err)
	}
	m, ok := f.(Merger)
	if !ok {
		t.Fatalf("the GitHub adapter does not implement Merger")
	}
	return m
}

// --- Gitea fake ---

// startGiteaMergeFake serves fx as Gitea's REST API v1: the PR read the adapter
// gates on, and the merge endpoint, which records the Do style and head_commit_id
// it was sent.
func startGiteaMergeFake(t *testing.T, fx *mergeFixture) Merger {
	t.Helper()
	owner, name := splitRepo(fx.Repo, "")
	prPath := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d", owner, name, fx.Number)

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+prPath, func(w http.ResponseWriter, _ *http.Request) {
		labels := make([]map[string]any, 0, len(fx.Labels))
		for _, l := range fx.Labels {
			labels = append(labels, map[string]any{"name": l})
		}
		mergeCommit := ""
		if fx.AlreadyMerged {
			mergeCommit = fx.MergeSHA
		}
		state := "open"
		if fx.AlreadyMerged {
			state = "closed"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number":           fx.Number,
			"title":            "fix: a change",
			"state":            state,
			"merged":           fx.AlreadyMerged,
			"merge_commit_sha": mergeCommit,
			"labels":           labels,
			"head":             map[string]any{"ref": "feature/x", "sha": fx.HeadSHA},
			"base":             map[string]any{"ref": "main"},
		})
	})
	mux.HandleFunc("POST "+prPath+"/merge", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Do           string `json:"Do"`
			HeadCommitID string `json:"head_commit_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fx.recordMerge(body.Do, body.HeadCommitID)
		w.Header().Set("Content-Type", "application/json")
		if fx.HeadMovedAtMerge {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "head commit has changed"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message":          "pull request merged",
			"sha":              fx.MergeSHA,
			"merge_commit_sha": fx.MergeSHA,
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to the Gitea merge fake: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	f, err := NewForge(KindGitea, "test-token", Options{BaseURL: srv.URL, Org: owner})
	if err != nil {
		t.Fatalf("NewForge(gitea): %v", err)
	}
	m, ok := f.(Merger)
	if !ok {
		t.Fatalf("the Gitea adapter does not implement Merger")
	}
	return m
}
