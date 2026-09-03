package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func verifyTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestParsePRURL(t *testing.T) {
	cases := []struct {
		name        string
		url         string
		wantErr     bool
		owner, repo string
		number      int
	}{
		{"canonical", "https://github.com/hivecommons/hive/pull/2565", false, "hivecommons", "hive", 2565},
		{"trailing files", "https://github.com/hivecommons/hive/pull/2565/files", false, "hivecommons", "hive", 2565},
		{"with fragment", "https://github.com/o/r/pull/7#discussion_r1", false, "o", "r", 7},
		{"with query", "https://github.com/o/r/pull/7?w=1", false, "o", "r", 7},
		{"ghe host", "https://ghe.example.com/o/r/pull/12", false, "o", "r", 12},
		{"whitespace", "   https://github.com/o/r/pull/9   ", false, "o", "r", 9},
		{"empty", "", true, "", "", 0},
		{"not a pr", "https://github.com/o/r/issues/9", true, "", "", 0},
		{"random text", "the agent finished the work", true, "", "", 0},
		{"zero number", "https://github.com/o/r/pull/0", true, "", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := ParsePRURL(tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %+v", tc.url, ref)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.url, err)
			}
			if ref.Owner != tc.owner || ref.Repo != tc.repo || ref.Number != tc.number {
				t.Fatalf("parsed %+v, want %s/%s#%d", ref, tc.owner, tc.repo, tc.number)
			}
		})
	}
}

func TestPRBaseRepoMatches(t *testing.T) {
	cases := []struct {
		base, expected string
		want           bool
	}{
		{"kubestellar/hive", "hive", true},               // bare expected, repo matches
		{"kubestellar/hive", "Hive", true},               // case-insensitive
		{"kubestellar/hive", "kubestellar/hive", true},   // full match
		{"kubestellar/hive", "someone/hive", false},      // owner differs
		{"kubestellar/hive", "kubestellar/other", false}, // repo differs
		{"kubestellar/hive", "other", false},             // bare, repo differs
		{"", "hive", false},                              // empty base
		{"kubestellar/hive", "", false},                  // empty expected
	}
	for _, tc := range cases {
		if got := prBaseRepoMatches(tc.base, tc.expected); got != tc.want {
			t.Errorf("prBaseRepoMatches(%q,%q)=%v want %v", tc.base, tc.expected, got, tc.want)
		}
	}
}

