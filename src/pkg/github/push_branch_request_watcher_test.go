package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// pushExecCall records one git invocation the watcher made through the
// pushBranchExec seam.
type pushExecCall struct {
	dir  string
	env  []string
	args []string
}

// withPushExec installs a stub git runner for the test and returns the calls
// it receives. sha is what `rev-parse` reports; pushErr, when non-nil, fails
// the `push` invocation with output pushOut.
func withPushExec(t *testing.T, sha string, pushErr error, pushOut string) *[]pushExecCall {
	t.Helper()
	var calls []pushExecCall
	old := pushBranchExec
	pushBranchExec = func(_ context.Context, dir string, env []string, name string, args ...string) (string, error) {
		calls = append(calls, pushExecCall{dir: dir, env: env, args: append([]string{name}, args...)})
		if len(args) > 0 && args[0] == "rev-parse" {
			return sha + "\n", nil
		}
		if pushErr != nil {
			return pushOut, pushErr
		}
		return "", nil
	}
	t.Cleanup(func() { pushBranchExec = old })
	return &calls
}

// withPushDir points the watcher at a temp request dir for the test.
func withPushDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := pushBranchRequestDirForTest
	pushBranchRequestDirForTest = dir
	t.Cleanup(func() { pushBranchRequestDirForTest = old })
	return dir
}

// pushTestClient returns a client whose push authorizer ALLOWS everything and
// whose default-branch cache is pre-seeded, so the watcher's own gates can be
// exercised without a GitHub server. Authorization itself is tested separately.
func pushTestClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	c := NewClientForTest(srvURL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.pushBranchAuthz = func(agent string, uid int) error { return nil }
	c.storeDefaultBranch("o/r", "main")
	return c
}

func readPushBranchResult(t *testing.T, reqPath string) PushBranchResponse {
	t.Helper()
	out := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading result %s: %v", out, err)
	}
	var resp PushBranchResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("bad result json: %v", err)
	}
	return resp
}

