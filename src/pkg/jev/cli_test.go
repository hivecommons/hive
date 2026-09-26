package jev

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRun_RefusedWhenModeOff: with HIVE_JEV_MODE unset (the default) the CLI
// exits 1 with a pointer at jev_mode and never contacts an endpoint.
func TestRun_RefusedWhenModeOff(t *testing.T) {
	t.Setenv(ModeEnvVar, "")
	called := false
	ep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer ep.Close()
	var out, errb bytes.Buffer
	code := Run([]string{"decide", "--question", "q", "--option", "a", "--option", "b", "--endpoint", ep.URL}, strings.NewReader(""), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "jev_mode: assist") || called {
		t.Fatalf("code=%d called=%v stderr=%s", code, called, errb.String())
	}
}

func TestRun_UsageErrors(t *testing.T) {
	t.Setenv(ModeEnvVar, "assist")
	for name, args := range map[string][]string{
		"no args":         nil,
		"unknown sub":     {"frob"},
		"missing q":       {"decide", "--option", "a", "--option", "b"},
		"two state flags": {"decide", "--question", "q", "--option", "a", "--option", "b", "--state", "{}", "--state-stdin"},
		"one option":      {"decide", "--question", "q", "--option", "a", "--endpoint", "http://127.0.0.1:1"},
		"positional":      {"decide", "--question", "q", "extra"},
	} {
		var out, errb bytes.Buffer
		if code := Run(args, strings.NewReader(""), &out, &errb); code != 2 {
			t.Errorf("%s: code=%d stderr=%s", name, code, errb.String())
		}
	}
}

// TestRun_DecideRoundTrip drives the real CLI against a fake decision endpoint:
// the request carries the parsed flags (option descriptions, levels, stdin
// state, no identity header), and the endpoint's JSON is printed as-is.
func TestRun_DecideRoundTrip(t *testing.T) {
	t.Setenv(ModeEnvVar, "assist")
	var got Request
	var gotAuth string
	ep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != DecidePath || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Proxy-Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(Result{Answer: "1.5", Confidence: 0.6, Model: "m", InputTokens: 9})
	}))
	defer ep.Close()
	t.Setenv(EndpointEnvVar, ep.URL)

	var out, errb bytes.Buffer
	code := Run([]string{"decide", "--type", "score", "--question", "risk?", "--level", "low", "--level", "high", "--state-stdin"},
		strings.NewReader(`{"diff":"x"}`), &out, &errb)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errb.String())
	}
	if got.Type != TypeScore || len(got.Levels) != 2 || string(got.State) != `{"diff":"x"}` {
		t.Errorf("request = %+v", got)
	}
	if gotAuth != "" {
		t.Errorf("CLI must not assert an identity header (UID is the only identity): %q", gotAuth)
	}
	var res Result
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Answer != "1.5" || res.InputTokens != 9 {
		t.Errorf("stdout = %s (%v)", out.String(), err)
	}

	// name=description options become Descriptions.
	out.Reset()
	code = Run([]string{"decide", "--question", "which?", "--option", "a=first thing", "--option", "b"}, strings.NewReader(""), &out, &errb)
	if code != 0 || got.Options[0] != "a" || got.Descriptions["a"] != "first thing" || len(got.Descriptions) != 1 {
		t.Errorf("code=%d request=%+v", code, got)
	}
}

// TestRun_EndpointRefusalIsExit1: a 403 from the hive (jev_mode off server-side)
// surfaces the server's reason and exits 1, not 2.
func TestRun_EndpointRefusalIsExit1(t *testing.T) {
	t.Setenv(ModeEnvVar, "assist")
	ep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, errorBody{"jev_mode is off for agent scanner"})
	}))
	defer ep.Close()
	var out, errb bytes.Buffer
	code := Run([]string{"decide", "--question", "q", "--option", "a", "--option", "b", "--endpoint", ep.URL}, strings.NewReader(""), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "jev_mode is off for agent scanner") || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%s", code, out.String(), errb.String())
	}
}

