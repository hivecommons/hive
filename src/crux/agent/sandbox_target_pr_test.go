package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
)

const (
	targetHeadSHA  = "1111111111111111111111111111111111111111"
	targetMovedSHA = "2222222222222222222222222222222222222222"
	targetNewSHA   = "3333333333333333333333333333333333333333"
	targetBranch   = "hive/dep-bump/20261116120000"
	targetAgent    = "dep-bump"
	targetPRNumber = 7
	targetRepo     = "o/r"
)

// targetRunner is a fake git for kicks on an existing PR. The PR head is
// fetched at targetHeadSHA (or fetchedHead), and after the run HEAD is the
// kick's own commit, targetNewSHA.
type targetRunner struct {
	mu          sync.Mutex
	fetches     []string
	checkouts   []string
	pushArgs    []string
	diff        string
	fetchedHead string
}

func (r *targetRunner) Run(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
	if name != "git" {
		return nil, errors.New("unexpected command")
	}
	effective := stripGitConfigArgs(args)
	joined := strings.Join(effective, " ")
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case len(effective) >= 4 && effective[0] == "clone":
		return nil, os.MkdirAll(filepath.Join(effective[len(effective)-1], ".git"), 0o770)
	case joined == "symbolic-ref --short refs/remotes/origin/HEAD":
		return []byte("origin/main\n"), nil
	case strings.HasPrefix(joined, "fetch origin "):
		r.fetches = append(r.fetches, strings.TrimPrefix(joined, "fetch origin "))
		return nil, nil
	case strings.HasPrefix(joined, "rev-parse refs/remotes/origin/"):
		head := r.fetchedHead
		if head == "" {
			head = targetHeadSHA
		}
		return []byte(head + "\n"), nil
	case strings.HasPrefix(joined, "checkout "):
		r.checkouts = append(r.checkouts, strings.TrimPrefix(joined, "checkout "))
		return nil, nil
	case joined == "rev-parse HEAD":
		return []byte(targetNewSHA + "\n"), nil
	case strings.HasPrefix(joined, "rev-list --count "):
		return []byte("1\n"), nil
	case strings.HasPrefix(joined, "diff --no-renames --name-only -z "):
		return []byte("fix.go\x00"), nil
	case strings.HasPrefix(joined, "diff --text "):
		return []byte(r.diff), nil
	case len(effective) > 0 && effective[0] == "push":
		r.pushArgs = append([]string(nil), effective...)
		return []byte("pushed\n"), nil
	default:
		return nil, nil
	}
}

func (r *targetRunner) pushed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pushArgs != nil
}

// fakeTargetReader answers PRPushTarget reads in order; the last answer
// repeats.
type fakeTargetReader struct {
	mu      sync.Mutex
	answers []crux.PRPushTarget
	err     error
	reads   int
}

func (f *fakeTargetReader) PRPushTarget(_ context.Context, repo string, n int) (crux.PRPushTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return crux.PRPushTarget{}, f.err
	}
	if repo != targetRepo || n != targetPRNumber {
		return crux.PRPushTarget{}, errors.New("unexpected target " + repo)
	}
	i := f.reads - 1
	if i >= len(f.answers) {
		i = len(f.answers) - 1
	}
	return f.answers[i], nil
}

func ownTargetPR() crux.PRPushTarget {
	return crux.PRPushTarget{
		Number: targetPRNumber, URL: "https://github.com/o/r/pull/7", State: "open",
		BaseRef: "main", HeadRef: targetBranch, HeadSHA: targetHeadSHA, HeadRepo: targetRepo,
		Author: "hive-app[bot]", AuthorAgent: targetAgent,
	}
}

// countingPR is a PR client that is NOT an AgentPRCreator.
type countingPR struct {
	mu    sync.Mutex
	calls int
}

func (p *countingPR) CreatePR(context.Context, string, string, string, string, string) (crux.PRResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return crux.PRResult{Number: 42, URL: "https://github.com/o/r/pull/42", Author: "hive-app[bot]"}, nil
}

