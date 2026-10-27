package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/ioscan"
)

type fakeClassifierSink struct {
	classifier ioscan.Classifier
	sets       int
	budget     int
}

func (f *fakeClassifierSink) SetClassifier(c ioscan.Classifier, _ ioscan.Thresholds) {
	f.classifier = c
	f.sets++
}

func (f *fakeClassifierSink) SetClassifierBudgetPerEval(n int) { f.budget = n }

type fakeClassifierAlerts struct {
	alerts map[string]string
	adds   int
	clears int
}

func (f *fakeClassifierAlerts) AddSystemAlert(id, severity, message string) {
	if f.alerts == nil {
		f.alerts = map[string]string{}
	}
	f.alerts[id] = severity + ": " + message
	f.adds++
}

func (f *fakeClassifierAlerts) ClearSystemAlert(id string) {
	delete(f.alerts, id)
	f.clears++
}

func (f *fakeClassifierAlerts) blocked() (string, bool) {
	msg, ok := f.alerts[ioscanClassifierBlockedAlertID]
	return msg, ok
}

func ioscanBootConfig(level int, backend, failMode string) *config.Config {
	return &config.Config{
		ACMMLevel: &level,
		Ioscan: config.IoscanConfig{Classifier: config.IoscanClassifierConfig{
			Backend:  backend,
			FailMode: failMode,
		}},
	}
}

func ioscanBootLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

var ioscanBootNow = func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) }

// A backend that cannot run tool-less (codex, gemini) is refused with one
// log line. At a fail-closed level the unavailable classifier is installed
// so untrusted text is withheld; at fail-open nothing is installed.
func TestInstallIoscanClassifier_RefusesNonToollessBackend(t *testing.T) {
	for _, backend := range []string{"codex", "gemini"} {
		t.Run(backend+" fail closed", func(t *testing.T) {
			sink := &fakeClassifierSink{}
			logger, logs := ioscanBootLogger()
			installIoscanClassifier(sink, nil, ioscanBootConfig(6, backend, ""), logger, ioscanBootNow)
			if _, ok := sink.classifier.(ioscan.UnavailableClassifier); !ok || sink.sets != 1 {
				t.Fatalf("installed %T (%d sets), want UnavailableClassifier once", sink.classifier, sink.sets)
			}
			if _, err := sink.classifier.Score(context.Background(), "text"); err == nil {
				t.Fatal("unavailable classifier should error")
			}
			if sink.budget != 0 {
				t.Fatalf("refused backend set a CLI budget: %d", sink.budget)
			}
			if n := strings.Count(logs.String(), "\n"); n != 1 || !strings.Contains(logs.String(), "backend refused") || !strings.Contains(logs.String(), "backend="+backend) {
				t.Fatalf("want exactly one refusal line, got %d:\n%s", n, logs.String())
			}
		})
		t.Run(backend+" fail open", func(t *testing.T) {
			sink := &fakeClassifierSink{}
			logger, logs := ioscanBootLogger()
			installIoscanClassifier(sink, nil, ioscanBootConfig(6, backend, "open"), logger, ioscanBootNow)
			if sink.sets != 0 {
				t.Fatalf("fail-open refusal installed %T", sink.classifier)
			}
			if n := strings.Count(logs.String(), "\n"); n != 1 || !strings.Contains(logs.String(), "backend refused") {
				t.Fatalf("want exactly one refusal line, got %d:\n%s", n, logs.String())
			}
		})
	}
}

func TestInstallIoscanClassifier_CLIBackendUsesCLIBudget(t *testing.T) {
	for _, backend := range []string{"claude", "copilot"} {
		sink := &fakeClassifierSink{}
		logger, logs := ioscanBootLogger()
		installIoscanClassifier(sink, nil, ioscanBootConfig(6, backend, ""), logger, ioscanBootNow)
		if sink.sets != 1 {
			t.Fatalf("%s: SetClassifier calls = %d, want 1", backend, sink.sets)
		}
		if _, ok := sink.classifier.(*ioscan.CachedClassifier); !ok {
			t.Fatalf("%s: installed %T, want *ioscan.CachedClassifier", backend, sink.classifier)
		}
		if sink.budget != ioscan.DefaultCLIClassifierBudgetPerEval {
			t.Fatalf("%s: budget = %d, want %d", backend, sink.budget, ioscan.DefaultCLIClassifierBudgetPerEval)
		}
		if !strings.Contains(logs.String(), "backend="+backend) || !strings.Contains(logs.String(), "backend_from_agents=false") || strings.Contains(logs.String(), "refused") {
			t.Fatalf("%s: unexpected boot log:\n%s", backend, logs.String())
		}
	}
	logger, _ := ioscanBootLogger()

	cfg := ioscanBootConfig(6, "claude", "")
	cfg.Ioscan.Classifier.CLIBudgetPerEval = 3
	sink := &fakeClassifierSink{}
	installIoscanClassifier(sink, nil, cfg, logger, ioscanBootNow)
	if sink.budget != 3 {
		t.Fatalf("explicit cli_budget_per_eval not used: %d", sink.budget)
	}
}

