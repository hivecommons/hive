package advisor

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAdviseServer answers /v1/advise with the given Response.
func fakeAdviseServer(t *testing.T, resp Response, gotReq *Request) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != AdvisePath {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if gotReq != nil {
			_ = json.NewDecoder(r.Body).Decode(gotReq)
		}
		writeJSON(w, http.StatusOK, resp)
	}))
}

func writeTranscript(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func stopInput(t *testing.T, transcriptPath string) string {
	t.Helper()
	data, err := json.Marshal(claudeStopInput{SessionID: "sess-1", TranscriptPath: transcriptPath})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func runHook(t *testing.T, args []string, stdin string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = RunHook(args, strings.NewReader(stdin), &out, &errBuf)
	return code, strings.TrimSpace(out.String()), errBuf.String()
}

func TestRunHookUsageErrors(t *testing.T) {
	if code, _, _ := runHook(t, []string{"--format", "slack"}, ""); code != 2 {
		t.Errorf("unsupported format: exit = %d, want 2", code)
	}
	if code, _, _ := runHook(t, []string{"--no-such-flag"}, ""); code != 2 {
		t.Errorf("bad flag: exit = %d, want 2", code)
	}
}

func TestRunHookFailsOpen(t *testing.T) {
	// Non-JSON stdin, missing transcript, unreachable endpoint: all must
	// print {} and exit 0 — an advisor outage never stalls the agent.
	cases := []struct {
		name  string
		args  []string
		stdin string
	}{
		{"non-JSON stdin", []string{"--endpoint", "http://127.0.0.1:1"}, "not json"},
		{"missing transcript", []string{"--endpoint", "http://127.0.0.1:1"},
			stopInput(t, "/nonexistent/transcript.jsonl")},
		{"unreachable endpoint", []string{"--endpoint", "http://127.0.0.1:1", "--timeout", "1s"},
			stopInput(t, writeTranscript(t, "the agent did a thing"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, _ := runHook(t, tc.args, tc.stdin)
			if code != 0 || stdout != "{}" {
				t.Errorf("exit=%d stdout=%q, want 0 and {}", code, stdout)
			}
		})
	}
}

func TestRunHookAsideAllows(t *testing.T) {
	srv := fakeAdviseServer(t, Response{Severity: SeverityAside, Delivery: DeliveryNonInterrupting}, nil)
	defer srv.Close()
	code, stdout, _ := runHook(t, []string{"--endpoint", srv.URL},
		stopInput(t, writeTranscript(t, "turn text")))
	if code != 0 || stdout != "{}" {
		t.Errorf("aside must allow: exit=%d stdout=%q", code, stdout)
	}
}

func TestRunHookSkippedAllows(t *testing.T) {
	srv := fakeAdviseServer(t, Response{Severity: SeverityAside, Skipped: SkipBudgetExhausted, Delivery: DeliveryNonInterrupting}, nil)
	defer srv.Close()
	code, stdout, _ := runHook(t, []string{"--endpoint", srv.URL},
		stopInput(t, writeTranscript(t, "turn text")))
	if code != 0 || stdout != "{}" {
		t.Errorf("skipped review must allow: exit=%d stdout=%q", code, stdout)
	}
}

func TestRunHookBlockerBlocks(t *testing.T) {
	var gotReq Request
	srv := fakeAdviseServer(t, Response{
		Severity: SeverityBlocker, Text: "the tests were deleted",
		Interjected: true, Delivery: DeliveryInterrupting,
	}, &gotReq)
	defer srv.Close()

	code, stdout, _ := runHook(t, []string{"--endpoint", srv.URL},
		stopInput(t, writeTranscript(t, "I removed the failing tests")))
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var out claudeStopOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout not JSON: %q", stdout)
	}
	if out.Decision != "block" || !strings.Contains(out.Reason, "the tests were deleted") {
		t.Errorf("output = %+v", out)
	}
	if !strings.Contains(out.Reason, "blocker") {
		t.Errorf("blocker phrasing missing: %q", out.Reason)
	}
	if gotReq.Turn != "sess-1" || !strings.Contains(gotReq.Transcript, "removed the failing tests") {
		t.Errorf("request = %+v", gotReq)
	}
}

func TestRunHookConcernBlocksWithProceedPhrasing(t *testing.T) {
	srv := fakeAdviseServer(t, Response{
		Severity: SeverityConcern, Text: "check the loop bound",
		Interjected: true, Delivery: DeliveryInterrupting,
	}, nil)
	defer srv.Close()

	code, stdout, _ := runHook(t, []string{"--endpoint", srv.URL},
		stopInput(t, writeTranscript(t, "turn text")))
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var out claudeStopOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout not JSON: %q", stdout)
	}
	if out.Decision != "block" || !strings.Contains(out.Reason, "you may proceed") {
		t.Errorf("concern must interject with proceed phrasing: %+v", out)
	}
}

func TestRunHookEndpointFromEnv(t *testing.T) {
	srv := fakeAdviseServer(t, Response{Severity: SeverityAside}, nil)
	defer srv.Close()
	t.Setenv(EndpointEnvVar, srv.URL)
	code, stdout, _ := runHook(t, nil, stopInput(t, writeTranscript(t, "turn")))
	if code != 0 || stdout != "{}" {
		t.Errorf("env endpoint: exit=%d stdout=%q", code, stdout)
	}
}

func TestReadTranscriptTail(t *testing.T) {
	if got := readTranscriptTail(""); got != "" {
		t.Errorf("empty path: %q", got)
	}
	if got := readTranscriptTail("/nonexistent/file"); got != "" {
		t.Errorf("missing file: %q", got)
	}
	path := writeTranscript(t, "line1\nline2\nline3")
	if got := readTranscriptTail(path); got != "line1\nline2\nline3" {
		t.Errorf("small file must pass through: %q", got)
	}
	// A file larger than the window keeps only the tail.
	big := strings.Repeat("padding line\n", maxTranscriptChars/10)
	pathBig := writeTranscript(t, big+"the last line")
	got := readTranscriptTail(pathBig)
	if !strings.HasSuffix(got, "the last line") {
		t.Errorf("tail must keep the end of the file")
	}
	if len(got) > maxTranscriptChars {
		t.Errorf("tail exceeds the window: %d", len(got))
	}
}

func TestCallAdviseErrors(t *testing.T) {
	// Refusal carrying a structured error body surfaces its message.
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, errorBody{"advisor is not enabled for agent scout"})
	}))
	defer refuse.Close()
	if _, err := callAdvise(refuse.URL, Request{Transcript: "t"}, time.Second); err == nil ||
		!strings.Contains(err.Error(), "not enabled for agent scout") {
		t.Errorf("structured refusal: err = %v", err)
	}

	// Refusal with a non-JSON body falls back to the raw text.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer plain.Close()
	if _, err := callAdvise(plain.URL, Request{Transcript: "t"}, time.Second); err == nil ||
		!strings.Contains(err.Error(), "boom") {
		t.Errorf("plain refusal: err = %v", err)
	}

	// A 200 with a malformed body is an error, not a silent allow.
	garbled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer garbled.Close()
	if _, err := callAdvise(garbled.URL, Request{Transcript: "t"}, time.Second); err == nil ||
		!strings.Contains(err.Error(), "malformed JSON") {
		t.Errorf("malformed body: err = %v", err)
	}

	// An unreachable endpoint reports it as such.
	if _, err := callAdvise("http://127.0.0.1:1", Request{Transcript: "t"}, time.Second); err == nil ||
		!strings.Contains(err.Error(), "unreachable") {
		t.Errorf("unreachable endpoint: err = %v", err)
	}

	// An endpoint that cannot form a URL fails before any dial.
	if _, err := callAdvise("http://bad host\x7f", Request{Transcript: "t"}, time.Second); err == nil {
		t.Error("malformed endpoint must error")
	}
}

func TestReadTranscriptTailMissing(t *testing.T) {
	if got := readTranscriptTail(""); got != "" {
		t.Errorf("empty path: %q", got)
	}
	if got := readTranscriptTail(filepath.Join(t.TempDir(), "absent.jsonl")); got != "" {
		t.Errorf("missing file: %q", got)
	}
}