func (p *countingPR) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func runTargetKick(t *testing.T, runner *targetRunner, reader crux.PRPushTargetReader) (SandboxKickResult, *countingPR, error) {
	t.Helper()
	pr := &countingPR{}
	exec := &SandboxExecutor{
		Runner:      runner,
		Launcher:    sandboxFakeLauncher{},
		Minter:      sandboxFakeMinter{},
		PushEnabled: true,
		PRClient:    pr,
		Logger:      discardLogger(),
	}
	if reader != nil {
		exec.PRTargetReader = reader
	}
	res, err := exec.Run(context.Background(), SandboxKickSpec{
		Agent: targetAgent, AgentConfig: configSnapshot{Backend: "claude"}, Message: "fix the red PR",
		Org: "o", Repo: "r", WorkspaceDir: t.TempDir(), Image: "img",
		TargetPR: targetPRNumber, TargetPRSource: sandboxTargetSourceFixFirst,
	})
	return res, pr, err
}

func TestSandboxTargetPRPushesToTheExistingBranch(t *testing.T) {
	runner := &targetRunner{}
	reader := &fakeTargetReader{answers: []crux.PRPushTarget{ownTargetPR()}}
	res, pr, err := runTargetKick(t, runner, reader)
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	if !runner.pushed() {
		t.Fatal("the kick's commits were not pushed")
	}
	joined := strings.Join(runner.pushArgs, " ")
	for _, want := range []string{"--force-with-lease=refs/heads/" + targetBranch + ":" + targetHeadSHA, "HEAD:refs/heads/" + targetBranch} {
		if !strings.Contains(joined, want) {
			t.Fatalf("push %q lacks %q", joined, want)
		}
	}
	if pr.calls != 0 {
		t.Fatalf("a push to an existing PR opened %d new PR(s)", pr.calls)
	}
	if res.TargetPR != targetPRNumber || res.PR == nil || res.PR.Number != targetPRNumber || !res.PR.AlreadyExisted || res.PR.URL != "https://github.com/o/r/pull/7" {
		t.Fatalf("result does not name the target PR: %+v pr=%+v", res, res.PR)
	}
	if res.Branch != targetBranch || res.BaseSHA != targetHeadSHA || res.CommitCount != 1 {
		t.Fatalf("branch=%s base=%s commits=%d", res.Branch, res.BaseSHA, res.CommitCount)
	}
	if res.Broker == nil || res.Broker.Lease != targetHeadSHA {
		t.Fatalf("broker result lacks the lease: %+v", res.Broker)
	}
	if reader.reads != 2 {
		t.Fatalf("target read %d times, want before checkout and before push", reader.reads)
	}
	wantFetch := "+refs/heads/" + targetBranch + ":refs/remotes/origin/" + targetBranch
	found := false
	for _, f := range runner.fetches {
		found = found || f == wantFetch
	}
	if !found {
		t.Fatalf("PR head not fetched: %v", runner.fetches)
	}
	if len(runner.checkouts) != 1 || runner.checkouts[0] != "-B "+targetBranch+" refs/remotes/origin/"+targetBranch {
		t.Fatalf("checkouts = %v", runner.checkouts)
	}
	prompt, err := os.ReadFile(filepath.Join(res.Workspace, sandboxPromptRelPath))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(prompt), sandboxTargetPromptMarker) || !strings.Contains(string(prompt), "o/r#7") || !strings.HasSuffix(string(prompt), "fix the red PR") {
		t.Fatalf("prompt = %q", prompt)
	}
}

