package automerge

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

const testHiveAppBotLogin = "kubestellar-hive[bot]"

type sweepPR struct {
	number          int
	author          string
	queuedBy        string
	reviewAuthor    string
	headSHA         string
	reviewCommitID  *string
	label           bool
	extraLabels     []string
	draft           bool
	mergeableState  string
	statusState     string
	checkStatus     string
	checkConclusion string
}

func TestSweepQueuedAutoMergesMergesLabelledGreenPRAudits(t *testing.T) {
	var audits []AutoMergeSweepEvent
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{{
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

	c := newAutoMergeSweepClient(api.URL)
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
	if audits[0].HeadSHA != "sha7" {
		t.Fatalf("audit head SHA = %q, want sha7", audits[0].HeadSHA)
	}
}

func TestSweepQueuedAutoMergesIgnoresForgedNonAppQueueApproval(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{{
		number:          7,
		author:          "alice",
		queuedBy:        "bob",
		reviewAuthor:    "mallory",
		label:           true,
		mergeableState:  "clean",
		statusState:     "success",
		checkStatus:     "completed",
		checkConclusion: "success",
	}}, &merged)
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepQueuedAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 0 || len(merged) != 0 || result.Skipped != 1 {
		t.Fatalf("result=%+v merge calls=%v, want non-App review ignored and PR skipped", result, merged)
	}
}

func TestSweepQueuedAutoMergesReportsMissingAppBotLoginOnce(t *testing.T) {
	var logs bytes.Buffer
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{
		{number: 7, author: "alice", queuedBy: "bob", label: true, mergeableState: "clean"},
		{number: 8, author: "carol", queuedBy: "dave", label: true, mergeableState: "clean"},
	}, &merged)
	defer api.Close()

	client := hgithub.NewClient("token", "acme", []string{"widget"}, slog.New(slog.NewTextHandler(&logs, nil)), api.URL)
	c := New(client, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepQueuedAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 0 || len(merged) != 0 || result.Skipped != 2 {
		t.Fatalf("result=%+v merge calls=%v, want both PRs skipped without merging", result, merged)
	}
	logText := logs.String()
	if count := strings.Count(logText, autoMergeWarnNoAppBotLogin); count != 1 {
		t.Fatalf("missing app bot login warnings = %d in %q, want exactly one", count, logText)
	}
	if !strings.Contains(logText, autoMergeReasonNoAppBotLogin) || !strings.Contains(logText, "App-authorship cannot be verified") {
		t.Fatalf("missing app bot login log = %q, want reason and cause", logText)
	}
}

func TestSweepQueuedAutoMergesDequeuesWhenApprovalHeadChanged(t *testing.T) {
	reviewHead := "oldsha"
	var merged []int
	var removedLabel, commented bool
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{{
		number:          7,
		author:          "alice",
		queuedBy:        "bob",
		headSHA:         "newsha",
		reviewCommitID:  &reviewHead,
		label:           true,
		mergeableState:  "clean",
		statusState:     "success",
		checkStatus:     "completed",
		checkConclusion: "success",
	}}, &merged, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/repos/acme/widget/issues/7/labels/"+hgithub.AutoMergeQueuedLabel:
			removedLabel = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			commented = true
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	})
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepQueuedAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 0 || len(merged) != 0 {
		t.Fatalf("merged result=%v merge calls=%v, want stale approval skipped", result.Merged, merged)
	}
	if !removedLabel || !commented {
		t.Fatalf("removedLabel=%v commented=%v, want queue invalidated with comment", removedLabel, commented)
	}
}

func TestSweepQueuedAutoMergesDequeuesWhenApprovalHeadMissing(t *testing.T) {
	missingHead := ""
	var merged []int
	var removedLabel, commented bool
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{{
		number:          7,
		author:          "alice",
		queuedBy:        "bob",
		reviewCommitID:  &missingHead,
		label:           true,
		mergeableState:  "clean",
		statusState:     "success",
		checkStatus:     "completed",
		checkConclusion: "success",
	}}, &merged, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/repos/acme/widget/issues/7/labels/"+hgithub.AutoMergeQueuedLabel:
			removedLabel = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			commented = true
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	})
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepQueuedAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 0 || len(merged) != 0 {
		t.Fatalf("merged result=%v merge calls=%v, want missing approval head skipped", result.Merged, merged)
	}
	if !removedLabel || !commented {
		t.Fatalf("removedLabel=%v commented=%v, want queue invalidated with comment", removedLabel, commented)
	}
}

func TestSweepQueuedAutoMergesSkipsRedUnlabelledAndDraft(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{
		{number: 7, author: "alice", queuedBy: "bob", label: true, mergeableState: "clean", statusState: "failure", checkStatus: "completed", checkConclusion: "success"},
		{number: 8, author: "carol", queuedBy: "dave", label: false, mergeableState: "clean", statusState: "success", checkStatus: "completed", checkConclusion: "success"},
		{number: 9, author: "erin", queuedBy: "frank", label: true, draft: true, mergeableState: "clean", statusState: "success", checkStatus: "completed", checkConclusion: "success"},
	}, &merged)
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
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
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{
		{number: 7, author: "alice", queuedBy: "bob", label: true, mergeableState: "clean", statusState: "success", checkStatus: "completed", checkConclusion: "success"},
		{number: 8, author: "carol", queuedBy: "dave", label: true, mergeableState: "clean", statusState: "success", checkStatus: "completed", checkConclusion: "success"},
	}, &merged)
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
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

	c := newAutoMergeSweepClient(api.URL)
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
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{{
		number: 7, author: "alice", queuedBy: "ALICE", label: true, mergeableState: "clean",
		statusState: "success", checkStatus: "completed", checkConclusion: "success",
	}}, &merged)
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepQueuedAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 0 || len(merged) != 0 {
		t.Fatalf("merged result=%v merge calls=%v, want self-queued PR skipped", result.Merged, merged)
	}
}

