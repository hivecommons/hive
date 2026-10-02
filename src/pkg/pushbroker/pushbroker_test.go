package pushbroker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type fakeMinter struct{ token string }

func (f fakeMinter) MintPushToken(context.Context, string) (string, error) { return f.token, nil }

type recordingRunner struct {
	envOnPush  []string
	argsOnPush []string
}

func (r *recordingRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	if name == "git" && slices.Contains(args, "push") {
		r.envOnPush = append([]string(nil), env...)
		r.argsOnPush = append([]string(nil), args...)
		return []byte("ok"), nil
	}
	return ExecRunner{}.Run(ctx, dir, env, name, args...)
}

type envRecordingRunner struct {
	env []string
}

func (r *envRecordingRunner) Run(_ context.Context, _ string, env []string, _ string, _ ...string) ([]byte, error) {
	r.env = append([]string(nil), env...)
	return []byte("ok"), nil
}

func TestGitUsesAgentIdentityEnv(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "bots.example.org")
	r := &envRecordingRunner{}

	if _, err := (&Broker{Workspace: fakeGitWorkspace(t), AgentName: "scanner", Runner: r}).git(context.Background(), "status"); err != nil {
		t.Fatalf("git: %v", err)
	}
	for _, want := range []string{
		"GIT_AUTHOR_NAME=scanner",
		"GIT_AUTHOR_EMAIL=scanner@bots.example.org",
		"GIT_COMMITTER_NAME=scanner",
		"GIT_COMMITTER_EMAIL=scanner@bots.example.org",
	} {
		if !slices.Contains(r.env, want) {
			t.Fatalf("git env missing %s: %v", want, r.env)
		}
	}
}

func TestBrokerRejectsTokenLikeSecretInOutgoingDiff(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "secret.txt", "token=ghp_abcdefghijklmnopqrstuvwxyz\n")
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}}).Run(context.Background())
	if err == nil {
		t.Fatal("expected secret rejection")
	}
	if !res.SecretRejected {
		t.Fatalf("SecretRejected=false, err=%v", err)
	}
}

func TestBrokerRejectsProtectedPaths(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, ".github/workflows/ci.yaml", "name: ci\n")
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}}).Run(context.Background())
	if err == nil {
		t.Fatal("expected protected path rejection")
	}
	if len(res.ProtectedReject) != 1 || res.ProtectedReject[0] != ".github/workflows/ci.yaml" {
		t.Fatalf("ProtectedReject=%v", res.ProtectedReject)
	}
}

// Git C-quotes paths holding non-ASCII bytes, `"`, `\` or control characters
// when listing them line-by-line; the surrounding quotes used to hide such a
// file from the protected-path guard. Both the base-less diff-tree path and
// the BaseRef diff path must see the raw name.
func TestBrokerRejectsProtectedPathsWithQuotedNames(t *testing.T) {
	quoted := []string{".github/workflows/dépl.yml", `policies/a"b.md`, "policies/c\td.md"}
	for _, withBase := range []bool{false, true} {
		dir := initRepo(t)
		b := &Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: &recordingRunner{}}
		if withBase {
			writeCommit(t, dir, "README.md", "hello\n")
			b.BaseRef = strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
		}
		for _, rel := range quoted {
			writeCommit(t, dir, rel, "name: ci\n")
		}
		res, err := b.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "protected paths changed") {
			t.Fatalf("withBase=%v: Run error = %v, want protected path rejection", withBase, err)
		}
		if res.Pushed {
			t.Fatalf("withBase=%v: broker pushed protected files: %+v", withBase, res)
		}
		if strings.Join(res.ProtectedReject, "\x00") != strings.Join(quoted, "\x00") {
			t.Fatalf("withBase=%v: ProtectedReject=%q want %q", withBase, res.ProtectedReject, quoted)
		}
	}
}

func TestSplitNUL(t *testing.T) {
	if got := splitNUL(nil); got != nil {
		t.Fatalf("splitNUL(nil)=%v want nil", got)
	}
	got := splitNUL([]byte("a b\x00\x00c\nd\x00"))
	if strings.Join(got, "|") != "a b|c\nd" {
		t.Fatalf("splitNUL=%q", got)
	}
}