func TestSandboxTargetPRRefusals(t *testing.T) {
	fork := ownTargetPR()
	fork.Fork, fork.HeadRepo = true, "forker/r"
	other := ownTargetPR()
	other.AuthorAgent = "scanner"
	unowned := ownTargetPR()
	unowned.AuthorAgent = ""
	closed := ownTargetPR()
	closed.State = "closed"
	badRef := ownTargetPR()
	badRef.HeadRef = "--upload-pack=evil"
	wrongNumber := ownTargetPR()
	wrongNumber.Number = 8
	moved := ownTargetPR()
	moved.HeadSHA = targetMovedSHA
	renamed := ownTargetPR()
	renamed.HeadRef = "other-branch"
	closedLater := ownTargetPR()
	closedLater.State = "closed"

	cases := []struct {
		name    string
		answers []crux.PRPushTarget
		readErr error
		runner  *targetRunner
		noRead  bool
		want    string
		// prep refusals stop the kick before the sandbox runs, so there
		// are no commits; push-time refusals withhold the run's commits.
		atPush bool
	}{
		{name: "fork", answers: []crux.PRPushTarget{fork}, want: "is a fork PR"},
		{name: "other agent", answers: []crux.PRPushTarget{other}, want: "opened by agent scanner, not dep-bump"},
		{name: "not hive-opened", answers: []crux.PRPushTarget{unowned}, want: "not opened by this hive"},
		{name: "closed", answers: []crux.PRPushTarget{closed}, want: "is closed, not open"},
		{name: "bad head ref", answers: []crux.PRPushTarget{badRef}, want: "will not check out"},
		{name: "wrong number", answers: []crux.PRPushTarget{wrongNumber}, want: "GitHub returned PR #8"},
		{name: "read error", readErr: errors.New("boom"), want: "reading o/r#7: boom"},
		{name: "no reader", noRead: true, want: "cannot read o/r#7"},
		{name: "fetch race", answers: []crux.PRPushTarget{ownTargetPR()}, runner: &targetRunner{fetchedHead: targetMovedSHA}, want: "moved while hive checked it out"},
		{name: "moved head", answers: []crux.PRPushTarget{ownTargetPR(), moved}, want: "head moved since checkout", atPush: true},
		{name: "renamed head", answers: []crux.PRPushTarget{ownTargetPR(), renamed}, want: "head branch changed", atPush: true},
		{name: "closed during run", answers: []crux.PRPushTarget{ownTargetPR(), closedLater}, want: "is closed, not open; nothing pushed", atPush: true},
		{name: "unreadable before push", answers: []crux.PRPushTarget{ownTargetPR()}, want: "re-reading o/r#7", atPush: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := tc.runner
			if runner == nil {
				runner = &targetRunner{}
			}
			var reader crux.PRPushTargetReader
			if !tc.noRead {
				fr := &fakeTargetReader{answers: tc.answers, err: tc.readErr}
				if tc.name == "unreadable before push" {
					reader = &failSecondRead{fakeTargetReader: fr}
				} else {
					reader = fr
				}
			}
			res, pr, err := runTargetKick(t, runner, reader)
			if !errors.Is(err, ErrSandboxTargetPRRefused) {
				t.Fatalf("err = %v, want ErrSandboxTargetPRRefused", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to say %q", err, tc.want)
			}
			if runner.pushed() || pr.calls != 0 {
				t.Fatalf("refused kick pushed=%v opened=%d", runner.pushed(), pr.calls)
			}
			if tc.atPush && (res.CommitCount != 1 || res.PRWithheld == "") {
				t.Fatalf("push-time refusal did not withhold the commits: %+v", res)
			}
			if !tc.atPush && len(runner.checkouts) != 0 && tc.name != "fetch race" {
				t.Fatalf("refused target was checked out: %v", runner.checkouts)
			}
		})
	}
}

// failSecondRead fails every read after the first.
type failSecondRead struct{ *fakeTargetReader }

func (f *failSecondRead) PRPushTarget(ctx context.Context, repo string, n int) (crux.PRPushTarget, error) {
	if f.reads >= 1 {
		f.reads++
		return crux.PRPushTarget{}, errors.New("rate limited")
	}
	return f.fakeTargetReader.PRPushTarget(ctx, repo, n)
}

func TestSandboxTargetPRScanFailurePushesNothing(t *testing.T) {
	runner := &targetRunner{diff: "diff --git a/fix.go b/fix.go\n+token=ghp_abcdefghijklmnopqrstuvwxyz\n"}
	reader := &fakeTargetReader{answers: []crux.PRPushTarget{ownTargetPR()}}
	res, pr, err := runTargetKick(t, runner, reader)
	if err == nil {
		t.Fatal("a secret in the outgoing diff must fail the kick")
	}
	if res.Broker == nil || !res.Broker.SecretRejected || res.Broker.Pushed {
		t.Fatalf("broker result = %+v", res.Broker)
	}
	if runner.pushed() || pr.calls != 0 {
		t.Fatalf("scan failure pushed=%v opened=%d", runner.pushed(), pr.calls)
	}
}