func TestAutoMergeSweepHelpersAndNilClient(t *testing.T) {
	var nilClient *Engine
	if _, err := nilClient.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != hgithub.ErrNoGitHubClient {
		t.Fatalf("nil sweep error = %v, want hgithub.ErrNoGitHubClient", err)
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
	c := &Engine{}
	c.warn("ignored")
	c.info("ignored")
	c.logger = slog.Default()
	c.warn("covered")
	c.info("covered")
}

func TestCommitGreenStatusAndCheckBranches(t *testing.T) {
	type status struct{ context, state string }
	type check struct{ name, status, conclusion string }
	tests := []struct {
		name       string
		statuses   []status
		checks     []check
		wantGreen  bool
		wantReason string
	}{
		{name: "failure status blocks", statuses: []status{{"ci/build", "failure"}}, wantReason: "status-failure"},
		{name: "error status blocks", statuses: []status{{"ci/build", "error"}}, wantReason: "status-error"},
		{name: "pending status blocks until CI completes", statuses: []status{{"ci/build", "pending"}}, wantReason: "status-pending"},
		{name: "in-progress check blocks until CI completes", statuses: []status{{"ci/build", "success"}}, checks: []check{{"build", "in_progress", ""}}, wantReason: "check-pending"},
		{name: "queued check blocks until CI completes", checks: []check{{"build", "queued", ""}}, wantReason: "check-pending"},
		{name: "check failure blocks", statuses: []status{{"ci/build", "success"}}, checks: []check{{"build", "completed", "failure"}}, wantReason: "check-failure"},
		{name: "pending meta status and check are ignored", statuses: []status{{"tide", "pending"}, {"ci/build", "success"}}, checks: []check{{"tide", "in_progress", ""}, {"build", "completed", "success"}}, wantGreen: true},
		{name: "no statuses or checks is green after mergeable gate", wantGreen: true},
		{name: "cancelled Playwright check with green build-gate is green", checks: []check{{"build-gate", "completed", "success"}, {"Playwright", "completed", "cancelled"}}, wantGreen: true},
		{name: "cancelled Mobile Browser Tests check with green build-gate is green", checks: []check{{"build-gate", "completed", "success"}, {"Mobile Browser Tests", "completed", "cancelled"}}, wantGreen: true},
		{name: "cancelled chromium shard check with green build-gate is green", checks: []check{{"build-gate", "completed", "success"}, {"Test (chromium, shard 3)", "completed", "cancelled"}}, wantGreen: true},
		{name: "in-progress chromium shard does not block", checks: []check{{"build-gate", "completed", "success"}, {"Test (chromium, shard 1)", "in_progress", ""}}, wantGreen: true},
		{name: "cancelled build-gate still blocks", checks: []check{{"build-gate", "completed", "cancelled"}}, wantReason: "check-cancelled"},
		{name: "failing build-gate still blocks alongside cancelled Playwright", checks: []check{{"build-gate", "completed", "failure"}, {"Playwright", "completed", "cancelled"}}, wantReason: "check-failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/status":
					statuses := []map[string]string{}
					combined := "success"
					for _, s := range tt.statuses {
						statuses = append(statuses, map[string]string{"context": s.context, "state": s.state})
						if s.state != "success" {
							combined = s.state
						}
					}
					json.NewEncoder(w).Encode(map[string]any{"state": combined, "total_count": len(statuses), "statuses": statuses})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/check-runs":
					runs := []map[string]string{}
					for _, c := range tt.checks {
						runs = append(runs, map[string]string{"name": c.name, "status": c.status, "conclusion": c.conclusion})
					}
					json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs})
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			// branch="" means the required-checks set is deliberately
			// unresolvable here, exercising the fail-closed
			// isMetaCheck/isIgnorableCICheck allowlist fallback path — see
			// TestCommitGreenRequiredChecksOnly for the required-set-known path.
			green, reason, err := c.commitGreen(context.Background(), "acme", "widget", "", "sha")
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
		{name: "missing queue approval", pr: sweepPR{number: 7, author: "alice", queuedBy: "bob", label: true, mergeableState: "clean"}, state: "open", labels: []map[string]string{{"name": hgithub.AutoMergeQueuedLabel}}, noReview: true, want: "no-hive-queue-approval"},
		{name: "untrusted queue approval", pr: sweepPR{number: 7, author: "alice", queuedBy: "bob", reviewAuthor: "mallory", label: true, mergeableState: "clean"}, state: "open", labels: []map[string]string{{"name": hgithub.AutoMergeQueuedLabel}}, want: autoMergeReasonUntrustedQueueApproval},
		{name: "not mergeable", pr: sweepPR{number: 7, author: "alice", queuedBy: "bob", label: true, mergeableState: "dirty"}, state: "open", labels: []map[string]string{{"name": hgithub.AutoMergeQueuedLabel}}, want: "not-mergeable"},
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
						reviewAuthor := tt.pr.reviewAuthor
						if reviewAuthor == "" {
							reviewAuthor = testHiveAppBotLogin
						}
						json.NewEncoder(w).Encode([]map[string]any{{"state": "APPROVED", "body": "Approved by @" + tt.pr.queuedBy + " for Hive auto-merge on green CI.", "commit_id": "sha7", "user": map[string]string{"login": reviewAuthor}}})
					}
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			_, reason, err := c.trySweepQueuedPR(context.Background(), "widget", "acme", "widget", 7, hgithub.AutoMergeQueuedLabel)
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
						"labels":          []map[string]string{{"name": hgithub.AutoMergeQueuedLabel}},
					})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
					json.NewEncoder(w).Encode([]map[string]any{{"state": "COMMENTED", "body": "noise"}, {"state": "APPROVED", "body": "Approved by @bob for Hive auto-merge on green CI.", "commit_id": "sha7", "user": map[string]string{"login": testHiveAppBotLogin}}})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha7/status":
					json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 1, "statuses": []map[string]string{{"context": "ci/build", "state": "success"}}})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha7/check-runs":
					json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "check_runs": []map[string]string{{"name": "build", "status": "completed", "conclusion": "success"}}})
				case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/merge":
					json.NewEncoder(w).Encode(tt.mergeResult)
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			_, reason, err := c.trySweepQueuedPR(context.Background(), "widget", "acme", "widget", 7, hgithub.AutoMergeQueuedLabel)
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
				"labels": []map[string]string{{"name": hgithub.AutoMergeQueuedLabel}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			json.NewEncoder(w).Encode([]map[string]any{{"state": "APPROVED", "body": "Approved by @bob for Hive auto-merge on green CI.", "commit_id": "sha7", "user": map[string]string{"login": testHiveAppBotLogin}}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha7/status":
			json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 1, "statuses": []map[string]string{{"context": "ci/build", "state": "success"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha7/check-runs":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "check_runs": []map[string]string{{"name": "build", "status": "completed", "conclusion": "success"}}})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/merge":
			http.Error(w, "conflict", http.StatusConflict)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	_, reason, err := c.trySweepQueuedPR(context.Background(), "widget", "acme", "widget", 7, hgithub.AutoMergeQueuedLabel)
	if err == nil || reason != "merge-failed" {
		t.Fatalf("trySweepQueuedPR reason=%q err=%v, want merge-failed error", reason, err)
	}
}

func TestLatestHiveQueueApprovalDoesNotTrustBodyWhenReviewerIsNotApp(t *testing.T) {
	var logs bytes.Buffer
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/widget/pulls/7/reviews" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode([]map[string]any{{
			"state":     "APPROVED",
			"body":      "Approved by @alice for Hive auto-merge on green CI.",
			"commit_id": "sha7",
			"user":      map[string]string{"login": "mallory"},
		}})
	}))
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	c.logger = slog.New(slog.NewTextHandler(&logs, nil))
	approval, ok, reason, err := c.latestHiveQueueApproval(context.Background(), "acme", "widget", 7)
	if err != nil {
		t.Fatalf("latestHiveQueueApproval returned error: %v", err)
	}
	if ok || approval.QueuedBy != "" || reason != autoMergeReasonUntrustedQueueApproval {
		t.Fatalf("approval=(%+v,%v,%q), want forged body ignored with untrusted reason", approval, ok, reason)
	}
	logText := logs.String()
	if !strings.Contains(logText, autoMergeWarnUntrustedQueueApproval) || !strings.Contains(logText, "mallory") || !strings.Contains(logText, "alice") {
		t.Fatalf("untrusted approval log = %q, want warning with review author and claimed user", logText)
	}
}

