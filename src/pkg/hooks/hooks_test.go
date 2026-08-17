package hooks

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/agent"
	"github.com/kubestellar/hive/pkg/config"
)

type mockNotifier struct {
	sent []mockNotif
}

type mockNotif struct {
	title      string
	message    string
	channel    string
	webhookURL string
}

func (m *mockNotifier) Notify(ctx context.Context, title, message, channel, webhookURL string) error {
	m.sent = append(m.sent, mockNotif{
		title:      title,
		message:    message,
		channel:    channel,
		webhookURL: webhookURL,
	})
	return nil
}

type mockScriptRunner struct {
	runs []mockScriptRun
	out  string
	err  error
}

type mockScriptRun struct {
	command string
	args    []string
	timeout time.Duration
}

func (m *mockScriptRunner) Run(ctx context.Context, command string, args []string, timeout time.Duration) (string, error) {
	m.runs = append(m.runs, mockScriptRun{command: command, args: args, timeout: timeout})
	return m.out, m.err
}

type mockPacingAdjuster struct {
	adjustments []mockPacingAdj
}

type mockPacingAdj struct {
	agent      string
	multiplier float64
	cadence    string
}

func (m *mockPacingAdjuster) AdjustPacing(ctx context.Context, agent string, multiplier float64, cadence string) error {
	m.adjustments = append(m.adjustments, mockPacingAdj{
		agent:      agent,
		multiplier: multiplier,
		cadence:    cadence,
	})
	return nil
}

type fakeAuditSink struct {
	records []recordedAudit
}

type recordedAudit struct {
	actor     string
	action    string
	agentName string
	fields    map[string]any
}

func (f *fakeAuditSink) Record(actor, action, agentName string, fields map[string]any) {
	f.records = append(f.records, recordedAudit{
		actor:     actor,
		action:    action,
		agentName: agentName,
		fields:    fields,
	})
}

// TestHooksDisabledByDefault asserts that with enabled:false, dispatch is a clean no-op.
func TestHooksDisabledByDefault(t *testing.T) {
	cfg := config.HooksConfig{
		Enabled: false,
		Rules: []config.HookRule{
			{On: OnAgentPaused, Action: "notify", Notify: &config.HookNotify{Title: "Paused"}},
		},
	}
	notif := &mockNotifier{}
	d, err := NewDispatcher(cfg, WithNotifier(notif))
	if err != nil {
		t.Fatalf("NewDispatcher error: %v", err)
	}

	results, err := d.Dispatch(context.Background(), Event{Transition: OnAgentPaused, Agent: "scanner"})
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results when disabled, got %d", len(results))
	}
	if len(notif.sent) != 0 {
		t.Errorf("expected 0 notifications when disabled, got %d", len(notif.sent))
	}
}