func TestSandboxTargetPRRunStageRefused(t *testing.T) {
	exec := &SandboxExecutor{Runner: &targetRunner{}, Launcher: sandboxFakeLauncher{}}
	_, err := exec.Run(context.Background(), SandboxKickSpec{
		Agent: targetAgent, Org: "o", Repo: "r", WorkspaceDir: t.TempDir(), Image: "img",
		Stage: "build", RunKey: "k", Generation: 1, TargetPR: targetPRNumber,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot push to an existing PR") {
		t.Fatalf("err = %v", err)
	}
}

func TestSandboxTargetPRFromKickMessage(t *testing.T) {
	fixFirst := "## 🔴 FIX-BEFORE-NEW — your open PRs with failing CI or merge conflicts (2)\n\n" +
		"These PRs are YOURS and they are red or conflicted.\n" +
		"  gh pr checkout <number> → fix using the evidence below → commit -s → git push\n\n" +
		"  #12 o/r — Contributor change\n" +
		"    ADOPTED contributor PR (stage 074, head repo c/r): the author is offline\n" +
		"  #7 r [topic: torch] — Bump torch\n" +
		"    failing: build\n\n"
	topics := "## 📌 Topic claims (one PR per topic)\n\n" +
		"You own these topics.\n" +
		"  torch → o/r#9 (branch hive/dep-bump/20261101000000)\n" +
		"Claim a new topic right after opening its PR.\n"
	cases := []struct {
		name, msg  string
		wantN      int
		wantSource string
	}{
		{"explicit", "[hive:target-pr o/r#5]\nfix it\n" + fixFirst, 5, sandboxTargetSourceExplicit},
		{"explicit after agent header", "[agent:dep-bump]\n[hive:target-pr o/r#5]\nfix it", 5, sandboxTargetSourceExplicit},
		{"explicit other repo falls through", "[hive:target-pr x/y#5]\n" + fixFirst, 7, sandboxTargetSourceFixFirst},
		{"red PR kick", "🔴 FIX-BEFORE-NEW — o/r#11 is red and has not moved\n\nRepair it", 11, sandboxTargetSourceRedPRKick},
		{"CI failure kick", "CI failed on o/r#13 (build) — repair it now. Detection was out-of-band\n" + fixFirst, 13, sandboxTargetSourceCIFailure},
		{"fix-before-new skips adopted", "Kick.\n\n" + fixFirst, 7, sandboxTargetSourceFixFirst},
		{"fix-before-new beats topic", topics + "\n" + fixFirst, 7, sandboxTargetSourceFixFirst},
		{"single topic claim", "Kick.\n\n" + topics, 9, sandboxTargetSourceTopicClaim},
		{"two topic claims", "Kick.\n\n" + topics + "  numpy → o/r#10\n", 0, ""},
		{"same topic PR twice", "Kick.\n\n" + topics + "  torch2 → o/r#9\n", 9, sandboxTargetSourceTopicClaim},
		{"topic in another repo", "Kick.\n\n## 📌 Topic claims (one PR per topic)\n  torch → x/y#9\n", 0, ""},
		{"quoted header mid-message is ignored", "Kick.\n🔴 FIX-BEFORE-NEW — o/r#11 is red and has not moved\n", 0, ""},
		{"row outside its section is ignored", "Kick.\n  #7 o/r — Bump torch\n", 0, ""},
		{"plain kick", "Do your cadence work.", 0, ""},
		{"empty", "", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, source := sandboxTargetPRFromKickMessage(tc.msg, "o/r")
			if n != tc.wantN || source != tc.wantSource {
				t.Fatalf("got (%d, %q), want (%d, %q)", n, source, tc.wantN, tc.wantSource)
			}
		})
	}
}

