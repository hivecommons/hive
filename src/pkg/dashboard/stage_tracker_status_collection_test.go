package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func TestStageTrackerRESTCheckPaginationMustCompleteBeforeProjection(t *testing.T) {
	for _, mode := range []string{"complete", "partial", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			reads := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/pulls/7"):
					fmt.Fprint(w, `{"number":7,"mergeable":true,"mergeable_state":"blocked","head":{"sha":"head"},"base":{"ref":"v5"}}`)
				case strings.HasSuffix(r.URL.Path, "/commits/head/check-runs"):
					reads++
					if mode == "malformed" {
						fmt.Fprint(w, `{}`)
					} else if r.URL.Query().Get("page") == "2" {
						if mode == "partial" {
							http.Error(w, "unavailable", http.StatusServiceUnavailable)
						} else {
							fmt.Fprint(w, `{"check_runs":[{"id":103,"name":"validate","head_sha":"head","app":{"id":11},"status":"completed","conclusion":"failure"}]}`)
						}
					} else {
						w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
						fmt.Fprint(w, `{"check_runs":[{"id":101,"name":"validate","head_sha":"head","app":{"id":11},"status":"completed","conclusion":"success"}]}`)
					}
				case strings.HasSuffix(r.URL.Path, "/protection/required_status_checks"):
					fmt.Fprint(w, `{"contexts":["validate"],"checks":[{"context":"validate","app_id":11}]}`)
				default:
					fmt.Fprint(w, `{}`)
				}
			}))
			defer srv.Close()
			client := github.NewClientForTest(srv.URL, "org", []string{"repo"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			client.SetGraphQLPRBatchConfig(func() bool { return false }, func() int { return 50 })
			prs := []github.PullRequest{{Repo: "org/repo", Number: 7, HeadSHA: "head", BaseRef: "v5", MergeableState: "blocked"}}
			client.EnrichCIStatus(context.Background(), prs)
			got := stageTrackerCIActivity(prs[0], time.Now())
			if mode == "complete" {
				if reads != 2 || got.State != "failing" || prs[0].CIStatus != "failure" {
					t.Fatalf("later-page required failure was lost: reads=%d, %+v", reads, got)
				}
			} else if got.State != "unknown" || prs[0].CIStatus == "success" ||
				(prs[0].Protection != nil && len(prs[0].Protection.MissingRequiredChecks) != 0) {
				t.Fatalf("incomplete check collection invented a verdict or absence: %+v", got)
			}
		})
	}
}

