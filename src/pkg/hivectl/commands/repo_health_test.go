package commands

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const repoHealthFixture = `{"repo":"o/r","score":72.5,"status":"reported","window_days":90,"min_n":10,"computed_at":"2026-10-28T08:00:00Z",
"components":[
{"name":"ci_pass_rate","status":"reported","score":0.9,"weight":20,"contribution":30.5,"n":40,"inputs":{"passed":36,"total":40}},
{"name":"signoff_rate","status":"not_reported","score":null,"weight":10,"contribution":null,"n":0,"inputs":{},"reason":"no local data"}]}`

func TestRepoHealthCommandPrintsComponents(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, repoHealthFixture)
	}))
	defer server.Close()

	out, _, err := execute(t, server, "", "repo", "health", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/api/repos/o/r/health" {
		t.Fatalf("path = %q", path)
	}
	for _, want := range []string{
		"o/r  score 72.5 (reported)  window 90d  min n 10  computed 2026-10-28T08:00:00Z",
		"COMPONENT", "ci_pass_rate", "30.5", "signoff_rate", "not_reported", "no local data",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}

	out, _, err = execute(t, server, "", "repo", "health", "o/r", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"passed": 36`) {
		t.Fatalf("json output lacks component inputs:\n%s", out)
	}
}

func TestRepoHealthCommandDisabledAndErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/repos/o/off/health":
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"ok":false,"error":"repo health unavailable: search is not enabled"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"ok":false,"error":"no health computed for o/new yet: no review evidence indexed"}`)
		}
	}))
	defer server.Close()

	_, _, err := execute(t, server, "", "repo", "health", "o/off")
	if err == nil || !strings.Contains(err.Error(), "search is disabled on this hive") {
		t.Fatalf("disabled: err = %v", err)
	}
	if ExitCode(err) != ExitAPI {
		t.Fatalf("disabled: exit = %d", ExitCode(err))
	}
	_, _, err = execute(t, server, "", "repo", "health", "o/new")
	if err == nil || !strings.Contains(err.Error(), "no health computed for o/new") {
		t.Fatalf("404: err = %v", err)
	}
	for _, bad := range []string{"o", "o/", "/r", "o/r/x"} {
		if _, _, err := execute(t, server, "", "repo", "health", bad); ExitCode(err) != ExitUsage {
			t.Fatalf("%q: exit=%d err=%v", bad, ExitCode(err), err)
		}
	}
}
