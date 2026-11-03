package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	forgequeue "github.com/hivecommons/hive/pkg/forgestate"
)

func TestNativeQueueObservationExactCandidateEvidence(t *testing.T) {
	for _, mode := range []string{"proven", "two-members", "second-member-absent", "position-gap", "no-event", "wrong-check-head", "foreign-app", "missing-check", "pending", "failed", "skipped", "policy-unavailable", "truncated", "bad-ancestry", "moved-ref", "queue-error", "no-queue"} {
		t.Run(mode, func(t *testing.T) {
			base, candidate, member, tree := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/graphql":
					if mode == "queue-error" {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					if mode == "no-queue" {
						_, _ = w.Write([]byte(`{"data":{"repository":{"mergeQueue":null}}}`))
						return
					}
					position, firstCandidate, extra := 1, candidate, ""
					if mode == "position-gap" {
						position = 2
					}
					if mode == "two-members" || mode == "second-member-absent" {
						firstCandidate = tree
						extra = fmt.Sprintf(`,{"id":"entry-8","position":2,"state":"AWAITING_CHECKS","baseCommit":{"oid":%q},"headCommit":{"oid":%q},"pullRequest":{"number":8,"headRefOid":%q,"baseRefName":"v5"}}`, tree, candidate, strings.Repeat("e", 40))
					}
					_, _ = fmt.Fprintf(w, `{"data":{"repository":{"mergeQueue":{"entries":{"pageInfo":{"hasNextPage":%t},"nodes":[{"id":"entry-7","position":%d,"state":"AWAITING_CHECKS","enqueuedAt":"2026-10-30T12:00:00Z","baseCommit":{"oid":%q},"headCommit":{"oid":%q},"pullRequest":{"number":7,"headRefOid":%q,"baseRefName":"v5"}}%s]}}}}}`, mode == "truncated", position, base, firstCandidate, member, extra)
				case strings.HasSuffix(r.URL.Path, "/actions/runs"):
					_, _ = fmt.Fprintf(w, `{"workflow_runs":[{"id":10,"event":"merge_group","head_sha":%q,"head_branch":"gh-readonly-queue/v5/pr-7"}]}`, candidate)
				case strings.HasSuffix(r.URL.Path, "/required_status_checks"):
					if mode == "policy-unavailable" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					_, _ = w.Write([]byte(`{"contexts":["test"],"checks":[{"context":"test","app_id":15368}]}`))
				case strings.Contains(r.URL.Path, "/rules/branches/"):
					_, _ = w.Write([]byte(`[]`))
				case strings.Contains(r.URL.Path, "/git/ref/"):
					sha := candidate
					if mode == "moved-ref" {
						sha = tree
					}
					_, _ = fmt.Fprintf(w, `{"object":{"sha":%q}}`, sha)
				case strings.Contains(r.URL.Path, "/git/commits/"):
					_, _ = fmt.Fprintf(w, `{"sha":%q,"tree":{"sha":%q}}`, candidate, tree)
				case strings.Contains(r.URL.Path, "/compare/"):
					ancestor := member
					if strings.Contains(r.URL.Path, strings.Repeat("e", 40)) {
						ancestor = strings.Repeat("e", 40)
						if mode == "second-member-absent" {
							ancestor = base
						}
					}
					if mode == "bad-ancestry" {
						ancestor = base
					}
					_, _ = fmt.Fprintf(w, `{"status":"ahead","merge_base_commit":{"sha":%q}}`, ancestor)
				case strings.HasSuffix(r.URL.Path, "/check-runs"):
					if !strings.Contains(r.URL.Path, candidate) {
						t.Errorf("CI queried on a PR head, not candidate: %s", r.URL)
					}
					if mode == "missing-check" {
						_, _ = w.Write([]byte(`{"check_runs":[]}`))
						return
					}
					checkHead, status, conclusion, app := candidate, "completed", "success", 15368
					switch mode {
					case "wrong-check-head":
						checkHead = member
					case "foreign-app":
						app = 42
					case "pending":
						status, conclusion = "in_progress", ""
					case "failed":
						conclusion = "failure"
					case "skipped":
						conclusion = "skipped"
					}
					_, _ = fmt.Fprintf(w, `{"check_runs":[{"id":40,"name":"test","head_sha":%q,"status":%q,"conclusion":%q,"app":{"id":%d},"html_url":"https://github.com/o/r/actions/runs/10/job/40"}]}`, checkHead, status, conclusion, app)
				case strings.HasSuffix(r.URL.Path, "/status"):
					_, _ = w.Write([]byte(`{"statuses":[]}`))
				default:
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer api.Close()
			client := NewClient("token", "o", []string{"r"}, nil, api.URL)
			hints := []forgequeue.QueueCandidate{{SHA: candidate, Ref: "refs/heads/gh-readonly-queue/v5/pr-7", BaseSHA: base}}
			if mode == "no-event" {
				hints = nil
			}
			got, err := client.ReadNativeQueueObservation(context.Background(), "o/r", "v5", hints)
			if mode == "queue-error" {
				if err == nil || got.State != "unavailable" {
					t.Fatal("queue read failure treated as success")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "no-queue" {
				if got.State != "not-enabled" || !got.Complete || len(got.Candidates) != 0 {
					t.Fatalf("disabled queue: %+v", got)
				}
				return
			}
			memberCount := 1
			if mode == "two-members" || mode == "second-member-absent" {
				memberCount = 2
			}
			if len(got.Entries) != memberCount || got.Entries[0].EnqueuedAt == nil || got.Entries[0].State != "AWAITING_CHECKS" || len(got.Candidates) != 1 {
				t.Fatalf("queue observations: %+v", got)
			}
			group := got.Candidates[0]
			proven := mode != "no-event" && mode != "truncated" && mode != "bad-ancestry" && mode != "moved-ref" && mode != "position-gap" && mode != "second-member-absent"
			if (group.Membership == "proven") != proven || proven && (len(group.Members) != memberCount || group.Members[0].Head != member || group.Tree != tree) ||
				!proven && len(group.Members) != 0 {
				t.Fatalf("unproved member mapping: %+v", group)
			}
			wantCI := "passed"
			switch mode {
			case "wrong-check-head", "foreign-app", "missing-check", "pending":
				wantCI = "pending"
			case "failed":
				wantCI = "failed"
			case "skipped":
				wantCI = "blocked"
			case "policy-unavailable", "moved-ref":
				wantCI = "unknown"
			}
			if group.CI != wantCI || (mode == "truncated" || mode == "position-gap") && (got.Complete || got.EntriesComplete) {
				t.Fatalf("candidate evidence: %+v", group)
			}
		})
	}
}
