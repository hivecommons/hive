package cline

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const help = "--json --provider --model --cwd --config --data-dir --timeout --auto-approve --thinking"

func credentials(t *testing.T, root, source string) {
	t.Helper()
	path := filepath.Join(root, "data", "settings")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "providers.json"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
}

const providerConfig = `{"version":1,"providers":{"openrouter":{"settings":{"providerId":"openrouter","apiKey":"test-key","model":"anthropic/test"},"updatedAt":"2026-11-02T00:00:00Z","tokenSource":"manual"},"other":{"settings":{"apiKey":"must-not-copy"}}},"modes":{}}`

func fakeCLI(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cline")
	script := "#!/bin/sh\nif [ \"$1\" = --help ]; then printf '%s\\n' '" + help + "'; exit 0; fi\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSelection(t *testing.T) {
	for _, invalid := range []string{"", "auto", "/model", "openai/", "bad provider/id"} {
		if _, _, err := Selection(invalid); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
	p, m, err := Selection("openrouter/anthropic/claude-sonnet-4-6")
	if err != nil || p != "openrouter" || m != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("%s %s %v", p, m, err)
	}
}

func TestProbe(t *testing.T) {
	if err := Probe(context.Background(), fakeCLI(t, "exit 99")); err != nil {
		t.Fatal(err)
	}
	if err := Probe(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing binary accepted")
	}
	old := filepath.Join(t.TempDir(), "old")
	if err := os.WriteFile(old, []byte("#!/bin/sh\necho 'old editor cli'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Probe(context.Background(), old); err == nil || !strings.Contains(err.Error(), "--json") {
		t.Fatalf("old CLI: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Probe(ctx, old); err == nil {
		t.Fatal("canceled probe accepted")
	}
}

func TestSelectedCredentials(t *testing.T) {
	root := t.TempDir()
	if _, err := selectedCredentials(root, "openrouter"); err == nil {
		t.Fatal("missing config accepted")
	}
	for _, source := range []string{"broken", `{"version":2}`, `{"version":1,"providers":{}}`,
		`{"version":1,"providers":{"openrouter":{"tokenSource":"oauth"}}}`,
		`{"version":1,"providers":{"openrouter":[]}}`} {
		credentials(t, root, source)
		if _, err := selectedCredentials(root, "openrouter"); err == nil {
			t.Errorf("accepted %s", source)
		}
	}
	credentials(t, root, providerConfig)
	out, err := selectedCredentials(root, "openrouter")
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out) || strings.Contains(string(out), "must-not-copy") || strings.Contains(string(out), `"other"`) {
		t.Fatalf("credential narrowing failed: %s", out)
	}
}

func TestCommandArgs(t *testing.T) {
	opts := options{cwd: "/work space", timeout: time.Minute, effort: "high"}
	args := commandArgs("openrouter", "anthropic/test", opts, "/private state")
	joined := strings.Join(args, "|")
	for _, expected := range []string{"--provider|openrouter", "--model|anthropic/test", "--cwd|/work space", "--config|/private state",
		"--data-dir|/private state/data", "--thinking|high", "--timeout|60", "--json"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("missing %s in %s", expected, joined)
		}
	}

	if strings.Contains(joined, "--key") || strings.Contains(joined, "prompt") {
		t.Fatal("secrets/prompts belong off argv")
	}
}

func TestWorkerEnv(t *testing.T) {
	in := []string{"PATH=/tools", "HTTPS_PROXY=http://policy", "NODE_EXTRA_CA_CERTS=/ca",
		"OPENAI_API_KEY=unrelated", "ANTHROPIC_API_KEY=unrelated", "DATABRICKS_TOKEN=unrelated",
		"CLINE_HUB_ADDRESS=host", "AWS_SECRET_ACCESS_KEY=unrelated", "KILO_AUTH_CONTENT=unrelated",
		"HIVE_CLINE_PROVIDERS_B64=secret", "HIVE_AGENT_TOKEN_CACHE=/scoped"}
	got := strings.Join(workerEnv(in), "\n")
	if strings.Contains(got, "unrelated") || strings.Contains(got, "secret") || strings.Contains(got, "CLINE_HUB") {
		t.Fatal(got)
	}
	for _, want := range []string{"PATH=/tools", "HTTPS_PROXY=http://policy", "NODE_EXTRA_CA_CERTS=/ca", "HIVE_AGENT_TOKEN_CACHE=/scoped"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %s", want)
		}
	}
}

