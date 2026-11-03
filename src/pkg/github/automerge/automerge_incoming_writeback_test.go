package automerge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

func TestAdoptionOriginalCloseRespectsPauseOnOwnPR(t *testing.T) {
	api := &adoptionOriginalAPI{originalMerged: true, originalHead: "04cab9b"}
	srv := api.server(t, nil)
	now := time.Now()
	engine := adoptionEngine(srv.URL, &now)
	gate := writeback.New(writeback.Config{}, writeback.Deps{
		RepoPause: func(string) (writeback.RepoPauseInfo, bool) {
			return writeback.RepoPauseInfo{By: "owner", Reason: "freeze"}, true
		},
	}, "")
	engine.transport.(*hgithub.Client).SetWritebackGate(gate)
	pr := &gh.PullRequest{
		Number: gh.Ptr(897), User: &gh.User{Login: gh.Ptr(testHiveAppBotLogin)},
		Head: &gh.PullRequestBranch{Ref: gh.Ptr("hive/adopt-885"), SHA: gh.Ptr("04cab9b"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
	}
	if engine.closeAdoptionPRIfOriginalMerged(context.Background(), "widget", "acme", "widget", pr) || len(api.closed)+len(api.comments) != 0 {
		t.Fatalf("paused adoption wrote: closed=%v comments=%q", api.closed, api.comments)
	}
	if suppressed := gate.Recent(); len(suppressed) != 1 || suppressed[0].Reason != writeback.ReasonRepoPaused {
		t.Fatalf("suppressed=%+v", suppressed)
	}
}

func TestIncomingAutomergeNoticesRespectContainment(t *testing.T) {
	for _, notice := range []string{"signoff", "untrusted-merger"} {
		t.Run(notice, func(t *testing.T) {
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
				}
				if r.URL.Path == "/repos/acme/widget/pulls/42" {
					_, _ = w.Write([]byte(`{"number":42,"user":{"login":"visitor"},"labels":[]}`))
				} else {
					_, _ = w.Write([]byte(`[]`))
				}
			}))
			defer srv.Close()
			engine := newAutoMergeSweepClient(srv.URL)
			gate := writeback.New(writeback.Config{}, writeback.Deps{LevelFor: func(string) int { return 1 }}, "")
			engine.transport.(*hgithub.Client).SetWritebackGate(gate)
			if notice == "signoff" {
				store := stagelabels.NewStore("", nil)
				obs := stageObservation{Owner: "acme", Repo: "widget", Display: "widget", Number: 42, HeadSHA: "head", Author: "visitor"}
				if count, err := engine.noteSignoffAttributionDecision(context.Background(), store, "acme/widget#42", obs); err != nil || count != 0 {
					t.Fatalf("count=%d err=%v", count, err)
				}
				if record, _ := store.Get("acme/widget#42"); record.SignoffAttributionNoted("head") {
					t.Fatal("suppressed notice was marked as posted")
				}
			} else if err := engine.commentUntrustedMerger(context.Background(), "acme", "widget", 42, "head", "untrusted"); err != nil {
				t.Fatal(err)
			}
			if writes != 0 || len(gate.Recent()) != 1 {
				t.Fatalf("writes=%d suppressed=%+v", writes, gate.Recent())
			}
		})
	}
}
