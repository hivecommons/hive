package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strconv"
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
// the `push` invocation with output pushOut. Every other command (the sign-off
// log, the config listing) succeeds with no output.
func withPushExec(t *testing.T, sha string, pushErr error, pushOut string) *[]pushExecCall {
	t.Helper()
	var calls []pushExecCall
	old := pushBranchExec
	commonDir := fakeCommonGitDir(t)
	pushBranchExec = func(_ context.Context, dir string, env []string, name string, args ...string) (string, error) {
		calls = append(calls, pushExecCall{dir: dir, env: env, args: append([]string{name}, args...)})
		if len(args) > 0 && args[0] == "rev-parse" {
			if strings.Contains(strings.Join(args, " "), "--git-common-dir") {
				return commonDir + "\nsha1\n", nil
			}
			return sha + "\n", nil
		}
		if pushErr != nil && len(args) > 0 && args[0] == "push" {
			return pushOut, pushErr
		}
		return "", nil
	}
	t.Cleanup(func() { pushBranchExec = old })
	return &calls
}

// fakeCommonGitDir is a directory shaped like a checkout's common git dir
// (it has an objects store), for the stub rev-parse to report.
func fakeCommonGitDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(dir+"/objects", 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
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
	if len(*calls) != 5 {
		t.Fatalf("git calls = %d, want rev-parse + sign-off log + config listing + git dir + push", len(*calls))
	}
	push := (*calls)[4]
	joined := strings.Join(push.args, " ")
	if push.dir == checkout || !strings.Contains(strings.Join(push.env, "\n"), "GIT_DIR="+push.dir) {
		t.Errorf("push ran in %q with env %v, want a hive-owned GIT_DIR, never the agent's checkout", push.dir, push.env)
	}
	if !strings.Contains(joined, "https://github.com/o/r.git") ||
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

	if len(*calls) != 5 {
		t.Fatalf("git calls = %d, want 5", len(*calls))
	}
	if joined := strings.Join((*calls)[4].args, " "); !strings.Contains(joined, "--force-with-lease") || strings.Contains(joined, " --force ") {
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

// TestPushBranchGitEnv_NeutralizesRepoLocalExecution verifies the env pairs
// override every repo-local channel that could make git run a program from
// the agent-owned checkout (hooks, fsmonitor, credential helpers), and that
// the auth header rides as an extraHeader on the exact remote.
func TestPushBranchGitEnv_NeutralizesRepoLocalExecution(t *testing.T) {
	dir := "/data/agents/lane/repo"
	remote := "https://github.com/acme/widgets.git"
	env := pushBranchGitEnv(dir, remote, "AUTHORIZATION: basic abc")

	got := map[string]string{}
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			got[k] = v
		}
	}
	if got["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT = %q, want 0", got["GIT_TERMINAL_PROMPT"])
	}
	want := map[string]string{
		"safe.directory":    dir,
		"core.hooksPath":    os.DevNull,
		"core.fsmonitor":    "false",
		"credential.helper": "",
		"http." + strings.TrimRight(remote, "/") + ".extraHeader": "AUTHORIZATION: basic abc",
		// hivecommons/hive#19959: where the credentialed request goes.
		"core.askPass":           "",
		"http.sslVerify":         "true",
		"http.followRedirects":   "initial",
		"push.recurseSubmodules": "no",
		"push.gpgSign":           "false",
		"log.showSignature":      "false",
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_SYSTEM":   os.DevNull,
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_ASKPASS":         "",
		"SSH_ASKPASS":         "",
		"GIT_ALLOW_PROTOCOL":  "https",
		"GIT_NO_LAZY_FETCH":   "1",
	} {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["GIT_ASKPASS"]; !ok {
		t.Error("GIT_ASKPASS is not overridden, so an inherited askpass program would run")
	}
	n, err := strconv.Atoi(got["GIT_CONFIG_COUNT"])
	if err != nil || n != len(want) {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want %d", got["GIT_CONFIG_COUNT"], len(want))
	}
	pairs := map[string]string{}
	for i := 0; i < n; i++ {
		pairs[got["GIT_CONFIG_KEY_"+strconv.Itoa(i)]] = got["GIT_CONFIG_VALUE_"+strconv.Itoa(i)]
	}
	for k, v := range want {
		if pairs[k] != v {
			t.Errorf("config %q = %q, want %q", k, pairs[k], v)
		}
	}
}

// TestPushBranchGitEnv_NoHeader verifies the extraHeader pair is omitted when
// no credential is available while the neutralizing pairs remain.
func TestPushBranchGitEnv_NoHeader(t *testing.T) {
	env := pushBranchGitEnv("/d", "https://github.com/a/b.git", "")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "extraHeader") {
		t.Errorf("env contains extraHeader without a credential: %q", joined)
	}
	if !strings.Contains(joined, "core.hooksPath") || !strings.Contains(joined, os.DevNull) {
		t.Errorf("env missing core.hooksPath override: %q", joined)
	}
}

