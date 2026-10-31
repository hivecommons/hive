package dashboard

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestOmpProbeRunsUnderGivenHome(t *testing.T) {
	t.Setenv("HOME", "/data/home/agents/x")
	var gotHome string
	orig := runOmpModelsProbe
	t.Cleanup(func() { runOmpModelsProbe = orig })
	runOmpModelsProbe = func(home string) ([]string, map[string][]string, error) {
		gotHome = home
		return []string{"anthropic/claude-opus-5"}, map[string][]string{"anthropic/claude-opus-5": {"low"}}, nil
	}
	r := newTestServer().discoverOmpModels()
	if gotHome != "/data/home/agents/x" {
		t.Fatalf("probe HOME = %q, want the agent home", gotHome)
	}
	if r.fallback || len(r.models) != 1 {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestOmpStaticEffortsMatchCatalog(t *testing.T) {
	for _, id := range []string{"openai-codex/gpt-5.6-terra", "openai-codex/gpt-5.6-sol", "openai-codex/gpt-5.6-luna"} {
		if got := ompStaticReasoningEfforts[id]; !equalStrings(got, []string{"low", "medium", "high", "xhigh", "max"}) {
			t.Errorf("%s efforts = %v", id, got)
		}
	}
	if got := ompStaticReasoningEfforts["google-antigravity/gemini-3.7-flash"]; !equalStrings(got, []string{"minimal", "low", "medium", "high"}) {
		t.Errorf("gemini-3.7-flash efforts = %v", got)
	}
}

func TestOmpHomeWithoutSignInIsNotAWarning(t *testing.T) {
	t.Setenv("HOME", "/data/home/agents/unsigned")
	orig := runOmpModelsProbe
	t.Cleanup(func() { runOmpModelsProbe = orig })
	runOmpModelsProbe = func(home string) ([]string, map[string][]string, error) {
		return nil, nil, errors.New("omp models failed: exit status 1")
	}
	var buf bytes.Buffer
	s := newTestServer()
	s.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := s.discoverOmpModels()
	if !r.fallback {
		t.Fatalf("expected static fallback when no HOME has a sign-in: %+v", r)
	}
	var sawMiss bool
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "found no models for this HOME") {
			sawMiss = true
			if strings.Contains(line, "level=WARN") {
				t.Fatalf("per-HOME miss must not log at WARN (fleet reporter files it as a defect): %s", line)
			}
		}
	}
	if !sawMiss {
		t.Fatal("per-HOME miss should still be logged below WARN")
	}
	if !strings.Contains(buf.String(), "level=WARN msg=\"omp model discovery unavailable") {
		t.Fatalf("all-homes-failed warning must remain: %s", buf.String())
	}
}
