package automerge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/prstatus"
)

type stageNoteCommenter struct {
	Transport
	calls  int
	repo   string
	number int
	body   string
	err    error
}

func (c *stageNoteCommenter) CreateIssueComment(_ context.Context, repo string, number int, body string) error {
	c.calls++
	c.repo, c.number, c.body = repo, number, body
	return c.err
}

func TestNormalStageNoteUsesCommentPrimitive(t *testing.T) {
	denied := errors.New("comment denied by write gate")
	for _, mentioning := range []bool{false, true} {
		for _, writeErr := range []error{nil, denied} {
			t.Run(fmt.Sprintf("mentioning=%t/denied=%t", mentioning, writeErr != nil), func(t *testing.T) {
				var rawWrites atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					rawWrites.Add(1)
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{}`))
				}))
				defer srv.Close()
				commenter := &stageNoteCommenter{err: writeErr}
				client := gh.NewClient(nil)
				client.BaseURL, _ = url.Parse(srv.URL + "/")
				e := &Engine{transport: commenter, gh: client}
				post := e.postStageNote
				if mentioning {
					post = e.postStageNoteMentioning
				}
				const body = "Stage regressed: @author please review"
				n, err := post(context.Background(), StageLabelPolicy{}, stageObservation{Owner: "acme", Repo: "widget", Number: 42}, prstatus.KeyStageRegressed, body)
				wantCount := 1
				if writeErr != nil {
					wantCount = 0
				}
				if !errors.Is(err, writeErr) || n != wantCount {
					t.Fatalf("post = (%d, %v), want (%d, %v)", n, err, wantCount, writeErr)
				}
				if commenter.calls != 1 || commenter.repo != "acme/widget" || commenter.number != 42 || commenter.body != body {
					t.Fatalf("comment primitive received %+v", commenter)
				}
				if rawWrites.Load() != 0 {
					t.Fatalf("stage note bypassed primitive: %d raw writes", rawWrites.Load())
				}
			})
		}
	}
}

// A transport without the gate must fail closed, rather than falling back to
// the engine's raw GitHub client (which is intentionally absent here).
func TestNormalStageNoteRequiresCommentPrimitive(t *testing.T) {
	e := &Engine{transport: struct{ Transport }{}}
	n, err := e.postStageNote(context.Background(), StageLabelPolicy{}, stageObservation{Owner: "acme", Repo: "widget", Number: 42}, prstatus.KeyStageRegressed, "regressed")
	if n != 0 || err == nil || !strings.Contains(err.Error(), "transport has no comment primitive") {
		t.Fatalf("post = (%d, %v), want missing-primitive error and no comment", n, err)
	}
}
