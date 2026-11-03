package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestParseVerdictBlocker(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   string // owner/repo#N, "" for no reference
	}{
		{"qualified", "waiting on ublue-os/server#450 to land", "ublue-os/server#450"},
		{"bare names the task repo", "blocked until #12 is fixed", "projectbluefin/testsuite#12"},
		{"issue URL", "needs https://github.com/ublue-os/server/issues/450 first", "ublue-os/server#450"},
		{"PR URL", "see https://github.com/o/r/pull/7 which has not merged", "o/r#7"},
		{"no reference", "the upstream library has not released the API this needs", ""},
		{"first of two wins", "blocked on a/b#1 and later c/d#2", "a/b#1"},
		{"URL before qualified", "https://github.com/a/b/issues/3 then c/d#4", "a/b#3"},
		{"self reference skipped", "#975 is blocked on ublue-os/server#450", "ublue-os/server#450"},
		{"qualified self reference skipped", "projectbluefin/testsuite#975 waits on #976", "projectbluefin/testsuite#976"},
		{"only self", "#975 cannot move", ""},
		{"html entity is not a ref", "it&#39;s waiting on the release", ""},
		{"anchor in a word is not a ref", "see foo#12 and section#3", ""},
		{"comment anchor ignored", "https://github.com/a/b/issues/9#issuecomment-123", "a/b#9"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseVerdictBlocker(tc.reason, "projectbluefin/testsuite", 975)
			if tc.want == "" {
				if ok {
					t.Fatalf("want no reference, got %s", got)
				}
				return
			}
			if !ok || got.String() != tc.want {
				t.Fatalf("want %s, got %s (ok=%v)", tc.want, got, ok)
			}
		})
	}
	if _, ok := ParseVerdictBlocker("blocked on #12", "", 1); ok {
		t.Fatal("a bare #N with no task repo must not resolve")
	}
}

// verdictBlockerFake serves the blocker read, the task issue's blocked_by
// list and comments, and records every write.
type verdictBlockerFake struct {
	mu        sync.Mutex
	blockers  map[string]map[string]any // "owner/repo#N" -> issue JSON; absent -> 404
	blockedBy []wireBlocker
	comments  []string
	links     []int64
	posted    []string
}

