package mergelane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ghub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

func TestRESTMutationPolicyDoesNotReserveCommentDedupe(t *testing.T) {
	for _, mutation := range []string{"update", "merge"} {
		t.Run(mutation, func(t *testing.T) {
			attempts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`{"number":42,"user":{"login":"visitor"},"labels":[{"name":"hive-review"}],"pull_request":{}}`))
					return
				}
				if strings.HasSuffix(r.URL.Path, "/comments") {
					_, _ = w.Write([]byte(`{"id":1,"html_url":"https://github.com/org/repo/issues/42#issuecomment-1"}`))
					return
				}
				attempts++
				if attempts == 1 {
					w.WriteHeader(http.StatusConflict)
					_, _ = w.Write([]byte(`{"message":"head moved"}`))
					return
				}
				if mutation == "merge" {
					_, _ = w.Write([]byte(`{"merged":true,"sha":"merged"}`))
					return
				}
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"message":"updating"}`))
			}))
			defer srv.Close()
			client := ghub.NewClientForTest(srv.URL, "org", []string{"repo"}, nil)
			yes := true
			gate := writeback.New(writeback.Config{Unsolicited: writeback.UnsolicitedConfig{Backfill: &yes}},
				writeback.Deps{LevelFor: func(string) int { return 6 }}, "")
			client.SetWritebackGate(gate)
			api := &RESTGitHub{Client: func() *ghub.Client { return client }}
			firstHead := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			secondHead := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			for i, head := range []string{firstHead, firstHead, secondHead} {
				var err error
				if mutation == "merge" {
					_, err = api.Merge(context.Background(), "org/repo", 42, head)
				} else {
					err = api.UpdateBranch(context.Background(), "org/repo", 42, head)
				}
				if i == 0 && !errors.Is(err, ErrHeadMoved) {
					t.Fatalf("first failed mutation: %v", err)
				}
				if i > 0 && err != nil {
					t.Fatalf("mutation retry/head %d blocked: %v", i, err)
				}
			}
			if attempts != 3 || len(gate.Recent()) != 0 {
				t.Fatalf("attempts=%d suppressed=%+v", attempts, gate.Recent())
			}
			if err := api.CreateIssueComment(context.Background(), "org/repo", 42, "a real notice"); err != nil {
				t.Fatal(err)
			}
			if err := api.CreateIssueComment(context.Background(), "org/repo", 42, "a real notice"); !errors.Is(err, writeback.ErrSuppressed) {
				t.Fatalf("real comment dedupe no longer enforced: %v", err)
			}
		})
	}
}

func TestRESTWritesRespectWritebackContainment(t *testing.T) {
	for _, operation := range []string{"comment", "update", "merge"} {
		t.Run(operation, func(t *testing.T) {
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
				}
				_, _ = w.Write([]byte(`{"number":42,"user":{"login":"visitor"},"labels":[],"pull_request":{}}`))
			}))
			defer srv.Close()
			client := ghub.NewClientForTest(srv.URL, "org", []string{"repo"}, nil)
			gate := writeback.New(writeback.Config{}, writeback.Deps{LevelFor: func(string) int { return 1 }}, "")
			client.SetWritebackGate(gate)
			api := &RESTGitHub{Client: func() *ghub.Client { return client }}
			ctx := context.Background()
			var err error
			switch operation {
			case "comment":
				err = api.CreateIssueComment(ctx, "org/repo", 42, "please update your branch")
			case "update":
				err = api.UpdateBranch(ctx, "org/repo", 42, "head")
			case "merge":
				_, err = api.Merge(ctx, "org/repo", 42, "head")
			}
			if !errors.Is(err, writeback.ErrSuppressed) || writes != 0 || len(gate.Recent()) != 1 {
				t.Fatalf("err=%v writes=%d suppressed=%+v", err, writes, gate.Recent())
			}
		})
	}
}

func TestExplicitLaneRequestPreservedExceptPause(t *testing.T) {
	client := ghub.NewClientForTest("http://127.0.0.1:1", "org", []string{"repo"}, nil)
	paused := false
	client.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{
		LevelFor: func(string) int { return 1 },
		RepoPause: func(string) (writeback.RepoPauseInfo, bool) {
			return writeback.RepoPauseInfo{By: "owner", Reason: "freeze"}, paused
		},
	}, ""))
	api := &RESTGitHub{}
	ctx := context.WithValue(context.Background(), mergeMethodKey{}, &mergeMethod{solicited: true})
	if err := api.allowWriteback(ctx, client, "org/repo", 42, writeback.KindContributorAdopt, "merge head"); err != nil {
		t.Fatalf("explicit merge request was refused: %v", err)
	}
	paused = true
	if err := api.allowWriteback(ctx, client, "org/repo", 42, writeback.KindContributorAdopt, "merge head"); !errors.Is(err, writeback.ErrSuppressed) {
		t.Fatalf("paused explicit merge request: %v", err)
	}
}
