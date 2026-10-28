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
