package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/blocker"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15099: an issue parked because the GitHub App cannot
// do the work (no `workflows` permission) is a blocker, not a question.
// "Let hive decide" must answer no-effect with the real options instead of
// un-parking it for the scanner to re-park (hivecommons/hive#14976).

const scannerWorkflowsReport = "Verified: the Dockerfile wrappers are gone. The remaining retry wrappers live in `.github/workflows/go-security-analysis.yml` and `.github/workflows/v2-ci.yml`, which the App token cannot push (no `workflows` permission); needs a human."

// swapBlockerLedger gives the test its own in-memory ledger.
func swapBlockerLedger(t *testing.T) *blocker.Ledger {
	t.Helper()
	l := blocker.NewLedger("")
	prev := blockerLedger
	blockerLedger = func() *blocker.Ledger { return l }
	t.Cleanup(func() { blockerLedger = prev })
	return l
}

func TestDecisionForBlockerWorkflowsPermission(t *testing.T) {
	l := swapBlockerLedger(t)
	at := time.Date(2026, 10, 20, 19, 33, 0, 0, time.UTC)
	if _, ok := l.Observe("hivecommons/hive", 14976, scannerWorkflowsReport, at); !ok {
		t.Fatal("scanner report not recorded as a blocker")
	}
	since := at.Add(-2 * time.Hour)
	it := StageTrackerItem{Repo: "hivecommons/hive", Number: 14976, Kind: "issue", Stage: stagelabels.IssueNeedsDecision.Number(), StageSince: &since, Modifiers: []string{"needs-human"}}
	it.Blocker = stageTrackerBlockerFor(it.Repo, it.Number)
	if it.Blocker == nil || it.Blocker.Kind != string(blocker.CapabilityWorkflowsPermission) || !it.Blocker.Capability || it.Blocker.Permission != "workflows" {
		t.Fatalf("row blocker = %+v", it.Blocker)
	}
	if it.Blocker.Short != "App lacks workflows permission" {
		t.Fatalf("pill word = %q", it.Blocker.Short)
	}
	bctx := blockerOptionsContext{AppSettingsURL: "https://github.com/apps/hive/installations/new", PatchWhy: decisionWhyPatchNoIdentity}
	d := stageTrackerDecisionNeededWith(it, nil, "", at, bctx)
	if d == nil {
		t.Fatal("nil decision")
	}
	if !strings.HasPrefix(d.Question, "Hive can't do this one: the GitHub App has no `workflows` permission, so it cannot push changes to .github/workflows/*.") || !strings.Contains(d.Question, "(a) grant the App `workflows: write`") || !strings.Contains(d.Question, "(d) keep waiting") {
		t.Fatalf("question = %q", d.Question)
	}
	if !strings.HasPrefix(d.Blocker, "Blocked · App lacks workflows permission") || !strings.Contains(d.Blocker, "cannot push (no `workflows` permission)") {
		t.Fatalf("blocker line = %q", d.Blocker)
	}
	if d.BlockerReason != "blocker:capability:workflows-permission" {
		t.Fatalf("blocker reason = %q", d.BlockerReason)
	}
	settings := decisionOptionByAction(t, d, blocker.ActionAppSettings)
	if !settings.Enabled || settings.URL != bctx.AppSettingsURL || !strings.Contains(settings.Label, "`workflows: write`") {
		t.Fatalf("settings option = %+v", settings)
	}
	if o := decisionOptionByAction(t, d, blocker.ActionRecheck); !o.Enabled {
		t.Fatalf("recheck option = %+v", o)
	}
	if o := decisionOptionByAction(t, d, blocker.ActionPatch); o.Enabled || o.ReasonIfDisabled != decisionWhyPatchNoIdentity {
		t.Fatalf("patch option must be disabled with its reason: %+v", o)
	}
	if o := decisionOptionByAction(t, d, blocker.ActionClosePartial); !o.Enabled {
		t.Fatalf("close-partial option = %+v", o)
	}
	if o := decisionOptionByAction(t, d, blocker.ActionWait); !o.Enabled {
		t.Fatalf("wait option = %+v", o)
	}
	// "Let hive decide" is offered disabled with words, never greyed silently.
	if o := decisionOptionByAction(t, d, decisionActionDelegate); o.Enabled || o.ReasonIfDisabled != decisionWhyBlockerCapability {
		t.Fatalf("delegate option = %+v", o)
	}
	// No App settings link known → the option says so.
	d2 := stageTrackerDecisionNeededWith(it, nil, "", at, blockerOptionsContext{})
	if o := decisionOptionByAction(t, d2, blocker.ActionAppSettings); o.Enabled || o.ReasonIfDisabled != decisionWhyNoAppSettings {
		t.Fatalf("settings option without URL = %+v", o)
	}
	// Held: close-partial waits for the hold to lift; the hold option leads.
	it.Hold = &StageTrackerHold{By: "owner"}
	it.Modifiers = append(it.Modifiers, modHold)
	d3 := stageTrackerDecisionNeededWith(it, nil, "", at, bctx)
	if o := decisionOptionByAction(t, d3, blocker.ActionClosePartial); o.Enabled {
		t.Fatalf("close-partial while held = %+v", o)
	}
}

