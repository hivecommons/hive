package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/betty"
	"github.com/hivecommons/hive/pkg/meetings/meetllm"
)

// cliLLM is a resolved agent-backend model on the claude CLI.
func cliLLM(binary, home string) config.ResolvedMeetingsLLM {
	return config.ResolvedMeetingsLLM{
		Backend: config.MeetingsLLMModeAgentBackend, AgentBackend: "claude", Model: config.DefaultMeetingsClaudeCLIModel,
		MaxInFlight: 2, TimeoutMS: config.DefaultMeetingsLLMTimeoutMS,
		CLI: config.ResolvedMeetingsCLI{
			Binary: binary, Home: home, AddressedTimeoutMS: 8000, UnpromptedTimeoutMS: 6000, RestyleTimeoutMS: 3000,
			OneShotTimeoutMS: 30000, RecycleTurns: 4,
		},
	}
}

// Spek on an agent backend calls `hive meeting-llm` with the backend, the
// model and the cold-start timeout; the CLI's config dir passes through,
// hive's tokens do not.
func TestMeetingSpekEnvAgentBackend(t *testing.T) {
	get := func(k string) string {
		return map[string]string{"PATH": "/bin", "HIVE_GITHUB_TOKEN": "ghp_x", "CLAUDE_CONFIG_DIR": "/cfg"}[k]
	}
	env := map[string]string{}
	for _, kv := range meetingSpekEnv(get, cliLLM("/usr/bin/claude", "/data/home"), "/usr/local/bin/hive", "/u.jsonl", "/slots", 500) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	for k, want := range map[string]string{
		"SPEK_MEETING_LLM": "command", "SPEK_MEETING_LLM_CMD": "/usr/local/bin/hive meeting-llm", "SPEK_MEETING_LLM_TIMEOUT_MS": "30000",
		"HIVE_MEETING_LLM_BACKEND": "claude", "HIVE_MEETING_LLM_MODEL": config.DefaultMeetingsClaudeCLIModel,
		"HIVE_MEETING_LLM_TIMEOUT_MS": "30000", "HIVE_MEETING_LLM_MAX_TOKENS": "500", "HIVE_MEETING_LLM_CLI_BINARY": "/usr/bin/claude",
		"HIVE_MEETING_LLM_CLI_HOME": "/data/home", "CLAUDE_CONFIG_DIR": "/cfg", "HIVE_MEETING_LLM_USAGE_FILE": "/u.jsonl",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if _, ok := env["HIVE_GITHUB_TOKEN"]; ok {
		t.Fatal("hive tokens must not reach spek")
	}
	bare := meetingSpekEnv(func(string) string { return "" }, cliLLM("", ""), "h", "u", "s", 0)
	for _, kv := range bare {
		if strings.HasPrefix(kv, "HIVE_MEETING_LLM_CLI_") || strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			t.Fatalf("unset value exported: %s", kv)
		}
	}
}

// On an agent backend the persona gets a warm CLI started for the meeting,
// and the CLI timeouts; a backend with no profile leaves her model-less.
func TestMeetingPersonaFactoryAgentBackend(t *testing.T) {
	store, err := meetings.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho start >> '"+dir+"/starts'\ncat > /dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	persona := cfg.Meetings.Persona.Resolve()
	logger := slog.New(slog.DiscardHandler)
	e := meetingPersonaFactory(cfg, persona, cliLLM(bin, dir), store, nil, nil, t.TempDir(), 100, false, logger, nil)(meetings.Meeting{ID: "m1"})
	if e == nil {
		t.Fatal("want an engine")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := os.ReadFile(filepath.Join(dir, "starts")); len(b) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the meeting CLI was not warmed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.Close()
	bad := cliLLM(bin, dir)
	bad.AgentBackend = "codex"
	if e := meetingPersonaFactory(cfg, persona, bad, store, nil, nil, "", 0, false, logger, nil)(meetings.Meeting{ID: "m2"}); e == nil {
		t.Fatal("want an engine without a model")
	} else {
		e.Close()
	}
	missing := cliLLM(filepath.Join(dir, "absent"), dir)
	if m, ok := meetingCLIModel(missing, meetllm.Config{MaxInFlight: 1}, "m3", logger); !ok || m.Config.Pool == nil {
		t.Fatal("a missing binary still yields a pool that retries the start")
	} else {
		_ = m.Close()
	}
}

func TestApplyMeetingCLITimeouts(t *testing.T) {
	s := betty.Settings{AddressedTimeout: time.Second, UnpromptedTimeout: time.Second, RestyleTimeout: time.Second}
	applyMeetingCLITimeouts(&s, config.ResolvedMeetingsCLI{})
	if s.AddressedTimeout != time.Second || s.UnpromptedTimeout != time.Second || s.RestyleTimeout != time.Second {
		t.Fatalf("zero CLI timeouts changed settings: %+v", s)
	}
	applyMeetingCLITimeouts(&s, cliLLM("", "").CLI)
	if s.AddressedTimeout != 8*time.Second || s.UnpromptedTimeout != 6*time.Second || s.RestyleTimeout != 3*time.Second {
		t.Fatalf("CLI timeouts %+v", s)
	}
}
