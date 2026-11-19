package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func governorPRModelListServer(t *testing.T, prs []ghpkg.PullRequest) *Server {
	t.Helper()
	s := covApiServer(t)
	dir := t.TempDir()
	s.SetContributorsDir(dir)
	if s.contributeHub != nil {
		s.contributeHub.taskRunLogFile = filepath.Join(dir, taskRunLogFileName)
	}
	s.deps.Scheduler = metricsSchedulerStub{actionable: &ghpkg.ActionableResult{PRs: ghpkg.PRResult{Attributed: prs}}}
	return s
}

func getGovernorPRModels(t *testing.T, s *Server, query string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleGovernorPRModels(rec, httptest.NewRequest(http.MethodGet, "/api/governor/pr-models?"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ?%s status = %d, want 200: %s", query, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func TestHandleGovernorPRModelsModelFilterListsPRs(t *testing.T) {
	now := time.Now().UTC()
	prs := []ghpkg.PullRequest{
		// claude-opus-5.5 under three spellings: merged, merged-after-rework, open, closed.
		{Repo: "org/a", Number: 1, Title: "first pass", URL: "https://github.com/org/a/pull/1", HiveAttributed: true, HiveModel: "claude-opus-5.5", HiveBackend: "claude", HiveAgent: "scanner", CreatedAt: now.Add(-1 * time.Hour), MergedAt: now, Rework: ghpkg.PRReworkStats{FirstPass: true}},
		{Repo: "org/a", Number: 2, Title: "reworked", URL: "https://github.com/org/a/pull/2", HiveAttributed: true, HiveModel: "anthropic/claude-opus-5-5", HiveBackend: "pi", HiveAgent: "fixer", CreatedAt: now.Add(-3 * time.Hour), MergedAt: now, Rework: ghpkg.PRReworkStats{ReviewRounds: 2, FixAttempts: 1, FixerModels: []string{"gpt-6.1-sol"}}},
		{Repo: "org/b", Number: 3, HiveAttributed: true, HiveModel: "Claude-Opus-5-5", HiveBackend: "claude", CreatedAt: now.Add(-2 * time.Hour), State: "open"},
		{Repo: "org/b", Number: 4, HiveAttributed: true, HiveModel: "claude-opus-5.5", HiveBackend: "claude", HiveAgent: "unknown", CreatedAt: now.Add(-4 * time.Hour), State: "closed", ClosedAt: now, Rework: ghpkg.PRReworkStats{ReviewRounds: 1, FixAttempts: 2}},
		// Another model, an out-of-window PR and an unattributed PR: never listed.
		{Repo: "org/a", Number: 5, HiveAttributed: true, HiveModel: "gpt-6.1-sol", HiveBackend: "codex", CreatedAt: now.Add(-1 * time.Hour), MergedAt: now, Rework: ghpkg.PRReworkStats{ReviewRounds: 9}},
		{Repo: "org/a", Number: 6, HiveAttributed: true, HiveModel: "claude-opus-5.5", HiveBackend: "claude", CreatedAt: now.AddDate(0, 0, -20), MergedAt: now},
		{Repo: "org/a", Number: 7, HiveAttributed: false, HiveModel: "claude-opus-5.5", CreatedAt: now},
	}
	s := governorPRModelListServer(t, prs)

	type listResp struct {
		Buckets      []governorPRModelBucket  `json:"buckets"`
		Total        int                      `json:"total"`
		PRs          *[]governorPRModelPRItem `json:"prs"`
		PRsTruncated bool                     `json:"prs_truncated"`
	}
	decode := func(body []byte) listResp {
		t.Helper()
		var out listResp
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode: %v: %s", err, body)
		}
		return out
	}

	canonical := decode(getGovernorPRModels(t, s, "window=7d&model=claude-opus-5.5"))
	raw := decode(getGovernorPRModels(t, s, "window=7d&model=anthropic%2Fclaude-opus-5-5"))
	if canonical.PRs == nil || raw.PRs == nil {
		t.Fatalf("prs missing with a model filter: canonical=%v raw=%v", canonical.PRs, raw.PRs)
	}
	canonJSON, _ := json.Marshal(canonical.PRs)
	rawJSON, _ := json.Marshal(raw.PRs)
	if string(canonJSON) != string(rawJSON) {
		t.Fatalf("raw spelling lists different PRs:\ncanonical %s\nraw       %s", canonJSON, rawJSON)
	}

	list := *canonical.PRs
	var gotOrder []int
	outcomes := map[string]int{}
	for _, pr := range list {
		gotOrder = append(gotOrder, pr.Number)
		outcomes[pr.Outcome]++
		if pr.Model != "claude-opus-5.5" {
			t.Fatalf("PR #%d model = %q, want the canonical id", pr.Number, pr.Model)
		}
	}
	// #4 and #2 both score 3: newer #2 first. Then #1 and #3 score 0: newer #1 first.
	if want := []int{2, 4, 1, 3}; fmt.Sprint(gotOrder) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", gotOrder, want)
	}
	var bucket *governorPRModelBucket
	for i := range canonical.Buckets {
		if canonical.Buckets[i].Model == "claude-opus-5.5" {
			bucket = &canonical.Buckets[i]
		}
	}
	if bucket == nil {
		t.Fatalf("no claude-opus-5.5 bucket: %+v", canonical.Buckets)
	}
	if len(list) != bucket.PRs || outcomes["merged"] != bucket.Merged || outcomes["closed_unmerged"] != bucket.ClosedUnmerged || outcomes["open"] != bucket.Open {
		t.Fatalf("list %d (outcomes %v) does not match bucket prs=%d merged=%d closed=%d open=%d", len(list), outcomes, bucket.PRs, bucket.Merged, bucket.ClosedUnmerged, bucket.Open)
	}
	reworked := list[0]
	if reworked.Repo != "org/a" || reworked.URL != "https://github.com/org/a/pull/2" || reworked.Title != "reworked" || reworked.Backend != "pi" || reworked.Agent != "fixer" ||
		reworked.ReviewRounds != 2 || reworked.FixAttempts != 1 || len(reworked.FixerModels) != 1 || reworked.FirstPass || reworked.MergedAt == "" || reworked.CreatedAt == "" {
		t.Fatalf("reworked PR item = %+v", reworked)
	}
	if first := list[2]; first.Number != 1 || !first.FirstPass || first.Outcome != "merged" {
		t.Fatalf("first-pass PR item = %+v", first)
	}
	if closed := list[1]; closed.Agent != "" || closed.MergedAt != "" || closed.Outcome != "closed_unmerged" {
		t.Fatalf("closed PR item = %+v, want no agent, no merged_at", closed)
	}
	if canonical.PRsTruncated {
		t.Fatal("prs_truncated set for 4 PRs")
	}

	// The 30d window picks up #6 too.
	if wide := decode(getGovernorPRModels(t, s, "window=30d&model=claude-opus-5.5")); wide.PRs == nil || len(*wide.PRs) != 5 {
		t.Fatalf("30d list = %v, want 5 PRs", wide.PRs)
	}

	// Unknown model: an empty list, not an error and not a missing field.
	unknownBody := getGovernorPRModels(t, s, "window=7d&model=no-such-model")
	var unknownRaw map[string]json.RawMessage
	if err := json.Unmarshal(unknownBody, &unknownRaw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(unknownRaw["prs"]) != "[]" {
		t.Fatalf("unknown model prs = %s, want []", unknownRaw["prs"])
	}

	// Without a model (or with a blank one) the response carries no prs fields
	// and is byte-identical to the unfiltered response.
	plain := getGovernorPRModels(t, s, "window=7d")
	blank := getGovernorPRModels(t, s, "window=7d&model=%20")
	var plainRaw map[string]json.RawMessage
	if err := json.Unmarshal(plain, &plainRaw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"prs", "prs_truncated"} {
		if _, ok := plainRaw[k]; ok {
			t.Fatalf("unfiltered response has %q: %s", k, plain)
		}
	}
	if string(plain) != string(blank) {
		t.Fatalf("blank model changed the response:\nplain %s\nblank %s", plain, blank)
	}
	// The filtered response is the usual body plus prs: the shared fields match.
	var filteredRaw map[string]json.RawMessage
	if err := json.Unmarshal(getGovernorPRModels(t, s, "window=7d&model=claude-opus-5.5"), &filteredRaw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"window", "total", "buckets", "unknown", "most_reworked", "min_merged_prs"} {
		if string(plainRaw[k]) != string(filteredRaw[k]) {
			t.Fatalf("field %q differs with the filter:\nplain    %s\nfiltered %s", k, plainRaw[k], filteredRaw[k])
		}
	}
}

func TestGovernorPRModelPRListCapsAt200(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	var prs []ghpkg.PullRequest
	for i := 0; i < governorPRModelPRsCap+1; i++ {
		prs = append(prs, ghpkg.PullRequest{
			Repo: "org/a", Number: i + 1, HiveAttributed: true, HiveModel: "gpt-6.1-terra", HiveBackend: "codex",
			CreatedAt: now.Add(-time.Duration(i+1) * time.Minute), MergedAt: now,
			Rework: ghpkg.PRReworkStats{FixAttempts: i % 7},
		})
	}
	list, truncated := governorPRModelPRList(prs, "7d", "gpt-6.1-terra", now)
	if !truncated || len(list) != governorPRModelPRsCap {
		t.Fatalf("len=%d truncated=%v, want %d and true", len(list), truncated, governorPRModelPRsCap)
	}
	for i := 1; i < len(list); i++ {
		a := list[i-1].ReviewRounds + list[i-1].FixAttempts
		b := list[i].ReviewRounds + list[i].FixAttempts
		if a < b || (a == b && list[i-1].CreatedAt < list[i].CreatedAt) {
			t.Fatalf("list not sorted at %d: %+v then %+v", i, list[i-1], list[i])
		}
	}
	full, truncated := governorPRModelPRList(prs[:governorPRModelPRsCap], "7d", "gpt-6.1-terra", now)
	if truncated || len(full) != governorPRModelPRsCap {
		t.Fatalf("at the cap: len=%d truncated=%v, want %d and false", len(full), truncated, governorPRModelPRsCap)
	}
	resp := withGovernorPRModelPRs(aggregateGovernorPRModels(prs, "7d", now), prs, "gpt-6.1-terra", now)
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["prs_truncated"]) != "true" {
		t.Fatalf("prs_truncated = %s, want true", raw["prs_truncated"])
	}
}
