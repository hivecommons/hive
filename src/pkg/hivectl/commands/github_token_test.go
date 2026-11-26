package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// assertNoToken fails when the token appears in any captured output.
func assertNoToken(t *testing.T, name, token string, err error, outputs ...string) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), token) {
		t.Fatalf("%s: the token was echoed in the error: %q", name, err)
	}
	for _, out := range outputs {
		if strings.Contains(out, token) {
			t.Fatalf("%s: the token was echoed in output: %q", name, out)
		}
	}
}

// fakeTerminal makes stdin look like a TTY whose hidden read returns line.
// The hooks are package globals, so tests using it do not run in parallel.
func fakeTerminal(t *testing.T, line string, readErr error) *atomic.Int32 {
	t.Helper()
	oldFd, oldRead := tokenTerminalFd, readHiddenLine
	t.Cleanup(func() { tokenTerminalFd, readHiddenLine = oldFd, oldRead })
	var reads atomic.Int32
	tokenTerminalFd = func(io.Reader) (uintptr, bool) { return 0, true }
	readHiddenLine = func(uintptr) ([]byte, error) {
		reads.Add(1)
		return []byte(line), readErr
	}
	return &reads
}

func TestGitHubTokenSetReadsStdinAndSendsJSON(t *testing.T) {
	var got map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/github-token" || r.Method != http.MethodPut {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("Content-Type = %q", ct)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"mode":"pat","fingerprint":"abc123def456","login":"octo"}`)
	}))
	defer server.Close()

	stdout, stderr, err := execute(t, server, "ghp_secret\n", "--output", "json", "github", "token", "set", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	if got["token"] != "ghp_secret" {
		t.Fatalf("body = %v", got)
	}
	if !strings.Contains(stdout, `"login": "octo"`) {
		t.Fatalf("stdout = %q", stdout)
	}
	assertNoToken(t, "stdin", "ghp_secret", nil, stdout, stderr)
}