func TestBrokerRejectsEmptyOutgoingCommitBeforePush(t *testing.T) {
	dir := initRepo(t)
	runGit(t, dir, "commit", "--allow-empty", "-m", "ci: retrigger tests")
	r := &recordingRunner{}

	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing to push empty commit") {
		t.Fatalf("Run error = %v, want empty commit rejection", err)
	}
	if res.Pushed || r.argsOnPush != nil {
		t.Fatalf("broker pushed after empty commit rejection: res=%+v args=%v", res, r.argsOnPush)
	}
}

func TestBrokerRejectsCommitMadeEmptyByNormalisation(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n")
	base := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	runGit(t, dir, "update-ref", "refs/remotes/origin/work", base)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n\n")
	r := &recordingRunner{}

	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "made empty by broker normalisation") {
		t.Fatalf("Run error = %v, want empty commit rejection after normalisation", err)
	}
	if res.Pushed || r.argsOnPush != nil {
		t.Fatalf("broker pushed after normalisation emptied the commit: res=%+v args=%v", res, r.argsOnPush)
	}
}

func TestBrokerFirstPushEmptyGuardChecksOnlyHead(t *testing.T) {
	dir := initRepo(t)
	runGit(t, dir, "commit", "--allow-empty", "-m", "historical empty commit")
	writeCommit(t, dir, "safe.txt", "new branch work\n")
	r := &recordingRunner{}

	res, err := (&Broker{Workspace: dir, Branch: "new-work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	if !res.Pushed {
		t.Fatal("Pushed=false")
	}
}

func TestRejectEmptyOutgoingCommitsSurfacesRevListFailure(t *testing.T) {
	git := &scriptedGit{
		replies: map[string]string{
			"rev-parse --verify origin/main": "base\n",
		},
		fails: map[string]error{
			"rev-list --reverse origin/main..HEAD": errors.New("bad revision"),
		},
	}
	err := (&Broker{Workspace: fakeGitWorkspace(t), Branch: "work", BaseRef: "origin/main", Runner: git}).rejectEmptyOutgoingCommits(context.Background(), "head")
	if err == nil || !strings.Contains(err.Error(), "reading outgoing commits for empty-commit guard") {
		t.Fatalf("rejectEmptyOutgoingCommits error = %v, want rev-list failure", err)
	}
}

func TestRejectEmptyOutgoingCommitsSurfacesListedCommitFailure(t *testing.T) {
	git := &scriptedGit{
		replies: map[string]string{
			"rev-parse --verify origin/main":       "base\n",
			"rev-list --reverse origin/main..HEAD": "badhead\n",
		},
		fails: map[string]error{
			"rev-list --parents -n 1 badhead": errors.New("corrupt commit"),
		},
	}
	err := (&Broker{Workspace: fakeGitWorkspace(t), Branch: "work", BaseRef: "origin/main", Runner: git}).rejectEmptyOutgoingCommits(context.Background(), "head")
	if err == nil || !strings.Contains(err.Error(), "reading parents for empty-commit guard") {
		t.Fatalf("rejectEmptyOutgoingCommits error = %v, want listed commit failure", err)
	}
}

func TestRejectEmptyOutgoingCommitsRejectsListedEmptyCommit(t *testing.T) {
	git := &scriptedGit{
		replies: map[string]string{
			"rev-parse --verify origin/main":       "base\n",
			"rev-list --reverse origin/main..HEAD": "deadbeef\n",
			"rev-list --parents -n 1 deadbeef":     "deadbeef parent\n",
			"diff-tree --quiet parent deadbeef":    "",
		},
	}
	err := (&Broker{Workspace: fakeGitWorkspace(t), Branch: "work", BaseRef: "origin/main", Runner: git}).rejectEmptyOutgoingCommits(context.Background(), "head")
	if err == nil || !strings.Contains(err.Error(), "refusing to push empty commit") {
		t.Fatalf("rejectEmptyOutgoingCommits error = %v, want empty listed commit rejection", err)
	}
}

func TestRejectEmptyOutgoingCommitsIgnoresEmptyUnknownHead(t *testing.T) {
	git := &scriptedGit{
		fails: map[string]error{
			"rev-parse --verify refs/remotes/origin/work": errors.New("unknown revision"),
		},
	}
	if err := (&Broker{Workspace: fakeGitWorkspace(t), Branch: "work", Runner: git}).rejectEmptyOutgoingCommits(context.Background(), " "); err != nil {
		t.Fatalf("rejectEmptyOutgoingCommits = %v, want nil for empty head", err)
	}
}

func TestRejectEmptyOutgoingCommitsSurfacesHeadOnlyParentFailure(t *testing.T) {
	git := &scriptedGit{
		fails: map[string]error{
			"rev-parse --verify refs/remotes/origin/work": errors.New("unknown revision"),
			"rev-list --parents -n 1 badhead":             errors.New("corrupt commit"),
		},
	}
	err := (&Broker{Workspace: fakeGitWorkspace(t), Branch: "work", Runner: git}).rejectEmptyOutgoingCommits(context.Background(), "badhead")
	if err == nil || !strings.Contains(err.Error(), "reading parents for empty-commit guard") {
		t.Fatalf("rejectEmptyOutgoingCommits error = %v, want parent read failure", err)
	}
}

func TestCommitHasEmptyTreeDeltaSurfacesParentFailure(t *testing.T) {
	git := &scriptedGit{
		fails: map[string]error{
			"rev-list --parents -n 1 bad": errors.New("corrupt commit"),
		},
	}

	_, err := (&Broker{Workspace: fakeGitWorkspace(t), Runner: git}).commitHasEmptyTreeDelta(context.Background(), "bad")
	if err == nil || !strings.Contains(err.Error(), "reading parents for empty-commit guard") {
		t.Fatalf("commitHasEmptyTreeDelta error = %v, want parent read failure", err)
	}
}

func TestBrokerSurfacesEmptyCommitCheckFailureAfterNormalisation(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n\n")
	r := &nthCallFailingRunner{failSubstr: "rev-list --parents -n 1", failOnCall: 2, failErr: errors.New("corrupt amended head")}
	_, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "reading parents for empty-commit guard") {
		t.Fatalf("Run error = %v, want post-normalisation empty-check failure", err)
	}
}

