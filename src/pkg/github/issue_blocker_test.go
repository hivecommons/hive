package github

import (
	"strings"
	"testing"
	"time"

	itemblocker "github.com/hivecommons/hive/pkg/blocker"
	"github.com/hivecommons/hive/pkg/stageengine"
)

// hivecommons/hive#15099: a needs-human report that names a capability the
// App lacks is recorded as a structured blocker where the hive reads
// comments — the issue_comment webhook, the task-list sweep's remainder
// comment — and the stage facts carry it so the engine parks the issue on
// the blocker instead of a bare needs-human.

const workflowsReport = "Verified: the remaining retry wrappers live in `.github/workflows/v2-ci.yml`, which the App token cannot push (no `workflows` permission); needs a human."

func swapSharedLedger(t *testing.T) *itemblocker.Ledger {
	t.Helper()
	l := itemblocker.NewLedger("")
	prev := itemblocker.SetShared(l)
	t.Cleanup(func() { itemblocker.SetShared(prev) })
	return l
}

func TestWebhookIssueCommentRecordsBlocker(t *testing.T) {
	l := swapSharedLedger(t)
	c, _ := setupWebhookCacheTest(t)
	payload := `{"action":"created","issue":{"number":14976},"comment":{"body":` + jsonString(workflowsReport) + `,"created_at":"2026-10-20T19:33:00Z"},"repository":{"full_name":"org/repo"}}`
	res, err := c.HandleWebhookEvent("issue_comment", []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if res.BlockerKind != string(itemblocker.CapabilityWorkflowsPermission) {
		t.Fatalf("blocker kind = %q (res %+v)", res.BlockerKind, res)
	}
	b, ok := l.Blocking(res.ConfiguredRepo, 14976)
	if !ok || b.Kind != itemblocker.CapabilityWorkflowsPermission || !b.Since.Equal(time.Date(2026, 10, 20, 19, 33, 0, 0, time.UTC)) {
		t.Fatalf("ledger = %+v, %v", b, ok)
	}
	// The marker alone is enough, prose or not; a PR comment is ignored; a
	// plain comment records nothing.
	res, err = c.HandleWebhookEvent("issue_comment", []byte(`{"action":"created","issue":{"number":15},"comment":{"body":"parked. <!-- hive:blocker kind=human-only:cla -->"},"repository":{"full_name":"org/repo"}}`))
	if err != nil || res.BlockerKind != string(itemblocker.HumanOnlyCLA) {
		t.Fatalf("marker comment: %+v %v", res, err)
	}
	res, err = c.HandleWebhookEvent("issue_comment", []byte(`{"action":"created","issue":{"number":16,"pull_request":{"url":"x"}},"comment":{"body":`+jsonString(workflowsReport)+`},"repository":{"full_name":"org/repo"}}`))
	if err != nil || res.BlockerKind != "" {
		t.Fatalf("PR comment: %+v %v", res, err)
	}
	res, err = c.HandleWebhookEvent("issue_comment", []byte(`{"action":"created","issue":{"number":17},"comment":{"body":"thanks, looking"},"repository":{"full_name":"org/repo"}}`))
	if err != nil || res.BlockerKind != "" {
		t.Fatalf("plain comment: %+v %v", res, err)
	}
	if _, ok := l.Get(res.ConfiguredRepo, 17); ok {
		t.Fatal("plain comment created a ledger entry")
	}
}

// hivecommons/hive#15273: a blocker recorded from a live issue_comment
// delivery (not seeded via Observe) is forgotten when GitHub later closes
// the issue, so capabilities.blocked_items stops counting it while other
// items keep their count. Negative cases (labeled, PR close, reopen) live in
// TestWebhookClosedIssueClearsBlocker.
func TestWebhookCommentSeededBlockerClearsOnClose(t *testing.T) {
	l := swapSharedLedger(t)
	c, _ := setupWebhookCacheTest(t)
	at := time.Date(2026, 10, 20, 19, 33, 0, 0, time.UTC)
	res, err := c.HandleWebhookEvent("issue_comment", []byte(`{"action":"created","issue":{"number":21},"comment":{"body":`+jsonString(workflowsReport)+`,"created_at":"2026-10-20T19:33:00Z"},"repository":{"full_name":"org/repo"}}`))
	if err != nil || res.BlockerKind == "" {
		t.Fatalf("seed: %+v %v", res, err)
	}
	repo := res.ConfiguredRepo
	l.Observe(repo, 22, workflowsReport, at)
	if _, ok := l.Blocking(repo, 21); !ok {
		t.Fatal("issue 21 not blocking before close")
	}
	if _, err := c.HandleWebhookEvent("issues", []byte(`{"action":"closed","issue":{"number":21,"state":"closed"},"repository":{"full_name":"org/repo"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Get(repo, 21); ok {
		t.Fatal("closed issue kept its ledger row")
	}
	if got := l.Capabilities(); got.BlockedItems != 1 {
		t.Fatalf("blocked_items = %+v, want only issue 22", got)
	}
}

func jsonString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

func TestRefsRemainderCommentCarriesBlockerMarker(t *testing.T) {
	merged := []mergedPRRef{{Number: 14990, Title: "retry wrappers", URL: "https://github.com/o/r/pull/14990", Remainder: workflowsReport, NeedsHuman: true}}
	body := renderRefsRemainderComment(merged, true, nil)
	if !strings.Contains(body, itemblocker.Marker(itemblocker.CapabilityWorkflowsPermission)) {
		t.Fatalf("marker missing:\n%s", body)
	}
	if k, ok := itemblocker.ParseMarker(body); !ok || k != itemblocker.CapabilityWorkflowsPermission {
		t.Fatalf("ParseMarker = %v, %v", k, ok)
	}
	// Without needs-human the remainder is a note, not a park: no marker.
	plain := renderRefsRemainderComment([]mergedPRRef{{Number: 1, Remainder: workflowsReport}}, false, nil)
	if strings.Contains(plain, "hive:blocker") {
		t.Fatalf("marker on a non-needs-human remainder:\n%s", plain)
	}
	// A needs-human remainder with no capability in it stays bare.
	bare := renderRefsRemainderComment([]mergedPRRef{{Number: 2, Remainder: "pick the retry budget", NeedsHuman: true}}, false, nil)
	if strings.Contains(bare, "hive:blocker") {
		t.Fatalf("marker on a plain needs-human remainder:\n%s", bare)
	}
}

func TestIssueStageFactsCarryBlocker(t *testing.T) {
	l := swapSharedLedger(t)
	at := time.Date(2026, 10, 20, 19, 33, 0, 0, time.UTC)
	l.Observe("hivecommons/hive", 14976, workflowsReport, at)
	in := IssueStageInput{Repo: "hivecommons/hive", Number: 14976, Bucket: workBucketFiltered, Reason: issueNeedsHumanLabel,
		Labels: []string{issueNeedsHumanLabel, HumanAckLabel, "stage/045-needs-decision"}}
	f := issueFactsFrom(in, nil)
	if f.Issue == nil || f.Issue.Blocker != string(itemblocker.CapabilityWorkflowsPermission) || f.Issue.BlockerShort != "App lacks workflows permission" {
		t.Fatalf("facts = %+v", f.Issue)
	}
	if f.Issue.ApprovedDirectionLabel != HumanAckLabel || !f.Park.NeedsHuman {
		t.Fatalf("facts = %+v park=%+v", f.Issue, f.Park)
	}
	d := stageengine.DeriveIssue(f)
	if d.Reason != "blocked: App lacks workflows permission" || len(d.Clear) != 1 || d.Clear[0] != HumanAckLabel {
		t.Fatalf("derived = %+v", d)
	}
}

// Ordinary GitHub closes must clear the same ledger used by capabilitiesStatus.
func TestWebhookClosedIssueClearsBlocker(t *testing.T) {
	for _, tt := range []struct {
		name, event, action, state, repo string
		pr, wantBlocked                  bool
	}{
		{"closed", "issues", "closed", "closed", "org/repo", false, false},
		{"closed comment", "issue_comment", "created", "closed", "org/repo", false, false},
		{"edited closed comment", "issue_comment", "edited", "closed", "org/repo", false, false},
		{"closed label event", "issues", "labeled", "closed", "ORG/REPO", false, false},
		{"open issue", "issues", "labeled", "open", "org/repo", false, true},
		{"reopened issue", "issues", "reopened", "open", "org/repo", false, true},
		{"unknown repository", "issues", "closed", "closed", "other/repo", false, true},
		{"PR comment", "issue_comment", "created", "closed", "org/repo", true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := swapSharedLedger(t)
			c, _ := setupWebhookCacheTest(t)
			l.Observe("org/repo", 7, workflowsReport, time.Now())
			l.Observe("org/repo", 8, workflowsReport, time.Now())
			l.Observe("repo", 7, workflowsReport, time.Now())
			pr := ""
			if tt.pr {
				pr = `,"pull_request":{"url":"x"}`
			}
			payload := `{"action":` + jsonString(tt.action) + `,"issue":{"number":7,"state":` + jsonString(tt.state) + pr + `},"comment":{"body":` + jsonString(workflowsReport) + `},"repository":{"full_name":` + jsonString(tt.repo) + `}}`
			res, err := c.HandleWebhookEvent(tt.event, []byte(payload))
			if err != nil {
				t.Fatal(err)
			}
			if _, blocked := l.Blocking("org/repo", 7); blocked != tt.wantBlocked {
				t.Fatalf("blocked = %v, want %v", blocked, tt.wantBlocked)
			}
			if _, blocked := l.Blocking("repo", 7); blocked != tt.wantBlocked {
				t.Fatalf("short-name blocked = %v, want %v", blocked, tt.wantBlocked)
			}
			wantCount := 1
			if tt.wantBlocked {
				wantCount += 2
			}
			if caps := l.Capabilities(); caps.BlockedItems != wantCount || caps.ByPermission["workflows"] != wantCount {
				t.Fatalf("capabilities = %+v, want %d blocked items", caps, wantCount)
			}
			if !tt.wantBlocked && res.BlockerKind != "" {
				t.Fatalf("closed issue recorded blocker %q", res.BlockerKind)
			}
		})
	}
}
