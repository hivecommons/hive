package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/notify"
)

type fakeLoginScanManager struct {
	statuses       map[string]*agent.AgentProcess
	outputs        map[string][]string
	validCreds     map[string]bool
	outputErrs     map[string]error
	firstOutputErr error

	getOutputCalls []string
	lines          []int
	refreshes      []string
	pauses         []string
	callOrder      []string
}

func (m *fakeLoginScanManager) AllStatuses() map[string]*agent.AgentProcess {
	return m.statuses
}

func (m *fakeLoginScanManager) GetOutput(name string, lines int) ([]string, error) {
	m.getOutputCalls = append(m.getOutputCalls, name)
	m.lines = append(m.lines, lines)
	if m.firstOutputErr != nil {
		err := m.firstOutputErr
		m.firstOutputErr = nil
		return nil, err
	}
	if err := m.outputErrs[name]; err != nil {
		return nil, err
	}
	return m.outputs[name], nil
}

func (m *fakeLoginScanManager) AgentHasValidCredential(agentName string) bool {
	return m.validCreds[agentName]
}

func (m *fakeLoginScanManager) RefreshAgentTokenFor(ctx context.Context, name string) error {
	m.refreshes = append(m.refreshes, name)
	m.callOrder = append(m.callOrder, "refresh:"+name)
	return nil
}

func (m *fakeLoginScanManager) Pause(name, trigger, reason string) error {
	m.pauses = append(m.pauses, name)
	m.callOrder = append(m.callOrder, "pause:"+name)
	return nil
}

type fakeLoginScanNotifier struct {
	sent []string
}

func (n *fakeLoginScanNotifier) Send(title, message string, priority notify.Priority) {
	n.sent = append(n.sent, fmt.Sprintf("%s|%s|%s", priority, title, message))
}

type fakeLoginScanAuditor struct {
	entries []string
}

func (a *fakeLoginScanAuditor) AuditLog(user, action, detail, agent string) {
	a.entries = append(a.entries, fmt.Sprintf("%s|%s|%s|%s", user, action, detail, agent))
}

func TestScanForLoginRequiredLoopBodyHandlesVerdicts(t *testing.T) {
	t.Parallel()

	mgr := &fakeLoginScanManager{
		statuses: map[string]*agent.AgentProcess{
			"clean":   {State: agent.StateRunning},
			"valid":   {State: agent.StateRunning},
			"defer":   {State: agent.StateRunning},
			"pause":   {State: agent.StateRunning},
			"stopped": {State: agent.StateStopped},
		},
		outputs: map[string][]string{
			"clean":   {"build passed", "no auth prompt here"},
			"valid":   {"Please log in to continue"},
			"defer":   {"Please log in to continue"},
			"pause":   {"Please log in to continue"},
			"stopped": {"Please log in to continue"},
		},
		validCreds: map[string]bool{"valid": true},
	}
	notifier := &fakeLoginScanNotifier{}
	auditor := &fakeLoginScanAuditor{}
	sightings := newLoginSightingTracker()
	sightings.observe("pause", true)

	cfg := loginScanLoopConfig([]string{"please log in"}, map[string]string{
		"clean":   "claude",
		"valid":   "claude",
		"defer":   "claude",
		"pause":   "copilot",
		"stopped": "claude",
	})

	scanForLoginRequired(context.Background(), cfg, mgr, notifier, auditor, restoreTestLogger(), sightings)

	assertSameElements(t, mgr.pauses, []string{"pause"})
	assertSameElements(t, mgr.refreshes, []string{"pause"})
	assertSameElements(t, notifier.sent, []string{
		"high|🔑 Login required: pause|Agent 'pause' needs authentication. Open the agent's terminal (tmux attach -t hive-pause) and run the login command for the CLI (copilot). Run: copilot auth login",
	})
	assertSameElements(t, auditor.entries, []string{"system|pause|trigger=login-detector|pause"})
	if got := sightings.observe("pause", true); got != 1 {
		t.Fatalf("pause should forget the prior sighting streak; next observation got %d, want 1", got)
	}
	for _, lines := range mgr.lines {
		if lines != 12 {
			t.Fatalf("GetOutput read %d lines, want tail window of 12", lines)
		}
	}
	if loginScanLoopContains(mgr.getOutputCalls, "stopped") {
		t.Fatal("stopped agents must not have their pane read")
	}
	if !indexBefore(mgr.callOrder, "refresh:pause", "pause:pause") {
		t.Fatalf("token refresh must happen before pause, call order: %v", mgr.callOrder)
	}
}