// End to end through the seam: a request file is pushed, consumed, audited
// with typed repo (and no target — a branch push has no number), and the push
// goes to the URL built from the validated repo, never one from the checkout.
func TestPushBranchRequestWatcher_PushesAndConsumes(t *testing.T) {
	c := pushTestClient(t, "http://127.0.0.1:1")
	calls := withPushExec(t, "cafe1234", nil, "")
	recs := captureAudit(c)
	dir := withPushDir(t)
	checkout := t.TempDir()

	reqPath, err := WritePushBranchRequest(dir, PushBranchRequest{Repo: "o/r", Branch: "scanner/fix-1", Dir: checkout, Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessPushBranchRequestsOnce(context.Background())

	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("request file should be removed after success")
	}
	resp := readPushBranchResult(t, reqPath)
	if !resp.OK || resp.Branch != "scanner/fix-1" || resp.SHA != "cafe1234" {
		t.Errorf("result = %+v, want ok with branch and sha", resp)
	}
	if len(*calls) != 2 {
		t.Fatalf("git calls = %d, want rev-parse + push", len(*calls))
	}
	push := (*calls)[1]
	joined := strings.Join(push.args, " ")
	if push.dir != checkout ||
		!strings.Contains(joined, "https://github.com/o/r.git") ||
		!strings.Contains(joined, "refs/heads/scanner/fix-1:refs/heads/scanner/fix-1") {
		t.Errorf("push call = %+v, want the validated repo's URL and a full refspec", push)
	}
	if strings.Contains(joined, "--force-with-lease") {
		t.Error("an unforced request pushed with --force-with-lease")
	}
	rec, ok := findAudit(*recs, AuditActionAgentBranchPushed)
	if !ok {
		t.Fatalf("push not audited: %+v", *recs)
	}
	if rec.Repo != "o/r" || rec.Target != 0 || rec.Agent != "scanner" {
		t.Errorf("audit typed fields = %+v, want repo=o/r target=0 agent=scanner", rec)
	}
	for _, want := range []string{"branch=scanner/fix-1", "sha=cafe1234"} {
		if !strings.Contains(rec.Detail, want) {
			t.Errorf("audit detail lost %q: %q", want, rec.Detail)
		}
	}
}

func TestPushBranchRequestWatcher_ForceWithLease(t *testing.T) {
	c := pushTestClient(t, "http://127.0.0.1:1")
	calls := withPushExec(t, "cafe1234", nil, "")
	dir := withPushDir(t)

	if _, err := WritePushBranchRequest(dir, PushBranchRequest{Repo: "o/r", Branch: "scanner/fix-2", Dir: t.TempDir(), Agent: "scanner", ForceWithLease: true}); err != nil {
		t.Fatal(err)
	}
	c.ProcessPushBranchRequestsOnce(context.Background())

	if len(*calls) != 2 {
		t.Fatalf("git calls = %d, want 2", len(*calls))
	}
	if joined := strings.Join((*calls)[1].args, " "); !strings.Contains(joined, "--force-with-lease") || strings.Contains(joined, " --force ") {
		t.Errorf("push args = %q, want --force-with-lease and never a plain --force", joined)
	}
}

// A nil authorizer fails CLOSED, exactly like the merge watcher.
func TestPushBranchRequestWatcher_NilAuthorizerFailsClosed(t *testing.T) {
	c := pushTestClient(t, "http://127.0.0.1:1")
	c.pushBranchAuthz = nil
	calls := withPushExec(t, "cafe1234", nil, "")
	dir := withPushDir(t)

	reqPath, err := WritePushBranchRequest(dir, PushBranchRequest{Repo: "o/r", Branch: "scanner/fix-1", Dir: t.TempDir(), Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessPushBranchRequestsOnce(context.Background())

	if len(*calls) != 0 {
		t.Fatalf("%d git calls with no authorizer, want 0", len(*calls))
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("request was not quarantined: %v", err)
	}
	if resp := readPushBranchResult(t, reqPath); resp.OK || !strings.Contains(resp.Error, "no authorizer") {
		t.Errorf("result does not explain the fail-closed denial: %+v", resp)
	}
}

func TestPushBranchRequestWatcher_DeniedByAuthorizer(t *testing.T) {
	c := pushTestClient(t, "http://127.0.0.1:1")
	c.pushBranchAuthz = func(agent string, uid int) error { return errors.New("agent \"scanner\" is not push-capable") }
	calls := withPushExec(t, "cafe1234", nil, "")
	dir := withPushDir(t)

	reqPath, err := WritePushBranchRequest(dir, PushBranchRequest{Repo: "o/r", Branch: "scanner/fix-1", Dir: t.TempDir(), Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessPushBranchRequestsOnce(context.Background())

	if len(*calls) != 0 {
		t.Fatalf("%d git calls after a denial, want 0", len(*calls))
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("request was not quarantined: %v", err)
	}
	if resp := readPushBranchResult(t, reqPath); resp.OK || !strings.Contains(resp.Error, "not push-capable") {
		t.Errorf("result does not carry the authorizer's reason: %+v", resp)
	}
}

// The default branch is refused whatever the allowlist says: the relay
// publishes topic branches, it never lands changes directly.
func TestPushBranchRequestWatcher_RefusesDefaultBranch(t *testing.T) {
	c := pushTestClient(t, "http://127.0.0.1:1")
	calls := withPushExec(t, "cafe1234", nil, "")
	dir := withPushDir(t)

	reqPath, err := WritePushBranchRequest(dir, PushBranchRequest{Repo: "o/r", Branch: "main", Dir: t.TempDir(), Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessPushBranchRequestsOnce(context.Background())

	if len(*calls) != 0 {
		t.Fatalf("%d git calls for a default-branch push, want 0", len(*calls))
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("refused push was not quarantined: %v", err)
	}
	if resp := readPushBranchResult(t, reqPath); resp.OK || !strings.Contains(resp.Error, "default branch") {
		t.Errorf("result does not explain the default-branch refusal: %+v", resp)
	}
}

func TestPushBranchRequestWatcher_RefusesInvalidBranchName(t *testing.T) {
	c := pushTestClient(t, "http://127.0.0.1:1")
	calls := withPushExec(t, "cafe1234", nil, "")
	dir := withPushDir(t)

	reqPath, err := WritePushBranchRequest(dir, PushBranchRequest{Repo: "o/r", Branch: "--force", Dir: t.TempDir(), Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessPushBranchRequestsOnce(context.Background())

	if len(*calls) != 0 {
		t.Fatalf("%d git calls for an invalid branch name, want 0", len(*calls))
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("refused push was not quarantined: %v", err)
	}
}

// Same footing as the PR and merge relays: for an enforced lane this watcher
// is the only agent-reachable push path, so it is the only place a repo scope
// can stop one.
func TestPushBranchRequestWatcher_RefusesOutOfScopeRepo(t *testing.T) {
	c := pushTestClient(t, "http://127.0.0.1:1")
	c.SetAgentRepoScopeFunc(onlyRepo("schema", "somewhere-else"))
	calls := withPushExec(t, "cafe1234", nil, "")
	dir := withPushDir(t)

	reqPath, err := WritePushBranchRequest(dir, PushBranchRequest{Repo: "o/r", Branch: "schema/fix-1", Dir: t.TempDir(), Agent: "schema"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessPushBranchRequestsOnce(context.Background())

	if len(*calls) != 0 {
		t.Fatalf("%d git calls on an out-of-scope repo, want 0", len(*calls))
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("refused push was not quarantined: %v", err)
	}
	if resp := readPushBranchResult(t, reqPath); resp.OK || !strings.Contains(resp.Error, "not scoped") {
		t.Errorf("result does not explain the scope: %+v", resp)
	}
}

// A failed push is retried a bounded number of times and then quarantined,
// with attempts accumulating across ticks through the result file.
func TestPushBranchRequestWatcher_RetriesThenExhausts(t *testing.T) {
	c := pushTestClient(t, "http://127.0.0.1:1")
	withPushExec(t, "cafe1234", errors.New("exit status 1"), "! [rejected] non-fast-forward")
	dir := withPushDir(t)

	reqPath, err := WritePushBranchRequest(dir, PushBranchRequest{Repo: "o/r", Branch: "scanner/fix-1", Dir: t.TempDir(), Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < pushBranchMaxAttempts; i++ {
		c.ProcessPushBranchRequestsOnce(context.Background())
		if _, err := os.Stat(reqPath); err != nil {
			t.Fatalf("request quarantined after %d attempts, want retry", i)
		}
		if resp := readPushBranchResult(t, reqPath); resp.Attempts != i || resp.OK {
			t.Fatalf("attempt %d result = %+v", i, resp)
		}
	}
	c.ProcessPushBranchRequestsOnce(context.Background())
	if _, err := os.Stat(reqPath + ".exhausted"); err != nil {
		t.Errorf("request not quarantined after max attempts: %v", err)
	}
	if resp := readPushBranchResult(t, reqPath); resp.Attempts != pushBranchMaxAttempts || !strings.Contains(resp.Error, "non-fast-forward") {
		t.Errorf("final result = %+v, want %d attempts and the git output", resp, pushBranchMaxAttempts)
	}
}

func TestValidPushBranchName(t *testing.T) {
	for name, want := range map[string]bool{
		"scanner/fix-9771": true,
		"fix":              true,
		"v5.1-rc.2":        true,
		"":                 false,
		"--force":          false,
		"-x":               false,
		"a..b":             false,
		"a//b":             false,
		"a b":              false,
		"a:b":              false,
		"a/":               false,
		"a.":               false,
		"a.lock":           false,
		"~head":            false,
	} {
		if got := validPushBranchName(name); got != want {
			t.Errorf("validPushBranchName(%q) = %v, want %v", name, got, want)
		}
	}
}

// One agent may not push another agent's (or the hive's own) working tree:
// when per-agent UIDs are in play, the checkout must be owned by the
// requesting agent's UID, the same anchor as the request file itself.
func TestValidatePushBranchDir(t *testing.T) {
	dir := t.TempDir()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner := fileOwnerUID(fi)

	if err := validatePushBranchDir(dir, owner); err != nil {
		t.Errorf("owner's own checkout refused: %v", err)
	}
	if err := validatePushBranchDir(dir, owner+1); err == nil {
		t.Error("a checkout owned by another uid was accepted")
	}
	if err := validatePushBranchDir(dir, 0); err != nil {
		t.Errorf("unverifiable ownership (no per-agent UIDs) must fall back to structural checks: %v", err)
	}
	for _, bad := range []string{"", "relative/path", "/does/not/exist-" + t.Name(), dir + "/../" + "x"} {
		if err := validatePushBranchDir(bad, 0); err == nil {
			t.Errorf("validatePushBranchDir(%q) accepted a bad dir", bad)
		}
	}
}
