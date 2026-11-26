package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

	stdout, _, err := execute(t, server, "ghp_secret\n", "--output", "json", "github", "token", "set", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	if got["token"] != "ghp_secret" {
		t.Fatalf("body = %v", got)
	}
	if strings.Contains(stdout, "ghp_secret") || !strings.Contains(stdout, `"login": "octo"`) {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestGitHubTokenSetReadsFile(t *testing.T) {
	var got map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"mode":"pat"}`)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "pat")
	if err := os.WriteFile(path, []byte("  ghp_file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := execute(t, server, "", "github", "token", "set", "--file", path); err != nil {
		t.Fatal(err)
	}
	if got["token"] != "ghp_file" {
		t.Fatalf("body = %v", got)
	}
}

func TestGitHubTokenSetRejectsBadInputWithoutCallingAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("API must not be called, got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	for name, tc := range map[string]struct {
		input string
		args  []string
	}{
		"no source":    {"", []string{"github", "token", "set"}},
		"both sources": {"x", []string{"github", "token", "set", "--stdin", "--file", "f"}},
		"empty":        {"  \n", []string{"github", "token", "set", "--stdin"}},
		"whitespace":   {"two words\n", []string{"github", "token", "set", "--stdin"}},
		"argv token":   {"", []string{"github", "token", "set", "ghp_argv"}},
	} {
		_, stderr, err := execute(t, server, tc.input, tc.args...)
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		if strings.Contains(err.Error(), "ghp_argv") || strings.Contains(stderr, "ghp_argv") {
			t.Fatalf("%s: the argv token was echoed: err=%q stderr=%q", name, err, stderr)
		}
	}
}

func TestGitHubTokenSetSurfacesRefusals(t *testing.T) {
	for _, status := range []int{http.StatusUnprocessableEntity, http.StatusTooManyRequests, http.StatusConflict} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			io.WriteString(w, `{"error":"refused for test"}`)
		}))
		_, _, err := execute(t, server, "ghp_x", "github", "token", "set", "--stdin")
		server.Close()
		if err == nil || !strings.Contains(err.Error(), "refused for test") {
			t.Fatalf("status %d: err = %v", status, err)
		}
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