func TestStageTrackerRESTCommitStatusCollectionAndRequiredBinding(t *testing.T) {
	for _, tc := range []struct {
		name, app, status, want string
		missing, red            bool
	}{
		{"any-app passing", "null", "success", "passing", false, false},
		{"any-app failing", "null", "failure", "failing", false, true},
		{"any-app pending", "null", "pending", "queued", false, false},
		{"any-app error", "-1", "error", "failing", false, true},
		{"explicit-any-app passing", "-1", "success", "passing", false, false},
		{"pinned-app cannot use unbound success", "11", "success", "unknown", false, false},
		{"pinned-app unbound pending", "11", "pending", "unknown", false, false},
		{"status-reader denied", "null", "denied", "unknown", false, false},
		{"status-reader partial", "null", "partial", "unknown", false, false},
		{"wrong-head statuses", "null", "wrong-head", "unknown", false, false},
		{"observed absent from both sources", "null", "absent", "missing", true, false},
		{"malformed status reader", "null", "malformed", "unknown", false, false},
		{"check and status passing", "null", "success", "passing", false, false},
		{"check and status failing", "null", "failure", "failing", false, true},
		{"check and status pending", "null", "pending", "queued", false, false},
		{"check and denied status", "null", "denied", "unknown", false, false},
		{"check and malformed status", "null", "malformed", "unknown", false, false},
		{"missing status context", "null", "malformed-context", "unknown", false, false},
		{"unknown published status state", "null", "mystery", "unknown", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statusReads := 0
			withCheck := strings.HasPrefix(tc.name, "check and")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/pulls/7"):
					fmt.Fprint(w, `{"number":7,"state":"open","mergeable":true,"mergeable_state":"blocked","head":{"sha":"head"},"base":{"ref":"v5"}}`)
				case strings.HasSuffix(r.URL.Path, "/check-runs"):
					if withCheck {
						fmt.Fprint(w, `{"total_count":1,"check_runs":[{"id":301,"name":"validate","head_sha":"head","app":{"id":22},"status":"completed","conclusion":"success"}]}`)
					} else {
						fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
					}
				case strings.HasSuffix(r.URL.Path, "/protection/required_status_checks"):
					fmt.Fprintf(w, `{"contexts":["validate"],"checks":[{"context":"validate","app_id":%s}]}`, tc.app)
				case strings.HasSuffix(r.URL.Path, "/status"):
					statusReads++
					switch tc.status {
					case "denied":
						http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
					case "partial":
						if r.URL.Query().Get("page") == "2" {
							http.Error(w, `{"message":"unavailable"}`, http.StatusServiceUnavailable)
						} else {
							w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
							fmt.Fprint(w, `{"sha":"head","statuses":[]}`)
						}
					case "wrong-head":
						fmt.Fprint(w, `{"sha":"old-head","statuses":[{"id":201,"context":"validate","state":"success"}]}`)
					case "absent":
						fmt.Fprint(w, `{"sha":"head","statuses":[]}`)
					case "malformed":
						fmt.Fprint(w, `{}`)
					case "malformed-context":
						fmt.Fprint(w, `{"sha":"head","statuses":[{"id":201,"state":"success"}]}`)
					default:
						fmt.Fprintf(w, `{"sha":"head","statuses":[{"id":201,"node_id":"actual-status-node","context":"validate","state":%q,"created_at":"2026-11-02T12:00:00Z","target_url":"https://github.com/org/repo/commit/head"}]}`, tc.status)
					}
				default:
					fmt.Fprint(w, `{}`)
				}
			}))
			defer srv.Close()
			client := github.NewClientForTest(srv.URL, "org", []string{"repo"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			client.SetGraphQLPRBatchConfig(func() bool { return false }, func() int { return 50 })
			prs := []github.PullRequest{{Repo: "org/repo", Number: 7, HeadSHA: "head", BaseRef: "v5", MergeableState: "blocked"}}
			client.EnrichCIStatus(context.Background(), prs)
			raw, err := json.Marshal(prs[0])
			if err != nil {
				t.Fatal(err)
			}
			var pr github.PullRequest
			if err := json.Unmarshal(raw, &pr); err != nil {
				t.Fatal(err)
			}
			if statusReads == 0 || pr.CIActivity == nil || !pr.CIActivity.RequiredKnown || pr.CIActivity.RequiredSource != "protection" {
				t.Fatalf("actual protection/status readers were bypassed: reads=%d, %+v", statusReads, pr.CIActivity)
			}
			if got := stageTrackerCIActivity(pr, time.Now()); got.State != tc.want {
				t.Fatalf("REST/checks/protection/status/JSON/projection = %+v, want %s; raw %s", got, tc.want, raw)
			}
			if pr.Protection == nil {
				t.Fatal("canonical protection facts were dropped")
			}
			if got := len(pr.Protection.MissingRequiredChecks) > 0; got != tc.missing {
				t.Fatalf("canonical missing=%v, want %v: %+v", got, tc.missing, pr.Protection)
			}
			if got := len(pr.Protection.FailingRequiredChecks) > 0; got != tc.red {
				t.Fatalf("canonical failing=%v, want %v: %+v", got, tc.red, pr.Protection)
			}
			if tc.want == "passing" || tc.want == "failing" || tc.want == "queued" {
				a := pr.CIActivity
				foundStatus := false
				for _, check := range a.Checks {
					foundStatus = foundStatus || (check.ID == 201 && check.Kind == "commit_status" && check.Head == "head")
				}
				if !a.StatusesKnown || a.StatusesObservedAt == nil || !foundStatus {
					t.Fatalf("real status identity/provenance was lost: %+v", a)
				}
				if !withCheck {
					staleChecks := time.Now().Add(-trackerActivityFreshWindow - time.Second)
					a.ObservedAt = &staleChecks
					got := stageTrackerCIActivity(pr, time.Now())
					if got.ObservedAt == nil || !got.ObservedAt.Equal(*a.StatusesObservedAt) || got.Active {
						t.Fatalf("status freshness inherited the unrelated check snapshot: %+v", got)
					}
				}
			}
		})
	}
}