func TestLatestHiveQueueApprovalKeepsLatestCommitID(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/widget/pulls/7/reviews" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode([]map[string]any{
			{"state": "APPROVED", "body": "Approved by @old for Hive auto-merge on green CI.", "commit_id": "oldsha", "user": map[string]string{"login": testHiveAppBotLogin}},
			{"state": "COMMENTED", "body": "Approved by @ignored for Hive auto-merge on green CI.", "commit_id": "ignored", "user": map[string]string{"login": testHiveAppBotLogin}},
			{"state": "APPROVED", "body": "Approved by @new for Hive auto-merge on green CI.", "commit_id": "newsha", "user": map[string]string{"login": testHiveAppBotLogin}},
		})
	}))
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	approval, ok, reason, err := c.latestHiveQueueApproval(context.Background(), "acme", "widget", 7)
	if err != nil {
		t.Fatalf("latestHiveQueueApproval returned error: %v", err)
	}
	if !ok || reason != "" || approval.QueuedBy != "new" || approval.HeadSHA != "newsha" {
		t.Fatalf("approval=(%+v,%v,%q), want new/newsha", approval, ok, reason)
	}
}

func TestInvalidateQueuedAutoMergeIgnoresMissingLabelAndComments(t *testing.T) {
	var commented bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/repos/acme/widget/issues/7/labels/"+hgithub.AutoMergeQueuedLabel:
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			commented = true
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	if err := c.invalidateQueuedAutoMerge(context.Background(), "acme", "widget", 7, hgithub.AutoMergeQueuedLabel, "re-queue required"); err != nil {
		t.Fatalf("invalidateQueuedAutoMerge returned error: %v", err)
	}
	if !commented {
		t.Fatal("expected stale queue invalidation comment")
	}
}

