package proxy

import (
	"errors"
	"strings"
	"testing"
)

func TestInferenceBackendErrorMessage(t *testing.T) {
	got := inferenceBackendErrorMessage(401, []byte("bad token"), "https://user:pw@gw.example/v1/chat/completions?k=1", "nginx")
	want := "inference backend returned 401: bad token (upstream: https://gw.example/v1/chat/completions, server: nginx)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	long := inferenceBackendErrorMessage(500, []byte(strings.Repeat("x", 500)), "", "")
	if !strings.HasPrefix(long, "inference backend returned 500: ") || strings.Contains(long, "upstream") {
		t.Fatalf("unexpected: %q", long)
	}
}

func TestInferenceBackendUnreachableMessage(t *testing.T) {
	got := inferenceBackendUnreachableMessage(errors.New("boom"), "http://gw:8000/v1/chat/completions")
	if got != "inference backend unreachable: boom (upstream: http://gw:8000/v1/chat/completions)" {
		t.Fatalf("got %q", got)
	}
}