// TestRun_HelpIsExit0: `decide --help` prints usage and exits 0 — it is not a
// usage error like a bad flag.
func TestRun_HelpIsExit0(t *testing.T) {
	t.Setenv(ModeEnvVar, "assist")
	var out, errb bytes.Buffer
	if code := Run([]string{"decide", "--help"}, strings.NewReader(""), &out, &errb); code != 0 || !strings.Contains(errb.String(), "--question") {
		t.Fatalf("code=%d stderr=%s", code, errb.String())
	}
	errb.Reset()
	if code := Run([]string{"decide", "--no-such-flag"}, strings.NewReader(""), &out, &errb); code != 2 || !strings.Contains(errb.String(), "no-such-flag") {
		t.Fatalf("code=%d stderr=%s", code, errb.String())
	}
}

// TestRun_StateSources: --state-file and --state each feed the request state;
// --criteria is passed verbatim; a missing state file is a usage error (2)
// raised before any endpoint call.
func TestRun_StateSources(t *testing.T) {
	t.Setenv(ModeEnvVar, "assist")
	var got Request
	calls := 0
	ep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		got = Request{}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("endpoint: bad JSON: %v", err)
		}
		writeJSON(w, http.StatusOK, Result{Answer: "0.5"})
	}))
	defer ep.Close()
	t.Setenv(EndpointEnvVar, ep.URL)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(stateFile, []byte(`{"from":"file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Run([]string{"decide", "--type", "probability", "--question", "q?", "--state-file", stateFile, "--criteria", `{"true":"t","false":"f"}`}, strings.NewReader(""), &out, &errb)
	if code != 0 || string(got.State) != `{"from":"file"}` || string(got.Criteria) != `{"true":"t","false":"f"}` || got.Type != TypeProbability {
		t.Fatalf("code=%d stderr=%s request=%+v", code, errb.String(), got)
	}
	code = Run([]string{"decide", "--type", "probability", "--question", "q?", "--state", `{"inline":true}`}, strings.NewReader(""), &out, &errb)
	if code != 0 || string(got.State) != `{"inline":true}` {
		t.Fatalf("code=%d stderr=%s request=%+v", code, errb.String(), got)
	}

	errb.Reset()
	before := calls
	code = Run([]string{"decide", "--type", "probability", "--question", "q?", "--state-file", filepath.Join(t.TempDir(), "missing.json")}, strings.NewReader(""), &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "--state-file") || calls != before {
		t.Fatalf("missing state file: code=%d calls=%d stderr=%s", code, calls-before, errb.String())
	}
	// Invalid state from a file is caught by Validate, still before the call.
	if err := os.WriteFile(stateFile, []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	code = Run([]string{"decide", "--type", "probability", "--question", "q?", "--state-file", stateFile}, strings.NewReader(""), &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "valid JSON") || calls != before {
		t.Fatalf("invalid state file: code=%d calls=%d stderr=%s", code, calls-before, errb.String())
	}
}

// TestRun_EndpointFailures: a refusal without a JSON error body is reported
// with the raw text, a 200 with malformed JSON is an error, and an
// unreachable endpoint is named as such. All exit 1.
func TestRun_EndpointFailures(t *testing.T) {
	t.Setenv(ModeEnvVar, "assist")
	t.Setenv(EndpointEnvVar, "")
	var body string
	var status int
	ep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	args := []string{"decide", "--question", "q", "--option", "a", "--option", "b", "--endpoint", ep.URL}

	body, status = "plain text refusal", http.StatusForbidden
	var out, errb bytes.Buffer
	if code := Run(args, strings.NewReader(""), &out, &errb); code != 1 || !strings.Contains(errb.String(), "HTTP 403") || !strings.Contains(errb.String(), "plain text refusal") {
		t.Fatalf("plain refusal: code=%d stderr=%s", code, errb.String())
	}

	body, status = `{"answer":`, http.StatusOK
	errb.Reset()
	if code := Run(args, strings.NewReader(""), &out, &errb); code != 1 || !strings.Contains(errb.String(), "malformed JSON") {
		t.Fatalf("malformed 200: code=%d stderr=%s", code, errb.String())
	}

	errb.Reset()
	ep.Close()
	if code := Run(args, strings.NewReader(""), &out, &errb); code != 1 || !strings.Contains(errb.String(), "unreachable") {
		t.Fatalf("closed endpoint: code=%d stderr=%s", code, errb.String())
	}
}
