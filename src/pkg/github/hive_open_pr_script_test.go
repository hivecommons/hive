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

// stageHiveOpenPRScript copies the real script into a temp root with its
// request dir redirected there, returning the staged script path, the request
// dir, and the root (usable as cwd and as scratch for body files).
func stageHiveOpenPRScript(t *testing.T) (scriptPath, reqDir, root string) {
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

	root = t.TempDir()
	reqDir = filepath.Join(root, "pr-requests")

	// Point the hard-coded request dir at the temp root. If this literal ever
	// stops matching, the test would silently write to the real /var/run path
	// (or nowhere), so fail loudly instead.
	text := string(src)
	const reqDirLiteral = "/var/run/hive-metrics/pr-requests"
	if !strings.Contains(text, reqDirLiteral) {
		t.Fatalf("hive-open-pr.sh no longer references %s; this test would cover nothing", reqDirLiteral)
	}
	scriptPath = filepath.Join(root, "hive-open-pr.sh")
	if err := os.WriteFile(scriptPath, []byte(strings.ReplaceAll(text, reqDirLiteral, reqDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	return scriptPath, reqDir, root
}

// runHiveOpenPR executes the real script with its request dir redirected into a
// temp root, and returns the single request it wrote.
func runHiveOpenPR(t *testing.T, args ...string) PRRequest {
	t.Helper()
	scriptPath, reqDir, root := stageHiveOpenPRScript(t)

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

// runHiveOpenPRExpectingRefusal executes the script expecting it to FAIL: it
// must exit non-zero, write NO request file, and explain itself on stderr.
// Returns the combined output for message assertions.
func runHiveOpenPRExpectingRefusal(t *testing.T, args ...string) string {
	t.Helper()
	scriptPath, reqDir, root := stageHiveOpenPRScript(t)

	cmd := exec.Command("bash", append([]string{scriptPath}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HIVE_AGENT=scanner")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("hive-open-pr.sh %v succeeded, want a loud refusal\n%s", args, out)
	}
	entries, globErr := filepath.Glob(filepath.Join(reqDir, "*.json"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused invocation must write no request, wrote: %v", entries)
	}
	return string(out)
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

// The empty-body family pins the fix for the footer-only PRs (e.g.
// Danathar/atomic-image-builder#223): the agent wrote a full body to a file
// and passed `--body-file`, which the old parser silently dropped, so the
// request carried body:"" and the opened PR's only content was the
// attribution trailer. The script must (a) honor --body-file exactly as gh
// does, and (b) refuse loudly to submit an empty body at all.

func TestHiveOpenPRScript_BodyFileIsRead(t *testing.T) {
	scriptPath, reqDir, root := stageHiveOpenPRScript(t)
	bodyPath := filepath.Join(root, "pr-body.md")
	const body = "## Change\n\nfixes the thing\n\nCloses #12\n"
	if err := os.WriteFile(bodyPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", scriptPath,
		"--repo", "o/r", "--head", "quality/fix-12",
		"--title", "[quality] fix", "--body-file", bodyPath)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HIVE_AGENT=quality")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hive-open-pr.sh --body-file: %v\n%s", err, out)
	}

	entries, err := filepath.Glob(filepath.Join(reqDir, "*.json"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("wrote %d request files (err %v), want 1", len(entries), err)
	}
	data, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	var req PRRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	// $(cat) drops trailing newlines; that is fine for a PR body, so compare
	// with the same trim rather than pretending the script preserves them.
	if req.Body != strings.TrimRight(body, "\n") {
		t.Fatalf("request body = %q, want the file's content %q", req.Body, body)
	}
}

func TestHiveOpenPRScript_EmptyBodyIsRefused(t *testing.T) {
	out := runHiveOpenPRExpectingRefusal(t,
		"--repo", "o/r", "--head", "quality/fix-1", "--title", "[quality] fix")
	if !strings.Contains(out, "empty body") {
		t.Fatalf("refusal must say the body is empty, got:\n%s", out)
	}
}

func TestHiveOpenPRScript_WhitespaceBodyIsRefused(t *testing.T) {
	runHiveOpenPRExpectingRefusal(t,
		"--repo", "o/r", "--head", "quality/fix-1", "--title", "[quality] fix",
		"--body", " \n\t ")
}

func TestHiveOpenPRScript_MissingBodyFileIsRefused(t *testing.T) {
	out := runHiveOpenPRExpectingRefusal(t,
		"--repo", "o/r", "--head", "quality/fix-1", "--title", "[quality] fix",
		"--body-file", "/nonexistent/pr-body.md")
	if !strings.Contains(out, "not readable") && !strings.Contains(out, "does not exist") {
		t.Fatalf("refusal must name the unreadable file, got:\n%s", out)
	}
}

// --issues declares the originating issue(s); the watcher verifies the body
// references each one. "#" prefixes, repetition, and comma lists all normalize.
func TestHiveOpenPRScript_IssuesFlagLandsInRequest(t *testing.T) {
	req := runHiveOpenPR(t,
		"--repo", "o/r", "--head", "quality/fix-12",
		"--title", "[quality] fix", "--body", "Closes #12, Closes #34, Refs #56 — docs half stays open",
		"--issues", "12,#34", "--issue", "56")
	if len(req.IssueN) != 3 || req.IssueN[0] != 12 || req.IssueN[1] != 34 || req.IssueN[2] != 56 {
		t.Fatalf("request issues = %v, want [12 34 56]", req.IssueN)
	}
}

func TestHiveOpenPRScript_NonNumericIssueIsRefused(t *testing.T) {
	runHiveOpenPRExpectingRefusal(t,
		"--repo", "o/r", "--head", "quality/fix-1", "--title", "[quality] fix",
		"--body", "Closes #12", "--issues", "twelve")
}

// runHiveOpenPRCapturing runs the script expecting success and returns both the
// request it wrote and everything it printed, so a test can assert on what the
// operator (or the agent reading its own transcript) was told.
func runHiveOpenPRCapturing(t *testing.T, args ...string) (PRRequest, string) {
	t.Helper()
	scriptPath, reqDir, root := stageHiveOpenPRScript(t)

	cmd := exec.Command("bash", append([]string{scriptPath}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HIVE_AGENT=scanner")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hive-open-pr.sh %v: %v\n%s", args, err, out)
	}

	entries, globErr := filepath.Glob(filepath.Join(reqDir, "*.json"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(entries) != 1 {
		t.Fatalf("wrote %d request files, want 1: %v", len(entries), entries)
	}
	data, readErr := os.ReadFile(entries[0])
	if readErr != nil {
		t.Fatal(readErr)
	}
	var req PRRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("request is not valid PRRequest JSON (%s): %v", data, err)
	}
	return req, string(out)
}

// TestHiveOpenPRScript_LabelFlagIsAcceptedQuietly covers the flag every
// hold-gated policy template tells agents to pass.
//
// The hold label IS applied -- server-side, by the watcher -- so warning
// "ignoring unrecognized flag --label" is not just noise: to an agent reading
// its own transcript mid-run it reads as "your PR will not be held", which is
// the opposite of what happens. The warning machinery itself is right and must
// survive for genuinely unknown flags, which the sibling test pins.
func TestHiveOpenPRScript_LabelFlagIsAcceptedQuietly(t *testing.T) {
	req, out := runHiveOpenPRCapturing(t,
		"--repo", "projectbluefin/dakota", "--head", "hive/fix-1",
		"--title", "t", "--body", "Closes #7", "--label", "hold",
	)
	if strings.Contains(out, "ignoring unrecognized flag") {
		t.Fatalf("--label must not warn; the label is applied server-side\n%s", out)
	}
	// The flag's VALUE must not survive into the request either.
	if req.Title != "t" || req.Body != "Closes #7" {
		t.Fatalf("--label consumed the wrong arguments: %+v", req)
	}
}

// TestHiveOpenPRScript_UnknownFlagStillWarns is the discriminating half: the
// fix above must not have turned the warning off for everything.
func TestHiveOpenPRScript_UnknownFlagStillWarns(t *testing.T) {
	_, out := runHiveOpenPRCapturing(t,
		"--repo", "projectbluefin/dakota", "--head", "hive/fix-1",
		"--title", "t", "--body", "Closes #7", "--not-a-real-flag",
	)
	if !strings.Contains(out, "ignoring unrecognized flag") {
		t.Fatalf("an unknown flag must still be reported\n%s", out)
	}
}

// TestHiveOpenPRScript_MultilineBodyIsValidJSONWithoutPython3 pins the
// fallback escaper against the case this script exists to get right.
//
// A --body-file body is multi-line by nature, and a literal newline inside a
// JSON string is invalid JSON. The escaper used when python3 is absent
// escaped only backslash and double-quote, so on such a host the fix for lost
// bodies produced a request the watcher could only quarantine: it failed safe,
// but it did not work. The test runs the script with a PATH that deliberately
// has no python3.
func TestHiveOpenPRScript_MultilineBodyIsValidJSONWithoutPython3(t *testing.T) {
	scriptPath, reqDir, root := stageHiveOpenPRScript(t)

	// A PATH carrying only what the fallback path needs -- and no python3.
	stubBin := filepath.Join(root, "nopy-bin")
	if err := os.MkdirAll(stubBin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"bash", "awk", "cat", "date", "mkdir", "tr"} {
		real, lookErr := exec.LookPath(tool)
		if lookErr != nil {
			t.Skipf("%s not available: %v", tool, lookErr)
		}
		if err := os.Symlink(real, filepath.Join(stubBin, tool)); err != nil {
			t.Fatal(err)
		}
	}

	body := "Summary line\n\nA second paragraph with a \"quote\", a \\ backslash,\n\tand a tab.\n"
	bodyFile := filepath.Join(root, "body.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(filepath.Join(stubBin, "bash"), scriptPath,
		"--repo", "projectbluefin/dakota", "--head", "hive/fix-1",
		"--title", "t", "--body-file", bodyFile,
	)
	cmd.Dir = root
	cmd.Env = []string{"HIVE_AGENT=scanner", "PATH=" + stubBin, "HOME=" + root}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed with no python3 on PATH: %v\n%s", err, out)
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

	// The assertion the old escaper failed: the request parses at all.
	var req PRRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("fallback escaper wrote invalid JSON (%s): %v", data, err)
	}
	// And the body survived intact, not merely parseably. The script strips
	// the trailing newline the same way the python3 path does.
	if want := strings.TrimRight(body, "\n"); req.Body != want {
		t.Fatalf("body did not round-trip\n got: %q\nwant: %q", req.Body, want)
	}
}
