package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
)

// These tests drive writeIntentVerdicts (cmd/hive/main.go) down the agent-PR
// SUCCESS path — the branch the existing nil-guard and fetch-failure tests
// never reach: a real (canned) GitHub API answers the evidence fetch, the
// verdict is evaluated with alignment, the misalignment advisory is minted,
// and the verdicts JSON is atomically written to intentVerdictsPath.
//
// They reuse the canned GitHub API from intent_evidence_fetch_test.go
// (evidenceServer serves acme/widgets#7) and redirect intentVerdictsPath —
// a package var precisely so tests can point it at a temp file — away from
// /var/run/hive-metrics.

// redirectVerdictsFile points intentVerdictsPath at a temp file for the
// duration of one test and returns that path.
func redirectVerdictsFile(t *testing.T) string {
	t.Helper()
	old := intentVerdictsPath
	path := filepath.Join(t.TempDir(), "intent-verdicts.json")
	intentVerdictsPath = path
	t.Cleanup(func() { intentVerdictsPath = old })
	return path
}

// intentTestConfig returns a config whose AI author matches the PR author
// used below, so writeIntentVerdicts takes the agent-PR evidence branch.
func intentTestConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Org: "acme", AIAuthor: "hive-bot"},
		Intent:  config.IntentConfig{Enforce: true},
	}
}

func agentPRActionable(title string) *github.ActionableResult {
	return &github.ActionableResult{
		PRs: github.PRResult{
			Items: []github.PullRequest{
				{Repo: "widgets", Number: 7, Title: title, Author: "hive-bot"},
			},
		},
	}
}