// A branch carrying a hive-authored commit without a Signed-off-by trailer
// is refused before anything reaches GitHub (hivecommons/hive#16619); the
// same commit by an outside contributor is the PR DCO check's business.
func TestPushBranchRequestWatcher_RefusesUnsignedHiveCommit(t *testing.T) {
	c := pushTestClient(t, "http://127.0.0.1:1")
	c.SetHiveIdentity(HiveIdentity{AIAuthor: "thatguywob", AppLogin: "hivecommons-hive[bot]"})
	dir := withPushDir(t)
	var calls []pushExecCall
	old := pushBranchExec
	pushBranchExec = func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		calls = append(calls, pushExecCall{args: append([]string{name}, args...)})
		switch args[0] {
		case "rev-parse":
			return "cafe1234\n", nil
		case "log":
			return "cafe1234\x1fthatguywob\x1fthatguywob@users.noreply.github.com\x1ffix: thing\n\nbody\n\x1e" +
				"beef5678\x1fscanner\x1f280983584+hivecommons-hive[bot]@users.noreply.github.com\x1ffix: other\n\nSigned-off-by: scanner <x@y>\n\x1e", nil
		}
		return "", nil
	}
	t.Cleanup(func() { pushBranchExec = old })

	reqPath, err := WritePushBranchRequest(dir, PushBranchRequest{Repo: "o/r", Branch: "scanner/fix-3", Dir: t.TempDir(), Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessPushBranchRequestsOnce(context.Background())

	resp := readPushBranchResult(t, reqPath)
	if resp.OK || !strings.Contains(resp.Error, "cafe1234") || !strings.Contains(resp.Error, "thatguywob") || !strings.Contains(resp.Error, "Signed-off-by") {
		t.Errorf("result = %+v, want a denial naming the unsigned hive commit", resp)
	}
	for _, call := range calls {
		if call.args[1] == "push" {
			t.Fatalf("unsigned hive commit was pushed: %v", call.args)
		}
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("denied request not parked: %v", err)
	}
}

func TestUnsignedHiveCommits(t *testing.T) {
	logins := []string{"hivecommons-hive[bot]", "thatguywob"}
	log := "a1\x1fthatguywob\x1fthatguywob@users.noreply.github.com\x1fsubject\n\x1e" +
		"a2\x1fscanner\x1f280983584+hivecommons-hive[bot]@users.noreply.github.com\x1fsubject\n\x1e" +
		"a3\x1fhivecommons-hive[bot]\x1fhivecommons-hive@hive.kubestellar.io\x1fsubject\n\nSigned-off-by: hivecommons-hive[bot] <hivecommons-hive@hive.kubestellar.io>\n\x1e" +
		"a4\x1fMallory\x1fmallory@example.com\x1fsubject\n\x1e"
	got := unsignedHiveCommits(log, HiveCommitIdentities{Logins: logins})
	if len(got) != 2 || got[0].SHA != "a1" || got[0].Author != "thatguywob" || got[1].SHA != "a2" || got[1].Author != "hivecommons-hive[bot]" {
		t.Errorf("unsignedHiveCommits = %+v, want a1 (ai_author) and a2 (App by noreply address) only", got)
	}
	if got := unsignedHiveCommits("", HiveCommitIdentities{Logins: logins}); len(got) != 0 {
		t.Errorf("empty log flagged %+v", got)
	}
}

// The forge push relay reuses the sign-off check with its own identities
// (hivecommons/hive#19960): a lane's commit address at the bot email domain is
// a hive identity, whatever the author name; anyone else is not checked.
func TestHiveCommitSignoffRefusalEmailDomains(t *testing.T) {
	ids := HiveCommitIdentities{Logins: []string{"hive-bot"}, EmailDomains: []string{" Bots.Example "}}
	unsigned := "b1\x1fsomeone\x1fworker@bots.example\x1fsubject\n\x1e"
	got := HiveCommitSignoffRefusal(unsigned, ids)
	want := `commit b1 is authored by hive identity "worker@bots.example" without a Signed-off-by trailer; hive-authored commits must be signed off at creation (git commit -s)`
	if got != want {
		t.Errorf("refusal = %q, want %q", got, want)
	}
	for name, log := range map[string]string{
		"signed":       "b2\x1fworker\x1fworker@bots.example\x1fsubject\n\nSigned-off-by: worker <worker@bots.example>\n\x1e",
		"other domain": "b3\x1fworker\x1fworker@bots.example.org\x1fsubject\n\x1e",
		"no address":   "b4\x1fworker\x1fworker\x1fsubject\n\x1e",
		"empty":        "",
	} {
		if got := HiveCommitSignoffRefusal(log, ids); got != "" {
			t.Errorf("%s: refusal = %q, want none", name, got)
		}
	}
	if got := HiveCommitSignoffRefusal(unsigned, HiveCommitIdentities{EmailDomains: []string{""}}); got != "" {
		t.Errorf("an empty domain matched: %q", got)
	}
}

func TestUnsignedHiveCommitLogArgs(t *testing.T) {
	got := strings.Join(UnsignedHiveCommitLogArgs("abc123"), " ")
	want := "log --no-merges --format=" + unsignedHiveCommitFormat + " abc123 --not --remotes"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

// The agent_branch_pushed entry both push relays record (#19960).
func TestAgentBranchPushedAuditRecord(t *testing.T) {
	rec := AgentBranchPushedAuditRecord("worker", "acme/widgets", "fix-1", "abc123", true)
	if rec.Action != AuditActionAgentBranchPushed || rec.Agent != "worker" || rec.Repo != "acme/widgets" || rec.Target != 0 {
		t.Errorf("record = %+v", rec)
	}
	for _, part := range []string{"repo=acme/widgets", "branch=fix-1", "sha=abc123", "force_with_lease=true"} {
		if !strings.Contains(rec.Detail, part) {
			t.Errorf("detail %q missing %q", rec.Detail, part)
		}
	}
}