func TestScanForLoginRequiredLoopBodyContinuesAfterPaneReadError(t *testing.T) {
	t.Parallel()

	mgr := &fakeLoginScanManager{
		statuses: map[string]*agent.AgentProcess{
			"one":   {State: agent.StateRunning},
			"two":   {State: agent.StateRunning},
			"three": {State: agent.StateRunning},
		},
		outputs: map[string][]string{
			"one":   {"Please log in to continue"},
			"two":   {"Please log in to continue"},
			"three": {"Please log in to continue"},
		},
		firstOutputErr: errors.New("tmux capture failed"),
	}
	notifier := &fakeLoginScanNotifier{}
	auditor := &fakeLoginScanAuditor{}
	sightings := newLoginSightingTracker()
	for name := range mgr.statuses {
		sightings.observe(name, true)
	}

	cfg := loginScanLoopConfig([]string{"please log in"}, map[string]string{
		"one": "claude", "two": "claude", "three": "claude",
	})

	scanForLoginRequired(context.Background(), cfg, mgr, notifier, auditor, restoreTestLogger(), sightings)

	if len(mgr.getOutputCalls) != len(mgr.statuses) {
		t.Fatalf("pane-read error must not abort the scan; GetOutput calls = %v, want %d calls",
			mgr.getOutputCalls, len(mgr.statuses))
	}
	if len(mgr.pauses) == 0 {
		t.Fatal("pane-read error aborted remaining agents; want at least one later pause")
	}
	if len(notifier.sent) != len(mgr.pauses) || len(auditor.entries) != len(mgr.pauses) {
		t.Fatalf("pause side effects incomplete: pauses=%v notifications=%v audit=%v",
			mgr.pauses, notifier.sent, auditor.entries)
	}
}

func TestScanForLoginRequiredLoopBodyAcceptsEmptyAgentList(t *testing.T) {
	t.Parallel()

	mgr := &fakeLoginScanManager{statuses: map[string]*agent.AgentProcess{}}
	cfg := loginScanLoopConfig([]string{"please log in"}, nil)

	scanForLoginRequired(context.Background(), cfg, mgr, &fakeLoginScanNotifier{}, &fakeLoginScanAuditor{}, restoreTestLogger(), newLoginSightingTracker())

	if len(mgr.getOutputCalls) != 0 || len(mgr.pauses) != 0 || len(mgr.refreshes) != 0 {
		t.Fatalf("empty agent list should do no work: reads=%v pauses=%v refreshes=%v",
			mgr.getOutputCalls, mgr.pauses, mgr.refreshes)
	}
}

func loginScanLoopConfig(patterns []string, backends map[string]string) *config.Config {
	agents := make(map[string]config.AgentConfig, len(backends))
	for name, backend := range backends {
		agents[name] = config.AgentConfig{Backend: backend}
	}
	return &config.Config{
		Agents: agents,
		Governor: config.GovernorConfig{
			Sensing: config.SensingConfig{LoginPatterns: patterns},
		},
	}
}

func assertSameElements(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	counts := make(map[string]int, len(got))
	for _, v := range got {
		counts[v]++
	}
	for _, v := range want {
		if counts[v] == 0 {
			t.Fatalf("got %v, want element %q", got, v)
		}
		counts[v]--
	}
}

func loginScanLoopContains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func indexBefore(values []string, first, second string) bool {
	firstIndex, secondIndex := -1, -1
	for i, v := range values {
		switch v {
		case first:
			firstIndex = i
		case second:
			secondIndex = i
		}
	}
	return firstIndex >= 0 && secondIndex >= 0 && firstIndex < secondIndex
}