func TestInvalidateQueuedAutoMergeReportsAPIErrors(t *testing.T) {
	tests := []struct {
		name       string
		deleteCode int
		commentOK  bool
	}{
		{name: "remove label error", deleteCode: http.StatusInternalServerError, commentOK: true},
		{name: "comment error", deleteCode: http.StatusOK, commentOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodDelete && r.URL.Path == "/repos/acme/widget/issues/7/labels/"+hgithub.AutoMergeQueuedLabel:
					w.WriteHeader(tt.deleteCode)
				case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/comments":
					if tt.commentOK {
						json.NewEncoder(w).Encode(map[string]any{"id": 1})
						return
					}
					http.Error(w, "boom", http.StatusInternalServerError)
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer api.Close()

			c := newAutoMergeSweepClient(api.URL)
			if err := c.invalidateQueuedAutoMerge(context.Background(), "acme", "widget", 7, hgithub.AutoMergeQueuedLabel, "re-queue required"); err == nil {
				t.Fatal("invalidateQueuedAutoMerge returned nil error")
			}
		})
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
			c := newAutoMergeSweepClient(api.URL)
			_, reason, err := c.commitGreen(context.Background(), "acme", "widget", "", "sha")
			if err == nil || reason != tt.wantReason {
				t.Fatalf("commitGreen reason=%q err=%v, want %q error", reason, err, tt.wantReason)
			}
		})
	}
}

// TestCommitGreenRequiredChecksOnly locks in the required-checks-ONLY gate:
// commitGreen must consult the branch's actual required status checks
// (Repositories.GetRequiredStatusChecks) and ignore any check/status whose
// context is not in that set, no matter its state — this replaces the old
// hardcoded isMetaCheck/isIgnorableCICheck allowlist that repeatedly missed
// non-required check names (#22471 "Detect untested files", #22450
// "Analyze (python)"/CodeQL).
func TestCommitGreenRequiredChecksOnly(t *testing.T) {
	tests := []struct {
		name             string
		requiredContexts []string
		statuses         []struct{ context, state string }
		checks           []struct{ name, status, conclusion string }
		wantGreen        bool
		wantReason       string
	}{
		{
			name:             "non-required cancelled check-run never blocks",
			requiredContexts: []string{"build-gate"},
			checks: []struct{ name, status, conclusion string }{
				{"build-gate", "completed", "success"},
				{"Detect untested files", "completed", "cancelled"},
			},
			wantGreen: true,
		},
		{
			name:             "non-required failing check-run (CodeQL) never blocks",
			requiredContexts: []string{"build-gate"},
			checks: []struct{ name, status, conclusion string }{
				{"build-gate", "completed", "success"},
				{"Analyze (python)", "completed", "failure"},
			},
			wantGreen: true,
		},
		{
			name:             "non-required pending check-run never blocks",
			requiredContexts: []string{"build-gate"},
			checks: []struct{ name, status, conclusion string }{
				{"build-gate", "completed", "success"},
				{"Analyze (python)", "in_progress", ""},
			},
			wantGreen: true,
		},
		{
			name:             "required check pending blocks",
			requiredContexts: []string{"build-gate"},
			checks: []struct{ name, status, conclusion string }{
				{"build-gate", "in_progress", ""},
			},
			wantReason: "check-pending",
		},
		{
			name:             "required check failure blocks",
			requiredContexts: []string{"build-gate"},
			checks: []struct{ name, status, conclusion string }{
				{"build-gate", "completed", "failure"},
			},
			wantReason: "check-failure",
		},
		{
			name:             "non-required failing status context never blocks",
			requiredContexts: []string{"build-gate"},
			statuses: []struct{ context, state string }{
				{"ci/legacy-lint", "failure"},
			},
			checks: []struct{ name, status, conclusion string }{
				{"build-gate", "completed", "success"},
			},
			wantGreen: true,
		},
		{
			name:             "required status context failure blocks",
			requiredContexts: []string{"ci/build"},
			statuses: []struct{ context, state string }{
				{"ci/build", "failure"},
			},
			wantReason: "status-failure",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/branches/main/protection/required_status_checks":
					json.NewEncoder(w).Encode(map[string]any{
						"strict":   true,
						"contexts": tt.requiredContexts,
					})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/status":
					statuses := []map[string]string{}
					combined := "success"
					for _, s := range tt.statuses {
						statuses = append(statuses, map[string]string{"context": s.context, "state": s.state})
						if s.state != "success" {
							combined = s.state
						}
					}
					json.NewEncoder(w).Encode(map[string]any{"state": combined, "total_count": len(statuses), "statuses": statuses})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/check-runs":
					runs := []map[string]string{}
					for _, c := range tt.checks {
						runs = append(runs, map[string]string{"name": c.name, "status": c.status, "conclusion": c.conclusion})
					}
					json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs})
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			green, reason, err := c.commitGreen(context.Background(), "acme", "widget", "main", "sha")
			if err != nil {
				t.Fatalf("commitGreen returned error: %v", err)
			}
			if green != tt.wantGreen || reason != tt.wantReason {
				t.Fatalf("commitGreen = (%v,%q), want (%v,%q)", green, reason, tt.wantGreen, tt.wantReason)
			}
		})
	}
}