func TestValidSandboxTargetBranch(t *testing.T) {
	for _, ok := range []string{"main", "hive/dep-bump/20261116120000", "feature/x-1.2"} {
		if !validSandboxTargetBranch(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", " x", "-x", "/x", "x/", "x.lock", "x.", "a..b", "a//b", "a@{1}", "a b", "a~1", "a^", "a:b", "a?", "a*", "a[", `a\b`, "a\x7f"} {
		if validSandboxTargetBranch(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestSandboxTargetRefusalUnknownStateAndRepo(t *testing.T) {
	t1 := ownTargetPR()
	t1.State = ""
	if r := sandboxTargetRefusal(t1, targetRepo, targetAgent, targetPRNumber); !strings.Contains(r, "in an unknown state") {
		t.Fatalf("refusal = %q", r)
	}
	t2 := ownTargetPR()
	t2.HeadRepo = ""
	if r := sandboxTargetRefusal(t2, targetRepo, targetAgent, targetPRNumber); !strings.Contains(r, "an unknown repository") {
		t.Fatalf("refusal = %q", r)
	}
	if r := sandboxTargetRefusal(ownTargetPR(), targetRepo, "DEP-BUMP", targetPRNumber); r != "" {
		t.Fatalf("agent names compare case-insensitively: %q", r)
	}
	if got := shortCommit("abc"); got != "abc" {
		t.Fatalf("shortCommit = %q", got)
	}
}

// agentPRClient is a PR client that credits the PRs it opens itself.
type agentPRClient struct {
	countingPR
	agent string
}

func (p *agentPRClient) CreateAgentPR(_ context.Context, agent, _, _, _, _, _ string) (crux.PRResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.agent = agent
	return crux.PRResult{Number: 42, URL: "https://github.com/o/r/pull/42"}, nil
}

func TestSandboxExecutorCreditsNewPRThroughAgentPRCreator(t *testing.T) {
	runner := &sandboxFakeRunner{commits: 1}
	pr := &agentPRClient{}
	exec := &SandboxExecutor{Runner: runner, Launcher: sandboxFakeLauncher{}, Minter: sandboxFakeMinter{}, PushEnabled: true, PRClient: pr}
	res, err := exec.Run(context.Background(), SandboxKickSpec{
		Agent: targetAgent, AgentConfig: configSnapshot{Backend: "claude"}, Message: "fix",
		Org: "o", Repo: "r", WorkspaceDir: t.TempDir(), Image: "img",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if pr.agent != targetAgent || pr.calls != 0 || !res.PRAttributed || res.PR == nil || res.PR.Number != 42 {
		t.Fatalf("agent=%q CreatePR calls=%d attributed=%v pr=%+v", pr.agent, pr.calls, res.PRAttributed, res.PR)
	}
	if !strings.HasPrefix(res.Branch, "hive/dep-bump/") {
		t.Fatalf("work branch = %q", res.Branch)
	}
}

type sandboxAuditLine struct{ agent, action, detail string }

func newSandboxTargetManager(t *testing.T, client PRCreator, runner sandboxCommandRunner) (*Manager, *[]sandboxAuditLine, *sync.Mutex) {
	t.Helper()
	yes := true
	cfg := map[string]config.AgentConfig{
		targetAgent: {Backend: "claude", Model: "sonnet", Sandbox: &config.AgentSandboxOverride{Enabled: &yes}},
	}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "o", Repos: []string{"r"}, ACMMLevel: 5, PRsAllowed: true})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "img", WorkspaceDir: t.TempDir()})
	m.SetSandboxLauncher(sandboxFakeLauncher{})
	m.setSandboxRunnerForTest(runner)
	m.SetSandboxPushMinter(sandboxFakeMinter{})
	m.SetSandboxPRClient(client)
	var mu sync.Mutex
	var lines []sandboxAuditLine
	m.SetSandboxAuditCallback(func(agent, action, detail string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, sandboxAuditLine{agent, action, detail})
	})
	if err := m.Start(context.Background(), targetAgent); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return m, &lines, &mu
}

func waitSandboxAudit(t *testing.T, lines *[]sandboxAuditLine, mu *sync.Mutex, action string) sandboxAuditLine {
	t.Helper()
	var got sandboxAuditLine
	waitFor(t, "sandbox audit "+action, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range *lines {
			if l.action == action {
				got = l
				return true
			}
		}
		return false
	})
	return got
}