func TestRender(t *testing.T) {
	cases := []struct {
		input, want string
		fails       bool
	}{
		{`{"type":"agent_event","event":{"type":"content_update","text":"private reasoning"}}` + "\n" + `{"type":"run_result","finishReason":"completed","text":"HIVE_VERDICT: complete","usage":{"inputTokens":3}}` + "\n", "HIVE_VERDICT: complete", false},
		{`{"type":"future_event"}` + "\n" + `{"type":"run_result","finishReason":"completed","text":"done"}`, "done", false},
		{"", "", true},
		{"not JSON\n", "", true},
		{`{"type":"run_result","finishReason":"aborted"}`, "", true},
		{`{"type":"run_result","finishReason":"max_iterations"}`, "", true},
		{`{"type":"run_aborted"}`, "", true},
		{`{"type":"error","message":"secret"}`, "", true},
		{`{"type":"agent_event","event":{"type":"error"}}`, "", true},
		{strings.Repeat("x", maxEventBytes+1), "", true},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		err := render(strings.NewReader(tc.input), &out)
		if (err != nil) != tc.fails {
			t.Errorf("render fails=%v: %v", tc.fails, err)
		}
		if !strings.Contains(out.String(), tc.want) || strings.Contains(out.String(), "private reasoning") {
			t.Errorf("unexpected rendering %q", out.String())
		}
	}
}

func TestRunValidation(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--not-a-flag"}, {"unexpected"}, {"--model", "auto"}, {"--model", "openai/test", "--timeout", "0s"},
		{"--model", "openai/test", "--thinking", "invalid"}} {
		var out bytes.Buffer
		code := Run(args, strings.NewReader(""), &out, &out)
		if len(args) == 1 && args[0] == "--help" {
			if code != 0 {
				t.Fatal("help failed")
			}
		} else if code == 0 {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestIsolatedTurnAndCheck(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	state := filepath.Join(root, "state")
	credentials(t, config, providerConfig)
	t.Setenv("CLINE_HUB_ADDRESS", "http://must-not-inherit.invalid")
	t.Setenv("CLINE_MCP_SETTINGS_PATH", "/must-not-inherit")
	binary := fakeCLI(t, `
test "$CLINE_SESSION_BACKEND_MODE" = local || exit 10
test -z "$CLINE_HUB_ADDRESS" || exit 11
test "$CLINE_MCP_SETTINGS_PATH" != /must-not-inherit || exit 12
grep -q '"mcpServers":{}' "$CLINE_MCP_SETTINGS_PATH" || exit 13
read -r prompt
test "$prompt" = "bounded task" || exit 14
printf '%s\n' '{"type":"agent_event","event":{"type":"iteration_start"}}'
printf '%s\n' '{"type":"run_result","finishReason":"completed","text":"done","usage":{"inputTokens":3}}'
`)
	opts := options{binary: binary, configDir: config, stateDir: state, cwd: root, model: "openrouter/anthropic/test", timeout: time.Minute}
	var out, stderr bytes.Buffer
	if err := run(opts, strings.NewReader("bounded task\n"), &out, &stderr); err != nil {
		t.Fatal(err, stderr.String())
	}
	if !strings.Contains(out.String(), "done") || !strings.Contains(out.String(), "CLINE_USAGE") {
		t.Fatalf("output %s", &out)
	}
	entries, err := os.ReadDir(state)
	if err != nil || len(entries) != 1 || entries[0].Name() != "usage" {
		t.Fatalf("private state leaked: %v %v", entries, err)
	}
	opts.check = true
	if err := run(opts, nil, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	opts.check = false
	opts.exportConfig = true
	out.Reset()
	if err := run(opts, nil, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out.Bytes()) || strings.Contains(out.String(), "must-not-copy") {
		t.Fatal("unsafe credential export")
	}
	opts.exportConfig = false
	opts.stageConfig = filepath.Join(root, "staged")
	if err := run(opts, nil, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	staged, err := os.ReadFile(filepath.Join(opts.stageConfig, "data", "settings", "providers.json"))
	if err != nil || strings.Contains(string(staged), "must-not-copy") {
		t.Fatalf("stage: %v", err)
	}
}

func TestTurnFailuresAndTimeout(t *testing.T) {
	root := t.TempDir()
	credentials(t, root, providerConfig)
	for _, body := range []string{"exit 7", "echo invalid", "exit 0", "printf '%s\\n' '{\"type\":\"run_result\",\"finishReason\":\"aborted\"}'"} {
		opts := options{binary: fakeCLI(t, body), configDir: root, stateDir: filepath.Join(root, "state"), cwd: root, model: "openrouter/test", timeout: time.Minute}
		if err := run(opts, strings.NewReader("task"), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Errorf("accepted failed CLI %q", body)
		}
	}
	opts := options{binary: fakeCLI(t, "sleep 30"), configDir: root, stateDir: filepath.Join(root, "state"), cwd: root, model: "openrouter/test", timeout: 100 * time.Millisecond}
	start := time.Now()
	if err := run(opts, strings.NewReader("task"), &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("timeout: %v", err)
	}
	if time.Since(start) > cancelGrace+2*time.Second {
		t.Fatal("cancellation did not bound process lifetime")
	}
	opts.binary = fakeCLI(t, "exit 0")
	opts.timeout = time.Minute
	opts.promptFile = filepath.Join(root, "missing")
	if err := run(opts, nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("missing prompt accepted")
	}
	prompt := filepath.Join(root, "prompt")
	if err := os.WriteFile(prompt, []byte("task"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts.promptFile = prompt
	opts.binary = fakeCLI(t, "cat >/dev/null; echo '{\"type\":\"run_result\",\"finishReason\":\"completed\",\"text\":\"done\"}'")
	if err := run(opts, nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}
