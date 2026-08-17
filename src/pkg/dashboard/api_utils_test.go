package dashboard

import (
	"net/http/httptest"
	"strings"
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