// TestCommitGreenRequiredChecksUnavailableFallsBack locks in the fail-closed
// fallback: when the required-checks set cannot be determined, commitGreen
// must NOT treat every check as ignorable — it falls back to the old
// isMetaCheck/isIgnorableCICheck allowlist rather than merging over
// everything.
func TestCommitGreenRequiredChecksUnavailableFallsBack(t *testing.T) {
	tests := []struct {
		name           string
		protectionCode int // HTTP status GetRequiredStatusChecks returns; 0 = branch-not-protected message
		checks         []struct{ name, status, conclusion string }
		wantGreen      bool
		wantReason     string
	}{
		{
			name:           "branch protection API error falls back to allowlist and still blocks unknown failing check",
			protectionCode: http.StatusInternalServerError,
			checks: []struct{ name, status, conclusion string }{
				{"build-gate", "completed", "success"},
				{"some-custom-check", "completed", "failure"},
			},
			wantReason: "check-failure",
		},
		{
			name:           "branch protection API error still allows fallback allowlist (Playwright) to pass",
			protectionCode: http.StatusInternalServerError,
			checks: []struct{ name, status, conclusion string }{
				{"build-gate", "completed", "success"},
				{"Playwright", "completed", "cancelled"},
			},
			wantGreen: true,
		},
		{
			name:           "unprotected branch (no required checks) is a known empty set, everything ignorable",
			protectionCode: 0,
			checks: []struct{ name, status, conclusion string }{
				{"anything", "completed", "failure"},
			},
			wantGreen: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/branches/main/protection/required_status_checks":
					if tt.protectionCode == 0 {
						w.WriteHeader(http.StatusNotFound)
						json.NewEncoder(w).Encode(map[string]any{"message": "Branch not protected"})
						return
					}
					http.Error(w, "boom", tt.protectionCode)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/status":
					json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 0})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/check-runs":
					runs := []map[string]string{}
					for _, c := range tt.checks {
						runs = append(runs, map[string]string{"name": c.name, "status": c.status, "conclusion": c.conclusion})
					}
					json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs})
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			green, reason, err := c.commitGreen(context.Background(), "acme", "widget", "main", "sha")
			if err != nil {
				t.Fatalf("commitGreen returned error: %v", err)
			}
			if green != tt.wantGreen || reason != tt.wantReason {
				t.Fatalf("commitGreen = (%v,%q), want (%v,%q)", green, reason, tt.wantGreen, tt.wantReason)
			}
		})
	}
}

// TestCommitGreenConfigRequiredChecksTakesPrecedence locks in that a
// config-declared required-checks set (SetRequiredChecks, mirroring
// config.AutoMergeConfig.RequiredCheckSet) is consulted BEFORE the
// branch-protection API — the primary path now that the Hive App token
// lacks administration:read. The mock server's protection endpoint is left
// unregistered (any request to it fails the test) to prove commitGreen never
// even calls it when a config set is installed.
func TestCommitGreenConfigRequiredChecksTakesPrecedence(t *testing.T) {
	tests := []struct {
		name           string
		requiredChecks map[string]bool
		checks         []struct{ name, status, conclusion string }
		wantGreen      bool
		wantReason     string
	}{
		{
			name:           "build-gate success + non-required cancelled check is green",
			requiredChecks: map[string]bool{"build-gate": true},
			checks: []struct{ name, status, conclusion string }{
				{"build-gate", "completed", "success"},
				{"Detect untested files", "completed", "cancelled"},
			},
			wantGreen: true,
		},
		{
			name:           "build-gate failure blocks even though it's the only required check",
			requiredChecks: map[string]bool{"build-gate": true},
			checks: []struct{ name, status, conclusion string }{
				{"build-gate", "completed", "failure"},
			},
			wantReason: "check-failure",
		},
		{
			name:           "non-required CodeQL failure never blocks",
			requiredChecks: map[string]bool{"build-gate": true},
			checks: []struct{ name, status, conclusion string }{
				{"build-gate", "completed", "success"},
				{"Analyze (python)", "completed", "failure"},
			},
			wantGreen: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/branches/main/protection/required_status_checks":
					t.Fatalf("commitGreen must not call the branch-protection API when a config required-checks set is installed")
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/status":
					json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 0})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/check-runs":
					runs := []map[string]string{}
					for _, c := range tt.checks {
						runs = append(runs, map[string]string{"name": c.name, "status": c.status, "conclusion": c.conclusion})
					}
					json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs})
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			c.SetRequiredChecks(tt.requiredChecks)
			green, reason, err := c.commitGreen(context.Background(), "acme", "widget", "main", "sha")
			if err != nil {
				t.Fatalf("commitGreen returned error: %v", err)
			}
			if green != tt.wantGreen || reason != tt.wantReason {
				t.Fatalf("commitGreen = (%v,%q), want (%v,%q)", green, reason, tt.wantGreen, tt.wantReason)
			}
		})
	}
}

// TestCommitGreenNoConfigRequiredChecksFallsBackToAPIThenAllowlist confirms
// the full precedence chain when no config set is installed
// (SetRequiredChecks never called / installed with an empty map): commitGreen
// falls through to the branch-protection API exactly as before #this-change,
// and if that is also unavailable, to the isMetaCheck/isIgnorableCICheck
// allowlist.
func TestCommitGreenNoConfigRequiredChecksFallsBackToAPIThenAllowlist(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/branches/main/protection/required_status_checks":
			w.WriteHeader(http.StatusInternalServerError)
			http.Error(w, "boom", http.StatusInternalServerError)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/status":
			json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 0})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/check-runs":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "check_runs": []map[string]string{
				{"name": "build-gate", "status": "completed", "conclusion": "success"},
				{"name": "Playwright", "status": "completed", "conclusion": "cancelled"},
			}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	// Deliberately do NOT call SetRequiredChecks: config did not declare a
	// list, so this must fall through to the API (which errors here) and
	// then to the allowlist fallback (Playwright is on it, so this stays
	// green).
	green, reason, err := c.commitGreen(context.Background(), "acme", "widget", "main", "sha")
	if err != nil {
		t.Fatalf("commitGreen returned error: %v", err)
	}
	if !green || reason != "" {
		t.Fatalf("commitGreen = (%v,%q), want (true,\"\")", green, reason)
	}
}