// A PR client that cannot credit the PR itself (the sidecar's callback
// adapter) still gets the PR credited: the manager records agent_pr_created
// through the sandbox audit callback, in the detail shape every governor
// resolver parses (hivecommons/hive#18575).
func TestManagerSandboxCreditsPRWhenClientCannot(t *testing.T) {
	m, lines, mu := newSandboxTargetManager(t, &countingPR{}, &sandboxFakeRunner{commits: 1})
	if err := m.SendKick(targetAgent, "fix"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	line := waitSandboxAudit(t, lines, mu, crux.AuditActionAgentPRCreated)
	if line.agent != targetAgent {
		t.Fatalf("credited to %q", line.agent)
	}
	for _, want := range []string{"repo=o/r", "number=42", "url=https://github.com/o/r/pull/42", "reused=false", "via=sandbox", "agent=dep-bump", "backend=claude", "model=sonnet", "head=hive/dep-bump/"} {
		if !strings.Contains(line.detail, want) {
			t.Fatalf("detail %q lacks %q", line.detail, want)
		}
	}
}

func TestManagerSandboxDoesNotDoubleCreditAttributedPR(t *testing.T) {
	m, lines, mu := newSandboxTargetManager(t, &agentPRClient{}, &sandboxFakeRunner{commits: 1})
	if err := m.SendKick(targetAgent, "fix"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	waitSandboxAudit(t, lines, mu, "sandbox_complete")
	mu.Lock()
	defer mu.Unlock()
	for _, l := range *lines {
		if l.action == crux.AuditActionAgentPRCreated {
			t.Fatalf("PR credited twice: %+v", l)
		}
	}
}

// targetPRClient opens PRs and reads push targets, like the hive's client.
type targetPRClient struct {
	countingPR
	fakeTargetReader
}

func TestManagerSandboxResolvesTargetPRFromKick(t *testing.T) {
	client := &targetPRClient{fakeTargetReader: fakeTargetReader{answers: []crux.PRPushTarget{ownTargetPR()}}}
	runner := &targetRunner{}
	m, lines, mu := newSandboxTargetManager(t, client, runner)
	if err := m.SendKick(targetAgent, "🔴 FIX-BEFORE-NEW — o/r#7 is red and has not moved\n\nRepair it"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	line := waitSandboxAudit(t, lines, mu, "sandbox_complete")
	if !strings.Contains(line.detail, "target_pr=7") {
		t.Fatalf("sandbox_complete = %q", line.detail)
	}
	if !runner.pushed() || client.count() != 0 {
		t.Fatalf("pushed=%v opened=%d", runner.pushed(), client.count())
	}
	mu.Lock()
	defer mu.Unlock()
	for _, l := range *lines {
		if l.action == crux.AuditActionAgentPRCreated {
			t.Fatalf("a push to an existing PR re-credited it: %+v", l)
		}
	}
}

func TestManagerSandboxAuditsTargetRefusal(t *testing.T) {
	other := ownTargetPR()
	other.AuthorAgent = "scanner"
	client := &targetPRClient{fakeTargetReader: fakeTargetReader{answers: []crux.PRPushTarget{other}}}
	runner := &targetRunner{}
	m, lines, mu := newSandboxTargetManager(t, client, runner)
	if err := m.SendKick(targetAgent, "[hive:target-pr o/r#7]\nfix it"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	line := waitSandboxAudit(t, lines, mu, "sandbox_target_pr_refused")
	for _, want := range []string{"repo=o/r", "number=7", "source=explicit", "opened by agent scanner"} {
		if !strings.Contains(line.detail, want) {
			t.Fatalf("detail %q lacks %q", line.detail, want)
		}
	}
	if runner.pushed() {
		t.Fatal("refused target pushed")
	}
}

// A kick that names a PR through a client that cannot read targets opens a
// new PR, as before the feature.
func TestManagerSandboxTargetWithoutReaderOpensNewPR(t *testing.T) {
	pr := &countingPR{}
	m, lines, mu := newSandboxTargetManager(t, pr, &sandboxFakeRunner{commits: 1})
	if err := m.SendKick(targetAgent, "[hive:target-pr o/r#7]\nfix it"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	waitSandboxAudit(t, lines, mu, "sandbox_complete")
	if got := pr.count(); got != 1 {
		t.Fatalf("CreatePR calls = %d", got)
	}
}
