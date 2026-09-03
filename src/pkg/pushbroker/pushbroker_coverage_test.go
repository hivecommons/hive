package pushbroker

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// scriptedGit answers git invocations from a table keyed on the joined argv,
// so the broker's decision tree (which base ref it diffs against, what it does
// when a step fails) can be driven without building a repository shaped for
// each branch. Any invocation the test did not script is an error rather than
// an empty success, so a change in the command sequence surfaces here instead
// of silently passing.
type scriptedGit struct {
	replies map[string]string
	fails   map[string]error
	calls   []string
	pushErr error
	pushed  bool
}

func (g *scriptedGit) Run(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	if slices.Contains(args, "push") {
		g.pushed = true
		g.calls = append(g.calls, "push")
		return []byte("ok"), g.pushErr
	}
	g.calls = append(g.calls, key)
	if err, ok := g.fails[key]; ok {
		return []byte("fatal: scripted failure"), err
	}
	if out, ok := g.replies[key]; ok {
		return []byte(out), nil
	}
	return nil, errors.New("unscripted git invocation: " + name + " " + key)
}

// fakeGitWorkspace is a directory that passes validate()'s repository check
// without a real git repo behind it — the scripted runner supplies the answers
// git would have given.
func fakeGitWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestValidateRejectsUnusableConfigurations(t *testing.T) {
	repoDir := fakeGitWorkspace(t)
	plainDir := t.TempDir()
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		broker  Broker
		wantErr string
	}{
		{"no workspace", Broker{Branch: "work", Repo: "o/r", Minter: fakeMinter{"t"}}, "workspace, branch, and repo are required"},
		{"no branch", Broker{Workspace: repoDir, Repo: "o/r", Minter: fakeMinter{"t"}}, "workspace, branch, and repo are required"},
		{"no repo", Broker{Workspace: repoDir, Branch: "work", Minter: fakeMinter{"t"}}, "workspace, branch, and repo are required"},
		{"no minter", Broker{Workspace: repoDir, Branch: "work", Repo: "o/r"}, "token minter is required"},
		{"workspace missing", Broker{Workspace: filepath.Join(plainDir, "absent"), Branch: "work", Repo: "o/r", Minter: fakeMinter{"t"}}, "workspace is not a directory"},
		{"workspace is a file", Broker{Workspace: file, Branch: "work", Repo: "o/r", Minter: fakeMinter{"t"}}, "workspace is not a directory"},
		{"workspace is not a repo", Broker{Workspace: plainDir, Branch: "work", Repo: "o/r", Minter: fakeMinter{"t"}}, "workspace is not a git repository"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.broker
			res, err := b.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Run error = %v, want it to mention %q", err, tc.wantErr)
			}
			// The Result is the audit record; a rejection that leaves Error
			// empty tells the operator nothing about why nothing was pushed.
			if res.Error == "" {
				t.Fatal("Result.Error is empty for a rejected run")
			}
			if res.Pushed {
				t.Fatal("Pushed=true for a rejected run")
			}
		})
	}
}

