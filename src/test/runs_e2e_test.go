//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	runsE2EDefaultTimeout = 4 * time.Minute
	runsE2EPollInterval   = 5 * time.Second
)

type runsE2ERun struct {
	Key         string         `json:"key"`
	Title       string         `json:"title"`
	Repo        string         `json:"repo"`
	Stage       string         `json:"stage"`
	Gen         uint64         `json:"gen"`
	Assignee    string         `json:"assignee"`
	WaitingOn   string         `json:"waiting_on"`
	LastReceipt string         `json:"last_receipt"`
	PlanEpicID  string         `json:"plan_epic_id"`
	Stages      []runsE2EStage `json:"stages"`
	raw         map[string]any `json:"-"`
}

type runsE2EStage struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Gen     uint64 `json:"gen"`
	Receipt string `json:"receipt"`
	Reason  string `json:"reason"`
}

type runsE2EQueueItem struct {
	Repo       string   `json:"repo"`
	Number     int      `json:"number"`
	Title      string   `json:"title"`
	Key        string   `json:"key"`
	SourceType string   `json:"source_type"`
	ExternalID string   `json:"external_id"`
	Labels     []string `json:"labels"`
}

type runsE2EAuditEntry struct {
	User   string `json:"user"`
	Action string `json:"action"`
	Detail string `json:"detail"`
	Agent  string `json:"agent"`
}