// An empty backend keeps today's HTTP path: the CLI budget is never set.
func TestInstallIoscanClassifier_EmptyBackendKeepsHTTPPath(t *testing.T) {
	sink := &fakeClassifierSink{}
	logger, logs := ioscanBootLogger()
	installIoscanClassifier(sink, nil, ioscanBootConfig(6, "", ""), logger, ioscanBootNow)
	if sink.budget != 0 {
		t.Fatalf("HTTP path set a CLI budget: %d", sink.budget)
	}
	if strings.Contains(logs.String(), "backend=") {
		t.Fatalf("HTTP path logged a backend:\n%s", logs.String())
	}
}

func TestInstallIoscanClassifier_DisabledInstallsNothing(t *testing.T) {
	sink := &fakeClassifierSink{}
	alerts := &fakeClassifierAlerts{}
	alerts.AddSystemAlert(ioscanClassifierBlockedAlertID, "error", "stale from an earlier boot")
	logger, logs := ioscanBootLogger()
	installIoscanClassifier(sink, alerts, ioscanBootConfig(1, "codex", ""), logger, ioscanBootNow)
	if sink.sets != 0 || logs.Len() != 0 {
		t.Fatalf("classifier off at L1 still acted: sets=%d logs=%q", sink.sets, logs.String())
	}
	if _, ok := alerts.blocked(); ok {
		t.Fatal("classifier off at L1 left a stale blocked-kick alert")
	}
}

// ioscanBootConfigWithAgents is a config with no backend set, no endpoint
// (HIVE_LITELLM_ENDPOINT cleared so the host cannot supply one) and one
// agent per listed backend.
func ioscanBootConfigWithAgents(t *testing.T, level int, backends ...string) *config.Config {
	t.Helper()
	t.Setenv(config.LiteLLMEndpointEnv, "")
	cfg := ioscanBootConfig(level, "", "")
	cfg.Agents = map[string]config.AgentConfig{}
	for i, b := range backends {
		cfg.Agents[fmt.Sprintf("agent-%d", i)] = config.AgentConfig{Backend: b}
	}
	return cfg
}

