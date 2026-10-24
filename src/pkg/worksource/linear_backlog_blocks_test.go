package worksource

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

const linearBlockIssueJSON = `{"data":{"issue":{"id":"uuid-1",
	"labels":{"nodes":[{"id":"lb-blocked","name":"Blocked"},{"id":"lb-fu","name":"followup"}]}}}}`

// LiftBlock removes `blocked` and comments; it never changes the workflow
// state, so a closed-unmerged PR's item stays open for re-triage (#13849).
func TestLinearBacklogSinkLiftsBlock(t *testing.T) {
	var removed []string
	var comments int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := decodeGQL(t, r)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(c.Query, "issueRemoveLabel"):
			removed = append(removed, c.Variables["labelId"].(string))
			_, _ = io.WriteString(w, `{"data":{"issueRemoveLabel":{"success":true}}}`)
		case strings.Contains(c.Query, "issueUpdate"):
			t.Errorf("LiftBlock must not change the issue's state: %q", c.Query)
		case strings.Contains(c.Query, "commentCreate"):
			comments++
			_, _ = io.WriteString(w, `{"data":{"commentCreate":{"success":true,"comment":{"id":"c1"}}}}`)
		case strings.Contains(c.Query, "issue(id"):
			_, _ = io.WriteString(w, linearBlockIssueJSON)
		default:
			t.Errorf("unexpected query %q", c.Query)
		}
	}))
	defer srv.Close()
	src := NewLinearSource(LinearConfig{APIKey: "lin-key", BaseURL: srv.URL, Teams: []LinearTeamConfig{{Key: "ENG", Repo: "o/r"}}}, srv.Client())
	sink := &linearBacklogSink{src: src}
	var resolver github.ReviewBacklogBlockResolver = sink
	ref := github.ReviewBacklogRef{ID: "uuid-1", Key: "ENG-12"}
	if err := resolver.LiftBlock(context.Background(), "o/r", ref, "unblocked"); err != nil {
		t.Fatalf("LiftBlock: %v", err)
	}
	if strings.Join(removed, ",") != "lb-blocked" || comments != 1 {
		t.Fatalf("lift removed=%v comments=%d", removed, comments)
	}
}

func TestLinearRemoveIssueLabelWithoutTheLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"issue":{"id":"uuid-1","labels":{"nodes":[]}}}}`)
	}))
	defer srv.Close()
	src := NewLinearSource(LinearConfig{APIKey: "lin-key", BaseURL: srv.URL}, srv.Client())
	// No `blocked` label to remove is not an error.
	if err := src.RemoveIssueLabel(context.Background(), "uuid-1", "blocked"); err != nil {
		t.Fatalf("RemoveIssueLabel without the label: %v", err)
	}
}