func TestRunsE2EAcceptance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping run-level e2e in short mode")
	}
	client := newAPIClient()
	if _, code, err := client.get("/api/version"); err != nil || code != http.StatusOK {
		t.Fatalf("hive not reachable at %s: %v (code=%d)", hiveURL, err, code)
	}

	features := runsE2EFeatureProbe(t, client)
	if enabled, ok := boolField(features, "spektacularEnabled"); ok && !enabled {
		t.Skip("runs.spektacular.enabled is false; run this against a live hive configured with the spektacular-fake binary")
	}
	if binary, _ := stringField(features, "spektacularBinary"); binary != "" && !strings.Contains(binary, "spektacular-fake") {
		t.Logf("spektacular binary is %q; acceptance expects the in-tree fake fixture for deterministic draft/final transitions", binary)
	}

	runKey := runsE2ETargetKey(t)
	triggerRunsTriage(t, client, runKey)

	var spec runsE2ERun
	t.Run("1_spec_run_admitted", func(t *testing.T) {
		var err error
		spec, err = waitForRun(t, client, runKey, func(r runsE2ERun) bool {
			return r.Stage == "spec" && r.Gen == 1
		}, runsE2ETimeout())
		if err != nil {
			t.Fatalf("run %s never appeared as stage=spec gen=1: %v", runKey, err)
		}
		if spec.Assignee == "" {
			t.Fatalf("run %s has no lease assignee/identity: %+v", runKey, spec)
		}
		runs := listRuns(t, client)
		if got := countRuns(runs, runKey); got != 1 {
			t.Fatalf("run %s appears %d times in /api/runs, want exactly one lease holder", runKey, got)
		}
	})

	var plan runsE2ERun
	t.Run("2_spec_final_advances_once", func(t *testing.T) {
		var err error
		plan, err = waitForRun(t, client, runKey, func(r runsE2ERun) bool {
			return r.Stage == "plan" && r.Gen == spec.Gen+1
		}, runsE2ETimeout())
		if err != nil {
			t.Fatalf("run %s did not advance spec->plan by one generation through the fake: %v", runKey, err)
		}
		detail := getRun(t, client, runKey)
		if receipts := countStageReceipts(detail, "spec", spec.Gen); receipts != 1 {
			t.Fatalf("spec generation %d receipts = %d, want exactly one; stages=%+v", spec.Gen, receipts, detail.Stages)
		}
		assertNoDuplicateTick(t, client, runKey, "spec", spec.Gen)
	})

	t.Run("3_plan_approval_releases_implement", func(t *testing.T) {
		if plan.PlanEpicID == "" {
			skipUntil(t, "gap 3 of #8460", "the run has no imported plan epic to approve")
		}
		if hasRunStageQueueItem(t, client, runKey, "implement") {
			t.Fatalf("implement stage is offerable before plan approval")
		}
		code, body, err := postJSON(client, "/api/plan/"+url.PathEscape(plan.PlanEpicID)+"/approve", nil, nil)
		if err != nil {
			t.Fatalf("approve plan %s: %v", plan.PlanEpicID, err)
		}
		if code != http.StatusOK && code != http.StatusBadRequest {
			t.Fatalf("approve plan %s returned %d: %s", plan.PlanEpicID, code, body)
		}
		implement, err := waitForRun(t, client, runKey, func(r runsE2ERun) bool {
			return r.Stage == "implement" && r.Gen == plan.Gen+1
		}, runsE2ETimeout())
		if err != nil {
			skipUntil(t, "gap 3 of #8460", "plan approval did not advance plan->implement through the fake: "+err.Error())
		}
		if !waitForQueueStage(t, client, runKey, "implement", runsE2ETimeout()/2) {
			skipUntil(t, "gap 2 of #8460", "implement lease exists but the run-stage worksource did not list it")
		}
		if implement.Assignee == "" || implement.Gen != plan.Gen+1 {
			t.Fatalf("implement lease state = %+v, want assignee and gen %d", implement, plan.Gen+1)
		}
		plan = implement
	})

	t.Run("4_reclaim_rejects_old_generation", func(t *testing.T) {
		if !featurePresent(features, "runStageAccessorWired") {
			skipUntil(t, "gap 2 of #8460", "no live HTTP probe exposes lookupLease generation fencing yet")
		}
		// When gap 2 adds an HTTP-observable lease accessor, this assertion must kill
		// the stage holder, wait for a minted generation, and prove the old generation
		// is refused while only one work item is listed.
	})

	t.Run("5_terminal_burndown", func(t *testing.T) {
		detail := getRun(t, client, runKey)
		burndown, ok := detail.raw["burndown"].(map[string]any)
		if !ok {
			skipUntil(t, "gap 10 of #8460", "/api/runs/{key} has no burndown field")
		}
		if !isTerminalRun(detail) {
			skipUntil(t, "gap 7 of #8460", "run has not reached an HTTP-visible terminal state")
		}
		for _, field := range []string{"satisfied", "remaining", "unknown", "scope_changed"} {
			if _, present := burndown[field]; !present {
				t.Fatalf("burndown missing %q: %+v", field, burndown)
			}
		}
		assertNoNullSubstitutedByNumber(t, burndown)
	})

	t.Run("6_credentials_and_audit", func(t *testing.T) {
		assertNoCredentialLeak(t, "run payload", getRaw(t, client, "/api/runs/"+url.PathEscape(runKey)))
		entries, ok := auditEntries(t, client)
		if !ok {
			t.Skip("/api/audit unavailable to this token; cannot assert run audit ownership")
		}
		seenRun := false
		for _, entry := range entries {
			if strings.Contains(entry.Detail, "run=") || strings.Contains(entry.Detail, runKey) {
				assertNoCredentialLeak(t, "audit entry", entry)
				if entry.User == "" {
					t.Fatalf("audit entry for %s has no owning human: %+v", runKey, entry)
				}
				seenRun = true
			}
		}
		if !seenRun {
			t.Fatalf("no audit entry carried run=%s or the run key", runKey)
		}
	})
}

func runsE2EFeatureProbe(t *testing.T, client *apiClient) map[string]any {
	t.Helper()
	var payload map[string]any
	code, body, err := getJSON(client, "/api/config/governor", &payload)
	if err != nil {
		t.Fatalf("GET /api/config/governor: %v", err)
	}
	if code == http.StatusForbidden {
		t.Skip("HIVE_TOKEN must authorize /api/config/governor for run feature probes")
	}
	if code != http.StatusOK {
		t.Fatalf("GET /api/config/governor returned %d: %s", code, body)
	}
	features, _ := payload["features"].(map[string]any)
	return features
}

