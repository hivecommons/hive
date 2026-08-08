package github

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

type sweepPR struct {
	number          int
	author          string
	queuedBy        string
	label           bool
	draft           bool
	mergeableState  string
	statusState     string
	checkStatus     string
	checkConclusion string
}

func TestSweepQueuedAutoMergesMergesLabelledGreenPRAudits(t *testing.T) {
	var audits []AutoMergeSweepEvent
	var merged []int
	api := newAutoMergeSweepAPI(t, AutoMergeQueuedLabel, []sweepPR{{
		number:          7,
		author:          "alice",
		queuedBy:        "bob",
		label:           true,
		mergeableState:  "clean",
		statusState:     "success",
		checkStatus:     "completed",
		checkConclusion: "success",
	}}, &merged)
	defer api.Close()

	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{
		Audit: func(event AutoMergeSweepEvent) { audits = append(audits, event) },
	})
	if err != nil {
		t.Fatalf("SweepQueuedAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 1 || len(merged) != 1 || merged[0] != 7 {
		t.Fatalf("merged result=%v merge calls=%v, want PR 7 merged", result.Merged, merged)
	}
	if len(audits) != 1 || audits[0].Repo != "widget" || audits[0].Number != 7 || audits[0].QueuedBy != "bob" {
		t.Fatalf("audit events = %#v, want PR 7 queued by bob", audits)
	}
}

func TestSweepQueuedAutoMergesSkipsRedUnlabelledAndDraft(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, AutoMergeQueuedLabel, []sweepPR{
		{number: 7, author: "alice", queuedBy: "bob", label: true, mergeableState: "clean", statusState: "failure", checkStatus: "completed", checkConclusion: "success"},
		{number: 8, author: "carol", queuedBy: "dave", label: false, mergeableState: "clean", statusState: "success", checkStatus: "completed", checkConclusion: "success"},
		{number: 9, author: "erin", queuedBy: "frank", label: true, draft: true, mergeableState: "clean", statusState: "success", checkStatus: "completed", checkConclusion: "success"},
	}, &merged)
	defer api.Close()

	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepQueuedAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 0 || len(merged) != 0 {
		t.Fatalf("merged result=%v merge calls=%v, want no merges", result.Merged, merged)
	}
	if result.Seen != 2 {
		t.Fatalf("seen=%d, want only the two labelled PRs", result.Seen)
	}
}

func TestSweepQueuedAutoMergesRespectsCap(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, AutoMergeQueuedLabel, []sweepPR{
		{number: 7, author: "alice", queuedBy: "bob", label: true, mergeableState: "clean", statusState: "success", checkStatus: "completed", checkConclusion: "success"},
		{number: 8, author: "carol", queuedBy: "dave", label: true, mergeableState: "clean", statusState: "success", checkStatus: "completed", checkConclusion: "success"},
	}, &merged)
	defer api.Close()

	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{MaxMerges: 1})
	if err != nil {
		t.Fatalf("SweepQueuedAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 1 || len(merged) != 1 || merged[0] != 7 {
		t.Fatalf("merged result=%v merge calls=%v, want only first PR merged", result.Merged, merged)
	}
}

func TestSweepQueuedAutoMergesHonorsConfiguredLabel(t *testing.T) {
	const label = "ship-it"
	var merged []int
	api := newAutoMergeSweepAPI(t, label, []sweepPR{{
		number: 7, author: "alice", queuedBy: "bob", label: true, mergeableState: "clean",
		statusState: "success", checkStatus: "completed", checkConclusion: "success",
	}}, &merged)
	defer api.Close()

	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	c.SetAutoMergeLabel(label)
	if _, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != nil {
		t.Fatalf("SweepQueuedAutoMerges returned error: %v", err)
	}
	if len(merged) != 1 || merged[0] != 7 {
		t.Fatalf("merge calls=%v, want custom-labelled PR merged", merged)
	}
}

func TestSweepQueuedAutoMergesRechecksSelfMergeBan(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, AutoMergeQueuedLabel, []sweepPR{{
		number: 7, author: "alice", queuedBy: "ALICE", label: true, mergeableState: "clean",
		statusState: "success", checkStatus: "completed", checkConclusion: "success",
	}}, &merged)
	defer api.Close()

	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepQueuedAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 0 || len(merged) != 0 {
		t.Fatalf("merged result=%v merge calls=%v, want self-queued PR skipped", result.Merged, merged)
	}
}