func TestCommitHasEmptyTreeDeltaHandlesRootCommit(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "safe.txt", "root content\n")
	head := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	empty, err := (&Broker{Workspace: dir}).commitHasEmptyTreeDelta(context.Background(), head)
	if err != nil {
		t.Fatalf("commitHasEmptyTreeDelta: %v", err)
	}
	if empty {
		t.Fatal("root commit adding a file was classified as empty")
	}
}

func TestShortSHALeavesShortValuesAlone(t *testing.T) {
	if got := shortSHA("abc123"); got != "abc123" {
		t.Fatalf("shortSHA = %q, want original short value", got)
	}
}

func TestBrokerRejectsLaneSignoffOnOtherAuthorsCommit(t *testing.T) {
	dir := initRepo(t)
	path := filepath.Join(dir, "safe.txt")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "safe.txt")
	runGit(t, dir, "commit", "--author", "Human Author <human@example.com>", "-m", "fix from human\n\nSigned-off-by: Hive Test <hive@example.com>")

	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing to push commit") || !strings.Contains(err.Error(), "Signed-off-by") {
		t.Fatalf("Run error = %v, want forged sign-off rejection", err)
	}
	if res.Pushed || r.argsOnPush != nil {
		t.Fatalf("broker pushed after forged sign-off rejection: res=%+v args=%v", res, r.argsOnPush)
	}
}

func TestBrokerAllowsLaneSignoffOnOwnCommit(t *testing.T) {
	dir := initRepo(t)
	runGit(t, dir, "commit", "--allow-empty", "-s", "-m", "agent-authored fix")
	_, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing to push empty commit") {
		t.Fatalf("Run error = %v, want empty-commit rejection only after sign-off guard passes", err)
	}
}

