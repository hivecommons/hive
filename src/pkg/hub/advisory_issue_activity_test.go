package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAdvisoryIssueActivityBuckets(t *testing.T) {
	t.Setenv(advisoryIssueAgingAfterEnv, "")
	t.Setenv(advisoryIssueStaleAfterEnv, "")
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	stamp := func(age time.Duration) string { return now.Add(-age).Format(time.RFC3339) }
	cases := []struct {
		name string
		age  time.Duration
		want string
	}{
		{"inside fresh", advisoryIssueAgingAfter - time.Second, advisoryIssueBucketFresh},
		{"at aging boundary still fresh", advisoryIssueAgingAfter, advisoryIssueBucketFresh},
		{"past aging", advisoryIssueAgingAfter + time.Second, advisoryIssueBucketAging},
		{"at stale boundary still aging", advisoryIssueStaleAfter, advisoryIssueBucketAging},
		{"past stale", advisoryIssueStaleAfter + time.Second, advisoryIssueBucketStale},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := advisoryIssueActivityFor(RegistryEntry{AdvisoryLastPostedAt: stamp(tc.age)}, now)
			if got.Bucket != tc.want {
				t.Fatalf("bucket = %q, want %q", got.Bucket, tc.want)
			}
			if got.LastActivityAt == "" {
				t.Fatal("expected lastActivityAt")
			}
		})
	}
}

func TestAdvisoryIssueActivityUsesDigestTimestampOnly(t *testing.T) {
	t.Setenv(advisoryIssueAgingAfterEnv, "")
	t.Setenv(advisoryIssueStaleAfterEnv, "")
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	digest := now.Add(-30 * time.Minute)
	got := advisoryIssueActivityFor(RegistryEntry{
		AdvisoryLastPostedAt: digest.Format(time.RFC3339),
		IssueHistory: []SparkPoint{
			{T: now.Add(-8 * time.Hour).Unix(), V: 1},
			{T: now.Add(-2 * time.Hour).Unix(), V: 2},
			{T: now.Add(-time.Hour).Unix(), V: 2},
		},
	}, now)
	if got.Bucket != advisoryIssueBucketFresh {
		t.Fatalf("bucket = %q, want fresh", got.Bucket)
	}
	if got.LastActivityAt != digest.Format(time.RFC3339) {
		t.Fatalf("lastActivityAt = %q, want digest timestamp %q", got.LastActivityAt, digest.Format(time.RFC3339))
	}
}

func TestAdvisoryIssueActivityUnknownWithNoSignal(t *testing.T) {
	t.Setenv(advisoryIssueAgingAfterEnv, "")
	t.Setenv(advisoryIssueStaleAfterEnv, "")
	got := advisoryIssueActivityFor(RegistryEntry{}, time.Now())
	if got.Bucket != advisoryIssueBucketUnknown || got.LastActivityAt != "" {
		t.Fatalf("activity = %+v, want unknown with no timestamp", got)
	}
}

func TestAdvisoryIssueActivityErrorIsStale(t *testing.T) {
	got := advisoryIssueActivityFor(RegistryEntry{AdvisoryError: "post failed"}, time.Now())
	if got.Bucket != advisoryIssueBucketStale {
		t.Fatalf("bucket = %q, want stale for advisory post error", got.Bucket)
	}
}

func TestAdvisoryIssueActivityThresholdEnvOverride(t *testing.T) {
	t.Setenv(advisoryIssueAgingAfterEnv, "2h")
	t.Setenv(advisoryIssueStaleAfterEnv, "4h")
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	got := advisoryIssueActivityFor(RegistryEntry{AdvisoryLastPostedAt: now.Add(-3 * time.Hour).Format(time.RFC3339)}, now)
	if got.Bucket != advisoryIssueBucketAging {
		t.Fatalf("bucket = %q, want aging with env thresholds", got.Bucket)
	}
}

