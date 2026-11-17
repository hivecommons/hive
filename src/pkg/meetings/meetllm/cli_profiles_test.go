package meetllm

import (
	"bytes"
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// fakeProfileCLI writes a fake one-shot CLI into dir: it records its argv,
// environment, working directory (and what is in it) and stdin, writes
// usage to the path after --usage-output-file when given, prints stdout and
// exits with code.
func fakeProfileCLI(t *testing.T, dir, name, stdout, usage string, code int) string {
	t.Helper()
	script := `#!/bin/sh
D='` + dir + `'
printf '%s\n' "$@" > "$D/argv"
env > "$D/env"
pwd > "$D/cwd"
ls -A > "$D/cwdlist"
cat > "$D/stdin"
u=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--usage-output-file" ]; then u="$a"; fi
  prev="$a"
done
if [ -n "$u" ] && [ -n '` + usage + `' ]; then printf '%s' '` + usage + `' > "$u"; fi
printf '%s' '` + stdout + `'
exit ` + strconv.Itoa(code) + `
`
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func readEnv(t *testing.T, dir string) map[string]string {
	t.Helper()
	env := map[string]string{}
	for _, kv := range readLines(t, filepath.Join(dir, "env")) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	return env
}

const (
	copilotOK = `{"type":"session.start","data":{"selectedModel":"gpt-x"}}
{"type":"assistant.message","data":{"content":""}}
{"type":"assistant.message","data":{"content":"copilot reply"}}
{"type":"result","exitCode":0,"usage":{"premiumRequests":1}}
`
	copilotUsage = `{"modelMetrics":{"gpt-x":{"usage":{"inputTokens":120,"outputTokens":30,"cacheReadTokens":100}},"aux":{"usage":{"inputTokens":5,"outputTokens":1}}}}`
	// copilotUsageTokens is 120+30 + 5+1: inputTokens already holds the
	// cached tokens.
	copilotUsageTokens = 156
)

func copilotConfig(t *testing.T, dir, stdout, usage string, code int, extra map[string]string) Config {
	env := map[string]string{EnvCopilotToken: "gho_meetings", "GH_TOKEN": "ghp_hive", "HIVE_GITHUB_TOKEN": "x"}
	for k, v := range extra {
		env[k] = v
	}
	return Config{
		Provider: ProviderAgentBackend, Backend: BackendCopilot, Model: "gpt-x",
		CLIBinary: fakeProfileCLI(t, dir, "copilot", stdout, usage, code), CLIHome: filepath.Join(dir, "home"),
		Timeout: 5 * time.Second, MaxInFlight: 1, UsageFile: filepath.Join(dir, "usage.jsonl"),
		Getenv: testEnv(env),
	}
}

// copilot runs once per call with no tools, the prompt on stdin, its own
// throwaway home, the agents' token and the usage from its report.
func TestCompleteCLIOnceCopilot(t *testing.T) {
	dir := t.TempDir()
	c := copilotConfig(t, dir, copilotOK, copilotUsage, 0, nil)
	c.System = "answer briefly"
	reply, err := Complete(context.Background(), c, nil, "what is hive?")
	if err != nil || reply != "copilot reply" {
		t.Fatalf("Complete = %q %v", reply, err)
	}
	if n, _ := UsageTotal(c.UsageFile); n != copilotUsageTokens {
		t.Fatalf("usage = %d", n)
	}
	argv := readLines(t, filepath.Join(dir, "argv"))
	for _, seq := range [][]string{
		{"--available-tools", copilotNoToolSentinel}, {"--disable-builtin-mcps"}, {"--no-custom-instructions"}, {"--no-ask-user"},
		{"--no-auto-update"}, {"--output-format", "json"}, {"--model", "gpt-x"}, {"--usage-output-file"},
	} {
		if !containsSeq(argv, seq) {
			t.Errorf("argv %q lacks %q", argv, seq)
		}
	}
	for _, banned := range []string{"-p", "--allow-all", "--allow-all-tools", "--yolo"} {
		if containsSeq(argv, []string{banned}) {
			t.Errorf("argv %q has %q", argv, banned)
		}
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if !strings.HasPrefix(string(stdin), "<engine>\n"+cliSystemPrompt) || !strings.Contains(string(stdin), "<instructions>\nanswer briefly") ||
		!strings.Contains(string(stdin), "<request>\nwhat is hive?\n</request>") {
		t.Fatalf("stdin %q", stdin)
	}
	env := readEnv(t, dir)
	cwd := strings.TrimSpace(strings.Join(readLines(t, filepath.Join(dir, "cwd")), ""))
	// pwd may print the physical path (macOS /private/var), so compare the
	// temporary directory by name.
	state := env["HOME"]
	if filepath.Base(cwd) != cliWorkDir || filepath.Base(state) != cliStateDir || filepath.Base(filepath.Dir(state)) != filepath.Base(filepath.Dir(cwd)) ||
		env["COPILOT_HOME"] != state || env[EnvCopilotToken] != "gho_meetings" || env["NO_COLOR"] != "1" {
		t.Fatalf("cwd %q env %v", cwd, env)
	}
	for _, k := range []string{"GH_TOKEN", "HIVE_GITHUB_TOKEN"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s reached the CLI", k)
		}
	}
	if l := readLines(t, filepath.Join(dir, "cwdlist")); len(l) != 1 || l[0] != "" {
		t.Fatalf("working directory not empty: %q", l)
	}
	if _, err := os.Stat(filepath.Dir(cwd)); !os.IsNotExist(err) {
		t.Fatalf("temporary directory kept: %v", err)
	}
}

func TestCompleteCLIOnceCopilotFailures(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := copilotConfig(t, dir, copilotOK, "", 0, map[string]string{EnvCopilotToken: ""})
	if _, err := completeCLIOnce(ctx, c, "q"); err == nil || !strings.Contains(err.Error(), EnvCopilotToken) {
		t.Fatalf("no token err = %v", err)
	}
	quota := `{"type":"session.error","data":{"errorType":"quota","message":"over"}}` + "\n" + `{"type":"result","exitCode":1}`
	c = copilotConfig(t, t.TempDir(), quota, copilotUsage, 1, nil)
	if _, err := completeCLIOnce(ctx, c, "q"); err == nil || !strings.Contains(err.Error(), "(quota)") {
		t.Fatalf("quota err = %v", err)
	}
	if n, _ := UsageTotal(c.UsageFile); n != copilotUsageTokens {
		t.Fatalf("failed turn usage = %d", n)
	}
	c = copilotConfig(t, t.TempDir(), `{"type":"result","exitCode":3}`, "", 3, nil)
	if _, err := completeCLIOnce(ctx, c, "q"); err == nil || !strings.Contains(err.Error(), "(exit 3)") {
		t.Fatalf("exit err = %v", err)
	}
	c = copilotConfig(t, t.TempDir(), `{"type":"session.error","data":{}}`, "", 0, nil)
	if _, err := completeCLIOnce(ctx, c, "q"); err == nil || !strings.Contains(err.Error(), "(error)") {
		t.Fatalf("untyped error err = %v", err)
	}
	c = copilotConfig(t, t.TempDir(), "banner only", "", 1, nil)
	if _, err := completeCLIOnce(ctx, c, "q"); err == nil || !strings.Contains(err.Error(), "CLI failed") {
		t.Fatalf("garbage err = %v", err)
	}
	c = copilotConfig(t, t.TempDir(), `{"type":"result","exitCode":0}`, "not json", 0, nil)
	if _, err := completeCLIOnce(ctx, c, "q"); err == nil || !strings.Contains(err.Error(), "no text") {
		t.Fatalf("empty reply err = %v", err)
	}
	// Without a model copilot uses its own default: no --model is passed.
	dir = t.TempDir()
	c = copilotConfig(t, dir, copilotOK, "", 0, nil)
	c.Model = ""
	if reply, err := completeCLIOnce(ctx, c, "q"); err != nil || reply != "copilot reply" {
		t.Fatalf("no model = %q %v", reply, err)
	}
	if containsSeq(readLines(t, filepath.Join(dir, "argv")), []string{"--model"}) {
		t.Fatal("empty model passed")
	}
}

const ompOK = `{"type":"session","version":3}
{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"q"}]}}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"omp "},{"type":"text","text":"reply"}],"usage":{"input":10,"output":4,"cacheRead":0,"cacheWrite":0,"totalTokens":14},"stopReason":"stop"}}
{"type":"agent_end","messages":[]}
`

func ompConfig(t *testing.T, dir, stdout string, code int) Config {
	return Config{
		Provider: ProviderAgentBackend, Backend: BackendOMP, Model: "anthropic/claude-sonnet-5-5",
		CLIBinary: fakeProfileCLI(t, dir, "omp", stdout, "", code), CLIHome: filepath.Join(dir, "home"),
		Timeout: 5 * time.Second, MaxInFlight: 1, UsageFile: filepath.Join(dir, "usage.jsonl"),
		Getenv: testEnv(map[string]string{"OMP_AUTH_BROKER_URL": "https://broker", "OMP_AUTH_BROKER_TOKEN": "obt", "GH_TOKEN": "ghp"}),
	}
}

// omp runs once per call with no tools in an isolated profile, the prompt on
// stdin, and the usage from its assistant messages.
func TestCompleteCLIOnceOMP(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := ompConfig(t, dir, ompOK, 0)
	reply, err := completeCLIOnce(ctx, c, "what is hive?")
	if err != nil || reply != "omp reply" {
		t.Fatalf("omp = %q %v", reply, err)
	}
	if n, _ := UsageTotal(c.UsageFile); n != 14 {
		t.Fatalf("usage = %d", n)
	}
	argv := readLines(t, filepath.Join(dir, "argv"))
	for _, seq := range [][]string{
		{"-p", "--mode", "json", "--no-tools"}, {"--no-lsp"}, {"--no-extensions"}, {"--no-skills"}, {"--no-rules"}, {"--no-session"},
		{"--no-context-files"}, {"--profile", ompProfile}, {"--system-prompt", cliSystemPrompt}, {"--model", "anthropic/claude-sonnet-5-5"},
	} {
		if !containsSeq(argv, seq) {
			t.Errorf("argv %q lacks %q", argv, seq)
		}
	}
	env := readEnv(t, dir)
	if env["HOME"] != filepath.Join(dir, "home") || env["OMP_AUTH_BROKER_URL"] != "https://broker" || env["OMP_AUTH_BROKER_TOKEN"] != "obt" {
		t.Fatalf("env %v", env)
	}
	if _, ok := env["GH_TOKEN"]; ok {
		t.Fatal("GH_TOKEN reached omp")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "stdin")); strings.Contains(string(b), "<engine>") || !strings.Contains(string(b), "<request>\nwhat is hive?") {
		t.Fatalf("stdin %q", b)
	}

	failed := `{"type":"message_end","message":{"role":"assistant","content":[],"usage":{"input":3,"output":0,"cacheRead":2,"cacheWrite":1},"stopReason":"error"}}`
	c = ompConfig(t, t.TempDir(), failed, 1)
	if _, err := completeCLIOnce(ctx, c, "q"); err == nil || !strings.Contains(err.Error(), "(error)") {
		t.Fatalf("error turn err = %v", err)
	}
	if n, _ := UsageTotal(c.UsageFile); n != 6 {
		t.Fatalf("summed usage = %d", n)
	}
	c = ompConfig(t, t.TempDir(), `{"type":"session"}`, 0)
	if _, err := completeCLIOnce(ctx, c, "q"); err == nil || !strings.Contains(err.Error(), "no result") {
		t.Fatalf("no message err = %v", err)
	}
	c.Model = ""
	if _, err := completeCLIOnce(ctx, c, "q"); err == nil || !strings.Contains(err.Error(), "needs a model") {
		t.Fatalf("no model err = %v", err)
	}
}

// Only claude keeps a warm process; the profile list matches the config's.
func TestCompletionProfileCapabilities(t *testing.T) {
	if !SupportsWarm(BackendClaude) || SupportsWarm(BackendCopilot) || SupportsWarm(BackendOMP) || SupportsWarm("codex") {
		t.Fatal("warm capability")
	}
	if !HasCompletionProfile("Copilot") || HasCompletionProfile("gemini") {
		t.Fatal("profile lookup")
	}
	if got, want := WarmBackends(), config.MeetingsWarmCLIBackends; !reflect.DeepEqual(got, want) {
		t.Fatalf("warm %v, config %v", got, want)
	}
	if _, err := NewCLISession(Config{Backend: BackendCopilot}); err == nil || !strings.Contains(err.Error(), "no warm mode") {
		t.Fatalf("copilot session err = %v", err)
	}
	if _, err := (completionProfile{}).callEnviron(nil, "", "/s"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvCopilotToken, "")
	if _, err := completionProfiles[BackendCopilot].callEnviron(nil, "", "/s"); err == nil {
		t.Fatal("want the copilot token required")
	}
}

// `hive meeting-llm` runs copilot with no model and passes the sign-in
// variables it was given.
func TestRunCopilotOneShot(t *testing.T) {
	dir := t.TempDir()
	bin := fakeProfileCLI(t, dir, "copilot", copilotOK, copilotUsage, 0)
	usage := filepath.Join(dir, "usage.jsonl")
	env := testEnv(map[string]string{
		EnvBackend: "copilot", EnvCLIBinary: bin, EnvUsageFile: usage, EnvTimeoutMS: "5000", EnvCopilotToken: "gho_x",
	})
	var out, errb bytes.Buffer
	if code := Run(nil, strings.NewReader("p"), &out, &errb, env); code != 0 || out.String() != "copilot reply" {
		t.Fatalf("Run = %d %q %q", code, out.String(), errb.String())
	}
	if n, _ := UsageTotal(usage); n != copilotUsageTokens {
		t.Fatalf("usage = %d", n)
	}
	if readEnv(t, dir)[EnvCopilotToken] != "gho_x" {
		t.Fatal("token not passed")
	}
	if _, err := FromEnv(testEnv(map[string]string{EnvBackend: "omp"})); err == nil {
		t.Fatal("want omp to need a model")
	}
}

func TestHTTPClientCABundle(t *testing.T) {
	if c, err := HTTPClient(""); err != nil || c == nil || c.Transport != nil {
		t.Fatalf("default client %+v %v", c, err)
	}
	dir := t.TempDir()
	if _, err := HTTPClient(filepath.Join(dir, "absent.pem")); err == nil {
		t.Fatal("want a read error")
	}
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := HTTPClient(bad); err == nil {
		t.Fatal("want a PEM error")
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := HTTPClient(ca)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("private CA not trusted: %v", err)
	}
	_ = resp.Body.Close()
	var out, errb bytes.Buffer
	env := testEnv(map[string]string{EnvEndpoint: srv.URL, EnvModel: "m", EnvCABundle: bad})
	if code := Run(nil, strings.NewReader("p"), &out, &errb, env); code != 2 || !strings.Contains(errb.String(), "PEM") {
		t.Fatalf("Run with a bad CA bundle = %d %q", code, errb.String())
	}
}