func TestBrokerFirstPushSignoffGuardChecksOnlyHead(t *testing.T) {
	dir := initRepo(t)
	path := filepath.Join(dir, "history.txt")
	if err := os.WriteFile(path, []byte("historical\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "history.txt")
	runGit(t, dir, "commit", "--author", "Human Author <human@example.com>", "-m", "historical commit\n\nSigned-off-by: Hive Test <hive@example.com>")
	writeCommit(t, dir, "safe.txt", "new branch work\n")

	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "new-work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	if !res.Pushed {
		t.Fatal("Pushed=false")
	}
}

func TestRejectForgedLaneSignoffsSurfacesConfigAndLogFailures(t *testing.T) {
	cases := []struct {
		name    string
		git     *scriptedGit
		wantErr string
	}{
		{
			name: "author ident",
			git: &scriptedGit{fails: map[string]error{
				"var GIT_AUTHOR_IDENT": errors.New("missing ident"),
			}},
			wantErr: "reading git author identity",
		},
		{
			name: "log",
			git: &scriptedGit{
				replies: map[string]string{
					"var GIT_AUTHOR_IDENT": "Hive Test <hive@example.com> 1700000000 +0000\n",
				},
				fails: map[string]error{
					"log -1 --format=%H%x00%an%x00%ae%x00%cn%x00%ce%x00%B%x1e HEAD": errors.New("bad log"),
				},
			},
			wantErr: "reading outgoing commits for sign-off guard",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Broker{Workspace: fakeGitWorkspace(t), Runner: tc.git}).rejectForgedLaneSignoffs(context.Background(), "", false)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("rejectForgedLaneSignoffs error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestRejectForgedLaneSignoffsSkipsUncheckableRecords(t *testing.T) {
	cases := []struct {
		name    string
		replies map[string]string
	}{
		{
			name: "empty identity",
			replies: map[string]string{
				"var GIT_AUTHOR_IDENT": " <hive@example.com> 1700000000 +0000\n",
			},
		},
		{
			name: "no email in ident",
			replies: map[string]string{
				"var GIT_AUTHOR_IDENT": "malformed-ident-with-no-brackets\n",
			},
		},
		{
			name: "malformed log record",
			replies: map[string]string{
				"var GIT_AUTHOR_IDENT": "Hive Test <hive@example.com> 1700000000 +0000\n",
				"log -1 --format=%H%x00%an%x00%ae%x00%cn%x00%ce%x00%B%x1e HEAD": "not-enough-fields\x1e",
			},
		},
		{
			name: "own authored commit",
			replies: map[string]string{
				"var GIT_AUTHOR_IDENT": "Hive Test <hive@example.com> 1700000000 +0000\n",
				"log -1 --format=%H%x00%an%x00%ae%x00%cn%x00%ce%x00%B%x1e HEAD": "abc\x00Hive Test\x00hive@example.com\x00Hive Test\x00hive@example.com\x00Signed-off-by: Hive Test <hive@example.com>\x1e",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Broker{Workspace: fakeGitWorkspace(t), Runner: &scriptedGit{replies: tc.replies}}).rejectForgedLaneSignoffs(context.Background(), "", false)
			if err != nil {
				t.Fatalf("rejectForgedLaneSignoffs = %v, want nil", err)
			}
		})
	}
}

func TestRejectForgedLaneSignoffsUsesAgentIdentityInsteadOfGitConfig(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "bots.example.org")
	git := &scriptedGit{replies: map[string]string{
		"log -1 --format=%H%x00%an%x00%ae%x00%cn%x00%ce%x00%B%x1e HEAD": "abc123\x00Human Author\x00human@example.com\x00Human Author\x00human@example.com\x00fix\n\nSigned-off-by: scanner <scanner@bots.example.org>\x1e",
	}}

	err := (&Broker{Workspace: fakeGitWorkspace(t), AgentName: "scanner", Runner: git}).rejectForgedLaneSignoffs(context.Background(), "", false)
	if err == nil || !strings.Contains(err.Error(), "refusing to push commit") {
		t.Fatalf("rejectForgedLaneSignoffs = %v, want forged lane sign-off rejection", err)
	}
	if slices.Contains(git.calls, "var GIT_AUTHOR_IDENT") {
		t.Fatalf("guard read git author identity from config/env fallback despite AgentName: calls=%v", git.calls)
	}
}

func TestRejectForgedLaneSignoffsAllowsAgentAuthoredCommit(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "bots.example.org")
	git := &scriptedGit{replies: map[string]string{
		"log -1 --format=%H%x00%an%x00%ae%x00%cn%x00%ce%x00%B%x1e HEAD": "abc123\x00scanner\x00scanner@bots.example.org\x00scanner\x00scanner@bots.example.org\x00fix\n\nSigned-off-by: scanner <scanner@bots.example.org>\x1e",
	}}

	if err := (&Broker{Workspace: fakeGitWorkspace(t), AgentName: "scanner", Runner: git}).rejectForgedLaneSignoffs(context.Background(), "", false); err != nil {
		t.Fatalf("rejectForgedLaneSignoffs = %v, want nil", err)
	}
	if slices.Contains(git.calls, "var GIT_AUTHOR_IDENT") {
		t.Fatalf("guard read git author identity from config/env fallback despite AgentName: calls=%v", git.calls)
	}
}