func TestDecisionForBlockerLoopGuardDisablesDelegate(t *testing.T) {
	l := swapBlockerLedger(t)
	at := time.Date(2026, 10, 20, 18, 11, 0, 0, time.UTC)
	reason := "needs-human: flaky retry wrappers; a maintainer should pick the retry budget"
	l.Observe("hivecommons/hive", 14990, reason, at)
	l.NoteDelegatedUnpark("hivecommons/hive", 14990, at.Add(6*time.Minute))
	if b := stageTrackerBlockerFor("hivecommons/hive", 14990); b != nil {
		t.Fatalf("a plain reason answered once is not a blocker yet: %+v", b)
	}
	// The scanner re-applies the same reason after the un-park: parked on
	// the loop guard, delegate disabled with the reason text.
	l.Observe("hivecommons/hive", 14990, reason, at.Add(26*time.Minute))
	b := stageTrackerBlockerFor("hivecommons/hive", 14990)
	if b == nil || b.Kind != string(blocker.HumanOnlyReasked) || !b.Reapplied || b.Capability {
		t.Fatalf("loop guard blocker = %+v", b)
	}
	since := at
	it := StageTrackerItem{Repo: "hivecommons/hive", Number: 14990, Kind: "issue", Stage: stagelabels.IssueNeedsDecision.Number(), StageSince: &since, Modifiers: []string{"needs-human"}, Blocker: b}
	d := stageTrackerDecisionNeededWith(it, nil, "", at, blockerOptionsContext{})
	if o := decisionOptionByAction(t, d, decisionActionDelegate); o.Enabled || o.ReasonIfDisabled != decisionWhyBlockerReasked {
		t.Fatalf("delegate option = %+v", o)
	}
	if _, err := optionByActionErr(d.Options, blocker.ActionRecheck); err == nil {
		t.Fatalf("a human-only blocker offers no permission re-check: %+v", d.Options)
	}
	// A different reason after the un-park is a new question, not a loop.
	l2 := swapBlockerLedger(t)
	l2.Observe("o/r", 1, reason, at)
	l2.NoteDelegatedUnpark("o/r", 1, at.Add(time.Minute))
	l2.Observe("o/r", 1, "needs-human: the fix needs a design call on the cache key", at.Add(2*time.Minute))
	if b := stageTrackerBlockerFor("o/r", 1); b != nil {
		t.Fatalf("new reason tripped the loop guard: %+v", b)
	}
}

func optionByActionErr(opts []StageTrackerDecisionOption, action string) (StageTrackerDecisionOption, error) {
	for _, o := range opts {
		if o.Action == action {
			return o, nil
		}
	}
	return StageTrackerDecisionOption{}, errNoOption
}

var errNoOption = errorString("no such option")

type errorString string

func (e errorString) Error() string { return string(e) }