func TestSetRequiredChecksEmptyMapIsNotDeclared(t *testing.T) {
	c := newAutoMergeSweepClient("http://unused.invalid")
	c.SetRequiredChecks(map[string]bool{})
	if set, ok := c.configRequiredChecks(); ok || set != nil {
		t.Fatalf("configRequiredChecks() after SetRequiredChecks(empty) = (%v,%v), want (nil,false)", set, ok)
	}
	c.SetRequiredChecks(map[string]bool{"build-gate": true})
	if set, ok := c.configRequiredChecks(); !ok || !set["build-gate"] {
		t.Fatalf("configRequiredChecks() after SetRequiredChecks(build-gate) = (%v,%v), want ({build-gate:true},true)", set, ok)
	}
	c.SetRequiredChecks(nil)
	if set, ok := c.configRequiredChecks(); ok || set != nil {
		t.Fatalf("configRequiredChecks() after SetRequiredChecks(nil) = (%v,%v), want (nil,false)", set, ok)
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

	c := newAutoMergeSweepClient(api.URL)
	issues, err := c.listQueuedPullRequestIssues(context.Background(), "acme", "widget", hgithub.AutoMergeQueuedLabel)
	if err != nil {
		t.Fatalf("listQueuedPullRequestIssues returned error: %v", err)
	}
	if len(issues) != 2 || issues[0].GetNumber() != 1 || issues[1].GetNumber() != 2 {
		t.Fatalf("issues = %#v, want pages 1 and 2", issues)
	}
}

// selfAuthoredPR describes one PR in the fake API used by the
// SweepSelfAuthoredAutoMerges tests below.
type selfAuthoredPR struct {
	number          int
	author          string
	draft           bool
	mergeableState  string
	statusState     string
	checkStatus     string
	checkConclusion string
	// headSHAOverride, when set, is returned by the SECOND PR fetch (the
	// merge-time re-check) instead of the first-fetch head SHA — simulates a
	// push landing between evaluation and merge.
	headSHAOverride string
}

func newSelfAuthoredAutoMergeAPI(t *testing.T, prs []selfAuthoredPR, merged *[]int) *httptest.Server {
	t.Helper()
	byNumber := make(map[int]*selfAuthoredPR, len(prs))
	fetchCount := make(map[int]int)
	for i := range prs {
		byNumber[prs[i].number] = &prs[i]
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls":
			var out []map[string]any
			for _, pr := range prs {
				out = append(out, map[string]any{
					"number": pr.number,
					"draft":  pr.draft,
					"user":   map[string]string{"login": pr.author},
				})
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/pulls/"):
			number := pathNumber(t, r.URL.Path, "/repos/acme/widget/pulls/", "")
			pr := byNumber[number]
			fetchCount[number]++
			headSHA := "sha" + strconv.Itoa(pr.number)
			if fetchCount[number] > 1 && pr.headSHAOverride != "" {
				headSHA = pr.headSHAOverride
			}
			json.NewEncoder(w).Encode(map[string]any{
				"number":          pr.number,
				"state":           "open",
				"draft":           pr.draft,
				"mergeable_state": pr.mergeableState,
				"mergeable":       pr.mergeableState == "clean",
				"user":            map[string]string{"login": pr.author},
				"head":            map[string]string{"sha": headSHA},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/commits/") && strings.HasSuffix(r.URL.Path, "/status"):
			number := shaNumber(t, r.URL.Path)
			pr := byNumber[number]
			json.NewEncoder(w).Encode(map[string]any{
				"state":       pr.statusState,
				"total_count": 1,
				"statuses":    []map[string]string{{"context": "ci/build", "state": pr.statusState}},
			})
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

func TestSweepSelfAuthoredAutoMergesMergesGreenAppPRWithoutHumanReview(t *testing.T) {
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{{
		number: 11, author: testHiveAppBotLogin, mergeableState: "clean",
		statusState: "success", checkStatus: "completed", checkConclusion: "success",
	}}, &merged)
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	var audits []AutoMergeSweepEvent
	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{
		Audit: func(event AutoMergeSweepEvent) { audits = append(audits, event) },
	})
	if err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 1 || len(merged) != 1 || merged[0] != 11 {
		t.Fatalf("merged result=%v merge calls=%v, want PR 11 merged without any human review", result.Merged, merged)
	}
	if len(audits) != 1 || audits[0].Number != 11 || audits[0].Author != testHiveAppBotLogin || audits[0].QueuedBy != "" {
		t.Fatalf("audit events = %#v, want App-authored PR 11 with no queuer", audits)
	}
}

func TestSweepSelfAuthoredAutoMergesIgnoresNonAppAuthoredPR(t *testing.T) {
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{{
		number: 11, author: "alice", mergeableState: "clean",
		statusState: "success", checkStatus: "completed", checkConclusion: "success",
	}}, &merged)
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 0 || len(merged) != 0 || result.Seen != 0 {
		t.Fatalf("result=%+v merge calls=%v, want non-App-authored PR never listed or merged", result, merged)
	}
}

func TestSweepSelfAuthoredAutoMergesSkipsHeadChangedSinceEval(t *testing.T) {
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{{
		number: 11, author: testHiveAppBotLogin, mergeableState: "clean",
		statusState: "success", checkStatus: "completed", checkConclusion: "success",
		headSHAOverride: "newsha11",
	}}, &merged)
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 0 || len(merged) != 0 {
		t.Fatalf("merged result=%v merge calls=%v, want a head move between eval and merge to block the merge", result.Merged, merged)
	}
}

func TestSweepSelfAuthoredAutoMergesSkipsWhenChecksNotGreen(t *testing.T) {
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{{
		number: 11, author: testHiveAppBotLogin, mergeableState: "clean",
		statusState: "pending", checkStatus: "completed", checkConclusion: "success",
	}}, &merged)
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 0 || len(merged) != 0 {
		t.Fatalf("merged result=%v merge calls=%v, want pending CI to block the merge", result.Merged, merged)
	}
}

func TestSweepSelfAuthoredAutoMergesSkipsDraftAndNotMergeable(t *testing.T) {
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{
		{number: 11, author: testHiveAppBotLogin, draft: true, mergeableState: "clean", statusState: "success", checkStatus: "completed", checkConclusion: "success"},
		{number: 12, author: testHiveAppBotLogin, mergeableState: "dirty", statusState: "success", checkStatus: "completed", checkConclusion: "success"},
	}, &merged)
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 0 || len(merged) != 0 {
		t.Fatalf("merged result=%v merge calls=%v, want draft and dirty PRs skipped", result.Merged, merged)
	}
}