// An explicit BaseRef is what the caller passes when the PR is against
// something other than the tracked remote branch; it has to win over the
// remote-ref default or the broker diffs the wrong range and can miss a
// secret or a protected path introduced earlier in the branch.
func TestRunPrefersAnExplicitBaseRef(t *testing.T) {
	git := &scriptedGit{replies: map[string]string{
		"rev-parse HEAD":                        "cafebabe\n",
		"rev-parse --verify origin/main":        "deadbeef\n",
		"diff --name-only origin/main...HEAD":   "pkg/safe.go\n",
		"diff --no-ext-diff origin/main...HEAD": "+// harmless\n",
	}}
	res, err := (&Broker{
		Workspace: fakeGitWorkspace(t), Branch: "work", BaseRef: "origin/main",
		Repo: "kubestellar/hive", Minter: fakeMinter{"ghs_tok"}, Runner: git,
	}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	if !res.Pushed || res.Commit != "cafebabe" {
		t.Fatalf("res = %+v, want a push at cafebabe", res)
	}
	if slices.Contains(git.calls, "rev-parse --verify refs/remotes/origin/work") {
		t.Fatalf("broker consulted the remote ref despite an explicit BaseRef: %v", git.calls)
	}
}

// With no BaseRef and no tracked remote branch (the first push of a new
// branch), the broker falls back to the HEAD commit itself — otherwise a brand
// new branch would scan nothing and push unreviewed.
func TestRunFallsBackToHeadCommitWhenNoBaseExists(t *testing.T) {
	noBase := errors.New("unknown revision")
	git := &scriptedGit{
		replies: map[string]string{
			"rev-parse HEAD": "abc123\n",
			"diff-tree --root --no-commit-id --name-only -r HEAD": "pkg/new.go\n\n  \n",
			"show --format= --no-ext-diff HEAD":                   "+// new file\n",
		},
		fails: map[string]error{"rev-parse --verify refs/remotes/origin/work": noBase},
	}
	res, err := (&Broker{
		Workspace: fakeGitWorkspace(t), Branch: "work", Repo: "kubestellar/hive",
		Minter: fakeMinter{"ghs_tok"}, Runner: git,
	}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	if !slices.Equal(res.ChangedFiles, []string{"pkg/new.go"}) {
		t.Fatalf("ChangedFiles = %v, want the blank lines dropped", res.ChangedFiles)
	}
}

// A BaseRef that no longer resolves (a rebased or deleted base branch) must
// not abort the push: the broker falls back to the tracked remote ref so the
// scan still happens against a real range rather than being skipped.
func TestRunFallsBackToRemoteRefWhenBaseRefIsGone(t *testing.T) {
	git := &scriptedGit{
		replies: map[string]string{
			"rev-parse HEAD": "abc123\n",
			"rev-parse --verify refs/remotes/origin/work":        "999888\n",
			"diff --name-only refs/remotes/origin/work...HEAD":   "pkg/safe.go\n",
			"diff --no-ext-diff refs/remotes/origin/work...HEAD": "+// harmless\n",
		},
		fails: map[string]error{"rev-parse --verify origin/deleted": errors.New("unknown revision")},
	}
	res, err := (&Broker{
		Workspace: fakeGitWorkspace(t), Branch: "work", BaseRef: "origin/deleted",
		Repo: "kubestellar/hive", Minter: fakeMinter{"ghs_tok"}, Runner: git,
	}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	if !res.Pushed {
		t.Fatalf("res = %+v, want the push to proceed via the remote ref", res)
	}
}

func TestRunSurfacesEachFailureStage(t *testing.T) {
	head := map[string]string{"rev-parse HEAD": "abc123\n"}
	clean := map[string]string{
		"rev-parse HEAD": "abc123\n",
		"diff-tree --root --no-commit-id --name-only -r HEAD": "pkg/safe.go\n",
		"show --format= --no-ext-diff HEAD":                   "+// harmless\n",
	}
	noBase := map[string]error{"rev-parse --verify refs/remotes/origin/work": errors.New("unknown revision")}

	cases := []struct {
		name    string
		git     *scriptedGit
		minter  TokenMinter
		wantErr string
	}{
		{
			name:    "HEAD unreadable",
			git:     &scriptedGit{fails: map[string]error{"rev-parse HEAD": errors.New("not a repository")}},
			minter:  fakeMinter{"ghs_tok"},
			wantErr: "reading HEAD",
		},
		{
			name:    "diff enumeration fails",
			git:     &scriptedGit{replies: head, fails: mergeErrs(noBase, map[string]error{"diff-tree --root --no-commit-id --name-only -r HEAD": errors.New("broken index")})},
			minter:  fakeMinter{"ghs_tok"},
			wantErr: "broken index",
		},
		{
			name: "nothing committed",
			git: &scriptedGit{
				replies: map[string]string{"rev-parse HEAD": "abc123\n", "diff-tree --root --no-commit-id --name-only -r HEAD": "\n"},
				fails:   noBase,
			},
			minter:  fakeMinter{"ghs_tok"},
			wantErr: "no committed changes to push",
		},
		{
			name: "diff read fails",
			git: &scriptedGit{
				replies: map[string]string{"rev-parse HEAD": "abc123\n", "diff-tree --root --no-commit-id --name-only -r HEAD": "pkg/safe.go\n"},
				fails:   mergeErrs(noBase, map[string]error{"show --format= --no-ext-diff HEAD": errors.New("bad object")}),
			},
			minter:  fakeMinter{"ghs_tok"},
			wantErr: "bad object",
		},
		{
			name:    "minting fails",
			git:     &scriptedGit{replies: clean, fails: noBase},
			minter:  errMinter{errors.New("installation suspended")},
			wantErr: "minting push token",
		},
		{
			// An empty token would authenticate as nobody and produce a
			// confusing server-side 403 instead of a broker-side rejection.
			name:    "minter returns nothing",
			git:     &scriptedGit{replies: clean, fails: noBase},
			minter:  fakeMinter{"   "},
			wantErr: "minter returned empty token",
		},
		{
			name:    "push rejected",
			git:     &scriptedGit{replies: clean, fails: noBase, pushErr: errors.New("protected branch")},
			minter:  fakeMinter{"ghs_tok"},
			wantErr: "git push failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := (&Broker{
				Workspace: fakeGitWorkspace(t), Branch: "work", Repo: "kubestellar/hive",
				Minter: tc.minter, Runner: tc.git, Logger: slog.New(slog.DiscardHandler),
			}).Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Run error = %v, want it to mention %q", err, tc.wantErr)
			}
			if res.Pushed {
				t.Fatal("Pushed=true after a failure")
			}
			if res.Error != err.Error() {
				t.Fatalf("Result.Error = %q, want it to match the returned error %q", res.Error, err)
			}
			if res.FinishedAt.IsZero() {
				t.Fatal("FinishedAt is zero — the audit record has no end time")
			}
		})
	}
}

