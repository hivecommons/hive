package pushbroker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// A push to an existing PR branch (hivecommons/hive#18574) carries a lease on
// the head the kick checked out and adds only commits built on it.
func TestBrokerLeasedPushToExistingBranch(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "base.txt", "base\n")
	lease := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	writeCommit(t, dir, "fix.txt", "fix\n")
	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "work", BaseRef: lease, LeaseSHA: lease, Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_lease"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	if !res.Pushed || res.Lease != lease {
		t.Fatalf("pushed=%v lease=%q", res.Pushed, res.Lease)
	}
	if !slices.Contains(r.argsOnPush, "--force-with-lease=refs/heads/work:"+lease) {
		t.Fatalf("push lacks the lease: %v", r.argsOnPush)
	}
	if got := r.argsOnPush[len(r.argsOnPush)-2:]; got[0] != DefaultRemote || got[1] != "HEAD:refs/heads/work" {
		t.Fatalf("push target = %v", got)
	}
	if !slices.Equal(res.ChangedFiles, []string{"fix.txt"}) {
		t.Fatalf("scans judged %v, want only the kick's own commit", res.ChangedFiles)
	}
}

func TestBrokerRefusesLeasedPushThatRewritesTheBranch(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "base.txt", "base\n")
	runGit(t, dir, "checkout", "-b", "side")
	writeCommit(t, dir, "side.txt", "theirs\n")
	lease := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	runGit(t, dir, "checkout", "work")
	writeCommit(t, dir, "fix.txt", "fix\n")
	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "work", BaseRef: lease, LeaseSHA: lease, Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_lease"}, Runner: r}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not descend from") {
		t.Fatalf("err = %v", err)
	}
	if res.Pushed || r.argsOnPush != nil {
		t.Fatal("a push that drops the PR's commits must not run")
	}
}

func TestBrokerRefusesMalformedLease(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "fix.txt", "fix\n")
	for _, lease := range []string{"abc", "--upload-pack=x", strings.Repeat("g", 40)} {
		_, err := (&Broker{Workspace: dir, Branch: "work", LeaseSHA: lease, Repo: "hivecommons/hive", Minter: fakeMinter{"t"}}).Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "is not a full commit SHA") {
			t.Fatalf("lease %q: err = %v", lease, err)
		}
	}
}

// failingPushRunner refuses the push as a remote whose branch moved would.
type failingPushRunner struct{}

func (failingPushRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	if name == "git" && slices.Contains(args, "push") {
		return []byte("! [rejected] HEAD -> work (stale info)"), errors.New("exit status 1")
	}
	return ExecRunner{}.Run(ctx, dir, env, name, args...)
}

func TestBrokerReportsLeaseRejection(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "base.txt", "base\n")
	lease := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	writeCommit(t, dir, "fix.txt", "fix\n")
	res, err := (&Broker{Workspace: dir, Branch: "work", BaseRef: lease, LeaseSHA: lease, Repo: "hivecommons/hive", Minter: fakeMinter{"t"}, Runner: failingPushRunner{}}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is never overwritten") || res.Pushed {
		t.Fatalf("err = %v pushed=%v", err, res.Pushed)
	}
}