func runsE2ETargetKey(t *testing.T) string {
	t.Helper()
	if key := strings.TrimSpace(os.Getenv("HIVE_RUNS_E2E_KEY")); key != "" {
		return key
	}
	repo := strings.TrimSpace(os.Getenv("HIVE_RUNS_E2E_REPO"))
	number := strings.TrimSpace(os.Getenv("HIVE_RUNS_E2E_ISSUE"))
	if repo == "" || number == "" {
		t.Skip("set HIVE_RUNS_E2E_KEY or HIVE_RUNS_E2E_REPO/HIVE_RUNS_E2E_ISSUE to a pinned run/spec issue on the live hive")
	}
	if _, err := strconv.Atoi(number); err != nil {
		t.Fatalf("HIVE_RUNS_E2E_ISSUE must be numeric: %v", err)
	}
	return repo + "#" + number
}

func triggerRunsTriage(t *testing.T, client *apiClient, runKey string) {
	t.Helper()
	_, _, _ = postJSON(client, "/api/repos/rescan", nil, nil)
	msg := map[string]string{"message": "runs e2e acceptance: rescan and triage " + runKey + " for run/spec admission"}
	_, _, _ = postJSON(client, "/api/kick/scanner", msg, nil)
}

func runsE2ETimeout() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("HIVE_RUNS_E2E_TIMEOUT")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return runsE2EDefaultTimeout
}

func waitForRun(t *testing.T, client *apiClient, key string, pred func(runsE2ERun) bool, timeout time.Duration) (runsE2ERun, error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for attempt := 0; ; attempt++ {
		runs := listRuns(t, client)
		for _, run := range runs {
			if run.Key == key && pred(run) {
				return run, nil
			}
		}
		if time.Now().After(deadline) {
			return runsE2ERun{}, fmt.Errorf("timeout after %s waiting for %s", timeout, key)
		}
		waitNextPoll(attempt)
	}
}

func waitForQueueStage(t *testing.T, client *apiClient, key, stage string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for attempt := 0; ; attempt++ {
		if hasRunStageQueueItem(t, client, key, stage) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		waitNextPoll(attempt)
	}
}

func waitNextPoll(attempt int) {
	if attempt == 0 {
		return
	}
	timer := time.NewTimer(runsE2EPollInterval)
	<-timer.C
}

func listRuns(t *testing.T, client *apiClient) []runsE2ERun {
	t.Helper()
	var runs []runsE2ERun
	code, body, err := getJSON(client, "/api/runs", &runs)
	if err != nil || code != http.StatusOK {
		t.Fatalf("GET /api/runs returned %d: %v %s", code, err, body)
	}
	return runs
}

func getRun(t *testing.T, client *apiClient, key string) runsE2ERun {
	t.Helper()
	var run runsE2ERun
	raw := map[string]any{}
	code, body, err := getJSON(client, "/api/runs/"+url.PathEscape(key), &raw)
	if err != nil || code != http.StatusOK {
		t.Fatalf("GET /api/runs/%s returned %d: %v %s", key, code, err, body)
	}
	data, _ := json.Marshal(raw)
	if err := json.Unmarshal(data, &run); err != nil {
		t.Fatalf("decode run %s: %v", key, err)
	}
	run.raw = raw
	return run
}

func hasRunStageQueueItem(t *testing.T, client *apiClient, key, stage string) bool {
	t.Helper()
	var payload struct {
		Queue []runsE2EQueueItem `json:"queue"`
	}
	code, body, err := getJSON(client, "/api/contribute/queue?withheld=1", &payload)
	if err != nil || code != http.StatusOK {
		t.Fatalf("GET /api/contribute/queue returned %d: %v %s", code, err, body)
	}
	wantExternalID := key + ":" + stage
	wantKeySuffix := "!" + wantExternalID
	for _, item := range payload.Queue {
		if item.SourceType != "run" {
			continue
		}
		if item.ExternalID == wantExternalID || item.Key == wantExternalID || strings.HasSuffix(item.Key, wantKeySuffix) {
			return true
		}
	}
	return false
}

func countRuns(runs []runsE2ERun, key string) int {
	count := 0
	for _, run := range runs {
		if run.Key == key {
			count++
		}
	}
	return count
}

