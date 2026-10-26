package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestSanitizeFilenameComponent(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello-world", "hello-world"},
		{"hello world", "hello-world"},
		{"test_file", "test_file"},
		{"../../etc/passwd", "------etc-passwd"},
		{"<script>alert(1)</script>", "-script-alert-1---script-"},
		{"", ""},
		{"abc123", "abc123"},
	}
	for _, tt := range tests {
		got := sanitizeFilenameComponent(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeFilenameComponent(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDecodeBodyTooLarge(t *testing.T) {
	large := strings.Repeat("x", 2<<20)
	req := httptest.NewRequest("POST", "/api/test", strings.NewReader(large))
	var result struct{}
	if err := decodeBody(req, &result); err == nil {
		t.Error("expected error for oversized body")
	}
}

type failingWriter struct {
	header http.Header
	err    error
}

func (f *failingWriter) Header() http.Header        { return f.header }
func (f *failingWriter) Write([]byte) (int, error)  { return 0, f.err }
func (f *failingWriter) WriteHeader(statusCode int) {}

func TestIsClientDisconnect(t *testing.T) {
	for _, err := range []error{
		&net.OpError{Op: "write", Err: os.NewSyscallError("write", syscall.EPIPE)},
		fmt.Errorf("wrap: %w", syscall.ECONNRESET),
		context.Canceled,
		http.ErrHandlerTimeout,
		net.ErrClosed,
	} {
		if !isClientDisconnect(err) {
			t.Errorf("%v should be a client disconnect", err)
		}
	}
	if isClientDisconnect(&json.UnsupportedTypeError{}) || isClientDisconnect(errors.New("boom")) {
		t.Error("genuine encode errors must not be client disconnects")
	}
}

func TestJSONResponseLogLevels(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	jsonResponse(&failingWriter{header: http.Header{}, err: syscall.EPIPE}, map[string]string{"a": "b"})
	if strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "level=DEBUG") {
		t.Errorf("broken pipe should log at DEBUG only, got:\n%s", buf.String())
	}

	buf.Reset()
	jsonResponse(&failingWriter{header: http.Header{}}, map[string]interface{}{"c": make(chan int)})
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("marshal failure should log at WARN, got:\n%s", buf.String())
	}
}