func TestRejectForgedLaneSignoffsRejectsLaneSignoffWithDifferentCommitter(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "bots.example.org")
	git := &scriptedGit{replies: map[string]string{
		"log -1 --format=%H%x00%an%x00%ae%x00%cn%x00%ce%x00%B%x1e HEAD": "abc123\x00scanner\x00scanner@bots.example.org\x00Human Committer\x00human@example.com\x00fix\n\nSigned-off-by: scanner <scanner@bots.example.org>\x1e",
	}}

	err := (&Broker{Workspace: fakeGitWorkspace(t), AgentName: "scanner", Runner: git}).rejectForgedLaneSignoffs(context.Background(), "", false)
	if err == nil || !strings.Contains(err.Error(), "committed by Human Committer <human@example.com>") {
		t.Fatalf("rejectForgedLaneSignoffs = %v, want committer mismatch rejection", err)
	}
}

func TestRejectForgedLaneSignoffsLogsAuthorMismatchWithoutLaneSignoff(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "bots.example.org")
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	git := &scriptedGit{replies: map[string]string{
		"log -1 --format=%H%x00%an%x00%ae%x00%cn%x00%ce%x00%B%x1e HEAD": "abc123\x00Human Author\x00human@example.com\x00scanner\x00scanner@bots.example.org\x00fix\n\nSigned-off-by: Human Author <human@example.com>\x1e",
	}}

	err := (&Broker{Workspace: fakeGitWorkspace(t), AgentName: "scanner", Runner: git, Logger: logger}).rejectForgedLaneSignoffs(context.Background(), "", false)
	if err != nil {
		t.Fatalf("rejectForgedLaneSignoffs = %v, want nil", err)
	}
	if !strings.Contains(logs.String(), "outgoing commit author differs") {
		t.Fatalf("warning log missing author mismatch: %s", logs.String())
	}
}

func TestLaneGitIdentityInvalidAgentSkipsWithoutGitFallback(t *testing.T) {
	git := &scriptedGit{}

	name, email, err := (&Broker{Workspace: fakeGitWorkspace(t), AgentName: "bad agent", Runner: git}).laneGitIdentity(context.Background())
	if err != nil {
		t.Fatalf("laneGitIdentity error = %v", err)
	}
	if name != "" || email != "" {
		t.Fatalf("laneGitIdentity = (%q, %q), want empty identity", name, email)
	}
	if slices.Contains(git.calls, "var GIT_AUTHOR_IDENT") {
		t.Fatalf("invalid AgentName should not fall back to git config/env: calls=%v", git.calls)
	}
}

func TestGitEnvInvalidAgentDoesNotAppendLaneIdentity(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "")
	env := (&Broker{AgentName: "bad agent"}).gitEnv()
	for _, entry := range env {
		if entry == "GIT_AUTHOR_EMAIL=bad agent@hive.kubestellar.io" || entry == "GIT_COMMITTER_EMAIL=bad agent@hive.kubestellar.io" {
			t.Fatalf("gitEnv appended malformed lane identity: %v", env)
		}
	}
}

func TestParseGitIdent(t *testing.T) {
	cases := []struct {
		name      string
		ident     string
		wantName  string
		wantEmail string
		wantOK    bool
	}{
		{
			name:      "well formed",
			ident:     "scanner <scanner@hive.kubestellar.io> 1700000000 +0000",
			wantName:  "scanner",
			wantEmail: "scanner@hive.kubestellar.io",
			wantOK:    true,
		},
		{
			name:      "extra whitespace",
			ident:     "  Hive Test   <hive@example.com>   1700000000 +0000  \n",
			wantName:  "Hive Test",
			wantEmail: "hive@example.com",
			wantOK:    true,
		},
		{
			name:   "no brackets",
			ident:  "not-a-valid-ident",
			wantOK: false,
		},
		{
			name:   "empty",
			ident:  "",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, email, ok := parseGitIdent(tc.ident)
			if ok != tc.wantOK || name != tc.wantName || email != tc.wantEmail {
				t.Fatalf("parseGitIdent(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.ident, name, email, ok, tc.wantName, tc.wantEmail, tc.wantOK)
			}
		})
	}
}