// The overridable knobs exist so callers can target a non-default remote,
// tighten the protected set, and make the audit timestamps deterministic; each
// has to actually reach the Result and the git argv.
func TestBrokerHonorsRemoteProtectedPathAndClockOverrides(t *testing.T) {
	fixed := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	git := &scriptedGit{
		replies: map[string]string{
			"rev-parse HEAD": "abc123\n",
			"diff-tree --root --no-commit-id --name-only -r HEAD": "docs/notes.md\n",
		},
		fails: map[string]error{"rev-parse --verify refs/remotes/upstream/work": errors.New("unknown revision")},
	}
	res, err := (&Broker{
		Workspace: fakeGitWorkspace(t), Branch: "work", Repo: "kubestellar/hive",
		Remote: "upstream", ProtectedPaths: []string{"docs/"},
		Minter: fakeMinter{"ghs_tok"}, Runner: git, Now: func() time.Time { return fixed },
	}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "protected paths changed") {
		t.Fatalf("Run error = %v, want the custom protected path to reject the push", err)
	}
	if res.Remote != "upstream" {
		t.Fatalf("Remote = %q, want the override", res.Remote)
	}
	if !slices.Equal(res.ProtectedReject, []string{"docs/notes.md"}) {
		t.Fatalf("ProtectedReject = %v, want docs/notes.md", res.ProtectedReject)
	}
	if !res.StartedAt.Equal(fixed) || !res.FinishedAt.Equal(fixed) {
		t.Fatalf("timestamps = %v/%v, want the injected clock %v", res.StartedAt, res.FinishedAt, fixed)
	}
}

func TestGitHubAppMinterRequiresAuth(t *testing.T) {
	if _, err := (GitHubAppMinter{}).MintPushToken(context.Background(), "kubestellar/hive"); err == nil {
		t.Fatal("MintPushToken: expected an error with nil App auth")
	}
}

// The point of minting per push is that the credential cannot reach any other
// repository the installation covers, and that an unspecified tier lands on the
// least-privileged default rather than on whatever the installation grants.
func TestGitHubAppMinterScopesTokenToTheSingleRepo(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_scoped","expires_at":"2099-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	auth, err := ghpkg.NewAppAuthFromPEM(1234, 5678, testRSAKeyPEM(t), slog.New(slog.DiscardHandler), srv.URL)
	if err != nil {
		t.Fatalf("NewAppAuthFromPEM: %v", err)
	}
	token, err := (GitHubAppMinter{Auth: auth}).MintPushToken(context.Background(), " kubestellar/hive ")
	if err != nil {
		t.Fatalf("MintPushToken: %v", err)
	}
	if token != "ghs_scoped" {
		t.Fatalf("token = %q, want the minted token", token)
	}
	repos, _ := body["repositories"].([]any)
	if len(repos) != 1 || repos[0] != "hive" {
		t.Fatalf("repositories = %v, want exactly [hive]", body["repositories"])
	}
	// DefaultTier is "contributor": issues/contents/pull_requests write, no merge rights.
	perms, _ := body["permissions"].(map[string]any)
	if perms["contents"] != "write" || perms["pull_requests"] != "write" {
		t.Fatalf("permissions = %v, want the contributor tier", perms)
	}
	if _, hasChecks := perms["checks"]; hasChecks {
		t.Fatalf("default tier requested merger-shaped permissions: %v", perms)
	}
}

// A repo string with no owner cannot be narrowed to a single repository, so the
// minter must leave the scope unrestricted rather than send a bogus name that
// GitHub would reject.
func TestGitHubAppMinterLeavesScopeOpenForAnUnqualifiedRepo(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_open","expires_at":"2099-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	auth, err := ghpkg.NewAppAuthFromPEM(1234, 5678, testRSAKeyPEM(t), slog.New(slog.DiscardHandler), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (GitHubAppMinter{Auth: auth, Tier: "newcomer"}).MintPushToken(context.Background(), "hive"); err != nil {
		t.Fatalf("MintPushToken: %v", err)
	}
	if _, ok := body["repositories"]; ok {
		t.Fatalf("repositories = %v, want the field omitted", body["repositories"])
	}
	if perms, _ := body["permissions"].(map[string]any); perms["contents"] != "read" {
		t.Fatalf("permissions = %v, want the newcomer tier", body["permissions"])
	}
}

// ExecRunner is the production runner; the interface contract it has to honor
// is that a non-zero exit surfaces as an error with the combined output kept.
func TestExecRunnerReturnsCombinedOutputAndExitError(t *testing.T) {
	out, err := ExecRunner{}.Run(context.Background(), t.TempDir(), []string{"PATH=" + os.Getenv("PATH")}, "sh", "-c", "echo hello; echo bad 1>&2; exit 3")
	if err == nil {
		t.Fatal("Run: expected an error for a non-zero exit")
	}
	if !strings.Contains(string(out), "hello") || !strings.Contains(string(out), "bad") {
		t.Fatalf("output = %q, want both streams", out)
	}
}

func mergeErrs(maps ...map[string]error) map[string]error {
	out := map[string]error{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

type errMinter struct{ err error }

func (m errMinter) MintPushToken(context.Context, string) (string, error) { return "", m.err }

func testRSAKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
