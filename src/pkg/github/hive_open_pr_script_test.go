package github

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/internal/testutil"
)

// bin/hive-open-pr.sh is what agents run INSTEAD of `gh pr create`; it writes
// the PRRequest this package's watcher consumes. It used to default BASE to
// "main", which meant an agent that (correctly) said nothing about the base
// still pinned every PR to "main" — so no amount of default-branch resolution
// inside CreatePR could have helped: the request already carried the wrong
// answer (kubestellar/hive#4928).
//
// These tests exercise the real script, following gh_app_token_script_test.go,
// rather than a paraphrase of it.

const hiveOpenPRScriptPath = "../../../bin/hive-open-pr.sh"

// runHiveOpenPR executes the real script with its request dir redirected into a
// temp root, and returns the single request it wrote.
func runHiveOpenPR(t *testing.T, args ...string) PRRequest {
	t.Helper()
	src, err := os.ReadFile(hiveOpenPRScriptPath)
	if err != nil {
		testutil.SkipfUnlessRequired(t, "hive-open-pr.sh not readable from this package: %v", err)
	}
	for _, tool := range []string{"bash", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available: %v", tool, err)
		}
	}

	root := t.TempDir()
	reqDir := filepath.Join(root, "pr-requests")

	// Point the hard-coded request dir at the temp root. If this literal ever
	// stops matching, the test would silently write to the real /var/run path
	// (or nowhere), so fail loudly instead.
	text := string(src)
	const reqDirLiteral = "/var/run/hive-metrics/pr-requests"
	if !strings.Contains(text, reqDirLiteral) {
		t.Fatalf("hive-open-pr.sh no longer references %s; this test would cover nothing", reqDirLiteral)
	}
	scriptPath := filepath.Join(root, "hive-open-pr.sh")
	if err := os.WriteFile(scriptPath, []byte(strings.ReplaceAll(text, reqDirLiteral, reqDir)), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", append([]string{scriptPath}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HIVE_AGENT=scanner")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hive-open-pr.sh %v: %v\n%s", args, err, out)
	}

	entries, err := filepath.Glob(filepath.Join(reqDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("wrote %d request files, want 1: %v", len(entries), entries)
	}
	data, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	var req PRRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("request is not valid PRRequest JSON (%s): %v", data, err)
	}
	return req
}

// TestHiveOpenPRScript_OmittedBaseLeavesBaseUnset asserts the invariant this
// bug broke: an agent that says nothing about --base must not have the script
// silently pin "main" into the request. A test that only checked "the request
// has SOME base" would pass even with the old BASE="main" default.
func TestHiveOpenPRScript_OmittedBaseLeavesBaseUnset(t *testing.T) {
	req := runHiveOpenPR(t,
		"--repo", "projectbluefin/dakota", "--head", "hive/fix-1",
		"--title", "fix a thing", "--body", "body")

	if req.Base != "" {
		t.Fatalf("request pinned base %q; an omitted --base must stay empty so the hive "+
			"resolves the repository's default branch", req.Base)
	}
	if req.Repo != "projectbluefin/dakota" || req.Head != "hive/fix-1" || req.Title != "fix a thing" {
		t.Fatalf("request lost fields: %+v", req)
	}
}

func TestHiveOpenPRScript_ExplicitBaseIsPreserved(t *testing.T) {
	req := runHiveOpenPR(t,
		"--repo", "projectbluefin/dakota", "--head", "hive/fix-1",
		"--base", "release-1.2", "--title", "fix a thing", "--body", "body")

	if req.Base != "release-1.2" {
		t.Fatalf("request base = %q, want the explicitly requested release-1.2", req.Base)
	}
}

func TestHiveOpenPRScript_ExplicitBaseEqualsFormIsPreserved(t *testing.T) {
	req := runHiveOpenPR(t,
		"--repo=projectbluefin/dakota", "--head=hive/fix-1",
		"--base=release-1.2", "--title=fix a thing", "--body=body")

	if req.Base != "release-1.2" {
		t.Fatalf("request base = %q, want release-1.2", req.Base)
	}
}
