package pushbroker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/prosefilter"
)

const emDashForTest = prosefilter.EmDash

// commitWithMessage commits a file with an exact commit message.
func commitWithMessage(t *testing.T, dir, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	msgFile := filepath.Join(t.TempDir(), "msg")
	if err := os.WriteFile(msgFile, []byte(msg), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "--cleanup=verbatim", "-F", msgFile)
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
	msg := "fix: a " + emDashForTest + " b\n"
	commitWithMessage(t, dir, msg)
	if _, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: &recordingRunner{}}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := headMessage(t, dir); got != msg {
		t.Fatalf("switch off still rewrote the message: %q", got)
	}
}

func TestBrokerSurfacesCommitMessageAmendFailure(t *testing.T) {
	dir := initRepo(t)
	commitWithMessage(t, dir, "fix: a "+emDashForTest+" b\n")
	r := &failingRunner{failSubstr: "--cleanup=verbatim", failErr: errors.New("amend refused")}
	_, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "normalising commit message dashes") {
		t.Fatalf("Run error = %v, want it to mention normalising commit message dashes", err)
	}
}

func TestBrokerSurfacesCommitMessageReadFailure(t *testing.T) {
	dir := initRepo(t)
	commitWithMessage(t, dir, "fix: plain\n")
	r := &failingRunner{failSubstr: "cat-file commit HEAD", failErr: errors.New("object missing")}
	_, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "reading HEAD commit message") {
		t.Fatalf("Run error = %v, want it to mention reading HEAD commit message", err)
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
