package hooks

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kubestellar/hive/pkg/agent"
	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/logscrub"
)

// Notifier defines the interface for sending external notifications.
type Notifier interface {
	Notify(ctx context.Context, title, message, channel, webhookURL string) error
}

// ScriptRunner defines the interface for executing script actions.
type ScriptRunner interface {
	Run(ctx context.Context, command string, args []string, timeout time.Duration) (string, error)
}

// PacingAdjuster defines the interface for adjusting governor cadences.
type PacingAdjuster interface {
	AdjustPacing(ctx context.Context, agent string, multiplier float64, cadence string) error
}

// DefaultScriptRunner executes shell commands with process isolation, timeout, and secret scrubbing.
type DefaultScriptRunner struct{}

func (d *DefaultScriptRunner) Run(ctx context.Context, command string, args []string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctxTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctxTimeout, "sh", "-c", command)
	if len(args) > 0 {
		cmd.Args = append(cmd.Args, args...)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}

	// Secret scrub the raw output before returning
	scrubbed := logscrub.ScrubString(output)
	return strings.TrimSpace(scrubbed), err
}

// Dispatcher executes matched hook actions for state transition events.
type Dispatcher struct {
	enabled   bool
	registry  *Registry
	notifier  Notifier
	scriptRun ScriptRunner
	pacingAdj PacingAdjuster
	auditSink agent.AuditSink
}

// Option configures a Dispatcher.
type Option func(*Dispatcher)

// WithNotifier installs a custom notification sink.
func WithNotifier(n Notifier) Option {
	return func(d *Dispatcher) { d.notifier = n }
}

// WithScriptRunner installs a custom script execution engine.
func WithScriptRunner(s ScriptRunner) Option {
	return func(d *Dispatcher) { d.scriptRun = s }
}

// WithPacingAdjuster installs a custom pacing handler.
func WithPacingAdjuster(p PacingAdjuster) Option {
	return func(d *Dispatcher) { d.pacingAdj = p }
}

// WithAuditSink installs the durable audit sink for hook execution records.
func WithAuditSink(sink agent.AuditSink) Option {
	return func(d *Dispatcher) { d.auditSink = sink }
}

// NewDispatcher creates a new Dispatcher from a HooksConfig.
func NewDispatcher(cfg config.HooksConfig, opts ...Option) (*Dispatcher, error) {
	reg, err := NewRegistryFromConfig(cfg)
	if err != nil {
		return nil, err
	}

	d := &Dispatcher{
		enabled:   cfg.Enabled,
		registry:  reg,
		scriptRun: &DefaultScriptRunner{},
	}
	for _, opt := range opts {
		opt(d)
	}
	return d, nil
}

// SetEnabled dynamically enables or disables hook execution.
func (d *Dispatcher) SetEnabled(enabled bool) {
	d.enabled = enabled
}

// IsEnabled reports whether hooks are currently active.
func (d *Dispatcher) IsEnabled() bool {
	return d.enabled
}

// Registry returns the underlying hook rule registry.
func (d *Dispatcher) Registry() *Registry {
	return d.registry
}

// Dispatch evaluates matching hook rules for the event and executes their configured actions.
func (d *Dispatcher) Dispatch(ctx context.Context, event Event) ([]Result, error) {
	if !d.enabled || d.registry == nil {
		return nil, nil
	}

	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}

	rules := d.registry.Match(event)
	if len(rules) == 0 {
		return nil, nil
	}

	results := make([]Result, 0, len(rules))
	for _, rule := range rules {
		start := time.Now()
		res := Result{
			RuleName: rule.Name,
			On:       rule.On,
			Action:   rule.Action,
		}

		var execErr error
		switch rule.Action {
		case "notify":
			execErr = d.execNotify(ctx, event, rule, &res)
		case "script":
			execErr = d.execScript(ctx, event, rule, &res)
		case "pacing":
			execErr = d.execPacing(ctx, event, rule, &res)
		case "audit":
			execErr = d.execAudit(ctx, event, rule, &res)
		default:
			execErr = fmt.Errorf("unknown action %q", rule.Action)
		}

		res.Duration = time.Since(start)
		if execErr != nil {
			res.Success = false
			res.Error = execErr.Error()
		} else {
			res.Success = true
		}

		// Emit result to durable audit log
		if d.auditSink != nil {
			actor := "system"
			d.auditSink.Record(actor, agent.AuditHookFired, event.Agent, res.AuditFields(event))
		}

		results = append(results, res)
	}

	return results, nil
}

func (d *Dispatcher) execNotify(ctx context.Context, event Event, rule config.HookRule, res *Result) error {
	if rule.Notify == nil {
		return fmt.Errorf("hook rule %q specifies notify action but has no notify configuration", rule.Name)
	}

	title, _ := RenderTemplate(rule.Notify.Title, event)
	msg, _ := RenderTemplate(rule.Notify.Message, event)
	if title == "" {
		title = fmt.Sprintf("Hook triggered: %s", event.Transition)
	}
	if msg == "" {
		msg = fmt.Sprintf("Event on %s for agent %s (reason: %s)", event.Transition, event.Agent, event.Reason)
	}

	res.Output = fmt.Sprintf("title: %s; message: %s", title, msg)

	if d.notifier != nil {
		return d.notifier.Notify(ctx, title, msg, rule.Notify.Channel, rule.Notify.WebhookURL)
	}
	return nil
}

func (d *Dispatcher) execScript(ctx context.Context, event Event, rule config.HookRule, res *Result) error {
	if rule.Script == nil || rule.Script.Command == "" {
		return fmt.Errorf("hook rule %q specifies script action but has no command", rule.Name)
	}

	cmdStr, _ := RenderTemplate(rule.Script.Command, event)
	var args []string
	for _, arg := range rule.Script.Args {
		rendered, _ := RenderTemplate(arg, event)
		args = append(args, rendered)
	}

	timeout := time.Duration(rule.Script.TimeoutS) * time.Second
	if d.scriptRun == nil {
		d.scriptRun = &DefaultScriptRunner{}
	}

	output, err := d.scriptRun.Run(ctx, cmdStr, args, timeout)
	res.Output = output
	return err
}

func (d *Dispatcher) execPacing(ctx context.Context, event Event, rule config.HookRule, res *Result) error {
	if rule.Pacing == nil {
		return fmt.Errorf("hook rule %q specifies pacing action but has no pacing configuration", rule.Name)
	}

	agentName := rule.Pacing.Agent
	if agentName == "" {
		agentName = event.Agent
	}
	res.Output = fmt.Sprintf("adjusted pacing for agent %s: mult=%.2f cadence=%s", agentName, rule.Pacing.Multiplier, rule.Pacing.Cadence)

	if d.pacingAdj != nil {
		return d.pacingAdj.AdjustPacing(ctx, agentName, rule.Pacing.Multiplier, rule.Pacing.Cadence)
	}
	return nil
}

func (d *Dispatcher) execAudit(ctx context.Context, event Event, rule config.HookRule, res *Result) error {
	msg := ""
	if rule.Audit != nil {
		msg, _ = RenderTemplate(rule.Audit.Message, event)
	}
	if msg == "" {
		msg = fmt.Sprintf("Hook audit: transition=%s agent=%s reason=%s", event.Transition, event.Agent, event.Reason)
	}
	res.Output = msg
	return nil
}