func TestSweepSelfAuthoredAutoMergesReportsNoAppBotLogin(t *testing.T) {
	var logs bytes.Buffer
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{{
		number: 11, author: "alice", mergeableState: "clean",
	}}, &merged)
	defer api.Close()

	client := hgithub.NewClient("token", "acme", []string{"widget"}, slog.New(slog.NewTextHandler(&logs, nil)), api.URL)
	c := New(client, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 0 || len(merged) != 0 {
		t.Fatalf("result=%+v merge calls=%v, want sweep to no-op without an App bot login", result, merged)
	}
	if !strings.Contains(logs.String(), autoMergeWarnNoAppBotLogin) {
		t.Fatalf("logs = %q, want no-app-bot-login warning", logs.String())
	}
}

func TestSweepSelfAuthoredAutoMergesNilClient(t *testing.T) {
	var nilClient *Engine
	if _, err := nilClient.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != hgithub.ErrNoGitHubClient {
		t.Fatalf("nil sweep error = %v, want hgithub.ErrNoGitHubClient", err)
	}
}

// TestStartSelfAuthoredAutoMergeSweepDisabledBelowMinACMMLevel reproduces the
// ks/hive bug: an ACMM L4 hive (acmm_level: 4) must never start the
// self-authored auto-merge loop, even though the auto_merge.self_authored
// config flag defaults to ON. l4.md is explicit: "DO NOT merge pull requests
// — L4 is not an auto-merge level." The starter must no-op (no goroutine, no
// HTTP calls) and log once explaining why.
func TestStartSelfAuthoredAutoMergeSweepDisabledBelowMinACMMLevel(t *testing.T) {
	var logs bytes.Buffer
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{{
		number: 1, author: testHiveAppBotLogin, mergeableState: "clean",
		statusState: "success", checkStatus: "completed", checkConclusion: "success",
	}}, &merged)
	defer api.Close()

	client := hgithub.NewClient("token", "acme", []string{"widget"}, slog.New(slog.NewTextHandler(&logs, nil)), api.URL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	c := New(client, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l4 := 4
	c.StartSelfAuthoredAutoMergeSweep(ctx, 0, false, &l4)

	if !strings.Contains(logs.String(), "self-authored auto-merge sweep disabled") {
		t.Fatalf("logs = %q, want disabled-sweep INFO log", logs.String())
	}
	if len(merged) != 0 {
		t.Fatalf("merged = %v, want no merges at ACMM L4 (below config.SelfMergeMinACMMLevel)", merged)
	}
}

// TestStartSelfAuthoredAutoMergeSweepEnabledAtMinACMMLevel is the positive
// case: at config.SelfMergeMinACMMLevel (L6, matching console) the loop
// starts and logs its normal startup message, not the disabled message.
func TestStartSelfAuthoredAutoMergeSweepEnabledAtMinACMMLevel(t *testing.T) {
	var logs bytes.Buffer
	client := hgithub.NewClient("token", "acme", []string{"widget"}, slog.New(slog.NewTextHandler(&logs, nil)), "http://127.0.0.1:0")
	client.SetAppBotLogin(testHiveAppBotLogin)
	c := New(client, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})

	ctx, cancel := context.WithCancel(context.Background())
	l6 := config.SelfMergeMinACMMLevel
	c.StartSelfAuthoredAutoMergeSweep(ctx, 0, true, &l6)
	cancel()

	if strings.Contains(logs.String(), "self-authored auto-merge sweep disabled") {
		t.Fatalf("logs = %q, want no disabled-sweep log at min ACMM level", logs.String())
	}
	if !strings.Contains(logs.String(), "self-authored automerge sweep started") {
		t.Fatalf("logs = %q, want normal startup log at min ACMM level", logs.String())
	}
}

// TestStartSelfAuthoredAutoMergeSweepNilClient guards the nil-client no-op
// path still holds with the new acmmAllowed/acmmLevel parameters.
func TestStartSelfAuthoredAutoMergeSweepNilClient(t *testing.T) {
	var nilClient *Engine
	l6 := config.SelfMergeMinACMMLevel
	nilClient.StartSelfAuthoredAutoMergeSweep(context.Background(), 0, true, &l6)
}

// testTrustedMergers are the logins the sweep fixtures treat as holding the
// merger tier. "bob" is the queuer in every fixture that is EXPECTED to merge,
// so granting it here preserves each existing test's original intent (a
// legitimate queued merge lands) now that the sweep enforces the trusted-merger
// gate (audit F3). Anyone NOT listed is untrusted and must not merge.
var testTrustedMergers = map[string]bool{"bob": true, "carol": true}