// An agent PR whose evidence fetch succeeds, whose body links an issue the
// canned API also serves, and whose diff stays inside the referenced scope
// must come out Tier-1 AUTHORIZED with a non-nil aligned Alignment verdict —
// and the whole run must be recorded in the verdicts file: enforced flag,
// repo, number, author, and the verdict itself.
func TestWriteIntentVerdictsAgentPRSuccessAuthorizedAndPersisted(t *testing.T) {
	verdictsFile := redirectVerdictsFile(t)

	srv := &evidenceServer{
		prBody:       "Fixes the widgets crash.\n\nCloses #42",
		changedFiles: 1,
		filePages: [][]map[string]any{{
			changedFileJSON("pkg/widgets/widgets.go", "modified", 5, 1),
		}},
		issues: map[string]map[string]any{
			"acme/widgets#42": {
				"number": 42,
				"title":  "Crash in pkg/widgets",
				"body":   "The widgets package crashes on empty input.",
			},
		},
	}

	verdicts := writeIntentVerdicts(context.Background(), intentTestConfig(),
		srv.client(t), agentPRActionable("Fix widgets crash"), nil, restoreTestLogger())

	v, ok := verdicts["acme/widgets/7"]
	if !ok {
		t.Fatalf("verdicts = %#v, want key acme/widgets/7", verdicts)
	}
	if !v.AgentPR {
		t.Error("agent-authored PR lost its AgentPR flag on the success path")
	}
	if !v.Authorized {
		t.Errorf("PR with linked issue #42 denied: tier=%d reason=%q", v.Tier, v.Reason)
	}
	if !v.Evidence.LinkedIssue {
		t.Error("Closes #42 in the body must surface as Evidence.LinkedIssue")
	}
	if v.Alignment == nil {
		t.Fatal("success path must attach an Alignment verdict, got nil")
	}
	if v.Alignment.Misaligned() {
		t.Errorf("in-scope diff flagged misaligned: %+v", v.Alignment)
	}
	if !v.MergeAllowed() {
		t.Error("authorized+aligned verdict must be MergeAllowed")
	}

	// The verdict must also have been persisted for the dashboard/merge gate.
	data, err := os.ReadFile(verdictsFile)
	if err != nil {
		t.Fatalf("verdicts file not written: %v", err)
	}
	var payload struct {
		GeneratedAt string `json:"generated_at"`
		Enforced    bool   `json:"enforced"`
		Verdicts    []struct {
			Repo     string         `json:"repo"`
			Number   int            `json:"number"`
			Author   string         `json:"author"`
			Enforced bool           `json:"enforced"`
			Verdict  intent.Verdict `json:"verdict"`
			Classify string         `json:"classification_reason"`
		} `json:"verdicts"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("verdicts file is not valid JSON: %v\n%s", err, data)
	}
	if payload.GeneratedAt == "" {
		t.Error("verdicts payload missing generated_at")
	}
	if !payload.Enforced {
		t.Error("payload.enforced must mirror cfg.Intent.Enforce=true")
	}
	if len(payload.Verdicts) != 1 {
		t.Fatalf("verdicts file has %d records, want 1", len(payload.Verdicts))
	}
	rec := payload.Verdicts[0]
	if rec.Repo != "acme/widgets" || rec.Number != 7 || rec.Author != "hive-bot" {
		t.Errorf("record = %+v, want acme/widgets#7 by hive-bot", rec)
	}
	if !rec.Enforced || !rec.Verdict.Authorized {
		t.Errorf("record verdict = %+v, want enforced+authorized", rec)
	}
	if rec.Classify == "" {
		t.Error("record missing classification_reason")
	}
}

// A Tier-1 agent PR whose body links NO issue must be denied on the success
// path too — evidence fetched fine, the gate itself says no.
func TestWriteIntentVerdictsAgentPRWithoutLinkedIssueDenied(t *testing.T) {
	redirectVerdictsFile(t)

	srv := &evidenceServer{
		prBody: "No issue reference here.",
		filePages: [][]map[string]any{{
			changedFileJSON("pkg/widgets/widgets.go", "modified", 5, 1),
		}},
	}

	verdicts := writeIntentVerdicts(context.Background(), intentTestConfig(),
		srv.client(t), agentPRActionable("Refactor internals"), nil, restoreTestLogger())

	v, ok := verdicts["acme/widgets/7"]
	if !ok {
		t.Fatalf("verdicts = %#v, want key acme/widgets/7", verdicts)
	}
	if v.Authorized {
		t.Errorf("Tier-1 agent PR without a linked issue authorized: %+v", v)
	}
	if v.MergeAllowed() {
		t.Error("denied verdict must not be MergeAllowed")
	}
}

// alignmentModelServer serves /v1/chat/completions with a fixed model verdict
// (or a fixed HTTP failure), standing in for the LiteLLM reviewer endpoint.
func alignmentModelServer(t *testing.T, status, rationale string, httpStatus int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if httpStatus != http.StatusOK {
			http.Error(w, `{"message":"reviewer down"}`, httpStatus)
			return
		}
		content := fmt.Sprintf(`{"status":%q,"confidence":0.9,"rationale":%q}`, status, rationale)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": content}},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// configWithAlignmentModel wires cfg.Intent.AlignmentModel plus a reviewer
// endpoint so writeIntentVerdicts constructs the AlignmentReviewer.
func configWithAlignmentModel(endpoint string) *config.Config {
	cfg := intentTestConfig()
	cfg.Intent.AlignmentModel = "test-judge"
	cfg.Governor.Trajectory.Endpoint = endpoint
	return cfg
}

// When the configured alignment model says MISALIGNED, the merged verdict
// must flip to misaligned even though the deterministic checks were clean,
// MergeAllowed must go false, and exactly one alignment-drift advisory bead
// must be minted into the bead store.
func TestWriteIntentVerdictsModelMisalignmentDeniesAndRecordsAdvisory(t *testing.T) {
	// The reviewer endpoint must come from the config under test, not from a
	// stray environment override.
	t.Setenv(config.LiteLLMEndpointEnv, "")
	redirectVerdictsFile(t)

	model := alignmentModelServer(t, "misaligned", "diff serves no stated intent", http.StatusOK)

	srv := &evidenceServer{
		prBody: "Cleanup.\n\nCloses #42",
		filePages: [][]map[string]any{{
			changedFileJSON("pkg/widgets/widgets.go", "modified", 5, 1),
		}},
		issues: map[string]map[string]any{
			"acme/widgets#42": {
				"number": 42,
				"title":  "Crash in pkg/widgets",
				"body":   "The widgets package crashes on empty input.",
			},
		},
	}

	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("beads.NewStore: %v", err)
	}
	stores := map[string]*beads.Store{"intent": store}

	verdicts := writeIntentVerdicts(context.Background(), configWithAlignmentModel(model.URL),
		srv.client(t), agentPRActionable("Fix widgets crash"), stores, restoreTestLogger())

	v, ok := verdicts["acme/widgets/7"]
	if !ok {
		t.Fatalf("verdicts = %#v, want key acme/widgets/7", verdicts)
	}
	if v.Alignment == nil || v.Alignment.Model == nil {
		t.Fatalf("model reviewer ran but verdict carries no model alignment: %+v", v.Alignment)
	}
	if !v.Alignment.Misaligned() {
		t.Errorf("model said misaligned but merged verdict = %+v", v.Alignment)
	}
	if v.MergeAllowed() {
		t.Error("misaligned verdict must not be MergeAllowed even when authorized")
	}

	wantTitle := "Intent alignment drift in acme/widgets#7"
	var found int
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Type == beads.TypeAdvisory && b.Title == wantTitle {
			found++
		}
	}
	if found != 1 {
		t.Errorf("advisory beads titled %q = %d, want exactly 1", wantTitle, found)
	}
}

// A reviewer endpoint that answers 500 must FAIL OPEN: the deterministic
// alignment verdict stands, the model error is recorded on the verdict for
// the operator, and the PR is not denied for the reviewer's outage.
func TestWriteIntentVerdictsModelReviewerErrorFailsOpen(t *testing.T) {
	t.Setenv(config.LiteLLMEndpointEnv, "")
	redirectVerdictsFile(t)

	model := alignmentModelServer(t, "", "", http.StatusInternalServerError)

	srv := &evidenceServer{
		prBody: "Fixes the widgets crash.\n\nCloses #42",
		filePages: [][]map[string]any{{
			changedFileJSON("pkg/widgets/widgets.go", "modified", 5, 1),
		}},
		issues: map[string]map[string]any{
			"acme/widgets#42": {
				"number": 42,
				"title":  "Crash in pkg/widgets",
				"body":   "The widgets package crashes on empty input.",
			},
		},
	}

	verdicts := writeIntentVerdicts(context.Background(), configWithAlignmentModel(model.URL),
		srv.client(t), agentPRActionable("Fix widgets crash"), nil, restoreTestLogger())

	v, ok := verdicts["acme/widgets/7"]
	if !ok {
		t.Fatalf("verdicts = %#v, want key acme/widgets/7", verdicts)
	}
	if v.Alignment == nil {
		t.Fatal("alignment verdict missing after reviewer failure")
	}
	if v.Alignment.ModelError == "" {
		t.Error("reviewer 500 must be recorded as Alignment.ModelError")
	}
	if v.Alignment.Model != nil {
		t.Errorf("failed review must not attach a model verdict, got %+v", v.Alignment.Model)
	}
	if v.Alignment.Misaligned() {
		t.Error("reviewer outage must fail open, not deny the PR")
	}
	if !v.Authorized || !v.MergeAllowed() {
		t.Errorf("verdict after fail-open = %+v, want authorized and merge-allowed", v)
	}
}

// AlignmentModel set but NO reviewer endpoint resolvable: the reviewer is
// disabled (warn-and-continue), and the run still completes with the
// deterministic alignment verdict attached — configuring the model name
// without an endpoint must never make the whole intent gate fall over.
func TestWriteIntentVerdictsAlignmentModelWithoutEndpointDisablesReviewer(t *testing.T) {
	t.Setenv(config.LiteLLMEndpointEnv, "")
	redirectVerdictsFile(t)

	cfg := intentTestConfig()
	cfg.Intent.AlignmentModel = "test-judge" // endpoint left empty everywhere

	srv := &evidenceServer{
		prBody: "Fixes the widgets crash.\n\nCloses #42",
		filePages: [][]map[string]any{{
			changedFileJSON("pkg/widgets/widgets.go", "modified", 5, 1),
		}},
		issues: map[string]map[string]any{
			"acme/widgets#42": {
				"number": 42,
				"title":  "Crash in pkg/widgets",
				"body":   "The widgets package crashes on empty input.",
			},
		},
	}

	verdicts := writeIntentVerdicts(context.Background(), cfg,
		srv.client(t), agentPRActionable("Fix widgets crash"), nil, restoreTestLogger())

	v, ok := verdicts["acme/widgets/7"]
	if !ok {
		t.Fatalf("verdicts = %#v, want key acme/widgets/7", verdicts)
	}
	if v.Alignment == nil {
		t.Fatal("deterministic alignment must still run without a reviewer")
	}
	if v.Alignment.Model != nil {
		t.Errorf("no reviewer was constructible, yet a model verdict appeared: %+v", v.Alignment.Model)
	}
	if !v.Authorized {
		t.Errorf("verdict = %+v, want authorized despite missing reviewer endpoint", v)
	}
}

// A linked issue the API cannot serve (404) must not fail the verdict: the
// issue-text fetch error is logged and alignment proceeds on the PR text
// alone. This is the issueErr warn branch inside the success path.
func TestWriteIntentVerdictsIssueTextFetchFailureIsNonFatal(t *testing.T) {
	redirectVerdictsFile(t)

	srv := &evidenceServer{
		// Body links #42 for evidence, but the canned API serves no issues,
		// so fetchIntentIssueTexts fails while the verdict still evaluates.
		prBody: "Fixes the widgets crash in pkg/widgets.\n\nCloses #42",
		filePages: [][]map[string]any{{
			changedFileJSON("pkg/widgets/widgets.go", "modified", 5, 1),
		}},
	}

	verdicts := writeIntentVerdicts(context.Background(), intentTestConfig(),
		srv.client(t), agentPRActionable("Fix widgets crash"), nil, restoreTestLogger())

	v, ok := verdicts["acme/widgets/7"]
	if !ok {
		t.Fatalf("verdicts = %#v, want key acme/widgets/7", verdicts)
	}
	if !v.Authorized {
		t.Errorf("issue-text 404 must not deny the verdict: %+v", v)
	}
	if v.Alignment == nil {
		t.Error("alignment must still be evaluated when issue texts are unavailable")
	}
	if strings.TrimSpace(v.Reason) == "" {
		t.Error("verdict must carry a reason")
	}
}