// prJSON builds a minimal go-github PR body with the base repo full name and
// author login the tests care about.
func prJSON(baseFullName, authorLogin string, number int) string {
	owner, repo := splitRepo(baseFullName)
	m := map[string]any{
		"number":   number,
		"html_url": "https://github.com/" + baseFullName + "/pull/" + itoa(number),
		"user":     map[string]any{"login": authorLogin},
		"base": map[string]any{
			"repo": map[string]any{
				"name":      repo,
				"full_name": baseFullName,
				"owner":     map[string]any{"login": owner},
			},
		},
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// verifyMux serves GET /repos/{owner}/{repo}/pulls/{number}. When status != 200
// it returns an error body to exercise the API-error path.
func verifyMux(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			http.Error(w, `{"message":"boom"}`, status)
			return
		}
		_, _ = io.WriteString(w, body)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestVerifyReportedPR_AllChecksPass(t *testing.T) {
	ts := verifyMux(t, http.StatusOK, prJSON("kubestellar/hive", "alice", 2565))
	c := NewClientForTest(ts.URL, "kubestellar", []string{"hive"}, verifyTestLogger())

	res := c.VerifyReportedPR(context.Background(), "hive", "https://github.com/hivecommons/hive/pull/2565", "alice")
	if !res.Verified {
		t.Fatalf("expected verified, got reason=%q err=%v", res.Reason, res.Err)
	}
	if res.Author != "alice" || res.BaseRepo != "kubestellar/hive" {
		t.Fatalf("unexpected resolved fields: %+v", res)
	}
}

func TestVerifyReportedPR_CaseInsensitiveAuthorAndRepo(t *testing.T) {
	ts := verifyMux(t, http.StatusOK, prJSON("KubeStellar/Hive", "Alice", 2565))
	c := NewClientForTest(ts.URL, "kubestellar", []string{"hive"}, verifyTestLogger())

	res := c.VerifyReportedPR(context.Background(), "kubestellar/hive", "https://github.com/hivecommons/hive/pull/2565", "alice")
	if !res.Verified {
		t.Fatalf("expected verified (case-insensitive), reason=%q", res.Reason)
	}
}

func TestVerifyReportedPR_WrongRepo(t *testing.T) {
	// PR base repo is a DIFFERENT repo than the assignment — the "first PR URL
	// mentioned anywhere" fallback risk. Must NOT verify.
	ts := verifyMux(t, http.StatusOK, prJSON("kubestellar/other", "alice", 99))
	c := NewClientForTest(ts.URL, "kubestellar", []string{"hive"}, verifyTestLogger())

	res := c.VerifyReportedPR(context.Background(), "hive", "https://github.com/kubestellar/other/pull/99", "alice")
	if res.Verified {
		t.Fatalf("expected NOT verified for wrong repo")
	}
	if res.Err != nil {
		t.Fatalf("wrong-repo is a clean negative, want no Err, got %v", res.Err)
	}
}

func TestVerifyReportedPR_WrongAuthor(t *testing.T) {
	ts := verifyMux(t, http.StatusOK, prJSON("kubestellar/hive", "mallory", 2565))
	c := NewClientForTest(ts.URL, "kubestellar", []string{"hive"}, verifyTestLogger())

	res := c.VerifyReportedPR(context.Background(), "hive", "https://github.com/hivecommons/hive/pull/2565", "alice")
	if res.Verified {
		t.Fatalf("expected NOT verified for wrong author")
	}
	if res.Err != nil {
		t.Fatalf("wrong-author is a clean negative, want no Err, got %v", res.Err)
	}
}

func TestVerifyReportedPR_DoesNotExist(t *testing.T) {
	ts := verifyMux(t, http.StatusNotFound, "")
	c := NewClientForTest(ts.URL, "kubestellar", []string{"hive"}, verifyTestLogger())

	res := c.VerifyReportedPR(context.Background(), "hive", "https://github.com/hivecommons/hive/pull/404", "alice")
	if res.Verified {
		t.Fatalf("expected NOT verified for nonexistent PR")
	}
	if res.Err == nil {
		t.Fatalf("a 404 lookup should surface Err so the caller can log it")
	}
}

func TestVerifyReportedPR_APIError(t *testing.T) {
	ts := verifyMux(t, http.StatusInternalServerError, "")
	c := NewClientForTest(ts.URL, "kubestellar", []string{"hive"}, verifyTestLogger())

	res := c.VerifyReportedPR(context.Background(), "hive", "https://github.com/hivecommons/hive/pull/2565", "alice")
	if res.Verified {
		t.Fatalf("expected NOT verified on API error (fail closed on trust)")
	}
	if res.Err == nil {
		t.Fatalf("transient API error should surface Err")
	}
}

func TestVerifyReportedPR_UnparseableURL(t *testing.T) {
	ts := verifyMux(t, http.StatusOK, prJSON("kubestellar/hive", "alice", 1))
	c := NewClientForTest(ts.URL, "kubestellar", []string{"hive"}, verifyTestLogger())

	res := c.VerifyReportedPR(context.Background(), "hive", "not a url", "alice")
	if res.Verified {
		t.Fatalf("expected NOT verified for unparseable URL")
	}
	if res.Err != nil {
		t.Fatalf("unparseable URL is a clean negative (nothing to look up), got Err %v", res.Err)
	}
}

func TestVerifyReportedPR_NilClient(t *testing.T) {
	var c *Client
	res := c.VerifyReportedPR(context.Background(), "hive", "https://github.com/hivecommons/hive/pull/1", "alice")
	if res.Verified {
		t.Fatalf("nil client must never verify")
	}
	if res.Err == nil {
		t.Fatalf("nil client should report ErrNoGitHubClient")
	}
}
