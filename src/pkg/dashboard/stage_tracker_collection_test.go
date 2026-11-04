package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/cruxtest"
	cruxruntime "github.com/hivecommons/hive/crux/runtime"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

func TestStageTrackerGraphQLCollectionPreservesRequiredProducer(t *testing.T) {
	requiredFailure := `{"__typename":"CheckRun","databaseId":101,"name":"build","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-11-02T21:00:00Z","completedAt":"2026-11-02T21:01:00Z","checkSuite":{"app":{"databaseId":11},"commit":{"oid":"head"}}}`
	optionalSuccess := `{"__typename":"CheckRun","databaseId":202,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-11-02T21:00:00Z","checkSuite":{"app":{"databaseId":22},"commit":{"oid":"head"}}}`
	cases := []struct {
		name, first, second, app, want string
		denied                         bool
	}{
		{"required first", requiredFailure, optionalSuccess, "11", "failing", false},
		{"required last", optionalSuccess, requiredFailure, "11", "failing", false},
		{"any app null", requiredFailure, optionalSuccess, "null", "unknown", false},
		{"any app explicit", optionalSuccess, requiredFailure, "-1", "unknown", false},
		{"unknown run identity", strings.Replace(requiredFailure, `"databaseId":101`, `"databaseId":null`, 1), strings.Replace(optionalSuccess, `"databaseId":22`, `"databaseId":11`, 1), "11", "unknown", false},
		{"unknown producer", strings.Replace(requiredFailure, `"databaseId":11`, `"databaseId":null`, 1), optionalSuccess, "11", "unknown", false},
		{"equal identity conflicting result", requiredFailure, strings.Replace(strings.Replace(optionalSuccess, `"databaseId":202`, `"databaseId":101`, 1), `"databaseId":22`, `"databaseId":11`, 1), "11", "unknown", false},
		{"wrong head cannot replace failure", requiredFailure, strings.Replace(strings.Replace(optionalSuccess, `"databaseId":22`, `"databaseId":11`, 1), `"oid":"head"`, `"oid":"old-head"`, 1), "11", "failing", false},
		{"real ID orders retry", requiredFailure, strings.Replace(optionalSuccess, `"databaseId":22`, `"databaseId":11`, 1), "11", "passing", false},
		{"queued newer ID has no start time", requiredFailure, strings.Replace(strings.Replace(strings.Replace(optionalSuccess, `"databaseId":22`, `"databaseId":11`, 1), `"status":"COMPLETED"`, `"status":"QUEUED"`, 1), `"startedAt":"2026-11-02T21:00:00Z"`, `"startedAt":null`, 1), "11", "queued", false},
		{"unbound single producer", optionalSuccess, optionalSuccess, "null", "passing", false},
		{"missing binding detail", optionalSuccess, optionalSuccess, "missing", "unknown", false},
		{"reader denied names-only fallback", requiredFailure, optionalSuccess, "11", "unknown", true},
		{"reader denied", requiredFailure, optionalSuccess, "11", "unknown", true},
	}
	cyclic := []string{
		`{"__typename":"CheckRun","databaseId":101,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-11-02T12:05:00Z","checkSuite":{"app":{"databaseId":11},"commit":{"oid":"head"}}}`,
		`{"__typename":"CheckRun","databaseId":102,"name":"build","status":"QUEUED","startedAt":null,"checkSuite":{"app":{"databaseId":11},"commit":{"oid":"head"}}}`,
		`{"__typename":"CheckRun","databaseId":103,"name":"build","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-11-02T12:04:00Z","checkSuite":{"app":{"databaseId":11},"commit":{"oid":"head"}}}`,
	}
	for i, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		cases = append(cases, struct {
			name, first, second, app, want string
			denied                         bool
		}{fmt.Sprintf("cyclic ordering %d", i), cyclic[order[0]] + "," + cyclic[order[1]], cyclic[order[2]], "11", "failing", false})
	}
	for i, count := 0, len(cases); i < count; i++ {
		reversed := cases[i]
		reversed.name += " reversed"
		reversed.first, reversed.second = reversed.second, reversed.first
		cases = append(cases, reversed)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/graphql":
					fmt.Fprintf(w, `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":7,"headRefOid":"head","baseRefOid":"base","baseRefName":"v5","headRefName":"repair","updatedAt":"2026-11-02T21:00:00Z","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","commits":{"nodes":[{"commit":{"oid":"head","statusCheckRollup":{"contexts":{"nodes":[%s,%s]}}}}]}}]}}}}`, tc.first, tc.second)
				case strings.HasSuffix(r.URL.Path, "/protection/required_status_checks"):
					if tc.denied {
						http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
					} else if tc.app == "missing" {
						fmt.Fprint(w, `{"contexts":["build"],"checks":[{"context":"build"}]}`)
					} else {
						fmt.Fprintf(w, `{"contexts":["build"],"checks":[{"context":"build","app_id":%s}]}`, tc.app)
					}
				case strings.Contains(r.URL.Path, "/branches/") || strings.Contains(r.URL.Path, "/rules/"):
					if strings.Contains(tc.name, "names-only") {
						fmt.Fprint(w, `{"protected":true,"protection":{"required_status_checks":{"contexts":["build"]}}}`)
					} else {
						http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
					}
				case strings.HasSuffix(r.URL.Path, "/pulls"):
					fmt.Fprint(w, `[{"number":7,"state":"open","title":"Repair source","user":{"login":"person"},"updated_at":"2026-11-02T21:00:00Z","head":{"sha":"head","ref":"repair","repo":{"full_name":"org/repo"}},"base":{"sha":"base","ref":"v5","repo":{"full_name":"org/repo"}}}]`)
				case strings.HasSuffix(r.URL.Path, "/pulls/7"):
					fmt.Fprint(w, `{"number":7,"state":"open","mergeable":true,"mergeable_state":"clean","head":{"sha":"head"},"base":{"ref":"v5"}}`)
				case strings.HasSuffix(r.URL.Path, "/commits/head/check-runs"):
					t.Error("GraphQL check evidence unexpectedly fell back to REST")
					fmt.Fprint(w, `{"check_runs":[]}`)
				case strings.HasSuffix(r.URL.Path, "/commits/v5/check-runs"):
					fmt.Fprint(w, `{"check_runs":[]}`)
				default:
					fmt.Fprint(w, `[]`)
				}
			}))
			defer srv.Close()
			client := github.NewClientForTest(srv.URL, "org", []string{"org/repo"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			client.SetGraphQLPRBatchConfig(func() bool { return true }, func() int { return 50 })
			if _, err := client.EnumerateActionable(context.Background()); err != nil {
				t.Fatal(err)
			}
			prs := []github.PullRequest{{Repo: "org/repo", Number: 7, HeadSHA: "head", BaseRef: "v5", UpdatedAt: time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)}}
			client.EnrichCIStatus(context.Background(), prs)
			bytes, err := json.Marshal(prs[0])
			if err != nil {
				t.Fatal(err)
			}
			var projected github.PullRequest
			if err := json.Unmarshal(bytes, &projected); err != nil {
				t.Fatal(err)
			}
			if projected.CIActivity == nil || projected.CIActivity.Source != "github-graphql-cache" {
				t.Fatalf("current-head GraphQL collection provenance was lost: %+v", projected.CIActivity)
			}
			got := stageTrackerCIActivity(projected, time.Now())
			if got.State != tc.want {
				t.Fatalf("collection/cache/dedup/serialization/projection = %+v, want %s; evidence %+v", got, tc.want, projected.CIActivity)
			}
			if strings.HasPrefix(tc.name, "cyclic ordering") && projected.CIStatus != "failure" {
				t.Fatalf("GraphQL collection changed canonical CI with input order: %s", projected.CIStatus)
			}
			if !tc.denied && (projected.CIActivity == nil || len(projected.CIActivity.RequiredChecks) != 1) {
				t.Fatal("required context/app binding was lost")
			}
			if (tc.denied || tc.app == "missing") && projected.CIActivity.RequiredKnown {
				t.Fatal("reader denial was called observed required policy")
			}
			stale := time.Now().Add(-trackerActivityFreshWindow - time.Second)
			projected.CIActivity.ObservedAt = &stale
			// A stale snapshot keeps its observation time for the tooltip;
			// Active follows the state alone (hivecommons/hive#16643).
			if activity := stageTrackerCIActivity(projected, time.Now()); activity.Active != (activity.State == "running" || activity.State == "queued") || activity.ObservedAt == nil || !activity.ObservedAt.Equal(stale) {
				t.Fatalf("a stale serialized collection snapshot changed the activity's fill or lost its age: %+v", activity)
			}
			if got.State == "failing" {
				expectedID := int64(101)
				if strings.HasPrefix(tc.name, "cyclic ordering") {
					expectedID = 103
				}
				found := false
				for _, check := range projected.CIActivity.Checks {
					if check.ID == expectedID && check.AppID != nil && *check.AppID == 11 && check.Head == "head" && check.Since != nil {
						found = true
					}
				}
				if !found {
					t.Fatal("real check ID/app/head/transition time was lost")
				}
			}
		})
	}
}