func TestAutoMergeSweepHelpersAndNilClient(t *testing.T) {
	var nilClient *Client
	if _, err := nilClient.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != ErrNoGitHubClient {
		t.Fatalf("nil sweep error = %v, want ErrNoGitHubClient", err)
	}
	if got := parseHiveQueueReview("Approved by @Alice-1 for Hive auto-merge on green CI."); got != "Alice-1" {
		t.Fatalf("parseHiveQueueReview = %q, want Alice-1", got)
	}
	if got := parseHiveQueueReview("Approved for Hive auto-merge on green CI."); got != "" {
		t.Fatalf("legacy body parsed as %q, want empty", got)
	}
	if !hasLabel([]string{"LGTM"}, "lgtm") || hasLabel([]string{"hold"}, "lgtm") {
		t.Fatal("hasLabel case-insensitive matching failed")
	}
	if !isGitHubStatus(&gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}, http.StatusNotFound) {
		t.Fatal("isGitHubStatus did not match GitHub status")
	}
	if isGitHubStatus(context.Canceled, http.StatusNotFound) {
		t.Fatal("isGitHubStatus matched a non-GitHub error")
	}
	c := &Client{}
	c.warn("ignored")
	c.info("ignored")
	c.logger = slog.Default()
	c.warn("covered")
	c.info("covered")
}

func TestCommitGreenStatusAndCheckBranches(t *testing.T) {
	tests := []struct {
		name            string
		statusState     string
		statusTotal     int
		checkStatus     string
		checkConclusion string
		wantGreen       bool
		wantReason      string
	}{
		{name: "failure status blocks", statusState: "failure", statusTotal: 1, wantReason: "status-failure"},
		{name: "pending status with no red checks relies on mergeable", statusState: "pending", statusTotal: 1, checkStatus: "queued", wantGreen: true},
		{name: "check failure blocks", statusState: "success", statusTotal: 1, checkStatus: "completed", checkConclusion: "failure", wantReason: "check-failure"},
		{name: "no statuses or checks is green after mergeable gate", statusState: "pending", statusTotal: 0, wantGreen: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/status":
					json.NewEncoder(w).Encode(map[string]any{"state": tt.statusState, "total_count": tt.statusTotal})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/check-runs":
					runs := []map[string]string{}
					if tt.checkStatus != "" {
						runs = append(runs, map[string]string{"name": "build", "status": tt.checkStatus, "conclusion": tt.checkConclusion})
					}
					json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs})
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer api.Close()
			c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
			green, reason, err := c.commitGreen(context.Background(), "acme", "widget", "sha")
			if err != nil {
				t.Fatalf("commitGreen returned error: %v", err)
			}
			if green != tt.wantGreen || reason != tt.wantReason {
				t.Fatalf("commitGreen = (%v,%q), want (%v,%q)", green, reason, tt.wantGreen, tt.wantReason)
			}
		})
	}
}

func TestTrySweepQueuedPRSkipBranches(t *testing.T) {
	tests := []struct {
		name     string
		pr       sweepPR
		state    string
		labels   []map[string]string
		noReview bool
		want     string
	}{
		{name: "closed", pr: sweepPR{number: 7, author: "alice", queuedBy: "bob", label: true}, state: "closed", want: "closed"},
		{name: "label removed", pr: sweepPR{number: 7, author: "alice", queuedBy: "bob", label: true}, state: "open", labels: nil, want: "label-removed"},
		{name: "missing queue approval", pr: sweepPR{number: 7, author: "alice", queuedBy: "bob", label: true, mergeableState: "clean"}, state: "open", labels: []map[string]string{{"name": AutoMergeQueuedLabel}}, noReview: true, want: "no-hive-queue-approval"},
		{name: "not mergeable", pr: sweepPR{number: 7, author: "alice", queuedBy: "bob", label: true, mergeableState: "dirty"}, state: "open", labels: []map[string]string{{"name": AutoMergeQueuedLabel}}, want: "not-mergeable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
					json.NewEncoder(w).Encode(map[string]any{
						"number":          7,
						"state":           tt.state,
						"mergeable_state": tt.pr.mergeableState,
						"mergeable":       tt.pr.mergeableState == "clean",
						"user":            map[string]string{"login": tt.pr.author},
						"head":            map[string]string{"sha": "sha7"},
						"labels":          tt.labels,
					})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
					if tt.noReview {
						json.NewEncoder(w).Encode([]map[string]any{})
					} else {
						json.NewEncoder(w).Encode([]map[string]any{{"state": "APPROVED", "body": "Approved by @" + tt.pr.queuedBy + " for Hive auto-merge on green CI."}})
					}
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer api.Close()
			c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
			_, reason, err := c.trySweepQueuedPR(context.Background(), "widget", "acme", "widget", 7, AutoMergeQueuedLabel)
			if err != nil {
				t.Fatalf("trySweepQueuedPR returned error: %v", err)
			}
			if reason != tt.want {
				t.Fatalf("reason = %q, want %q", reason, tt.want)
			}
		})
	}
}

