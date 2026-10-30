package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

func TestWritebackItemForNumberMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/org/repo/issues/42" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"number":42,"user":{"login":"hive[bot]"},"labels":[{"name":"hive-review"}],"created_at":"2026-10-29T00:00:00Z","pull_request":{"url":"https://api.github.com/repos/org/repo/pulls/42"}}`))
	}))
	defer srv.Close()
	client := newTestClient(t, srv, "org", []string{"repo"})
	client.SetAppBotLogin("hive[bot]")
	client.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{}, ""))
	item, err := client.WritebackItemForNumber(context.Background(), "org/repo", 42)
	if err != nil {
		t.Fatal(err)
	}
	if item.Number != 42 || !item.IsPR || !item.HiveAuthored || item.Author != "hive[bot]" || item.CreatedAt.IsZero() || len(item.Labels) != 1 || item.Labels[0] != "hive-review" {
		t.Fatalf("incomplete writeback metadata: %+v", item)
	}
}

func TestIncomingIssueWritesRespectContainment(t *testing.T) {
	for _, operation := range []string{"fold", "blocker"} {
		t.Run(operation, func(t *testing.T) {
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
				}
				if r.URL.Path == "/repos/org/repo/issues/42/comments" {
					_, _ = w.Write([]byte(`[]`))
				} else {
					_, _ = w.Write([]byte(`{"number":42,"user":{"login":"visitor"},"labels":[],"state":"open"}`))
				}
			}))
			defer srv.Close()
			client := newTestClient(t, srv, "org", []string{"repo"})
			gate := writeback.New(writeback.Config{}, writeback.Deps{LevelFor: func(string) int { return 1 }}, "")
			client.SetWritebackGate(gate)
			var err error
			if operation == "fold" {
				err = client.commentOnHumanFiledTwin(context.Background(), "org", "repo", &gh.Issue{Number: gh.Ptr(42), User: &gh.User{Login: gh.Ptr("visitor")}}, "new finding")
			} else {
				_, err = client.RecordVerdictBlocker(context.Background(), "org/repo", 42, VerdictBlocker{Repo: "org/repo", Number: 43})
			}
			if !errors.Is(err, writeback.ErrSuppressed) || writes != 0 || len(gate.Recent()) != 1 {
				t.Fatalf("err=%v writes=%d suppressed=%+v", err, writes, gate.Recent())
			}
		})
	}
}