func TestBrokerPushSanitizesCredentialEnvironmentAndWorkspace(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "safe.txt", "hello\n")
	r := &recordingRunner{}
	t.Setenv("GITHUB_TOKEN", "ghp_should_not_leave_hive")
	t.Setenv("HIVE_GITHUB_TOKEN", "ghp_full_token")
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_minted_push_token"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	if !res.Pushed {
		t.Fatal("Pushed=false")
	}
	if !slices.Contains(r.argsOnPush, "core.hooksPath=/dev/null") || !slices.Contains(r.argsOnPush, "--no-verify") {
		t.Fatalf("push did not disable hooks: %v", r.argsOnPush)
	}
	for _, env := range r.envOnPush {
		if strings.Contains(env, "should_not_leave_hive") || strings.Contains(env, "full_token") || strings.HasPrefix(env, "GITHUB_TOKEN=") || strings.HasPrefix(env, "HIVE_GITHUB_TOKEN=") {
			t.Fatalf("credential leaked into push env: %q", env)
		}
	}
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), "ghs_minted_push_token") {
			t.Fatalf("minted token written to workspace file %s", path)
		}
		return nil
	})
}

// Audit F5 (CWE-214). The minted push token used to be passed as
// `-c http.extraHeader=Authorization: Bearer <token>`, putting a live
// credential in git's argv and therefore in /proc/<pid>/cmdline, which is
// world-readable. Agents run under their own UIDs in this container, so any of
// them could read another tenant's push token for the duration of the push.
//
// The token must now travel in the environment (owner-readable only) and reach
// git through a credential helper. The sibling sanitize test walks the
// workspace and the inherited environment but never inspected argv, which is
// why this was invisible to a green suite.
func TestF5_PushTokenNeverAppearsInGitArgv(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "safe.txt", "hello\n")
	r := &recordingRunner{}
	const token = "ghs_f5_secret_push_token"

	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{token}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	if !res.Pushed {
		t.Fatal("Pushed=false")
	}

	for _, arg := range r.argsOnPush {
		if strings.Contains(arg, token) {
			t.Fatalf("F5: push token leaked into git argv (readable via /proc/<pid>/cmdline): %q", arg)
		}
	}

	// Positive control: if the token is not actually reaching git, the push is
	// silently unauthenticated and this test would pass for the wrong reason.
	var delivered bool
	for _, env := range r.envOnPush {
		if env == pushTokenEnvVar+"="+token {
			delivered = true
		}
	}
	if !delivered {
		t.Fatalf("F5: token was not delivered via %s — the push would be unauthenticated", pushTokenEnvVar)
	}
	var helper bool
	for _, arg := range r.argsOnPush {
		if strings.HasPrefix(arg, "credential.helper=") {
			helper = true
			if !strings.Contains(arg, pushTokenEnvVar) {
				t.Fatalf("F5: credential helper does not read %s: %q", pushTokenEnvVar, arg)
			}
		}
	}
	if !helper {
		t.Fatal("F5: no credential helper configured — git cannot authenticate from the environment")
	}
}

