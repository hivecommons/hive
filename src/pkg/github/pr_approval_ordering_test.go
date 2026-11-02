package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine/stageenginetest"
)

func TestOperatorApprovalCommitsBeforeForbiddenOrHungReporting(t *testing.T) {
	for _, mode := range []string{"forbidden", "hung"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "approvals.json")
			store := review.NewHumanChangeRequests(path)
			var reports atomic.Int32
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls/") {
					_, _ = w.Write([]byte(`{"state":"open","title":"fix","body":"body","user":{"login":"hive[bot]"},"head":{"sha":"head"},"labels":[]}`))
					return
				}
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`[]`))
					return
				}
				// Reload from disk, not just memory: reporting is downstream
				// of the exact native approval's persistent commit point.
				_, _ = io.Copy(io.Discard, r.Body)
				req, ok := review.NewHumanChangeRequests(path).For("org/repo", 1, "head")
				if !ok || !req.IsApproval() || req.Actor != "owner" {
					t.Error("external reporting started before native approval committed")
				}
				reports.Add(1)
				if mode == "hung" {
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
			}))
			defer server.Close()
			defer close(release)
			c := newTestClient(t, server, "org", []string{"repo"})
			c.SetStageFactSink(&stageenginetest.Recorder{})
			event, err := c.RecordOperatorPullRequestApprovalWith(context.Background(), "org/repo", 1, "owner", PullRequestApprovalOptions{
				HumanChangeRequests: store, ExpectedHead: "head", ExpectedBodyDigest: review.PullRequestBodyDigest("fix", "body"), ExpectedAuthor: "hive[bot]",
				ReportingTimeout: 100 * time.Millisecond,
			})
			if err != nil || !event.Recorded || event.ReportingError == "" || reports.Load() == 0 {
				t.Fatalf("durable application and reporting failure must be distinct: %+v, %v", event, err)
			}
			if facts := prDecisionFacts(t, c); len(facts) != 1 || facts[0].Head != "head" {
				t.Fatalf("native stage projection did not precede reporting: %+v", facts)
			}
		})
	}
}

func TestOperatorApprovalRejectsChangedIdentityAndEmptyActorBeforeRecord(t *testing.T) {
	for _, mode := range []string{"head", "body", "author", "empty-actor", "revoked-actor", "hold", "sentinel", "closed"} {
		t.Run(mode, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				state := "open"
				if mode == "closed" {
					state = "closed"
				}
				if strings.HasSuffix(r.URL.Path, "/permission") {
					_, _ = w.Write([]byte(`{"permission":"read"}`))
					return
				}
				var labels []map[string]string
				if mode == "hold" || mode == "sentinel" {
					labels = append(labels, map[string]string{"name": mode})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"state": state, "title": "fix", "body": "body", "labels": labels, "user": map[string]string{"login": "hive[bot]"}, "head": map[string]string{"sha": "head"}})
			}))
			defer server.Close()
			c := newTestClient(t, server, "org", []string{"repo"})
			store := review.NewHumanChangeRequests("")
			opts := PullRequestApprovalOptions{HumanChangeRequests: store, ExpectedHead: "head", ExpectedBodyDigest: review.PullRequestBodyDigest("fix", "body"), ExpectedAuthor: "hive[bot]"}
			actor := "owner"
			switch mode {
			case "head":
				opts.ExpectedHead = "old-head"
			case "body":
				opts.ExpectedBodyDigest = review.PullRequestBodyDigest("fix", "old-body")
			case "author":
				opts.ExpectedAuthor = "someone-else"
			case "empty-actor":
				actor = ""
			case "revoked-actor", "hold", "sentinel":
				opts.VerifyActorWrite = true
				opts.GuardSentinelLabel = "sentinel"
			}
			event, err := c.RecordOperatorPullRequestApprovalWith(context.Background(), "org/repo", 1, actor, opts)
			if err == nil || event.Recorded || store.Len() != 0 || writes.Load() != 0 {
				t.Fatalf("changed or unauthorized target approved: %+v err=%v writes=%d", event, err, writes.Load())
			}
		})
	}
}

func TestQueueApprovalRefusesChangedHeadBeforeAnyMutation(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		_, _ = w.Write([]byte(`{"state":"open","user":{"login":"contributor"},"head":{"sha":"new"}}`))
	}))
	defer server.Close()
	c := newTestClient(t, server, "org", []string{"repo"})
	if _, err := c.QueuePRAutoMergeWithNoteResultAtHead(context.Background(), "org/repo", 1, "owner", "decision", "old"); err == nil || writes.Load() != 0 {
		t.Fatalf("stale queue approval wrote externally: err=%v writes=%d", err, writes.Load())
	}
}
