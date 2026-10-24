package proxy

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// TestStartInferenceTranslatorPortBusyReturnsError pins the error contract of
// StartInferenceTranslator: when the fixed local port (InferenceTranslatePort)
// cannot be bound, the function returns ListenAndServe's error to its caller
// instead of swallowing it. That non-nil return is what lets the composition
// root notice the translator never came up rather than assuming a silent
// goroutine is serving.
//
// The test holds the port itself so the bind inside StartInferenceTranslator
// must fail. On a host where a live proxy already occupies the port, the
// test's own bind fails instead — and StartInferenceTranslator still errors
// for the same reason — so the assertion holds either way without touching
// any live listener. The call runs in a goroutine with a timeout only as a
// backstop: if some pathological environment let the server actually start,
// the test fails loudly rather than hanging the suite.
func TestStartInferenceTranslatorPortBusyReturnsError(t *testing.T) {
	addr := fmt.Sprintf("127.0.0.1:%d", InferenceTranslatePort)
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		// We won the port; keep it held until the assertion below completes.
		defer func() { _ = ln.Close() }()
	}

	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	errCh := make(chan error, 1)
	go func() { errCh <- p.StartInferenceTranslator() }()

	select {
	case startErr := <-errCh:
		if startErr == nil {
			t.Fatal("StartInferenceTranslator returned nil error while the port was busy")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StartInferenceTranslator did not return within 5s with the port busy")
	}
}
