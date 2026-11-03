package commands

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const searchFixture = `{"query":"flaky","total":3,"hits":[
{"id":"e1","repo":"o/r","kind":"finding","at":"2026-10-20T10:00:00Z","url":"https://github.com/o/r/pull/7","visibility":"private","title":"flaky test in ci","score":1,"terms":["flaky"],"highlight":{"snippet":"flaky test","spans":[[0,5]]}},
{"id":"b2","repo":"o/r","kind":"bead","at":"2026-10-21T10:00:00Z","url":"https://github.com/o/r/issues/9","visibility":"private","score":1,"terms":["flaky"],"highlight":{"snippet":"retry the flaky job","spans":[]},"redacted":true}],
"facets":{"kind":[{"value":"finding","count":2},{"value":"bead","count":1}],"repo":[],"verdict":[],"sentinel.rule":[]},
"coverage":{"o/broken":"corrupt index"}}`

func TestSearchCommandSendsFiltersAndPrintsTable(t *testing.T) {
	var path string
	var query url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		path, query = r.URL.Path, r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, searchFixture)
	}))
	defer server.Close()

	out, errOut, err := execute(t, server, "", "search", "flaky", "test",
		"--kind", "finding,bead", "--repo", "o/r", "--from", "2026-10-01", "--to", "2026-10-28", "--limit", "5")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/api/search" {
		t.Fatalf("path = %q", path)
	}
	for key, want := range map[string]string{
		"q": "flaky test", "kind": "finding,bead", "repo": "o/r",
		"from": "2026-10-01", "to": "2026-10-28", "limit": "5",
	} {
		if got := query.Get(key); got != want {
			t.Fatalf("%s = %q, want %q (query %v)", key, got, want, query)
		}
	}
	for _, want := range []string{
		"KIND", "finding", "flaky test in ci", "https://github.com/o/r/pull/7", "2026-10-20",
		"retry the flaky job [redacted]", "https://github.com/o/r/issues/9", "2 of 3 hits",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "o/broken index unreadable (corrupt index)") {
		t.Fatalf("coverage warning missing: %q", errOut)
	}
}

func TestSearchCommandJSONPassesResponseThrough(t *testing.T) {
	var rawQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, searchFixture)
	}))
	defer server.Close()

	out, _, err := execute(t, server, "", "search", "--kind", "bead", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rawQuery, "q=") {
		t.Fatalf("empty query must not send q: %q", rawQuery)
	}
	for _, want := range []string{`"total": 3`, `"sentinel.rule"`, `"redacted": true`} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestSearchCommandDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"ok":false,"error":"search not enabled"}`)
	}))
	defer server.Close()

	_, _, err := execute(t, server, "", "search", "anything")
	if err == nil || !strings.Contains(err.Error(), "search is disabled on this hive") || !strings.Contains(err.Error(), "search.enabled: true") {
		t.Fatalf("err = %v", err)
	}
	if ExitCode(err) != ExitAPI {
		t.Fatalf("exit = %d want %d", ExitCode(err), ExitAPI)
	}
}

func TestSearchCommandErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("q") == "forbidden" {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"ok":false,"error":"merger or owner access required"}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"ok":false,"error":"repo must be owner/repo"}`)
	}))
	defer server.Close()

	_, _, err := execute(t, server, "", "search", "forbidden")
	if ExitCode(err) != ExitAuth || strings.Contains(err.Error(), "disabled") {
		t.Fatalf("403: exit=%d err=%v", ExitCode(err), err)
	}
	_, _, err = execute(t, server, "", "search", "x", "--repo", "bad")
	if ExitCode(err) != ExitAPI || !strings.Contains(err.Error(), "repo must be owner/repo") {
		t.Fatalf("400: exit=%d err=%v", ExitCode(err), err)
	}
	if _, _, err := execute(t, server, "", "search", "x", "--limit", "-1"); ExitCode(err) != ExitUsage {
		t.Fatalf("negative limit: exit=%d err=%v", ExitCode(err), err)
	}
}
