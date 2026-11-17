package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/agent/panesignal"
	"github.com/hivecommons/hive/crux/config"
)

// claudeFirstRunNoticePane is Claude Code's first-run security notice as the
// research-1 pane rendered it (hive-research-1.jsonl), one line per row.
const claudeFirstRunNoticePane = ` Security notes:

 1. Claude can make mistakes
    You should always review Claude's responses, especially when
    running code.

 2. Due to prompt injection risks, only use it with code you trust
    For more details see:
    https://code.claude.com/docs/en/security

 Press Enter to continue…`

// claudeFirstRunNoticeFlatPane is the same screen exactly as quoted from the
// pane log, flattened onto one line.
const claudeFirstRunNoticeFlatPane = "1. Claude can make mistakes… 2. Due to prompt injection risks, only use it with code you trust… Press Enter to continue…"

// firstRunIdlePane is the Claude Code input prompt the notice gives way to.
const firstRunIdlePane = "────────\n❯ \n────────\n  ? for shortcuts"

func TestClaudeFirstRunNotice_Recognised(t *testing.T) {
	for name, pane := range map[string]string{"rendered": claudeFirstRunNoticePane, "flat": claudeFirstRunNoticeFlatPane} {
		t.Run(name, func(t *testing.T) {
			if !panesignal.ShowsClaudeFirstRunNotice(pane) {
				t.Fatal("first-run security notice not recognised")
			}
			key, label, ok := blockingPromptKey("claude", pane)
			if !ok || label != claudeFirstRunNoticeLabel {
				t.Fatalf("blockingPromptKey = (%q, %q, %v), want the first-run notice", key, label, ok)
			}
			if key != "" {
				t.Errorf("key = %q, want \"\" (Enter alone continues)", key)
			}
			if !paneShowsConsentScreen(pane) {
				t.Error("the notice must not read as a ready input prompt")
			}
			if title, ok := claudePermissionDialog(pane); ok {
				t.Errorf("notice misread as a permission dialog %q (option 1 would be typed into it)", title)
			}
		})
	}
}

func TestClaudeFirstRunNotice_NotMatchedOnPartialOrScrolledAway(t *testing.T) {
	cases := map[string]string{
		// After Enter the CLI draws its prompt below the notice: lingering
		// scrollback must not hold readiness or re-fire Enter.
		"scrolled away": claudeFirstRunNoticePane + "\n\n" + firstRunIdlePane,
		// One distinctive phrase plus the footer is not enough.
		"no injection line": " 1. Claude can make mistakes\n\n Press Enter to continue…",
		"no mistakes line":  " 2. Due to prompt injection risks, only use it with code you trust\n\n Press Enter to continue…",
		"no footer":         " 1. Claude can make mistakes\n 2. Due to prompt injection risks, only use it with code you trust",
	}
	for name, pane := range cases {
		t.Run(name, func(t *testing.T) {
			if panesignal.ShowsClaudeFirstRunNotice(pane) {
				t.Errorf("matched a pane that is not a live first-run notice:\n%s", pane)
			}
		})
	}
}

// The notice's "Due to prompt injection risks" line contains the
// "prompt injection" kick-refusal pattern; it is CLI text, not a refusal.
func TestCheckKickRefusal_IgnoresFirstRunNotice(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"research-1": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	agent := m.agents["research-1"]
	for _, line := range strings.Split(claudeFirstRunNoticePane, "\n") {
		m.checkKickRefusal(agent, line)
	}
	if agent.KickRefused {
		t.Errorf("first-run notice read as a kick refusal: %q", agent.KickRefusalReason)
	}
}

// firstRunScript is a scripted pane: the first-run notice until Enter, then
// the idle input prompt. stuck keeps the notice up whatever is pressed.
type firstRunScript struct {
	mu    sync.Mutex
	stuck bool
	keys  []string
}

func (s *firstRunScript) capture(*AgentProcess) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stuck || !slices.Contains(s.keys, "Enter") {
		return claudeFirstRunNoticePane
	}
	return firstRunIdlePane
}

func (s *firstRunScript) sendKeys(_ *AgentProcess, keys ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, keys...)
}

func (s *firstRunScript) keySnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.keys)
}

