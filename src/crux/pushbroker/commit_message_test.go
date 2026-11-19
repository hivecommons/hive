package pushbroker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/prosefilter"
)

const emDashForTest = prosefilter.EmDash

const testSignoff = "Signed-off-by: Hive Test <hive@example.com>\n"

// commitWithMessage commits a file with an exact commit message, adding the
// committer's Signed-off-by trailer the broker requires when it is missing,
// and returns the message as committed.
func commitWithMessage(t *testing.T, dir, msg string) string {
	t.Helper()
	if !strings.Contains(msg, testSignoff) {
		msg += "\n" + testSignoff
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	msgFile := filepath.Join(t.TempDir(), "msg")
	if err := os.WriteFile(msgFile, []byte(msg), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "--cleanup=verbatim", "-F", msgFile)
	return msg
}

func headMessage(t *testing.T, dir string) string {
	t.Helper()
	raw := runGitOutput(t, dir, "cat-file", "commit", "HEAD")
	_, msg, _ := strings.Cut(raw, "\n\n")
	return msg
}

func TestBrokerRewritesHeadCommitMessageDashes(t *testing.T) {
	dir := initRepo(t)
	msg := "fix: a " + emDashForTest + " b\n\nWhy " + emDashForTest + " because.\n\n```\nkeep " + emDashForTest + " this\n```\n\nSigned-off-by: Hive Test <hive@example.com>\n"
	commitWithMessage(t, dir, msg)
	before := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: &recordingRunner{}}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	want := "fix: a - b\n\nWhy - because.\n\n```\nkeep " + emDashForTest + " this\n```\n\nSigned-off-by: Hive Test <hive@example.com>\n"
	if got := headMessage(t, dir); got != want {
		t.Fatalf("HEAD message = %q, want %q", got, want)
	}
	after := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	if after == before || res.Commit != after {
		t.Fatalf("Result.Commit = %s, HEAD %s (was %s): the amend must be what is pushed", res.Commit, after, before)
	}
}

// The success path logs how many dashes it rewrote, so an operator can tell
// the amend ran; the count is the drop between the original and the result.
func TestBrokerLogsHeadCommitMessageRewrite(t *testing.T) {
	dir := initRepo(t)
	commitWithMessage(t, dir, "fix: a "+emDashForTest+" b\n\nWhy "+emDashForTest+" because.\n")
	var buf strings.Builder
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: &recordingRunner{},
		Logger: slog.New(slog.NewTextHandler(&buf, nil))}).Run(context.Background())
	if err != nil || !res.Pushed {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if strings.Contains(headMessage(t, dir), emDashForTest) {
		t.Fatalf("dashes survived the amend: %q", headMessage(t, dir))
	}
	if log := buf.String(); !strings.Contains(log, "rewrote dashes in the HEAD commit message") || !strings.Contains(log, "dashes=2") {
		t.Fatalf("rewrite not logged with its dash count: %s", log)
	}
}

func TestBrokerLeavesCleanCommitMessageAlone(t *testing.T) {
	dir := initRepo(t)
	commitWithMessage(t, dir, "fix: a - b\n\nlevels 3"+prosefilter.EnDash+"5\n")
	before := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: &recordingRunner{}}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Commit != before {
		t.Fatalf("a commit with nothing to rewrite was amended: %s -> %s", before, res.Commit)
	}
}

func TestBrokerCommitMessageSwitchOff(t *testing.T) {
	t.Setenv(prosefilter.AgentWritesEnv, "false")
	dir := initRepo(t)
	msg := commitWithMessage(t, dir, "fix: a "+emDashForTest+" b\n")
	if _, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: &recordingRunner{}}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := headMessage(t, dir); got != msg {
		t.Fatalf("switch off still rewrote the message: %q", got)
	}
}

// A dash rewrite never blocks a push: an amend or read failure is logged and
// the commit leaves as the agent wrote it.
func TestBrokerCommitMessageFailuresDoNotBlockPush(t *testing.T) {
	for _, substr := range []string{"--cleanup=verbatim", "cat-file commit HEAD"} {
		t.Run(substr, func(t *testing.T) {
			dir := initRepo(t)
			msg := commitWithMessage(t, dir, "fix: a "+emDashForTest+" b\n")
			var buf strings.Builder
			r := &nthCallFailingRunner{failSubstr: substr, failOnCall: 1, failErr: errors.New("injected")}
			res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r,
				Logger: slog.New(slog.NewTextHandler(&buf, nil))}).Run(context.Background())
			if err != nil || !res.Pushed {
				t.Fatalf("Run = %+v, %v; want the push to go ahead", res, err)
			}
			if got := headMessage(t, dir); got != msg {
				t.Fatalf("message changed despite the failure: %q", got)
			}
			if !strings.Contains(buf.String(), "could not rewrite dashes") {
				t.Fatalf("failure not logged: %s", buf.String())
			}
		})
	}
}

func TestSanitizeCommitMessage(t *testing.T) {
	cases := map[string]string{
		"subject only " + emDashForTest + " x":               "subject only - x",
		"s " + emDashForTest + " t\n\nbody " + emDashForTest: "s - t\n\nbody -",
		"clean\n\nbody\n": "clean\n\nbody\n",
	}
	for in, want := range cases {
		if got := sanitizeCommitMessage(in); got != want {
			t.Errorf("sanitizeCommitMessage(%q) = %q, want %q", in, got, want)
		}
	}
}