// With no backend set and no model endpoint, a hive whose agents run on a
// tool-less-capable CLI gets the classifier through that CLI (#14499): the
// most common such backend, ties by name.
func TestIoscanClassifierFallbackBackend(t *testing.T) {
	cases := map[string]struct {
		agents []string
		want   string
	}{
		"no agents":            {nil, ""},
		"only refused":         {[]string{"codex", "gemini", "litellm"}, ""},
		"copilot only":         {[]string{"copilot", "copilot"}, "copilot"},
		"majority wins":        {[]string{"copilot", "claude", "copilot", "codex"}, "copilot"},
		"tie breaks by name":   {[]string{"copilot", "claude"}, "claude"},
		"case and space trim":  {[]string{" Copilot "}, "copilot"},
		"refused do not count": {[]string{"codex", "codex", "codex", "copilot"}, "copilot"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ioscanClassifierFallbackBackend(ioscanBootConfigWithAgents(t, 6, tc.agents...)); got != tc.want {
				t.Fatalf("fallback = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInstallIoscanClassifier_FallsBackToAgentCLIBackend(t *testing.T) {
	sink := &fakeClassifierSink{}
	alerts := &fakeClassifierAlerts{}
	alerts.AddSystemAlert(ioscanClassifierBlockedAlertID, "error", "stale from an earlier boot")
	logger, logs := ioscanBootLogger()
	installIoscanClassifier(sink, alerts, ioscanBootConfigWithAgents(t, 6, "copilot", "copilot", "codex"), logger, ioscanBootNow)
	if _, ok := sink.classifier.(*ioscan.CachedClassifier); !ok || sink.sets != 1 {
		t.Fatalf("installed %T (%d sets), want *ioscan.CachedClassifier once", sink.classifier, sink.sets)
	}
	if sink.budget != ioscan.DefaultCLIClassifierBudgetPerEval {
		t.Fatalf("fallback CLI backend did not set the CLI budget: %d", sink.budget)
	}
	if !strings.Contains(logs.String(), "backend=copilot") || !strings.Contains(logs.String(), "backend_from_agents=true") {
		t.Fatalf("boot log does not say the backend came from the agents:\n%s", logs.String())
	}
	if _, ok := alerts.blocked(); ok {
		t.Fatal("running classifier left the blocked-kick alert up")
	}
}

// An explicit backend or a resolved endpoint wins over the agent fallback.
func TestInstallIoscanClassifier_FallbackYieldsToExplicitConfig(t *testing.T) {
	cfg := ioscanBootConfigWithAgents(t, 6, "copilot")
	cfg.Ioscan.Classifier.Backend = "claude"
	sink := &fakeClassifierSink{}
	logger, logs := ioscanBootLogger()
	installIoscanClassifier(sink, nil, cfg, logger, ioscanBootNow)
	if !strings.Contains(logs.String(), "backend=claude") {
		t.Fatalf("explicit backend overridden:\n%s", logs.String())
	}

	cfg = ioscanBootConfigWithAgents(t, 6, "copilot")
	cfg.Governor.LiteLLM.Endpoint = "http://litellm.example:4000"
	sink = &fakeClassifierSink{}
	logger, logs = ioscanBootLogger()
	installIoscanClassifier(sink, nil, cfg, logger, ioscanBootNow)
	if sink.budget != 0 || strings.Contains(logs.String(), "backend=") {
		t.Fatalf("resolved endpoint should keep the HTTP path: budget=%d\n%s", sink.budget, logs.String())
	}
}

// A required classifier that cannot run raises one persistent dashboard
// alert naming the fixes; a fail-open hive gets no alert.
func TestInstallIoscanClassifier_RequiredButUnavailableRaisesAlert(t *testing.T) {
	sink := &fakeClassifierSink{}
	alerts := &fakeClassifierAlerts{}
	logger, logs := ioscanBootLogger()
	installIoscanClassifier(sink, alerts, ioscanBootConfigWithAgents(t, 6, "codex"), logger, ioscanBootNow)
	if _, ok := sink.classifier.(ioscan.UnavailableClassifier); !ok {
		t.Fatalf("installed %T, want UnavailableClassifier", sink.classifier)
	}
	msg, ok := alerts.blocked()
	if !ok || alerts.adds != 1 {
		t.Fatalf("blocked-kick alert not raised exactly once: adds=%d alerts=%v", alerts.adds, alerts.alerts)
	}
	for _, want := range []string{"error: ", "ACMM L6", "withheld", "ioscan.classifier.backend", "claude, copilot", "fail_mode: open", "governor.litellm"} {
		if !strings.Contains(msg, want) {
			t.Errorf("alert %q lacks %q", msg, want)
		}
	}
	if !strings.Contains(logs.String(), "kicks carrying it are blocked") {
		t.Errorf("boot log does not say kicks are blocked:\n%s", logs.String())
	}

	// A refused explicit backend at fail-closed raises the same alert.
	sink, alerts = &fakeClassifierSink{}, &fakeClassifierAlerts{}
	installIoscanClassifier(sink, alerts, ioscanBootConfig(6, "codex", ""), logger, ioscanBootNow)
	if msg, ok := alerts.blocked(); !ok || !strings.Contains(msg, "codex") {
		t.Fatalf("refused backend did not raise the blocked-kick alert: %v", alerts.alerts)
	}

	// Fail-open: no alert, and a stale one is cleared.
	sink, alerts = &fakeClassifierSink{}, &fakeClassifierAlerts{}
	alerts.AddSystemAlert(ioscanClassifierBlockedAlertID, "error", "stale")
	installIoscanClassifier(sink, alerts, ioscanBootConfig(6, "", "open"), logger, ioscanBootNow)
	if _, ok := alerts.blocked(); ok || sink.sets != 0 {
		t.Fatalf("fail-open raised or kept the blocked-kick alert: %v sets=%d", alerts.alerts, sink.sets)
	}

	// No dashboard: nothing panics.
	installIoscanClassifier(&fakeClassifierSink{}, nil, ioscanBootConfigWithAgents(t, 6, "codex"), logger, ioscanBootNow)
}