func newFirstRunHarness(t *testing.T, stuck bool, logger *slog.Logger) (*Manager, *AgentProcess, *firstRunScript) {
	t.Helper()
	if logger == nil {
		logger = discardLogger()
	}
	m := NewManager(map[string]config.AgentConfig{"research-1": {Backend: "claude"}}, logger, ProjectContext{})
	m.mu.Lock()
	agent := m.agents["research-1"]
	m.mu.Unlock()
	script := &firstRunScript{stuck: stuck}
	ft := termSeams(m)
	ft.captureVisiblePane = script.capture
	ft.capturePane = script.capture
	ft.sendKeys = script.sendKeys
	ft.sleepDuringPromptDismiss = func(time.Duration) {}
	return m, agent, script
}

func shrinkInputPromptWait(t *testing.T, timeout time.Duration) {
	t.Helper()
	origTimeout, origPoll := inputPromptTimeout, inputPromptPollInterval
	inputPromptTimeout = timeout
	inputPromptPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { inputPromptTimeout, inputPromptPollInterval = origTimeout, origPoll })
}

// A kick's readiness wait answers the notice with Enter and then finds the
// prompt, instead of waiting out inputPromptTimeout.
func TestWaitForInputPrompt_FirstRunNoticeDismissedWithEnter(t *testing.T) {
	shrinkInputPromptWait(t, 5*time.Second)
	m, agent, script := newFirstRunHarness(t, false, nil)

	ready, reason := m.waitForInputPromptForAgentReason(agent, nil)
	if !ready {
		t.Fatalf("wait did not reach the prompt after the notice: %q", reason)
	}
	if got := script.keySnapshot(); !slices.Equal(got, []string{"Enter"}) {
		t.Errorf("keys = %v, want exactly one Enter", got)
	}
}

// A notice that survives its Enters fails the kick at once, naming the screen
// and keeping the "did not reach input prompt" text pkg/research retries on.
func TestWaitForInputPrompt_FirstRunNoticeStuckFailsFast(t *testing.T) {
	const generousTimeout = 30 * time.Second
	shrinkInputPromptWait(t, generousTimeout)
	m, agent, script := newFirstRunHarness(t, true, nil)

	start := time.Now()
	ready, reason := m.waitForInputPromptForAgentReason(agent, nil)
	if elapsed := time.Since(start); elapsed >= generousTimeout/2 {
		t.Fatalf("stuck notice took %v; it must fail fast, not run out inputPromptTimeout", elapsed)
	}
	if ready {
		t.Fatal("a pane stuck on the notice reported ready")
	}
	if want := blockingScreenNotReadyReason(claudeFirstRunNoticeLabel); reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
	err := kickPromptNotReadyErr(agent.Name, reason)
	if !strings.Contains(err.Error(), "did not reach input prompt") || !strings.Contains(err.Error(), claudeFirstRunNoticeLabel) {
		t.Errorf("kick error %q must keep \"did not reach input prompt\" and name the screen", err)
	}
	if got := len(script.keySnapshot()); got != claudeFirstRunNoticeEnterAttempts {
		t.Errorf("Enters sent = %d, want %d", got, claudeFirstRunNoticeEnterAttempts)
	}
}

// The kick paths' consent recovery answers the notice in place: a restart
// would relaunch into the same un-onboarded HOME and the same notice.
func TestAcceptBypassConsentForKick_ContinuesFirstRunNotice(t *testing.T) {
	m, agent, script := newFirstRunHarness(t, false, nil)

	if !m.acceptBypassConsentForKick(agent, claudeFirstRunNoticePane) {
		t.Fatal("notice not answered in place; the kick would restart into it again")
	}
	if got := script.keySnapshot(); !slices.Equal(got, []string{"Enter"}) {
		t.Errorf("keys = %v, want exactly one Enter", got)
	}
}

// At launch the lifetime prompt watcher answers the notice with a bare Enter
// (no digit), and the sighting is logged once per agent.
func TestWatchForTrustPrompt_FirstRunNoticeSendsEnterAndLogsOnce(t *testing.T) {
	var logs bytes.Buffer
	var logMu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &logMu, w: &logs}, nil))
	m, agent, script := newFirstRunHarness(t, false, logger)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.watchForTrustPromptForAgent(agent, ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(script.keySnapshot(), "Enter") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if got := script.keySnapshot(); !slices.Equal(got, []string{"Enter"}) {
		t.Fatalf("watcher keys = %v, want exactly one Enter", got)
	}
	// A later kick seeing the same screen must not log it again.
	m.noteClaudeFirstRunNotice(agent, "kick")
	logMu.Lock()
	out := logs.String()
	logMu.Unlock()
	if n := strings.Count(out, "first-run security notice on screen"); n != 1 {
		t.Errorf("first-run notice logged %d times, want once:\n%s", n, out)
	}
}