func (f *verdictBlockerFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(path, "/dependencies/blocked_by") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(f.blockedBy)
		case strings.HasSuffix(path, "/dependencies/blocked_by") && r.Method == http.MethodPost:
			var body struct {
				IssueID int64 `json:"issue_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.links = append(f.links, body.IssueID)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(path, "/comments") && r.Method == http.MethodGet:
			out := make([]map[string]any, 0, len(f.comments))
			for i, c := range f.comments {
				out = append(out, map[string]any{"id": i + 1, "body": c})
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(path, "/comments") && r.Method == http.MethodPost:
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.posted = append(f.posted, body.Body)
			f.comments = append(f.comments, body.Body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1000 + len(f.posted), "body": body.Body})
		case strings.HasPrefix(path, "/repos/") && strings.Contains(path, "/issues/") && r.Method == http.MethodGet:
			var owner, repo string
			var n int
			parts := strings.Split(strings.TrimPrefix(path, "/repos/"), "/")
			if len(parts) == 4 {
				owner, repo = parts[0], parts[1]
				fmt.Sscanf(parts[3], "%d", &n)
			}
			issue, ok := f.blockers[fmt.Sprintf("%s/%s#%d", owner, repo, n)]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(issue)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRecordVerdictBlocker_OpenIssueLinksOnceByID(t *testing.T) {
	f := &verdictBlockerFake{blockers: map[string]map[string]any{
		"ublue-os/server#450": {"id": 987654, "number": 450, "state": "open"},
	}}
	c := newTestClient(t, f.server(t), "projectbluefin", []string{"testsuite"})
	ref := VerdictBlocker{Repo: "ublue-os/server", Number: 450}

	got, err := c.RecordVerdictBlocker(context.Background(), "projectbluefin/testsuite", 975, ref)
	if err != nil || got != VerdictBlockerLinked {
		t.Fatalf("want linked, got %q err=%v", got, err)
	}
	if len(f.links) != 1 || f.links[0] != 987654 {
		t.Fatalf("AddBlockedBy must be called once with the blocker's database id, got %v", f.links)
	}

	// Re-delivery: the link now exists on GitHub, so nothing is written again.
	f.blockedBy = []wireBlocker{{Number: 450, State: "open", RepositoryURL: "https://api.github.com/repos/ublue-os/server"}}
	got, err = c.RecordVerdictBlocker(context.Background(), "projectbluefin/testsuite", 975, ref)
	if err != nil || got != VerdictBlockerAlreadyLinked {
		t.Fatalf("want already_linked, got %q err=%v", got, err)
	}
	if len(f.links) != 1 || len(f.posted) != 0 {
		t.Fatalf("re-delivery must write nothing, links=%v posted=%v", f.links, f.posted)
	}
}

func TestRecordVerdictBlocker_OpenPRPostsMarkerOnce(t *testing.T) {
	f := &verdictBlockerFake{blockers: map[string]map[string]any{
		"o/r#7": {"id": 77, "number": 7, "state": "open", "pull_request": map[string]any{"url": "https://api.github.com/repos/o/r/pulls/7"}},
	}}
	c := newTestClient(t, f.server(t), "o", []string{"r"})
	ref := VerdictBlocker{Repo: "o/r", Number: 7}

	got, err := c.RecordVerdictBlocker(context.Background(), "o/r", 5, ref)
	if err != nil || got != VerdictBlockerMarked {
		t.Fatalf("want marked, got %q err=%v", got, err)
	}
	if len(f.posted) != 1 || !strings.Contains(f.posted[0], BlockedOnMarker("o/r", 7)) {
		t.Fatalf("the blocked-on marker comment must be posted once, got %v", f.posted)
	}
	if repo, pr, ok := ParseBlockedOnMarker(f.posted[0]); !ok || repo != "o/r" || pr != 7 {
		t.Fatalf("the posted comment must parse as the blocked-on marker the sweep reads, got %s#%d ok=%v", repo, pr, ok)
	}
	if len(f.links) != 0 {
		t.Fatalf("a PR blocker must not be linked natively, got %v", f.links)
	}

	got, err = c.RecordVerdictBlocker(context.Background(), "o/r", 5, ref)
	if err != nil || got != VerdictBlockerAlreadyMarked || len(f.posted) != 1 {
		t.Fatalf("re-delivery must not post twice, got %q err=%v posted=%d", got, err, len(f.posted))
	}
}

func TestRecordVerdictBlocker_ClosedOrMissingWritesNothing(t *testing.T) {
	f := &verdictBlockerFake{blockers: map[string]map[string]any{
		"o/r#8": {"id": 88, "number": 8, "state": "closed"},
	}}
	c := newTestClient(t, f.server(t), "o", []string{"r"})

	got, err := c.RecordVerdictBlocker(context.Background(), "o/r", 5, VerdictBlocker{Repo: "o/r", Number: 8})
	if err != nil || got != VerdictBlockerClosed {
		t.Fatalf("closed blocker: want closed, got %q err=%v", got, err)
	}
	if _, err := c.RecordVerdictBlocker(context.Background(), "o/r", 5, VerdictBlocker{Repo: "o/r", Number: 404}); err == nil {
		t.Fatal("a 404 on the blocker read must be reported")
	}
	if len(f.links) != 0 || len(f.posted) != 0 {
		t.Fatalf("closed or missing blockers must write nothing, links=%v posted=%v", f.links, f.posted)
	}
}

func TestRecordVerdictBlocker_RejectsBadInput(t *testing.T) {
	var nilClient *Client
	if _, err := nilClient.RecordVerdictBlocker(context.Background(), "o/r", 1, VerdictBlocker{Repo: "o/r", Number: 2}); err == nil {
		t.Fatal("nil client must error")
	}
	f := &verdictBlockerFake{}
	c := newTestClient(t, f.server(t), "o", []string{"r"})
	if _, err := c.RecordVerdictBlocker(context.Background(), "o/r", 1, VerdictBlocker{Repo: "o/r", Number: 0}); err == nil {
		t.Fatal("a zero blocker number must error")
	}
}