func TestTrySweepQueuedPRMissingHeadAndMergeNotApplied(t *testing.T) {
	tests := []struct {
		name        string
		head        map[string]string
		mergeResult map[string]any
		want        string
	}{
		{name: "missing head", head: nil, want: "missing-head-sha"},
		{name: "merge not applied", head: map[string]string{"sha": "sha7"}, mergeResult: map[string]any{"merged": false}, want: "merge-not-applied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
					json.NewEncoder(w).Encode(map[string]any{
						"number":          7,
						"state":           "open",
						"mergeable_state": "clean",
						"mergeable":       true,
						"user":            map[string]string{"login": "alice"},
						"head":            tt.head,
						"labels":          []map[string]string{{"name": AutoMergeQueuedLabel}},
					})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
					json.NewEncoder(w).Encode([]map[string]any{{"state": "COMMENTED", "body": "noise"}, {"state": "APPROVED", "body": "Approved by @bob for Hive auto-merge on green CI."}})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha7/status":
					json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 1})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha7/check-runs":
					json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "check_runs": []map[string]string{{"name": "build", "status": "completed", "conclusion": "success"}}})
				case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/merge":
					json.NewEncoder(w).Encode(tt.mergeResult)
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer api.Close()
			c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
			_, reason, err := c.trySweepQueuedPR(context.Background(), "widget", "acme", "widget", 7, AutoMergeQueuedLabel)
			if err != nil {
				t.Fatalf("trySweepQueuedPR returned error: %v", err)
			}
			if reason != tt.want {
				t.Fatalf("reason = %q, want %q", reason, tt.want)
			}
		})
	}
}

func TestTrySweepQueuedPRMergeError(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			json.NewEncoder(w).Encode(map[string]any{
				"number": 7, "state": "open", "mergeable_state": "clean", "mergeable": true,
				"user":   map[string]string{"login": "alice"},
				"head":   map[string]string{"sha": "sha7"},
				"labels": []map[string]string{{"name": AutoMergeQueuedLabel}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			json.NewEncoder(w).Encode([]map[string]any{{"state": "APPROVED", "body": "Approved by @bob for Hive auto-merge on green CI."}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha7/status":
			json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 1})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha7/check-runs":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "check_runs": []map[string]string{{"name": "build", "status": "completed", "conclusion": "success"}}})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/merge":
			http.Error(w, "conflict", http.StatusConflict)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()
	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	_, reason, err := c.trySweepQueuedPR(context.Background(), "widget", "acme", "widget", 7, AutoMergeQueuedLabel)
	if err == nil || reason != "merge-failed" {
		t.Fatalf("trySweepQueuedPR reason=%q err=%v, want merge-failed error", reason, err)
	}
}

func TestCommitGreenAPIErrors(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantReason string
	}{
		{name: "status error", statusCode: http.StatusInternalServerError, wantReason: "status-check"},
		{name: "check error", statusCode: http.StatusOK, wantReason: "check-runs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/status":
					if tt.statusCode != http.StatusOK {
						http.Error(w, "boom", tt.statusCode)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 1})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/check-runs":
					http.Error(w, "boom", http.StatusInternalServerError)
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer api.Close()
			c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
			_, reason, err := c.commitGreen(context.Background(), "acme", "widget", "sha")
			if err == nil || reason != tt.wantReason {
				t.Fatalf("commitGreen reason=%q err=%v, want %q error", reason, err, tt.wantReason)
			}
		})
	}
}

