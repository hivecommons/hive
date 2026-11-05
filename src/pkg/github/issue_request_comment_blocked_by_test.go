package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Danathar/tududi#512: a park that only defers to #511's decision links
// "blocked by #511" on the existing issue once it is parked and the comment
// is posted, and reports the link like a create does.
func TestIssueRequestWatcher_CommentLinksBlockedBy(t *testing.T) {
	var mu sync.Mutex
	var writes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "GET" && strings.HasSuffix(p, "/issues/511"):
			_, _ = io.WriteString(w, `{"id":5110,"number":511,"state":"open"}`)
		case r.Method == "GET" && strings.HasSuffix(p, "/issues/512"):
			_, _ = io.WriteString(w, `{"id":5120,"number":512}`)
		case r.Method == "GET" && strings.Contains(p, "/labels/"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == "POST" && strings.HasSuffix(p, "/labels"):
			var body []string
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, l := range body {
				writes = append(writes, "label:"+l)
			}
			_, _ = io.WriteString(w, `[{"name":"x"}]`)
		case r.Method == "POST" && strings.HasSuffix(p, "/issues/512/dependencies/blocked_by"):
			body, _ := io.ReadAll(r.Body)
			writes = append(writes, "blocked_by:"+strings.TrimSpace(string(body)))
			_, _ = io.WriteString(w, `{}`)
		case r.Method == "POST" && strings.HasSuffix(p, "/comments"):
			writes = append(writes, "comment")
			_, _ = io.WriteString(w, `{"id":1}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetAgentIssueDecisionPolicy("escalate")
	c.SetIssueFilter(needsDecisionFilter{label: "owner-call"})
	dir := withIssueDir(t)
	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 512, Agent: "scanner", NeedsDecision: true,
		Body: "**What I need from you:** Answer on #511; the same choice applies here.", BlockedBy: []int{511, 512},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	mu.Lock()
	got := strings.Join(writes, ",")
	mu.Unlock()
	if got != `label:owner-call,comment,blocked_by:{"issue_id":5110}` {
		t.Fatalf("writes = %q, want the park, the comment, then the blocked-by link to #511 (not itself)", got)
	}
	resp := readIssueResultFile(t, reqPath)
	if !resp.OK || resp.Number != 512 || fmt.Sprint(resp.BlockedByLinked) != "[511]" || len(resp.BlockedByErrors) != 0 {
		t.Fatalf("result = %+v, want ok on #512 with #511 linked", resp)
	}
}
