package integrated

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPriorSetupPullRequestIdentity(t *testing.T) {
	tests := []struct {
		name       string
		prior      Config
		hasPrior   bool
		wantNumber int
		wantHead   string
		wantErr    bool
	}{
		{name: "no prior", prior: Config{SetupPRNumber: 7, SetupHeadSHA: "head"}, wantNumber: 0, wantHead: ""},
		{name: "empty pre-PR recovery", prior: Config{SetupBranch: "hive/setup-123"}, hasPrior: true, wantNumber: 0, wantHead: ""},
		{name: "direct bootstrap", prior: Config{DefaultBranch: "main", SetupBranch: "main", SetupHeadSHA: strings.Repeat("a", 40), ExecutionMode: ExecutionLocal, Automation: AutomationRepairPR, VisualHive: true}, hasPrior: true, wantNumber: 0, wantHead: ""},
		{name: "prior pull request", prior: Config{SetupPRNumber: 7, SetupHeadSHA: "head"}, hasPrior: true, wantNumber: 7, wantHead: "head"},
		{name: "malformed negative number", prior: Config{SetupPRNumber: -1, SetupHeadSHA: "head"}, hasPrior: true, wantNumber: -1, wantHead: "head"},
		{name: "incomplete pull request identity", prior: Config{SetupPRNumber: 7}, hasPrior: true, wantNumber: 7, wantHead: ""},
		{name: "zero number invalid head", prior: Config{DefaultBranch: "main", SetupBranch: "main", SetupHeadSHA: "head", ExecutionMode: ExecutionLocal, Automation: AutomationRepairPR, VisualHive: true}, hasPrior: true, wantErr: true},
		{name: "zero number non-default branch", prior: Config{DefaultBranch: "main", SetupBranch: "hive/setup-123", SetupHeadSHA: strings.Repeat("a", 40), ExecutionMode: ExecutionLocal, Automation: AutomationRepairPR, VisualHive: true}, hasPrior: true, wantErr: true},
		{name: "zero number pull request URL", prior: Config{DefaultBranch: "main", SetupBranch: "main", SetupPRURL: "https://example.test/pull/7", SetupHeadSHA: strings.Repeat("a", 40), ExecutionMode: ExecutionLocal, Automation: AutomationRepairPR, VisualHive: true}, hasPrior: true, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			number, head, err := priorSetupPullRequestIdentity(test.prior, test.hasPrior)
			if (err != nil) != test.wantErr {
				t.Fatalf("prior setup pull request identity error = %v; want error %t", err, test.wantErr)
			}
			if test.wantErr {
				return
			}
			if number != test.wantNumber || head != test.wantHead {
				t.Fatalf("prior setup pull request identity = (%d, %q); want (%d, %q)", number, head, test.wantNumber, test.wantHead)
			}
		})
	}
}

func TestStagedTreeMatchesExistingSetupBranch(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	checkout := filepath.Join(root, "checkout")

	runIntegratedGit(t, root, "init", "--bare", remote)
	runIntegratedGit(t, root, "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runIntegratedGit(t, seed, "add", ".")
	runIntegratedGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "seed")
	runIntegratedGit(t, seed, "remote", "add", "origin", remote)
	runIntegratedGit(t, seed, "push", "-u", "origin", "main")
	runIntegratedGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runIntegratedGit(t, root, "clone", remote, checkout)

	runIntegratedGit(t, checkout, "switch", "-C", "hive/setup", "origin/main")
	setupPath := filepath.Join(checkout, ".hive", "integrated.json")
	if err := os.MkdirAll(filepath.Dir(setupPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(setupPath, []byte("{\"version\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runIntegratedGit(t, checkout, "add", ".hive/integrated.json")
	runIntegratedGit(t, checkout, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "setup")
	runIntegratedGit(t, checkout, "push", "-u", "origin", "hive/setup")
	remoteSHA := strings.TrimSpace(integratedGitOutput(t, checkout, "rev-parse", "origin/hive/setup"))

	runIntegratedGit(t, checkout, "switch", "-C", "proof", "origin/main")
	if err := os.MkdirAll(filepath.Dir(setupPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(setupPath, []byte("{\"version\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runIntegratedGit(t, checkout, "add", ".hive/integrated.json")

	matches, sha, err := stagedTreeMatchesRemoteBranch(context.Background(), checkout, "hive/setup")
	if err != nil {
		t.Fatal(err)
	}
	if !matches || sha != remoteSHA {
		t.Fatalf("matching setup tree = %t, sha = %q; want true, %q", matches, sha, remoteSHA)
	}

	if err := os.WriteFile(setupPath, []byte("{\"version\":2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runIntegratedGit(t, checkout, "add", ".hive/integrated.json")
	matches, _, err = stagedTreeMatchesRemoteBranch(context.Background(), checkout, "hive/setup")
	if err != nil {
		t.Fatal(err)
	}
	if matches {
		t.Fatal("changed setup tree must not be treated as idempotent")
	}
}
