package main

import (
	"bytes"
	"context"
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

// A backend that cannot run tool-less (copilot, codex, gemini) is refused
// with one log line. At a fail-closed level the unavailable classifier is
// installed so untrusted text is withheld; at fail-open nothing is installed.
func TestInstallIoscanClassifier_RefusesNonToollessBackend(t *testing.T) {
	for _, backend := range []string{"copilot", "codex", "gemini"} {
		t.Run(backend+" fail closed", func(t *testing.T) {
			sink := &fakeClassifierSink{}
			logger, logs := ioscanBootLogger()
			installIoscanClassifier(sink, ioscanBootConfig(6, backend, ""), logger, ioscanBootNow)
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
			installIoscanClassifier(sink, ioscanBootConfig(6, backend, "open"), logger, ioscanBootNow)
			if sink.sets != 0 {
				t.Fatalf("fail-open refusal installed %T", sink.classifier)
			}
			if n := strings.Count(logs.String(), "\n"); n != 1 || !strings.Contains(logs.String(), "backend refused") {
				t.Fatalf("want exactly one refusal line, got %d:\n%s", n, logs.String())
			}
		})
	}
}

func TestInstallIoscanClassifier_ClaudeBackendUsesCLIBudget(t *testing.T) {
	sink := &fakeClassifierSink{}
	logger, logs := ioscanBootLogger()
	installIoscanClassifier(sink, ioscanBootConfig(6, "claude", ""), logger, ioscanBootNow)
	if sink.sets != 1 {
		t.Fatalf("SetClassifier calls = %d, want 1", sink.sets)
	}
	if _, ok := sink.classifier.(*ioscan.CachedClassifier); !ok {
		t.Fatalf("installed %T, want *ioscan.CachedClassifier", sink.classifier)
	}
	if sink.budget != ioscan.DefaultCLIClassifierBudgetPerEval {
		t.Fatalf("budget = %d, want %d", sink.budget, ioscan.DefaultCLIClassifierBudgetPerEval)
	}
	if !strings.Contains(logs.String(), "backend=claude") || strings.Contains(logs.String(), "refused") {
		t.Fatalf("unexpected boot log:\n%s", logs.String())
	}

	cfg := ioscanBootConfig(6, "claude", "")
	cfg.Ioscan.Classifier.CLIBudgetPerEval = 3
	sink = &fakeClassifierSink{}
	installIoscanClassifier(sink, cfg, logger, ioscanBootNow)
	if sink.budget != 3 {
		t.Fatalf("explicit cli_budget_per_eval not used: %d", sink.budget)
	}
}

// An empty backend keeps today's HTTP path: the CLI budget is never set.
func TestInstallIoscanClassifier_EmptyBackendKeepsHTTPPath(t *testing.T) {
	sink := &fakeClassifierSink{}
	logger, logs := ioscanBootLogger()
	installIoscanClassifier(sink, ioscanBootConfig(6, "", ""), logger, ioscanBootNow)
	if sink.budget != 0 {
		t.Fatalf("HTTP path set a CLI budget: %d", sink.budget)
	}
	if strings.Contains(logs.String(), "backend=") {
		t.Fatalf("HTTP path logged a backend:\n%s", logs.String())
	}
}

func TestInstallIoscanClassifier_DisabledInstallsNothing(t *testing.T) {
	sink := &fakeClassifierSink{}
	logger, logs := ioscanBootLogger()
	installIoscanClassifier(sink, ioscanBootConfig(1, "copilot", ""), logger, ioscanBootNow)
	if sink.sets != 0 || logs.Len() != 0 {
		t.Fatalf("classifier off at L1 still acted: sets=%d logs=%q", sink.sets, logs.String())
	}
}