func TestListQueuedPullRequestIssuesPaginates(t *testing.T) {
	var base string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("Link", `<`+base+`/repos/acme/widget/issues?page=2>; rel="next"`)
			json.NewEncoder(w).Encode([]map[string]any{{"number": 1}})
		case "2":
			json.NewEncoder(w).Encode([]map[string]any{{"number": 2}})
		default:
			t.Fatalf("unexpected page: %s", r.URL.RawQuery)
		}
	}))
	defer api.Close()
	base = api.URL

	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	issues, err := c.listQueuedPullRequestIssues(context.Background(), "acme", "widget", AutoMergeQueuedLabel)
	if err != nil {
		t.Fatalf("listQueuedPullRequestIssues returned error: %v", err)
	}
	if len(issues) != 2 || issues[0].GetNumber() != 1 || issues[1].GetNumber() != 2 {
		t.Fatalf("issues = %#v, want pages 1 and 2", issues)
	}
}

func newAutoMergeSweepAPI(t *testing.T, expectedLabel string, prs []sweepPR, merged *[]int) *httptest.Server {
	t.Helper()
	byNumber := make(map[int]sweepPR, len(prs))
	for _, pr := range prs {
		byNumber[pr.number] = pr
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues":
			if got := r.URL.Query().Get("labels"); got != expectedLabel {
				t.Fatalf("issues labels query = %q, want %q", got, expectedLabel)
			}
			var issues []map[string]any
			for _, pr := range prs {
				if !pr.label {
					continue
				}
				issues = append(issues, map[string]any{
					"number":       pr.number,
					"pull_request": map[string]any{"url": "https://api.example/pr/" + strconv.Itoa(pr.number)},
				})
			}
			json.NewEncoder(w).Encode(issues)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/pulls/") && strings.HasSuffix(r.URL.Path, "/reviews"):
			number := pathNumber(t, r.URL.Path, "/repos/acme/widget/pulls/", "/reviews")
			pr := byNumber[number]
			body := "Approved by @" + pr.queuedBy + " for Hive auto-merge on green CI."
			json.NewEncoder(w).Encode([]map[string]any{{"state": "APPROVED", "body": body}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/pulls/"):
			number := pathNumber(t, r.URL.Path, "/repos/acme/widget/pulls/", "")
			pr := byNumber[number]
			json.NewEncoder(w).Encode(map[string]any{
				"number":          pr.number,
				"state":           "open",
				"draft":           pr.draft,
				"mergeable_state": pr.mergeableState,
				"mergeable":       pr.mergeableState == "clean",
				"user":            map[string]string{"login": pr.author},
				"head":            map[string]string{"sha": "sha" + strconv.Itoa(pr.number)},
				"labels":          []map[string]string{{"name": expectedLabel}},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/commits/") && strings.HasSuffix(r.URL.Path, "/status"):
			number := shaNumber(t, r.URL.Path)
			pr := byNumber[number]
			total := 1
			json.NewEncoder(w).Encode(map[string]any{"state": pr.statusState, "total_count": total})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/commits/") && strings.HasSuffix(r.URL.Path, "/check-runs"):
			number := shaNumber(t, r.URL.Path)
			pr := byNumber[number]
			json.NewEncoder(w).Encode(map[string]any{
				"total_count": 1,
				"check_runs": []map[string]string{{
					"name":       "build",
					"status":     pr.checkStatus,
					"conclusion": pr.checkConclusion,
				}},
			})
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/pulls/") && strings.HasSuffix(r.URL.Path, "/merge"):
			number := pathNumber(t, r.URL.Path, "/repos/acme/widget/pulls/", "/merge")
			*merged = append(*merged, number)
			json.NewEncoder(w).Encode(map[string]any{"merged": true, "sha": "merge" + strconv.Itoa(number)})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
}

func pathNumber(t *testing.T, path, prefix, suffix string) int {
	t.Helper()
	raw := strings.TrimPrefix(path, prefix)
	if suffix != "" {
		raw = strings.TrimSuffix(raw, suffix)
	}
	number, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("parse number from %q: %v", path, err)
	}
	return number
}

func shaNumber(t *testing.T, path string) int {
	t.Helper()
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if strings.HasPrefix(part, "sha") {
			number, err := strconv.Atoi(strings.TrimPrefix(part, "sha"))
			if err != nil {
				t.Fatalf("parse sha number from %q: %v", path, err)
			}
			return number
		}
	}
	t.Fatalf("sha number not found in %q", path)
	return 0
}