func TestStageTrackerSuccessfulSourceEditCollectionThroughProjection(t *testing.T) {
	resetAgentLastActions(t)
	dir := t.TempDir()
	git := func(args ...string) string {
		output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "--quiet", "-b", "fixture")
	git("config", "user.name", "Activity Test")
	git("config", "user.email", "activity@example.invalid")
	git("remote", "add", "origin", "https://github.com/org/repo.git")
	file := filepath.Join(dir, "source.go")
	if err := os.WriteFile(file, []byte("package before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.go")
	git("commit", "--quiet", "-s", "-m", "fixture")
	now := time.Now().UTC()
	kick := now.Add(-time.Minute)
	editAt := now.Add(-time.Second)
	if err := os.WriteFile(file, []byte("package after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, editAt, editAt); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"path": file})
	transcript := fmt.Sprintf("{\"type\":\"tool.execution_start\",\"timestamp\":%q,\"data\":{\"toolCallId\":\"edit\",\"toolName\":\"edit\",\"arguments\":%s}}\n{\"type\":\"tool.execution_complete\",\"timestamp\":%q,\"data\":{\"toolCallId\":\"edit\",\"success\":true}}\n", editAt.Format(time.RFC3339Nano), args, now.Format(time.RFC3339Nano))
	observation := cruxruntime.ObserveCodeWorkTranscript(strings.NewReader(transcript), dir, kick, now)
	if observation == nil {
		t.Fatal("actual successful source edit was not collected")
	}
	proc := &crux.Process{Name: "fixer", Config: config.AgentConfig{Backend: "copilot", Enabled: true},
		State: crux.StateRunning, LastKick: &kick, CodeWork: observation, OutputBuffer: cruxtest.NewRingBuffer(10)}
	RecordAgentLastAction(github.AuditRecord{Repo: "org/repo", Target: 7, Agent: "fixer", Action: github.AuditActionAgentCommentCreated}, now)
	worker := buildOneAgent(t, proc)
	dispatch := review.DispatchState{GeneratedAt: now, Fixes: []review.PendingFix{
		{Repo: "org/repo", Number: 7, HeadSHA: git("rev-parse", "HEAD"), Agent: "fixer", Dispatched: kick},
	}}
	it := StageTrackerItem{Kind: "pr", Repo: "org/repo", Number: 7, Head: dispatch.Fixes[0].HeadSHA, Stage: trackerNumberFixing}
	stageTrackerApplyCodeActivity(&it, dispatch, &StatusPayload{Agents: []FrontendAgent{worker}}, now)
	if !it.CodeActivity.Active || it.DisplayColumn != trackerNumberPRInProgress {
		t.Fatalf("producer/process/frontend/assignment/projection lost actual source work: %+v", it.CodeActivity)
	}
	newKick := now
	proc.LastKick = &newKick
	worker = buildOneAgent(t, proc)
	if worker.CodeWork != nil {
		t.Fatal("a new round retained previous-round source evidence")
	}
}

func TestStageTrackerCollectedReviewReceiptDoesNotSettleAnotherAgent(t *testing.T) {
	now := time.Now().UTC()
	pr, dispatch, status := trackerActivityFixture(now)
	status.Agents[0].Name = "B"
	dispatch.Pending = []review.PendingReview{
		{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: review.PerspectiveCorrectness, Head: "alpha", Agent: "A", Dispatched: now},
		{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: review.PerspectiveCorrectness, Head: "beta", Agent: "B", Dispatched: now},
	}
	dir := t.TempDir()
	writeReport := func(agent, head string) {
		t.Helper()
		path := filepath.Join(dir, review.ReportFilePrefix(pr.Repo, pr.Number)+agent+".json")
		report := fmt.Sprintf(`{"lane":"review-swarm","kind":"review","findings":[],"prs_opened":[],"beads_filed":[],"summary":"Review complete","perspective":"correctness","verdict":"approve","repo":%q,"number":%d,"head_sha":%q,"head":%q,"reporter_agent":%q}`,
			pr.Repo, pr.Number, pr.HeadSHA, head, agent)
		if err := os.WriteFile(path, []byte(report), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now, now); err != nil {
			t.Fatal(err)
		}
	}
	collect := func() review.Aggregate {
		t.Helper()
		artifact, err := review.Collect(dir, review.AggregateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(artifact)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &artifact); err != nil {
			t.Fatal(err)
		}
		aggregate, ok := artifact.AggregateFor(pr.Repo, pr.Number, pr.HeadSHA)
		if !ok {
			t.Fatal("collected current-head report was lost")
		}
		return aggregate
	}
	writeReport("A", "alpha")
	aggregate := collect()
	bundle := &evidence.Bundle{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, UpdatedAt: now,
		Verdicts: []evidence.Verdict{{Perspective: "correctness", Verdict: "approve", RecordedAt: now}}}
	if got := stageTrackerReviewActivity(pr, bundle, &aggregate, dispatch, status, time.Now()); !got.Active || got.Actor != "B" {
		t.Fatalf("A's collected same-perspective report hid B: %+v", got)
	}
	writeReport("B", "beta")
	aggregate = collect()
	if !review.PendingReviewCompleted(dispatch.Pending[1], review.Artifact{Items: []review.Aggregate{aggregate}}) {
		t.Fatal("B's own collected receipt did not settle B")
	}
	if got := stageTrackerReviewActivity(pr, bundle, &aggregate, dispatch, status, time.Now()); got.Active {
		t.Fatalf("B's completed assignment remained active: %+v", got)
	}
}
