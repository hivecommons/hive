package commands

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
)

// TestClientRejectsInvalidOutputBeforeConnecting covers commandEnv.client()'s
// validOutput() usage-error branch.
func TestClientRejectsInvalidOutputBeforeConnecting(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, _, err := execute(t, server, "", "--output", "xml", "system", "status")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("err = %v, want usage error", err)
	}
	if !strings.Contains(err.Error(), "invalid --output") {
		t.Fatalf("err = %v", err)
	}
}

// TestExitCodeNilReturnsSuccess covers ExitCode's nil-error branch.
func TestExitCodeNilReturnsSuccess(t *testing.T) {
	if got := ExitCode(nil); got != ExitSuccess {
		t.Fatalf("ExitCode(nil) = %d, want %d", got, ExitSuccess)
	}
}

// TestExitCodeRequestTooLargeMapsToUsage covers ExitCode's
// RequestTooLargeError branch.
func TestExitCodeRequestTooLargeMapsToUsage(t *testing.T) {
	err := &hivectl.RequestTooLargeError{Size: 10, Limit: 5}
	if got := ExitCode(err); got != ExitUsage {
		t.Fatalf("ExitCode = %d, want %d", got, ExitUsage)
	}
}

// TestExitCodeConnectionErrorMapsToConnection covers ExitCode's
// ConnectionError branch.
func TestExitCodeConnectionErrorMapsToConnection(t *testing.T) {
	err := &hivectl.ConnectionError{Err: errors.New("boom")}
	if got := ExitCode(err); got != ExitConnection {
		t.Fatalf("ExitCode = %d, want %d", got, ExitConnection)
	}
}

// TestExitCodeDeadlineExceededMapsToConnection covers ExitCode's
// context.DeadlineExceeded branch.
func TestExitCodeDeadlineExceededMapsToConnection(t *testing.T) {
	if got := ExitCode(context.DeadlineExceeded); got != ExitConnection {
		t.Fatalf("ExitCode = %d, want %d", got, ExitConnection)
	}
}

// TestExitCodeGenericErrorMapsToFailure covers ExitCode's fallback branch
// for an error that matches none of the known types.
func TestExitCodeGenericErrorMapsToFailure(t *testing.T) {
	if got := ExitCode(errors.New("unknown")); got != ExitFailure {
		t.Fatalf("ExitCode = %d, want %d", got, ExitFailure)
	}
}

// TestRequireYesAllowsWhenTrue covers requireYes's success branch.
func TestRequireYesAllowsWhenTrue(t *testing.T) {
	if err := requireYes(true, "example"); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

// TestValidOutputAllValues covers validOutput's true and false branches.
func TestValidOutputAllValues(t *testing.T) {
	for _, value := range []string{"table", "json", "yaml", "jsonl"} {
		if !validOutput(value) {
			t.Fatalf("validOutput(%q) = false, want true", value)
		}
	}
	if validOutput("bogus") {
		t.Fatal("validOutput(bogus) = true, want false")
	}
}

// TestSystemEventsSuccessfulStream covers system.go's events RunE full
// success path: --follow set, --output jsonl, and a real SSE stream.
func TestSystemEventsSuccessfulStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/events" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: status\ndata: {\"state\":\"ok\"}\n\n"))
	}))
	defer server.Close()

	stdout, _, err := execute(t, server, "", "--output", "jsonl", "system", "events", "--follow")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"state":"ok"`) {
		t.Fatalf("stdout = %q", stdout)
	}
}

// TestSystemEventsTimeoutTreatedAsCleanEnd covers the branch mapping
// context.DeadlineExceeded from StreamSSE to a nil error (an intentional
// stream end condition, not a failure).
func TestSystemEventsTimeoutTreatedAsCleanEnd(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if ok {
			flusher.Flush()
		}
		// Block until the client's timeout fires; the client cancels the
		// request context, so this handler just needs to hang briefly.
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	command := NewRootCommand(strings.NewReader(""), &stdout, &stderr)
	command.SetArgs([]string{"--server", server.URL, "--output", "jsonl", "--timeout", "50ms", "system", "events", "--follow"})
	err := command.Execute()
	if err != nil {
		t.Fatalf("err = %v, want nil (timeout treated as clean end)", err)
	}
}

// TestSystemHealthStatusVersionSnapshot covers the generated system
// subcommands (health/status/version/snapshot).
func TestSystemHealthStatusVersionSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "health", path: "/api/health"},
		{name: "status", path: "/api/status"},
		{name: "version", path: "/api/version"},
		{name: "snapshot", path: "/api/snapshot"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != tc.path {
				t.Fatalf("%s: path = %q", tc.name, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{}`))
		}))
		if _, _, err := execute(t, server, "", "system", tc.name); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		server.Close()
	}
}