func TestDelegatedDecisionOnCapabilityBlockerIsNoEffect(t *testing.T) {
	swapBlockerLedger(t)
	mock := &repoItemDecisionMock{issueLabels: []string{"bug", "needs-human", "stage/045-needs-decision"}, priorComments: []string{scannerWorkflowsReport}}
	srv := newDelegatedServer(t, mock)

	w := postDelegated(t, srv, 7, "writer", `{"type":"issue","reason":"requires_human"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Status != delegatedJobStatusDone || job.Decision == nil || job.Decision.Verdict != delegatedecision.VerdictRefused || job.Decision.Rule != "blocker:capability:workflows-permission" {
		t.Fatalf("job = %+v", job)
	}
	res := job.Result
	if res == nil || res.Action != rowActionNoEffect || res.Why != "blocker:capability:workflows-permission" {
		t.Fatalf("result = %+v", res)
	}
	if !strings.HasPrefix(res.Outcome, "Hive can't do this one: the GitHub App has no `workflows` permission") || !strings.Contains(res.Next, "(a) grant the App `workflows: write`") {
		t.Fatalf("outcome=%q next=%q", res.Outcome, res.Next)
	}
	labels := optionLabels(res.Options)
	for _, want := range []string{"Grant the App `workflows: write`", "Re-check permissions", "Mark partially-addressed and close", "Keep waiting"} {
		if !strings.Contains(strings.Join(labels, "|"), want) {
			t.Fatalf("options %v missing %q", labels, want)
		}
	}
	// Nothing is written: no decision comment, no approved-direction, no
	// un-park reply, no label removed.
	if len(mock.comments) != 0 || len(mock.added) != 0 || len(mock.removed) != 0 || len(mock.edits) != 0 {
		t.Fatalf("wrote to GitHub: comments=%v added=%v removed=%v edits=%v", mock.comments, mock.added, mock.removed, mock.edits)
	}
	if !strings.Contains(strings.Join(decisionAuditActions(srv), ","), auditRepoItemDecisionDelegated) {
		t.Fatalf("audit missing: %v", decisionAuditActions(srv))
	}
	// Glossary: the why resolves to plain words, never the bare slug.
	if e, ok := delegatedecision.Explain(res.Why); !ok || e.Slug != delegatedecision.GlossaryBlocker {
		t.Fatalf("Explain(%q) = %+v, %v", res.Why, e, ok)
	}
}

func TestDelegatedUnparkIsNotedForTheLoopGuard(t *testing.T) {
	l := swapBlockerLedger(t)
	mock := &repoItemDecisionMock{issueLabels: []string{"bug", "stage/045-needs-decision"}}
	srv := newDelegatedServer(t, mock)
	w := postDelegated(t, srv, 7, "writer", `{"type":"issue","reason":"requires_human"}`, nil)
	job := decodeDelegatedJob(t, w)
	if job.Status != delegatedJobStatusDone || job.Decision == nil || job.Decision.Verdict != delegatedecision.VerdictApprove {
		t.Fatalf("job = %+v (%s)", job, w.Body.String())
	}
	e, ok := l.Get("testorg/testrepo", 7)
	if !ok || e.DelegatedUnparkAt.IsZero() {
		t.Fatalf("un-park not noted: %+v %v", e, ok)
	}
}

func fakeInstallationProbe(t *testing.T, perms map[string]string, err error) {
	t.Helper()
	prev := installationPermissionProbe
	installationPermissionProbe = func(context.Context, *ghpkg.AppAuth) (*ghpkg.InstallationInfo, error) {
		if err != nil {
			return nil, err
		}
		return &ghpkg.InstallationInfo{Permissions: perms}, nil
	}
	t.Cleanup(func() { installationPermissionProbe = prev })
}

func postBlockerAction(t *testing.T, srv *Server, action, user string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/repos/testorg/testrepo/items/7/blocker/"+action, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

func TestRepoItemBlockerRecheck(t *testing.T) {
	l := swapBlockerLedger(t)
	mock := &repoItemDecisionMock{issueLabels: []string{"needs-human", "stage/045-needs-decision"}}
	srv := newDelegatedServer(t, mock)
	at := time.Date(2026, 10, 20, 19, 33, 0, 0, time.UTC)
	// A grant stamps cleared-at from the ledger clock; pin it so the later
	// re-report (at+1h) is fresh rather than stale.
	l.SetNow(func() time.Time { return at })

	// No blocker on record → 404; not a capability → 409.
	if w := postBlockerAction(t, srv, "recheck", "writer"); w.Code != http.StatusNotFound {
		t.Fatalf("no blocker: code=%d body=%s", w.Code, w.Body.String())
	}
	l.Record("testorg/testrepo", 7, blocker.Blocker{Kind: blocker.HumanOnlyCLA, Since: at}, "cla")
	if w := postBlockerAction(t, srv, "recheck", "writer"); w.Code != http.StatusConflict {
		t.Fatalf("human-only: code=%d body=%s", w.Code, w.Body.String())
	}

	l.Clear("testorg/testrepo", 7)
	l.Observe("testorg/testrepo", 7, scannerWorkflowsReport, at)
	l.Observe("testorg/testrepo", 9, scannerWorkflowsReport, at)
	if w := postBlockerAction(t, srv, "recheck", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: code=%d", w.Code)
	}
	if w := postBlockerAction(t, srv, "recheck", "reader"); w.Code != http.StatusForbidden {
		t.Fatalf("reader: code=%d body=%s", w.Code, w.Body.String())
	}

	// Still missing: the blocker stands and the answer is the same
	// no-effect result the popover shows.
	fakeInstallationProbe(t, map[string]string{"contents": "write", "workflows": ""}, nil)
	w := postBlockerAction(t, srv, "recheck", "writer")
	if w.Code != http.StatusOK {
		t.Fatalf("recheck (missing): code=%d body=%s", w.Code, w.Body.String())
	}
	var resp blockerRecheckResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Granted || resp.Released != 0 || resp.Permission != "workflows" || resp.Result.Action != rowActionNoEffect || !strings.Contains(resp.Result.Outcome, "Still missing") {
		t.Fatalf("resp = %+v", resp)
	}
	if stageTrackerBlockerFor("testorg/testrepo", 7) == nil {
		t.Fatal("blocker cleared while the permission is still missing")
	}
	if caps := srv.capabilitiesStatus(); caps.BlockedItems != 2 || len(caps.Missing) != 1 || caps.Missing[0] != "workflows" || caps.ByPermission["workflows"] != 2 {
		t.Fatalf("capabilities = %+v", caps)
	}

	// Granted: every item blocked on that permission is released.
	fakeInstallationProbe(t, map[string]string{"contents": "write", "workflows": "write"}, nil)
	w = postBlockerAction(t, srv, "recheck", "writer")
	if w.Code != http.StatusOK {
		t.Fatalf("recheck (granted): code=%d body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Granted || resp.Released != 2 || resp.Result.Action != rowActionReleased {
		t.Fatalf("resp = %+v", resp)
	}
	if stageTrackerBlockerFor("testorg/testrepo", 7) != nil || stageTrackerBlockerFor("testorg/testrepo", 9) != nil {
		t.Fatal("blockers survived the grant")
	}
	if caps := srv.capabilitiesStatus(); caps.BlockedItems != 0 || len(caps.Missing) != 0 {
		t.Fatalf("capabilities after grant = %+v", caps)
	}
	var audited int
	for _, entry := range srv.GetAudit().Recent(20) {
		if entry.Action == auditRepoItemBlockerRecheck {
			audited++
		}
	}
	if audited != 2 {
		t.Fatalf("audit entries = %d", audited)
	}

	// Probe failure is reported, not swallowed.
	l.Observe("testorg/testrepo", 7, scannerWorkflowsReport, at.Add(time.Hour))
	fakeInstallationProbe(t, nil, errorString("installation lookup failed"))
	if w := postBlockerAction(t, srv, "recheck", "writer"); w.Code != http.StatusBadGateway {
		t.Fatalf("probe failure: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRepoItemBlockerClosePartial(t *testing.T) {
	l := swapBlockerLedger(t)
	mock := &repoItemDecisionMock{issueLabels: []string{"needs-human", "stage/045-needs-decision"}}
	srv := newDelegatedServer(t, mock)
	at := time.Date(2026, 10, 20, 19, 33, 0, 0, time.UTC)
	l.Observe("testorg/testrepo", 7, scannerWorkflowsReport, at)

	if w := postBlockerAction(t, srv, "close-partial", "reader"); w.Code != http.StatusForbidden {
		t.Fatalf("reader: code=%d", w.Code)
	}
	w := postBlockerAction(t, srv, "close-partial", "writer")
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(strings.Join(mock.added, ","), ghpkg.PartiallyAddressedLabel) {
		t.Fatalf("added = %v", mock.added)
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "workflows") || !strings.Contains(mock.comments[0], "@writer") {
		t.Fatalf("comments = %v", mock.comments)
	}
	if len(mock.edits) != 1 || mock.edits[0]["state"] != "closed" {
		t.Fatalf("edits = %v", mock.edits)
	}
	if stageTrackerBlockerFor("testorg/testrepo", 7) != nil {
		t.Fatal("ledger still holds the closed item")
	}
}

func TestStatusPayloadCarriesCapabilities(t *testing.T) {
	l := swapBlockerLedger(t)
	srv := newFullServer(t)
	l.Observe("testorg/testrepo", 7, scannerWorkflowsReport, time.Date(2026, 10, 20, 19, 33, 0, 0, time.UTC))
	caps := srv.capabilitiesStatus()
	b, err := json.Marshal(StatusPayload{Capabilities: caps})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Capabilities struct {
			Missing      []string       `json:"missing"`
			BlockedItems int            `json:"blocked_items"`
			ByPermission map[string]int `json:"by_permission"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Capabilities.BlockedItems != 1 || len(got.Capabilities.Missing) != 1 || got.Capabilities.Missing[0] != "workflows" || got.Capabilities.ByPermission["workflows"] != 1 {
		t.Fatalf("status capabilities = %s", b)
	}
}