// On a terminal the token is read with echo off, with or without --stdin, and
// only the prompt reaches the screen.
func TestGitHubTokenSetPromptsWithoutEcho(t *testing.T) {
	for _, args := range [][]string{
		{"github", "token", "set"},
		{"github", "token", "set", "--stdin"},
	} {
		reads := fakeTerminal(t, "ghp_typed", nil)
		var got map[string]string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"mode":"pat","login":"octo"}`)
		}))
		stdout, stderr, err := execute(t, server, "ignored-because-terminal", args...)
		server.Close()
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if reads.Load() != 1 || got["token"] != "ghp_typed" {
			t.Fatalf("%v: hidden reads = %d, body = %v", args, reads.Load(), got)
		}
		if !strings.Contains(stderr, githubTokenPrompt) {
			t.Fatalf("%v: stderr = %q, want the prompt", args, stderr)
		}
		assertNoToken(t, strings.Join(args, " "), "ghp_typed", nil, stdout, stderr)
	}
}

func TestGitHubTokenSetTerminalReadFailure(t *testing.T) {
	fakeTerminal(t, "", errors.New("tty gone"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("API must not be called, got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	if _, _, err := execute(t, server, "", "github", "token", "set"); err == nil || !strings.Contains(err.Error(), "tty gone") {
		t.Fatalf("err = %v", err)
	}
}

// Every way of putting the token on the command line is refused before any
// request, with an error that never repeats it: positional arguments, flag
// values, unknown flags shaped like the token, and the removed --file flag.
func TestGitHubTokenSetRejectsBadInputWithoutEchoOrCallingAPI(t *testing.T) {
	const token = "ghp_argvSecret123"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("API must not be called, got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	for name, tc := range map[string]struct {
		input string
		args  []string
	}{
		"no source, not a terminal": {token, []string{"github", "token", "set"}},
		"empty":                     {"  \n", []string{"github", "token", "set", "--stdin"}},
		"whitespace":                {"two words\n", []string{"github", "token", "set", "--stdin"}},
		"argv token":                {"", []string{"github", "token", "set", token}},
		"argv token after stdin":    {"", []string{"github", "token", "set", "--stdin", token}},
		"argv after --":             {"", []string{"github", "token", "set", "--", token}},
		"bool flag value":           {"", []string{"github", "token", "set", "--stdin=" + token}},
		"token as long flag":        {"", []string{"github", "token", "set", "--" + token}},
		"token as shorthand":        {"", []string{"github", "token", "set", "-" + token}},
		"unknown --token flag":      {"", []string{"github", "token", "set", "--token", token}},
		"unknown --token= flag":     {"", []string{"github", "token", "set", "--token=" + token}},
		"removed --file flag":       {"", []string{"github", "token", "set", "--file", token}},
		"global flag value":         {"", []string{"github", "token", "set", "--timeout", token}},
	} {
		_, stderr, err := execute(t, server, tc.input, tc.args...)
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		if ExitCode(err) != ExitUsage {
			t.Fatalf("%s: exit code = %d, want %d (err %v)", name, ExitCode(err), ExitUsage, err)
		}
		assertNoToken(t, name, token, err, stderr)
	}
}

// Server error text is printed, but never the token, even when a server or
// proxy quotes the request body back. The exit code is still the API one.
func TestGitHubTokenSetSurfacesRefusalsWithoutToken(t *testing.T) {
	const token = "ghp_refusedToken"
	for _, status := range []int{http.StatusUnprocessableEntity, http.StatusTooManyRequests, http.StatusConflict, http.StatusForbidden, http.StatusInternalServerError} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "refused for test: " + string(body)})
		}))
		stdout, stderr, err := execute(t, server, token, "github", "token", "set", "--stdin")
		server.Close()
		if err == nil || !strings.Contains(err.Error(), "refused for test") || !strings.Contains(err.Error(), redactedToken) {
			t.Fatalf("status %d: err = %v", status, err)
		}
		if ExitCode(err) == ExitUsage || ExitCode(err) == ExitSuccess {
			t.Fatalf("status %d: exit code = %d", status, ExitCode(err))
		}
		assertNoToken(t, http.StatusText(status), token, err, stdout, stderr)
	}
}

// A 2xx response that carries the token is not printed.
func TestGitHubTokenSetRefusesToPrintAnEchoedToken(t *testing.T) {
	const token = "ghp_echoedBack"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()
	stdout, stderr, err := execute(t, server, token, "--output", "json", "github", "token", "set", "--stdin")
	if err == nil {
		t.Fatal("expected an error for a response that echoes the token")
	}
	assertNoToken(t, "echo", token, err, stdout, stderr)
}

// The token goes to the configured server or nowhere: a redirect to another
// host is not followed and the target never sees the body.
func TestGitHubTokenSetNeverFollowsRedirects(t *testing.T) {
	const token = "ghp_redirectToken"
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		var hits atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/api/github-token", status)
		}))
		stdout, stderr, err := execute(t, origin, token, "github", "token", "set", "--stdin")
		origin.Close()
		target.Close()
		if err == nil {
			t.Fatalf("status %d: expected an error", status)
		}
		if n := hits.Load(); n != 0 {
			t.Fatalf("status %d: redirect target received %d request(s)", status, n)
		}
		assertNoToken(t, http.StatusText(status), token, err, stdout, stderr)
	}
}

// Plain http to a non-loopback server is refused before any connection.
func TestGitHubTokenSetRefusesPlainHTTPToRemoteServer(t *testing.T) {
	const token = "ghp_plainHTTP"
	var stdout, stderr bytes.Buffer
	command := NewRootCommand(strings.NewReader(token), &stdout, &stderr)
	command.SetArgs([]string{"--server", "http://hive.invalid:3001", "github", "token", "set", "--stdin"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v, want a refusal naming https", err)
	}
	if ExitCode(err) != ExitUsage {
		t.Fatalf("exit code = %d, want %d", ExitCode(err), ExitUsage)
	}
	assertNoToken(t, "plain http", token, err, stdout.String(), stderr.String())
}

func TestScrubTokenKeepsNilAndWrapsErrors(t *testing.T) {
	if scrubToken(nil, "x") != nil {
		t.Fatal("nil error must stay nil")
	}
	base := &usageError{message: "a ghp_z b"}
	if got := scrubToken(base, ""); got != error(base) {
		t.Fatalf("empty token must return the error unchanged, got %v", got)
	}
	wrapped := scrubToken(base, "ghp_z")
	var usage *usageError
	if !errors.As(wrapped, &usage) || wrapped.Error() != "a "+redactedToken+" b" {
		t.Fatalf("wrapped = %v", wrapped)
	}
}

func TestRedactTokens(t *testing.T) {
	for in, want := range map[string]string{
		`unknown command "ghp_abc123" for "hivectl"`: `unknown command "` + redactedToken + `" for "hivectl"`,
		"github_pat_11ABC_def and gho_x and ghs_y":   redactedToken + " and " + redactedToken + " and " + redactedToken,
		"ghu_a ghr_b": redactedToken + " " + redactedToken,
		"invalid --output \"table2\"; nothing secret":  "invalid --output \"table2\"; nothing secret",
		"ghx_notatoken and gh_alone stay as they were": "ghx_notatoken and gh_alone stay as they were",
	} {
		if got := RedactTokens(in); got != want {
			t.Errorf("RedactTokens(%q) = %q, want %q", in, got, want)
		}
	}
}

// Root-level flag and argument errors outside `github token set` redact
// token-shaped values too.
func TestRootErrorsRedactTokenShapedValues(t *testing.T) {
	const token = "ghp_rootLevel"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("API must not be called, got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	for name, args := range map[string][]string{
		"global flag value":  {"--timeout", token, "system", "status"},
		"status argv":        {"github", "token", "status", token},
		"unknown root flag":  {"--" + token},
		"other command argv": {"system", "status", token},
	} {
		_, stderr, err := execute(t, server, "", args...)
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		assertNoToken(t, name, token, err, stderr)
	}
}

func TestGitHubTokenStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/github-token" || r.Method != http.MethodGet {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"mode":"pat","configured":true,"fingerprint":"abc123def456"}`)
	}))
	defer server.Close()
	stdout, _, err := execute(t, server, "", "--output", "json", "github", "token", "status")
	if err != nil || !strings.Contains(stdout, "abc123def456") {
		t.Fatalf("stdout = %q err = %v", stdout, err)
	}
}