// kubestellar/hive#5116: five agent-authored PRs across four languages
// failed CI formatter gates (gofmt, cargo fmt, prettier) on a trailing blank
// line the underlying coding CLI left at EOF. Hive has no writer of its own
// in this path — the sandboxed CLI writes the file directly — so the broker
// normalises it at the one point hive already controls: right before the
// diff leaves the sandbox.
func TestBrokerStripsTrailingBlankLineBeforePush(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n\n")
	preAmendHead := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	r := &recordingRunner{}
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r, Logger: logger}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	if !res.Pushed {
		t.Fatal("Pushed=false")
	}
	got, readErr := os.ReadFile(filepath.Join(dir, "main.go"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if want := "package main\n\nfunc main() {}\n"; string(got) != want {
		t.Fatalf("trailing blank line not stripped: got %q, want %q", got, want)
	}
	// The amend path itself: HEAD must actually have moved (the strip amended
	// the commit), and the reported Commit must be the NEW head, not the one
	// the CLI originally committed with the trailing blank line still in it.
	if res.Commit == preAmendHead {
		t.Fatalf("commit did not change: still %s — normalisation amend did not run", preAmendHead)
	}
	postAmendHead := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	if res.Commit != postAmendHead {
		t.Fatalf("Result.Commit = %s, want the post-amend HEAD %s", res.Commit, postAmendHead)
	}
	if !strings.Contains(buf.String(), "pushbroker normalised trailing blank lines") {
		t.Fatalf("normalisation was not logged: %s", buf.String())
	}
}

// A file already ending in a single newline — the common, correctly
// formatted case — must be left byte-identical and must not needlessly
// amend the commit.
func TestBrokerLeavesCorrectlyFormattedFileAlone(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n")
	preAmendHead := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	if res.Commit != preAmendHead {
		t.Fatalf("commit changed for an already-clean file: before=%s after=%s", preAmendHead, res.Commit)
	}
	got, readErr := os.ReadFile(filepath.Join(dir, "main.go"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if want := "package main\n\nfunc main() {}\n"; string(got) != want {
		t.Fatalf("file mutated when it should not have been: got %q", got)
	}
}

// A file with no trailing newline at all is a different style question than
// #5116's "...\n\n" defect and must not be touched by this normalisation.
func TestBrokerDoesNotAddMissingTrailingNewline(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}")
	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	if !res.Pushed {
		t.Fatal("Pushed=false")
	}
	got, readErr := os.ReadFile(filepath.Join(dir, "main.go"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if want := "package main\n\nfunc main() {}"; string(got) != want {
		t.Fatalf("file with no trailing newline was mutated: got %q", got)
	}
}

// A binary file that happens to end in two 0x0a bytes must not be reinterpreted
// as text and truncated — the NUL-sniff heuristic exists precisely so a binary
// diff never gets treated like source.
func TestBrokerLeavesBinaryFileWithTrailingNewlinesAlone(t *testing.T) {
	dir := initRepo(t)
	binary := []byte{0x00, 0x01, 0x02, '\n', '\n'}
	path := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(path, binary, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "blob.bin")
	runGit(t, dir, "commit", "-m", "binary")
	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !slices.Equal(got, binary) {
		t.Fatalf("binary file mutated: got %v, want %v", got, binary)
	}
}

// looksBinary samples only the leading 8000 bytes (git's own heuristic
// window). A large, entirely clean text file must not be misclassified as
// binary just because it exceeds that sample size, and — the actual
// uncovered branch — the truncation itself (data[:sample]) must execute
// without a NUL anywhere in the full file.
func TestBrokerNormalisesLargeCleanTextFile(t *testing.T) {
	dir := initRepo(t)
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		b.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	body := b.String()                        // > 8000 bytes, well-formed text, single trailing newline
	writeCommit(t, dir, "big.txt", body+"\n") // add one extra blank line at EOF
	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	got, readErr := os.ReadFile(filepath.Join(dir, "big.txt"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != body {
		t.Fatalf("large clean text file not normalised correctly (len got=%d want=%d)", len(got), len(body))
	}
}

// A large binary file whose only NUL byte falls INSIDE the leading 8000-byte
// sample must still be caught — proving the sample window, not the whole
// file, is what looksBinary actually inspects.
func TestBrokerDetectsBinaryWithinLeadingSample(t *testing.T) {
	dir := initRepo(t)
	data := make([]byte, 9000)
	for i := range data {
		data[i] = 'x'
	}
	data[100] = 0x00 // well inside the 8000-byte sample
	data[len(data)-1] = '\n'
	data[len(data)-2] = '\n' // trailing blank line, which a text path WOULD strip
	path := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "blob.bin")
	runGit(t, dir, "commit", "-m", "large binary")
	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !slices.Equal(got, data) {
		t.Fatal("large binary file with a NUL in its leading sample was mutated")
	}
}

// A file consisting entirely of newlines is the degenerate input
// trimTrailingBlankLines guards explicitly: bytes.TrimRight would strip
// everything, so the function must fall back to a single newline rather than
// emit an empty file.
func TestBrokerCollapsesAllNewlineFileToOneNewline(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "blank.txt", "\n\n\n\n")
	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	got, readErr := os.ReadFile(filepath.Join(dir, "blank.txt"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "\n" {
		t.Fatalf("all-newline file = %q, want a single newline (not empty)", got)
	}
}

// failingRunner lets a test fail one specific git subcommand (matched by a
// substring of the joined argv) while every other invocation proceeds through
// the real git binary — used to exercise stripTrailingBlankLines' own error
// paths (git add / git commit --amend failing) without hand-rolling a full
// scripted double for the whole broker.
type failingRunner struct {
	failSubstr string
	failErr    error
}

func (f *failingRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	if name == "git" && strings.Contains(strings.Join(args, " "), f.failSubstr) {
		return []byte("fatal: injected failure"), f.failErr
	}
	if name == "git" && slices.Contains(args, "push") {
		return []byte("ok"), nil
	}
	return ExecRunner{}.Run(ctx, dir, env, name, args...)
}

// If `git add` fails while staging a normalised file, Run must surface that
// as a wrapped, attributable error rather than silently pushing the
// unnormalised commit.
func TestBrokerSurfacesGitAddFailureDuringNormalisation(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n\n")
	r := &failingRunner{failSubstr: "add --", failErr: errors.New("disk full")}
	_, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "normalising trailing newlines") {
		t.Fatalf("Run error = %v, want it to mention normalising trailing newlines", err)
	}
}

// Same shape for the amend itself: a failed `git commit --amend` must not be
// swallowed.
func TestBrokerSurfacesGitAmendFailureDuringNormalisation(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n\n")
	r := &failingRunner{failSubstr: "commit --amend", failErr: errors.New("hook rejected")}
	_, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "normalising trailing newlines") {
		t.Fatalf("Run error = %v, want it to mention normalising trailing newlines", err)
	}
}

// nthCallFailingRunner fails a matched git subcommand only on its Nth
// occurrence (1-indexed), letting an earlier identical invocation (e.g. the
// FIRST "rev-parse HEAD", before any normalisation) succeed while a LATER one
// (the post-amend re-read) fails. slices/strings-substring matched, same as
// failingRunner.
type nthCallFailingRunner struct {
	failSubstr string
	failOnCall int
	failErr    error
	seen       int
}

func (r *nthCallFailingRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	if name == "git" && strings.Contains(strings.Join(args, " "), r.failSubstr) {
		r.seen++
		if r.seen == r.failOnCall {
			return []byte("fatal: injected failure"), r.failErr
		}
	}
	if name == "git" && slices.Contains(args, "push") {
		return []byte("ok"), nil
	}
	return ExecRunner{}.Run(ctx, dir, env, name, args...)
}

// If HEAD cannot be re-read immediately after a successful amend, Run must
// surface that rather than report success with a stale (pre-amend) commit.
func TestBrokerSurfacesReadHeadFailureAfterAmend(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n\n")
	r := &nthCallFailingRunner{failSubstr: "rev-parse HEAD", failOnCall: 2, failErr: errors.New("index corrupt")}
	_, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "reading HEAD after newline normalisation") {
		t.Fatalf("Run error = %v, want it to mention reading HEAD after newline normalisation", err)
	}
}

// A changed file that becomes unreadable between being listed by `git diff
// --name-only` and stripTrailingBlankLines opening it (permission revoked
// mid-run, in practice a filesystem race) must surface as an attributable
// error rather than silently skip normalisation.
func TestBrokerSurfacesReadFailureDuringNormalisation(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores file permissions, so an unreadable-file test cannot fail as designed")
	}
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n\n")
	path := filepath.Join(dir, "main.go")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o644)
	r := &recordingRunner{}
	_, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "normalising trailing newlines") {
		t.Fatalf("Run error = %v, want it to mention normalising trailing newlines", err)
	}
}