func newAutoMergeSweepClient(apiURL string) *Engine {
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	// Audit F3: the sweep fails CLOSED without an authorizer, so every test
	// exercising the merge path must install one. Tests that assert the
	// fail-closed behaviour itself build their client without this helper.
	return New(client, Options{MergerAuthorizer: func(login string) bool {
		return testTrustedMergers[strings.ToLower(strings.TrimSpace(login))]
	}})
}

func newAutoMergeSweepAPI(t *testing.T, expectedLabel string, prs []sweepPR, merged *[]int, extras ...func(http.ResponseWriter, *http.Request)) *httptest.Server {
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
			headSHA := "sha" + strconv.Itoa(pr.number)
			if pr.headSHA != "" {
				headSHA = pr.headSHA
			}
			reviewCommitID := headSHA
			if pr.reviewCommitID != nil {
				reviewCommitID = *pr.reviewCommitID
			}
			reviewAuthor := pr.reviewAuthor
			if reviewAuthor == "" {
				reviewAuthor = testHiveAppBotLogin
			}
			json.NewEncoder(w).Encode([]map[string]any{{
				"state":     "APPROVED",
				"body":      body,
				"commit_id": reviewCommitID,
				"user":      map[string]string{"login": reviewAuthor},
			}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/pulls/"):
			number := pathNumber(t, r.URL.Path, "/repos/acme/widget/pulls/", "")
			pr := byNumber[number]
			headSHA := "sha" + strconv.Itoa(pr.number)
			if pr.headSHA != "" {
				headSHA = pr.headSHA
			}
			prLabels := []map[string]string{{"name": expectedLabel}}
			for _, extra := range pr.extraLabels {
				prLabels = append(prLabels, map[string]string{"name": extra})
			}
			json.NewEncoder(w).Encode(map[string]any{
				"number":          pr.number,
				"state":           "open",
				"draft":           pr.draft,
				"mergeable_state": pr.mergeableState,
				"mergeable":       pr.mergeableState == "clean",
				"user":            map[string]string{"login": pr.author},
				"head":            map[string]string{"sha": headSHA},
				"labels":          prLabels,
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/commits/") && strings.HasSuffix(r.URL.Path, "/status"):
			number := shaNumber(t, r.URL.Path)
			pr := byNumber[number]
			json.NewEncoder(w).Encode(map[string]any{
				"state":       pr.statusState,
				"total_count": 1,
				"statuses":    []map[string]string{{"context": "ci/build", "state": pr.statusState}},
			})
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
			for _, extra := range extras {
				extra(w, r)
				return
			}
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

// A sweep merge must land on the attribution audit trail as pr_merged — the
// activity collector counts that trail and the /fleet L6 health verdict is
// judged on merges. The sweep paths skipping this audit made merging hives
// read as "no merge in Nd" red (kubestellar/console, 2026-08-26).
func TestSweepQueuedAutoMergesRecordsPRMergedAuditTrail(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{{
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

	c := newAutoMergeSweepClient(api.URL)
	type auditRec struct{ action, detail, agent string }
	var trail []auditRec
	c.SetAttributionHooks(hgithub.AttributionHooks{
		Audit: func(action, detail, agent string) {
			trail = append(trail, auditRec{action, detail, agent})
		},
	})

	if _, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != nil {
		t.Fatalf("SweepQueuedAutoMerges returned error: %v", err)
	}
	if len(merged) != 1 {
		t.Fatalf("expected one merge, got %v", merged)
	}
	var got *auditRec
	for i := range trail {
		if trail[i].action == hgithub.AuditActionPRMerged {
			got = &trail[i]
		}
	}
	if got == nil {
		t.Fatalf("no %s entry on the attribution trail; trail=%#v", hgithub.AuditActionPRMerged, trail)
	}
	if got.agent != hgithub.AttributionAgentGovernor {
		t.Errorf("pr_merged agent = %q, want %q", got.agent, hgithub.AttributionAgentGovernor)
	}
	for _, want := range []string{"number=7", "method=squash"} {
		if !strings.Contains(got.detail, want) {
			t.Errorf("pr_merged detail missing %q: %q", want, got.detail)
		}
	}
}

// The self-authored sweep path has the same audit obligation as the queued
// one: without a pr_merged trail entry its merges are invisible to the
// activity collector and the merge-judged /fleet verdict.
func TestSweepSelfAuthoredAutoMergesRecordsPRMergedAuditTrail(t *testing.T) {
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{{
		number: 11, author: testHiveAppBotLogin, mergeableState: "clean",
		statusState: "success", checkStatus: "completed", checkConclusion: "success",
	}}, &merged)
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	type auditRec struct{ action, detail, agent string }
	var trail []auditRec
	c.SetAttributionHooks(hgithub.AttributionHooks{
		Audit: func(action, detail, agent string) {
			trail = append(trail, auditRec{action, detail, agent})
		},
	})

	if _, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	if len(merged) != 1 {
		t.Fatalf("expected one merge, got %v", merged)
	}
	var got *auditRec
	for i := range trail {
		if trail[i].action == hgithub.AuditActionPRMerged {
			got = &trail[i]
		}
	}
	if got == nil {
		t.Fatalf("no %s entry on the attribution trail; trail=%#v", hgithub.AuditActionPRMerged, trail)
	}
	if got.agent != hgithub.AttributionAgentGovernor {
		t.Errorf("pr_merged agent = %q, want %q", got.agent, hgithub.AttributionAgentGovernor)
	}
	for _, want := range []string{"number=11", "method=squash"} {
		if !strings.Contains(got.detail, want) {
			t.Errorf("pr_merged detail missing %q: %q", want, got.detail)
		}
	}
}