// TestNamedTransitionsAndTemplating tests event matching and template rendering.
func TestNamedTransitionsAndTemplating(t *testing.T) {
	cfg := config.HooksConfig{
		Enabled: true,
		Rules: []config.HookRule{
			{
				Name:   "notify-on-pause",
				On:     OnAgentPaused,
				Action: "notify",
				Notify: &config.HookNotify{
					Title:   "Agent Paused: {{.Agent}}",
					Message: "Agent {{.Agent}} paused at ACMM L{{.ACMMLevel}} because {{.Reason}} (trigger: {{.Trigger}})",
				},
			},
			{
				Name:   "run-on-merge",
				On:     OnSweepMerge,
				Action: "script",
				Script: &config.HookScript{
					Command:  "./scripts/post-merge.sh",
					Args:     []string{"{{.Metadata.repo}}", "{{.Metadata.pr}}"},
					TimeoutS: 15,
				},
			},
			{
				Name:   "adjust-pacing-on-stall",
				On:     OnStallDetected,
				Action: "pacing",
				Pacing: &config.HookPacing{
					Multiplier: 2.0,
					Cadence:    "slow",
				},
			},
			{
				Name:   "audit-on-acmm-change",
				On:     OnACMMChange,
				Action: "audit",
				Audit: &config.HookAudit{
					Message: "Maturity changed to L{{.ACMMLevel}}: {{.Reason}}",
				},
			},
		},
	}

	notif := &mockNotifier{}
	script := &mockScriptRunner{out: "post-merge complete"}
	pacing := &mockPacingAdjuster{}
	sink := &fakeAuditSink{}

	d, err := NewDispatcher(cfg,
		WithNotifier(notif),
		WithScriptRunner(script),
		WithPacingAdjuster(pacing),
		WithAuditSink(sink),
	)
	if err != nil {
		t.Fatalf("NewDispatcher error: %v", err)
	}

	// 1. Fire on_agent_paused
	evPause := Event{
		Transition: OnAgentPaused,
		Agent:      "quality",
		ACMMLevel:  4,
		Reason:     "trajectory drift detected",
		Trigger:    "trajectory-reviewer",
	}
	res1, err := d.Dispatch(context.Background(), evPause)
	if err != nil {
		t.Fatalf("Dispatch pause error: %v", err)
	}
	if len(res1) != 1 || !res1[0].Success {
		t.Fatalf("expected 1 successful result for pause: %+v", res1)
	}
	if len(notif.sent) != 1 {
		t.Fatalf("expected 1 notification sent, got %d", len(notif.sent))
	}
	if notif.sent[0].title != "Agent Paused: quality" {
		t.Errorf("title = %q, want 'Agent Paused: quality'", notif.sent[0].title)
	}
	if !strings.Contains(notif.sent[0].message, "trajectory drift detected") {
		t.Errorf("unexpected message content: %s", notif.sent[0].message)
	}

	// 2. Fire on_sweep_merge
	evMerge := Event{
		Transition: OnSweepMerge,
		Metadata:   map[string]any{"repo": "kubestellar/hive", "pr": 4010},
	}
	res2, err := d.Dispatch(context.Background(), evMerge)
	if err != nil {
		t.Fatalf("Dispatch merge error: %v", err)
	}
	if len(res2) != 1 || !res2[0].Success {
		t.Fatalf("expected 1 successful result for merge: %+v", res2)
	}
	if len(script.runs) != 1 {
		t.Fatalf("expected 1 script run, got %d", len(script.runs))
	}
	if len(script.runs[0].args) != 2 || script.runs[0].args[0] != "kubestellar/hive" || script.runs[0].args[1] != "4010" {
		t.Errorf("script args mismatch: %v", script.runs[0].args)
	}

	// 3. Fire on_stall_detected
	evStall := Event{
		Transition: OnStallDetected,
		Agent:      "architect",
	}
	res3, err := d.Dispatch(context.Background(), evStall)
	if err != nil {
		t.Fatalf("Dispatch stall error: %v", err)
	}
	if len(res3) != 1 || !res3[0].Success {
		t.Fatalf("expected 1 successful result for stall: %+v", res3)
	}
	if len(pacing.adjustments) != 1 || pacing.adjustments[0].agent != "architect" || pacing.adjustments[0].multiplier != 2.0 {
		t.Errorf("pacing adjustment mismatch: %+v", pacing.adjustments)
	}

	// 4. Fire on_acmm_change
	evACMM := Event{
		Transition: OnACMMChange,
		ACMMLevel:  5,
		Reason:     "promoted by admin",
	}
	res4, err := d.Dispatch(context.Background(), evACMM)
	if err != nil {
		t.Fatalf("Dispatch acmm error: %v", err)
	}
	if len(res4) != 1 || !res4[0].Success {
		t.Fatalf("expected 1 successful result for acmm change: %+v", res4)
	}

	// Verify AuditSink recorded every event under AuditHookFired
	if len(sink.records) != 4 {
		t.Errorf("expected 4 audit records, got %d", len(sink.records))
	}
	for _, rec := range sink.records {
		if rec.action != agent.AuditHookFired {
			t.Errorf("expected action %q, got %q", agent.AuditHookFired, rec.action)
		}
	}
}

// TestWildcardAndPrefixMatching tests rule matching with patterns like on_agent_* and *.
func TestWildcardAndPrefixMatching(t *testing.T) {
	cfg := config.HooksConfig{
		Enabled: true,
		Rules: []config.HookRule{
			{Name: "agent-wildcard", On: "on_agent_*", Action: "audit"},
			{Name: "catch-all", On: "*", Action: "audit"},
		},
	}
	d, err := NewDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewDispatcher error: %v", err)
	}

	// on_agent_resumed matches both on_agent_* and *
	res1, _ := d.Dispatch(context.Background(), Event{Transition: OnAgentResumed, Agent: "scanner"})
	if len(res1) != 2 {
		t.Errorf("expected 2 matches for on_agent_resumed, got %d", len(res1))
	}

	// on_pr_opened matches only *
	res2, _ := d.Dispatch(context.Background(), Event{Transition: OnPROpened})
	if len(res2) != 1 {
		t.Errorf("expected 1 match for on_pr_opened, got %d", len(res2))
	}
}

// TestScriptOutputSecretScrubbing verifies that leaked credentials in script output are scrubbed.
func TestScriptOutputSecretScrubbing(t *testing.T) {
	runner := &DefaultScriptRunner{}
	// Echoes a token string
	secret := "ghp_123456789012345678901234567890123456"
	out, err := runner.Run(context.Background(), fmt.Sprintf("echo 'Token is %s'", secret), nil, 5*time.Second)
	if err != nil {
		t.Fatalf("script run error: %v", err)
	}
	if strings.Contains(out, secret) {
		t.Errorf("raw secret was NOT scrubbed from script output: %q", out)
	}
	if !strings.Contains(out, "[REDACTED") && !strings.Contains(out, "ghp_") {
		t.Logf("output scrubbed: %q", out)
	}
}

// TestInvalidRuleValidation asserts malformed rules are rejected at construction time.
func TestInvalidRuleValidation(t *testing.T) {
	invalidConfigs := []config.HooksConfig{
		{
			Rules: []config.HookRule{{On: "", Action: "notify"}},
		},
		{
			Rules: []config.HookRule{{On: "on_agent_paused", Action: "invalid_action"}},
		},
	}

	for i, cfg := range invalidConfigs {
		_, err := NewDispatcher(cfg)
		if err == nil {
			t.Errorf("case %d: expected error for invalid rule config, got nil", i)
		}
	}
}