func TestAdvisoryIssueActivityIssue5916ContradictionUsesSingleFreshness(t *testing.T) {
	t.Setenv(advisoryIssueAgingAfterEnv, "")
	t.Setenv(advisoryIssueStaleAfterEnv, "")
	now := time.Date(2026, 9, 4, 3, 11, 53, 0, time.UTC)
	digest := time.Date(2026, 9, 3, 11, 41, 31, 0, time.UTC)
	e := RegistryEntry{
		AdvisoryLastPostedAt: digest.Format(time.RFC3339),
		GitHubAppState:       "ok",
		IssueHistory: []SparkPoint{
			{T: now.Add(-15 * time.Minute).Unix(), V: 1},
			{T: now.Add(-10 * time.Minute).Unix(), V: 2},
		},
	}

	got := advisoryIssueActivityFor(e, now)
	if got.Bucket != advisoryIssueBucketStale {
		t.Fatalf("freshness bucket = %q, want stale from advisory digest timestamp", got.Bucket)
	}
	if got.LastActivityAt != digest.Format(time.RFC3339) {
		t.Fatalf("lastActivityAt = %q, want digest timestamp %q", got.LastActivityAt, digest.Format(time.RFC3339))
	}
	stale, reason := advisoryStale(e, now)
	if !stale {
		t.Fatalf("advisoryStale = false, want true from same freshness struct")
	}
	if !strings.Contains(reason, digest.Format(time.RFC3339)) {
		t.Fatalf("reason = %q, want digest timestamp %q", reason, digest.Format(time.RFC3339))
	}
}

func TestHandleMyHivesIncludesAdvisoryIssueActivity(t *testing.T) {
	t.Setenv(advisoryIssueAgingAfterEnv, "")
	t.Setenv(advisoryIssueStaleAfterEnv, "")
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	now := time.Now().UTC()
	saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Hives: map[string]string{"h1": "owner", "hosted-empty": "owner"}})
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", Org: "acme", Status: "running"})
	saveSaaSHive(&SaaSHive{ID: "hosted-empty", Owner: "alice", Org: "available-slot", Status: statusAvailable})

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	s.registry.Hives = []RegistryEntry{{
		ID:                   "h1",
		Owner:                "alice",
		Org:                  "acme",
		Online:               true,
		AdvisoryLastPostedAt: now.Add(-advisoryIssueStaleAfter - time.Hour).Format(time.RFC3339),
		BudgetCurrentSpend:   int64ptr(87),
		BudgetLimit:          int64ptr(100),
	}}

	rec := httptest.NewRecorder()
	req := reqWithUser(http.MethodGet, "/api/saas/my-hives", "", "alice")
	s.handleMyHives(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("my-hives status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Hives []struct {
			ID                    string                `json:"id"`
			AdvisoryIssueActivity AdvisoryIssueActivity `json:"advisoryIssueActivity"`
			BudgetHealth          BudgetHealth          `json:"budgetHealth"`
			GitHubAppHealth       GitHubAppHealth       `json:"githubAppHealth"`
		} `json:"hives"`
		Summary map[string]int `json:"hives_summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal my-hives: %v", err)
	}
	byID := map[string]AdvisoryIssueActivity{}
	budgetByID := map[string]BudgetHealth{}
	ghAppByID := map[string]GitHubAppHealth{}
	for _, h := range resp.Hives {
		byID[h.ID] = h.AdvisoryIssueActivity
		budgetByID[h.ID] = h.BudgetHealth
		ghAppByID[h.ID] = h.GitHubAppHealth
	}
	if byID["h1"].Bucket != advisoryIssueBucketStale || byID["h1"].LastActivityAt == "" {
		t.Fatalf("h1 activity = %+v, want stale with timestamp", byID["h1"])
	}
	if byID["hosted-empty"].Bucket != advisoryIssueBucketUnknown || byID["hosted-empty"].LastActivityAt != "" {
		t.Fatalf("placeholder activity = %+v, want unknown/n/a", byID["hosted-empty"])
	}
	if resp.Summary["advisory_issue_stale"] != 1 || resp.Summary["advisory_issue_unknown"] != 1 {
		t.Fatalf("summary = %#v, want stale=1 unknown=1", resp.Summary)
	}
	if budgetByID["h1"].Bucket != budgetBucketWarning || budgetByID["h1"].UsedTokens != 87 || budgetByID["h1"].LimitTokens != 100 {
		t.Fatalf("h1 budget = %+v, want warning with numbers", budgetByID["h1"])
	}
	if budgetByID["hosted-empty"].Bucket != budgetBucketUnknown {
		t.Fatalf("placeholder budget = %+v, want unknown/n/a", budgetByID["hosted-empty"])
	}
	if resp.Summary["budget_warning"] != 1 || resp.Summary["budget_unknown"] != 1 {
		t.Fatalf("summary = %#v, want budget_warning=1 budget_unknown=1", resp.Summary)
	}
	if ghAppByID["h1"].Bucket != ghAppBucketUnknown || ghAppByID["hosted-empty"].Bucket != ghAppBucketUnknown {
		t.Fatalf("github app health = %#v, want unknown/n/a for rows without token signal", ghAppByID)
	}
	if resp.Summary["github_app_unknown"] != 2 {
		t.Fatalf("summary = %#v, want github_app_unknown=2", resp.Summary)
	}
}