// lockedWriter serialises writes from the watcher goroutine and the test.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// --- onboarding seed -----------------------------------------------------------

// signedInNotOnboardedSession is the shape the CLI writes for itself on a
// signed-in first start: an identity, but no hasCompletedOnboarding.
const signedInNotOnboardedSession = `{"oauthAccount":{"accountUuid":"u-1","emailAddress":"op@example.com"},"userID":"abc","numStartups":12345678901234567890}`

func readSessionJSON(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse %s: %v (%q)", path, err, data)
	}
	return got
}

// The research pool registers its agents with AddAgent at runtime. Their fresh
// HOME must come out of provisioning onboarded and with the work dir trusted,
// so the CLI never parks on the first-run notice in the first place.
func TestAddAgent_RuntimeAgentHomeGetsOnboardingSeed(t *testing.T) {
	shared := withSharedAgentHome(t)
	uidMapPath := filepath.Join(t.TempDir(), "uid-map.json")
	oldPath := UIDMapPath
	UIDMapPath = uidMapPath
	t.Cleanup(func() { UIDMapPath = oldPath })

	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{ACMMLevel: 6})
	u, _ := isolationTestUIDMap(t, "1")
	m.uidMap = u
	// The only signed-in source is one the CLI wrote without the onboarding
	// flag: adoption copies the identity, the seed must add the flag.
	writeSession(t, shared, signedInNotOnboardedSession)

	noRepos := false
	m.AddAgent("research-1", config.AgentConfig{
		Backend: "claude", Role: "research", Mode: "ADVISORY", IncludeRepos: &noRepos, OnDemand: true,
	})
	m.mu.RLock()
	agent, ok := m.agents["research-1"]
	m.mu.RUnlock()
	if !ok || agent.UID <= 0 {
		t.Fatalf("runtime agent not added with a UID: ok=%v", ok)
	}

	// What ensureTmuxSession runs for the agent before its first launch.
	m.setupInteractiveHome(agent, "claude")

	got := readSessionJSON(t, claudeSessionFile(interactiveHomePath("research-1")))
	if string(got[claudeOnboardingKey]) != "true" {
		t.Errorf("hasCompletedOnboarding = %s, want true", got[claudeOnboardingKey])
	}
	var want map[string]json.RawMessage
	if err := json.Unmarshal([]byte(signedInNotOnboardedSession), &want); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{claudeOAuthAccountKey, "userID", "numStartups"} {
		if string(got[key]) != string(want[key]) {
			t.Errorf("%s = %s, want it preserved as %s", key, got[key], want[key])
		}
	}
	var projects map[string]map[string]json.RawMessage
	if err := json.Unmarshal(got[claudeProjectsKey], &projects); err != nil {
		t.Fatalf("projects: %v", err)
	}
	workDir := filepath.Join(m.workDir, "research-1")
	if string(projects[workDir][claudeProjectTrustKey]) != "true" {
		t.Errorf("work dir %s not trusted: %v", workDir, projects)
	}
}

func TestMergeClaudeOnboardingSeed_MergeOnlyAndIdempotent(t *testing.T) {
	const dir = "/data/agents/research-1"
	in := `{"numStartups":12345678901234567890,"projects":{"` + dir + `":{"hasTrustDialogAccepted":false,"history":[1]}}}`

	out, changed, err := mergeClaudeOnboardingSeed([]byte(in), dir)
	if err != nil || !changed {
		t.Fatalf("merge = changed %v, err %v; want a change", changed, err)
	}
	if !strings.Contains(string(out), "12345678901234567890") {
		t.Errorf("numeric value not preserved verbatim: %s", out)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	if string(top[claudeOnboardingKey]) != "true" {
		t.Errorf("hasCompletedOnboarding = %s", top[claudeOnboardingKey])
	}
	if !strings.Contains(string(top[claudeProjectsKey]), `"hasTrustDialogAccepted":false`) || !strings.Contains(string(top[claudeProjectsKey]), `"history":[1]`) {
		t.Errorf("an existing trust decision or project state was overwritten: %s", top[claudeProjectsKey])
	}

	again, changed, err := mergeClaudeOnboardingSeed(out, dir)
	if err != nil || changed || !bytes.Equal(again, out) {
		t.Errorf("second merge changed %v (err %v); want a no-op", changed, err)
	}

	for _, bad := range []string{"{not json", "null", "[]"} {
		if _, _, err := mergeClaudeOnboardingSeed([]byte(bad), dir); err == nil {
			t.Errorf("merge of %q returned no error; an unparseable session must be left alone", bad)
		}
	}
}