// A changed file that cannot be WRITTEN back must surface as an attributable
// error, not a silently unnormalised push. os.WriteFile on an existing path
// opens O_WRONLY|O_TRUNC on the file itself (pushbroker.go's own doc comment
// notes this), so it is the FILE's write permission that has to be revoked —
// a read-only directory alone still permits truncating an existing file on at
// least one common platform, which is what made an earlier version of this
// test pass for the wrong reason.
func TestBrokerSurfacesWriteFailureDuringNormalisation(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores file permissions, so a read-only-file test cannot fail as designed")
	}
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n\n")
	path := filepath.Join(dir, "main.go")
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o644)
	r := &recordingRunner{}
	_, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "normalising trailing newlines") {
		t.Fatalf("Run error = %v, want it to mention normalising trailing newlines", err)
	}
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestProtectedPathViolationsTable(t *testing.T) {
	files := []string{"policies/guard.yaml", "OWNERS", "hive.yaml.dashboard", "pkg/safe.go"}
	got := ProtectedPathViolations(files, DefaultProtectedPaths)
	if strings.Join(got, ",") != "policies/guard.yaml,OWNERS,hive.yaml.dashboard" {
		t.Fatalf("got %v", got)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "work")
	runGit(t, dir, "config", "user.email", "hive@example.com")
	runGit(t, dir, "config", "user.name", "Hive Test")
	return dir
}

func writeCommit(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", rel)
	runGit(t, dir, "commit", "-m", "test")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}