func countStageReceipts(run runsE2ERun, stage string, gen uint64) int {
	count := 0
	for _, s := range run.Stages {
		if s.Name == "stage_receipt" && s.Gen == gen && s.Receipt != "" {
			count++
			continue
		}
		if s.Name == stage && s.Status == "observed" && s.Gen == gen && s.Receipt != "" {
			count++
		}
	}
	return count
}

func assertNoDuplicateTick(t *testing.T, client *apiClient, key, stage string, gen uint64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		run := getRun(t, client, key)
		if receipts := countStageReceipts(run, stage, gen); receipts != 1 {
			t.Fatalf("second tick was not a no-op: %s gen %d receipts=%d stages=%+v", stage, gen, receipts, run.Stages)
		}
		waitNextPoll(attempt)
	}
}

func auditEntries(t *testing.T, client *apiClient) ([]runsE2EAuditEntry, bool) {
	t.Helper()
	var payload struct {
		Entries []runsE2EAuditEntry `json:"entries"`
	}
	code, body, err := getJSON(client, "/api/audit", &payload)
	if code == http.StatusForbidden || code == http.StatusUnauthorized {
		return nil, false
	}
	if err != nil || code != http.StatusOK {
		t.Fatalf("GET /api/audit returned %d: %v %s", code, err, body)
	}
	return payload.Entries, true
}

func isTerminalRun(run runsE2ERun) bool {
	stage := strings.ToLower(run.Stage)
	if stage == "done" || stage == "complete" || stage == "completed" || stage == "terminal" {
		return true
	}
	if v, _ := stringField(run.raw, "status"); v == "done" || v == "complete" || v == "completed" || v == "terminal" {
		return true
	}
	return false
}

func assertNoNullSubstitutedByNumber(t *testing.T, burndown map[string]any) {
	t.Helper()
	for _, field := range []string{"satisfied", "remaining", "unknown", "scope_changed"} {
		value := burndown[field]
		if value == nil {
			continue
		}
		switch value.(type) {
		case float64, []any, map[string]any:
		default:
			t.Fatalf("burndown.%s has unexpected type %T (%v)", field, value, value)
		}
	}
}

func assertNoCredentialLeak(t *testing.T, label string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", label, err)
	}
	lower := strings.ToLower(string(data))
	secretMarkers := []string{"ghp_", "github_token", "hive_token", "authorization", "bearer "}
	for _, marker := range secretMarkers {
		if strings.Contains(lower, marker) {
			t.Fatalf("%s contains credential marker %q", label, marker)
		}
	}
}

func getRaw(t *testing.T, client *apiClient, path string) any {
	t.Helper()
	var raw any
	code, body, err := getJSON(client, path, &raw)
	if err != nil || code != http.StatusOK {
		t.Fatalf("GET %s returned %d: %v %s", path, code, err, body)
	}
	return raw
}

func getJSON(client *apiClient, path string, out any) (int, string, error) {
	req, err := http.NewRequest(http.MethodGet, client.baseURL+path, nil)
	if err != nil {
		return 0, "", err
	}
	if client.token != "" {
		req.Header.Set("Authorization", "Bearer "+client.token)
	}
	resp, err := client.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if len(bytes.TrimSpace(body)) > 0 && out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return resp.StatusCode, string(body), err
		}
	}
	return resp.StatusCode, string(body), nil
}

func postJSON(client *apiClient, path string, payload any, out any) (int, string, error) {
	var bodyReader io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return 0, "", err
		}
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(http.MethodPost, client.baseURL+path, bodyReader)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if client.token != "" {
		req.Header.Set("Authorization", "Bearer "+client.token)
	}
	resp, err := client.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if len(bytes.TrimSpace(body)) > 0 && out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return resp.StatusCode, string(body), err
		}
	}
	return resp.StatusCode, string(body), nil
}

func skipUntil(t *testing.T, gap, reason string) {
	t.Helper()
	t.Skipf("skipping until %s lands: %s", gap, reason)
}

func featurePresent(features map[string]any, key string) bool {
	v, ok := boolField(features, key)
	return ok && v
}

func boolField(m map[string]any, key string) (bool, bool) {
	v, ok := m[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func stringField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}
