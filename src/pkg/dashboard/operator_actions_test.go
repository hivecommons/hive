package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestOperatorActionStoreRecordSettlePersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.json")
	st := newOperatorActionStore(path)
	at := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	if err := st.Record("Org/Repo", 7, OperatorAction{Action: operatorActionLetHiveDecide, Actor: "clubanderson", At: at, Outcome: operatorActionOutcomePending, Stage: 45}); err != nil {
		t.Fatal(err)
	}
	if got := st.List("org/repo", 7); len(got) != 1 || got[0].Outcome != operatorActionOutcomePending {
		t.Fatalf("list after record = %+v", got)
	}
	ok, err := st.Settle("org/repo", 7, operatorActionLetHiveDecide, operatorActionOutcomeApplied, "hive decided: approve")
	if err != nil || !ok {
		t.Fatalf("settle ok=%v err=%v", ok, err)
	}
	if ok, _ := st.Settle("org/repo", 7, operatorActionLetHiveDecide, operatorActionOutcomeApplied, "again"); ok {
		t.Fatal("settle must only close a pending record")
	}
	for i := 0; i < operatorActionsMaxPerItem+3; i++ {
		_ = st.Record("org/repo", 7, OperatorAction{Action: operatorActionApprove, At: at.Add(time.Duration(i) * time.Minute), Outcome: operatorActionOutcomeApplied})
	}
	if got := st.List("org/repo", 7); len(got) != operatorActionsMaxPerItem {
		t.Fatalf("cap: %d records, want %d", len(got), operatorActionsMaxPerItem)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("ledger not persisted: %v", err)
	}
	again := newOperatorActionStore(path)
	if got := again.List("org/repo", 7); len(got) != operatorActionsMaxPerItem || got[len(got)-1].Action != operatorActionApprove {
		t.Fatalf("reload = %d records, last %+v", len(got), got[len(got)-1])
	}
	if keys := again.Keys(); len(keys) != 1 || keys[0] != "org/repo#7" {
		t.Fatalf("keys = %v", keys)
	}
}

func TestOperatorActionFromBody(t *testing.T) {
	cases := []struct {
		endpoint, body string
		want           operatorActionRequest
		ok             bool
	}{
		{"decision", `{"decision":"approve","head":"abc"}`, operatorActionRequest{Action: operatorActionApprove, Head: "abc"}, true},
		{"decision", `{"decision":"direction","comment":" ship B "}`, operatorActionRequest{Action: operatorActionGiveDirection, Note: "ship B"}, true},
		{"decision", `{"decision":"close-superseded","superseded_by":"#41"}`, operatorActionRequest{Action: operatorActionCloseSuperseded, Target: "#41"}, true},
		{"decision", `{"decision":"close-wontfix"}`, operatorActionRequest{Action: operatorActionWontFix}, true},
		{"decision", `{"decision":"merge-now","head":"abc"}`, operatorActionRequest{Action: operatorActionMergeNow, Head: "abc"}, true},
		{"decision", `{"decision":"queue-merge"}`, operatorActionRequest{Action: operatorActionQueueMerge}, true},
		{"decision", `{"decision":"re-author"}`, operatorActionRequest{Action: operatorActionReauthor}, true},
		{"decision", `{"decision":"adopt"}`, operatorActionRequest{Action: operatorActionAdopt}, true},
		{"decision", `{"decision":"adopt","force":true,"comment":"now"}`, operatorActionRequest{Action: operatorActionAdoptNow, Note: "now"}, true},
		{"decision", `{"decision":"reroute","to":"@bee2"}`, operatorActionRequest{Action: operatorActionReroute, Target: "bee2"}, true},
		{"decision", `{"decision":"bogus"}`, operatorActionRequest{}, false},
		{"decide-delegated", ``, operatorActionRequest{Action: operatorActionLetHiveDecide}, true},
		{"hold", `{"held":true}`, operatorActionRequest{Action: operatorActionHold}, true},
		{"hold", `{"held":false}`, operatorActionRequest{Action: operatorActionRelease}, true},
		{"hold", `{}`, operatorActionRequest{}, false},
		{"accept-triage", ``, operatorActionRequest{Action: operatorActionAcceptTriage}, true},
		{"send-to-human", `{"reason":"needs a human eye"}`, operatorActionRequest{Action: operatorActionMarkNeedsHuman, Note: "needs a human eye"}, true},
		{"labels", `{}`, operatorActionRequest{}, false},
	}
	for _, c := range cases {
		got, ok := operatorActionFromBody(c.endpoint, []byte(c.body))
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%s %s: got %+v ok=%v, want %+v ok=%v", c.endpoint, c.body, got, ok, c.want, c.ok)
		}
	}
}

func TestOperatorActionOutcome(t *testing.T) {
	cases := []struct {
		status      int
		body        string
		outcome     string
		detail      string
		head        string
		record      bool
		description string
	}{
		{200, `{"ok":true,"status":"unparked","head":"feedface"}`, operatorActionOutcomeApplied, "unparked", "feedface", true, "approve"},
		{200, `{"ok":true,"alreadyAccepted":true}`, operatorActionOutcomeNoOp, "already accepted", "", true, "accept-triage twice"},
		{200, `{"ok":true,"released":["hold","needs-human"]}`, operatorActionOutcomeApplied, "hold, needs-human", "", true, "release"},
		{200, `{"status":"done","decision":{"verdict":"approve","rule":"auto-merge"}}`, operatorActionOutcomeApplied, "hive decided: approve (auto-merge)", "", true, "delegated sync done"},
		{200, `{"status":"done","decision":{"verdict":"supersede","rule":"dup"},"result":{"action":"noop-already-applied","outcome":"No change — superseded at 2026-10-28T13:01:00Z","applied_at":"2026-10-28T13:01:00Z","next":"Still blocked by contributor-lane:changes-requested."}}`, operatorActionOutcomeNoOp, "the hive had already decided 'supersede' at 13:01 UTC and it has not taken effect — still blocked: contributor-lane:changes-requested", "", true, "delegated sync no-op"},
		{200, `{"status":"failed","error":"boom","decision":{"verdict":""}}`, operatorActionOutcomeFailed, "boom", "", true, "delegated sync failed"},
		{202, `{"status":"running"}`, operatorActionOutcomePending, "running", "", true, "delegated async"},
		{409, `{"error":"not parked"}`, operatorActionOutcomeNoOp, "not parked", "", true, "conflict"},
		{502, `{"error":"github down"}`, operatorActionOutcomeFailed, "github down", "", true, "upstream"},
		{200, `{"ok":true,"status":"refused","result":{"action":"refused","outcome":"Close refused: repo paused — o/r is paused by the operator."}}`, operatorActionOutcomeFailed, "Close refused: repo paused — o/r is paused by the operator.", "", true, "refused close (#15436)"},
		{200, `{"ok":true,"status":"closed","result":{"action":"closed"}}`, operatorActionOutcomeApplied, "closed", "", true, "closed"},
		{400, `{"error":"bad"}`, "", "", "", false, "rejected input"},
		{403, ``, "", "", "", false, "forbidden"},
	}
	for _, c := range cases {
		outcome, detail, head, record := operatorActionOutcome(c.status, []byte(c.body))
		if outcome != c.outcome || detail != c.detail || head != c.head || record != c.record {
			t.Errorf("%s: got (%q,%q,%q,%v) want (%q,%q,%q,%v)", c.description, outcome, detail, head, record, c.outcome, c.detail, c.head, c.record)
		}
	}
}

func TestSettleOperatorActionNoOp(t *testing.T) {
	s := &Server{operatorActionStore: newOperatorActionStore(filepath.Join(t.TempDir(), "ops.json"))}
	at := time.Date(2026, 10, 28, 14, 23, 0, 0, time.UTC)
	if err := s.operatorActions().Record("org/repo", 14822, OperatorAction{Action: operatorActionLetHiveDecide, Actor: "clubanderson", At: at, Outcome: operatorActionOutcomePending, Stage: 85}); err != nil {
		t.Fatal(err)
	}
	res := rowActionNoopResult(rowActionTarget("org/repo", rowActionTargetPR, 14822), "superseded", at.Add(-82*time.Minute), "contributor-lane:changes-requested")
	job := &delegatedDecisionJob{Status: delegatedJobStatusDone, Decision: &delegatedecision.Decision{Verdict: "supersede", Rule: "dup"}, Result: &res}
	s.settleOperatorAction("org/repo", 14822, job)
	got := s.operatorActions().List("org/repo", 14822)
	if len(got) != 1 || got[0].Outcome != operatorActionOutcomeNoOp {
		t.Fatalf("settle no-op = %+v", got)
	}
	want := "the hive had already decided 'supersede' at 13:01 UTC and it has not taken effect — still blocked: contributor-lane:changes-requested"
	if got[0].Detail != want {
		t.Fatalf("detail = %q\nwant   %q", got[0].Detail, want)
	}
}

// A job that settles immediately must see the pending row, including when
// the handler has already returned its 202 response.
func TestOperatorActionRecordedBeforeDelegatedStart(t *testing.T) {
	for _, outcome := range []string{operatorActionOutcomeApplied, operatorActionOutcomeNoOp, operatorActionOutcomeFailed} {
		t.Run(outcome, func(t *testing.T) {
			s := &Server{operatorActionStore: newOperatorActionStore("")}
			started := false
			h := s.recordOperatorAction(func(w http.ResponseWriter, r *http.Request) {
				w.(*operatorActionWriter).afterRecord = func() {
					started = true
					ok, err := s.operatorActions().Settle("org/repo", 7, operatorActionLetHiveDecide, outcome, "finished immediately")
					if !ok || err != nil {
						t.Errorf("settle before record: ok=%v err=%v", ok, err)
					}
				}
				w.WriteHeader(http.StatusAccepted)
				jsonResponse(w, map[string]string{"status": delegatedJobStatusRunning})
				if started {
					t.Error("job started before handler returned")
				}
			})
			r := httptest.NewRequest(http.MethodPost, "/api/repos/org/repo/items/7/decide-delegated", strings.NewReader(`{}`))
			r.SetPathValue("owner", "org")
			r.SetPathValue("repo", "repo")
			r.SetPathValue("number", "7")
			w := httptest.NewRecorder()
			h(w, r)
			got := s.operatorActions().List("org/repo", 7)
			if w.Code != http.StatusAccepted || !started || len(got) != 1 || got[0].Outcome != outcome || got[0].Detail != "finished immediately" {
				t.Fatalf("code=%d started=%v records=%+v", w.Code, started, got)
			}
		})
	}
}

func TestStageTrackerOperatorActionStates(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	window := 30 * time.Minute
	rec := func(action, outcome string, ago time.Duration, stage int) []OperatorAction {
		return []OperatorAction{{Action: action, Actor: "o", At: now.Add(-ago), Outcome: outcome, Stage: stage}}
	}
	if got := stageTrackerOperatorAction(nil, 85, nil, nil, now, window); got != nil {
		t.Fatalf("no records → nil, got %+v", got)
	}
	cases := []struct {
		name     string
		actions  []OperatorAction
		stage    int
		mods     []string
		wantSt   string
		wantOutc string
	}{
		{"fresh same stage", rec(operatorActionApprove, operatorActionOutcomeApplied, 5*time.Minute, 85), 85, nil, operatorActionStateWaiting, operatorActionOutcomeApplied},
		{"aged same stage", rec(operatorActionApprove, operatorActionOutcomeApplied, 42*time.Minute, 85), 85, nil, operatorActionStateStuck, operatorActionOutcomeApplied},
		{"moved on", rec(operatorActionApprove, operatorActionOutcomeApplied, 42*time.Minute, 85), 100, nil, operatorActionStateAddressed, operatorActionOutcomeApplied},
		{"failed then moved anyway", rec(operatorActionApprove, operatorActionOutcomeFailed, 42*time.Minute, 85), 100, nil, operatorActionStateAddressed, operatorActionOutcomeSuperseded},
		{"failed same stage", rec(operatorActionApprove, operatorActionOutcomeFailed, 5*time.Minute, 85), 85, nil, operatorActionStateFailed, operatorActionOutcomeFailed},
		{"pending", rec(operatorActionLetHiveDecide, operatorActionOutcomePending, 50*time.Minute, 45), 45, nil, operatorActionStatePending, operatorActionOutcomePending},
		{"hold in effect", rec(operatorActionHold, operatorActionOutcomeApplied, 50*time.Minute, 30), 30, []string{modHold}, operatorActionStateAddressed, operatorActionOutcomeApplied},
		{"hold not yet", rec(operatorActionHold, operatorActionOutcomeApplied, 50*time.Minute, 30), 30, nil, operatorActionStateStuck, operatorActionOutcomeApplied},
		{"release done", rec(operatorActionRelease, operatorActionOutcomeApplied, 1*time.Minute, 30), 30, nil, operatorActionStateAddressed, operatorActionOutcomeApplied},
		{"needs-human set", rec(operatorActionMarkNeedsHuman, operatorActionOutcomeApplied, 1*time.Minute, 85), 85, []string{modNeedsHuman}, operatorActionStateAddressed, operatorActionOutcomeApplied},
		{"triage accepted", rec(operatorActionAcceptTriage, operatorActionOutcomeApplied, 1*time.Minute, 15), 20, nil, operatorActionStateAddressed, operatorActionOutcomeApplied},
		{"triage still", rec(operatorActionAcceptTriage, operatorActionOutcomeApplied, 1*time.Minute, 15), 15, []string{modNeedsTriage}, operatorActionStateWaiting, operatorActionOutcomeApplied},
	}
	for _, c := range cases {
		got := stageTrackerOperatorAction(c.actions, c.stage, c.mods, nil, now, window)
		if got == nil || got.State != c.wantSt || got.Outcome != c.wantOutc || got.StuckAfterSeconds != 1800 {
			t.Errorf("%s: got %+v, want state=%s outcome=%s", c.name, got, c.wantSt, c.wantOutc)
		}
	}
}

func TestStageTrackerOperatorActionFollowedTrail(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	actions := []OperatorAction{
		{Action: operatorActionGiveDirection, At: now.Add(-time.Hour), Outcome: operatorActionOutcomeApplied, Stage: 45},
		{Action: operatorActionApprove, At: now.Add(-10 * time.Minute), Outcome: operatorActionOutcomeApplied, Stage: 85},
	}
	history := []StageTrackerHop{
		{Number: 45, At: now.Add(-2 * time.Hour)},
		{Number: 85, At: now.Add(-30 * time.Minute)},
		{Number: 90, At: now.Add(-8 * time.Minute), Reason: "approved-direction"},
		{Number: 100, At: now.Add(-1 * time.Minute)},
	}
	got := stageTrackerOperatorAction(actions, 100, nil, history, now, 30*time.Minute)
	if got.Action != operatorActionApprove || got.State != operatorActionStateAddressed {
		t.Fatalf("newest record wins: %+v", got)
	}
	if len(got.Followed) != 2 || got.Followed[0].Number != 90 || got.Followed[1].Number != 100 {
		t.Fatalf("followed = %+v, want the two hops after the approve, oldest first", got.Followed)
	}
	many := make([]StageTrackerHop, 0, operatorActionFollowedMax+4)
	for i := 0; i < operatorActionFollowedMax+4; i++ {
		many = append(many, StageTrackerHop{Number: 50 + i, At: now.Add(-time.Duration(operatorActionFollowedMax+4-i) * time.Second)})
	}
	if f := operatorActionFollowed(actions[1], many); len(f) != operatorActionFollowedMax || f[len(f)-1].Number != 50+operatorActionFollowedMax+3 {
		t.Fatalf("cap keeps the newest %d: %+v", operatorActionFollowedMax, f)
	}
}

// Posting a row action through the dashboard records it; the tracker row
// and the item detail both carry it afterwards.
func TestOperatorActionRecordedByDecisionEndpoint(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"bug", "stage/045-needs-decision"}}
	srv := newRepoItemDecisionServer(t, mock)
	srv.operatorActionStore = newOperatorActionStore(filepath.Join(t.TempDir(), "ops.json"))
	srv.stageStore = stagelabels.NewStore("", func() time.Time { return time.Now().UTC() })
	srv.stageStore.Observe(stagelabels.Key("testorg/testrepo", 7), stagelabels.NeedsDecision, "test")

	if w := postRepoItemDecision(t, srv, 7, "reader", `{"decision":"approve"}`); w.Code != http.StatusForbidden {
		t.Fatalf("reader: code=%d body=%s", w.Code, w.Body.String())
	}
	if got := srv.operatorActions().List("testorg/testrepo", 7); len(got) != 0 {
		t.Fatalf("a rejected request must not be recorded: %+v", got)
	}

	if w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"approve","comment":"looks right"}`); w.Code != http.StatusOK {
		t.Fatalf("approve: code=%d body=%s", w.Code, w.Body.String())
	}
	got := srv.operatorActions().List("testorg/testrepo", 7)
	if len(got) != 1 {
		t.Fatalf("records = %+v", got)
	}
	a := got[0]
	if a.Action != operatorActionApprove || a.Actor != "writer" || a.Outcome != operatorActionOutcomeApplied || a.Detail != "unparked" || a.Note != "looks right" || a.Stage != stagelabels.NeedsDecision.Number() || a.At.IsZero() {
		t.Fatalf("record = %+v", a)
	}

	// A second approve on the now un-parked issue is a 409: recorded as
	// a no-op so the owner sees "nothing to change", not nothing.
	mock.issueLabels = []string{"bug"}
	if w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"approve"}`); w.Code != http.StatusConflict {
		t.Fatalf("second approve: code=%d body=%s", w.Code, w.Body.String())
	}
	got = srv.operatorActions().List("testorg/testrepo", 7)
	if len(got) != 2 || got[1].Outcome != operatorActionOutcomeNoOp {
		t.Fatalf("records after conflict = %+v", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/repos/testorg/testrepo/items/7", nil)
	req.Header.Set("X-Hive-User", "writer")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("detail: code=%d body=%s", w.Code, w.Body.String())
	}
	var detail repoItemDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Repo != "testorg/testrepo" || detail.Number != 7 || detail.OperatorActionCount != 2 || len(detail.OperatorActions) != 2 || detail.LastOperatorAction == nil {
		t.Fatalf("detail = %s", w.Body.String())
	}
	if detail.LastOperatorAction.Action != operatorActionApprove || detail.LastOperatorAction.Outcome != operatorActionOutcomeNoOp {
		t.Fatalf("last = %+v", detail.LastOperatorAction)
	}
	var raw map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	for _, k := range []string{"operator_actions", "operator_action_count", "last_operator_action", "stage"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("detail payload lacks %q: %s", k, w.Body.String())
		}
	}

	items := []StageTrackerItem{{Repo: "testorg/testrepo", Number: 7, Stage: 45}, {Repo: "testorg/testrepo", Number: 8, Stage: 85}}
	srv.applyOperatorActions(items, time.Now().UTC())
	if items[0].OperatorActionCount != 2 || items[0].LastOperatorAction == nil || (items[0].LastOperatorAction.Outcome != operatorActionOutcomeNoOp && items[0].LastOperatorAction.Outcome != operatorActionOutcomeSuperseded) || items[0].LastOperatorAction.State == "" {
		t.Fatalf("tracker item count=%d last=%+v", items[0].OperatorActionCount, items[0].LastOperatorAction)
	}
	if items[1].OperatorActionCount != 0 || items[1].LastOperatorAction != nil {
		t.Fatalf("untouched item must stay bare: %+v", items[1])
	}
	payload, _ := json.Marshal(items[0])
	for _, k := range []string{`"last_operator_action":{`, `"operator_action_count":2`, `"state":"`, `"stuck_after_seconds":`} {
		if !strings.Contains(string(payload), k) {
			t.Errorf("row payload lacks %s: %s", k, payload)
		}
	}
}

// stageTrackerActedJS lifts the "did I act on this?" badge helpers the row
// renderer and the filter call (operator_actions.go payload fields). Callers
// lift STAGE_TRACKER_ICONS themselves: the history badge draws from it.
func stageTrackerActedJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		jsConstBlock(t, html, "STAGE_TRACKER_ACTED_LABELS"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTED_STATES"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTED_FILTERS"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTED_STUCK_DEFAULT_S"),
		jsConstLine(t, html, "STAGE_TRACKER_HISTORY_ICON"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTED_GLYPH"),
		jsConstBlock(t, html, "STAGE_TRACKER_ACTED_PLAIN"),
		jsFunc(t, html, "stageTrackerActedLabel"),
		jsFunc(t, html, "stageTrackerActedSource"),
		jsFunc(t, html, "stageTrackerActedState"),
		jsFunc(t, html, "stageTrackerActedAgo"),
		jsFunc(t, html, "stageTrackerActedOutcomeWords"),
		jsFunc(t, html, "stageTrackerActedStageName"),
		jsFunc(t, html, "stageTrackerActedPlain"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTED_OPTION_ID"),
		jsFunc(t, html, "stageTrackerActedOptions"),
		jsFunc(t, html, "stageTrackerActedTip"),
		jsFunc(t, html, "stageTrackerActedBadgeHtml"),
		jsFunc(t, html, "stageTrackerActedMatches"),
	}, "\n")
}

// hivecommons/hive#15188: an adopt-supersede that only queued the request
// label is `executing`, not `applied`; a dedup no-effect is `no-effect`; a
// capability-gap recommendation is `blocked`.
func TestSettledDelegatedOutcomeDistinguishesExecution(t *testing.T) {
	adopt := &delegatedecision.Decision{Verdict: delegatedecision.VerdictAdoptSupersede, Rule: "contributor-offline"}
	cases := []struct {
		name    string
		job     *delegatedDecisionJob
		outcome string
		detail  string
	}{
		{"adopt queued", &delegatedDecisionJob{Status: delegatedJobStatusDone, Decision: adopt, Result: &RowActionResult{Action: "adopt-supersede"}}, operatorActionOutcomeExecuting, "decided: adopt-supersede (contributor-offline) · executing…"},
		{"adopt no effect", &delegatedDecisionJob{Status: delegatedJobStatusDone, Decision: adopt, Result: &RowActionResult{Action: rowActionNoEffect, Outcome: "Hive decided X but it hasn't taken effect: the copy was closed."}}, operatorActionOutcomeNoEffect, "Hive decided X but it hasn't taken effect: the copy was closed"},
		{"capability gap", &delegatedDecisionJob{Status: delegatedJobStatusDone, Decision: &delegatedecision.Decision{Verdict: delegatedecision.VerdictRecommendOnly, Rule: delegatedecision.RuleCapabilityGap, KeepsPark: true}}, operatorActionOutcomeBlocked, ""},
		{"failed", &delegatedDecisionJob{Status: delegatedJobStatusFailed, Error: "boom", Decision: adopt}, operatorActionOutcomeFailed, "boom"},
		{"approve applied", &delegatedDecisionJob{Status: delegatedJobStatusDone, Decision: &delegatedecision.Decision{Verdict: delegatedecision.VerdictApprove, Rule: "default-release"}}, operatorActionOutcomeApplied, "hive decided: approve (default-release)"},
	}
	for _, c := range cases {
		outcome, detail := settledDelegatedOutcome(c.job)
		if outcome != c.outcome || (c.detail != "" && detail != c.detail) {
			t.Errorf("%s: outcome=%q detail=%q, want %q / %q", c.name, outcome, detail, c.outcome, c.detail)
		}
		if c.name == "capability gap" && !strings.Contains(detail, "workflows") {
			t.Errorf("capability gap detail should name the permission: %q", detail)
		}
	}
}

// The ✓ for an `executing` adopt-supersede follows the worker, not the
// stage: adopted → addressed "adopted as #N"; a workflow-path refusal →
// blocked on the workflows capability with a human next step; a closed copy
// → still executing; nothing on record past the window → stuck.
func TestStageTrackerOperatorActionExecutingFollowsTheWorker(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	window := 30 * time.Minute
	rec := func(ago time.Duration) []OperatorAction {
		return []OperatorAction{{Action: operatorActionLetHiveDecide, Actor: "o", At: now.Add(-ago), Outcome: operatorActionOutcomeExecuting, Detail: "decided: adopt-supersede (contributor-offline) · executing…", Stage: 85}}
	}
	cases := []struct {
		name     string
		actions  []OperatorAction
		stage    int
		exec     operatorActionExecution
		wantSt   string
		wantOutc string
		progress string
		blocker  string
		human    bool
	}{
		{"just decided", rec(2 * time.Minute), 85, operatorActionExecution{}, operatorActionStateExecuting, operatorActionOutcomeExecuting, "", "", false},
		{"adopted", rec(5 * time.Minute), 74, operatorActionExecution{AdoptedBy: 14977}, operatorActionStateAddressed, operatorActionOutcomeApplied, "adopted as #14977", "", false},
		{"adopted and superseded", rec(5 * time.Minute), 85, operatorActionExecution{AdoptedBy: 14977, Superseded: true}, operatorActionStateAddressed, operatorActionOutcomeApplied, "closed in its favour", "", false},
		{"copy closed, reopening", rec(5 * time.Minute), 85, operatorActionExecution{AdoptedBy: 14977, AdoptedByState: stageTrackerAdoptedClosed}, operatorActionStateExecuting, operatorActionOutcomeExecuting, "opening a fresh one", "", false},
		{"copies closed twice", rec(5 * time.Minute), 85, operatorActionExecution{AdoptedBy: 14977, AdoptedByState: stageTrackerAdoptedClosedTwice}, operatorActionStateFailed, operatorActionOutcomeExecuting, "could not adopt", "", true},
		{"workflow path refused", rec(5 * time.Minute), 85, operatorActionExecution{Refused: "adoption-stalled:contributor-lane:workflow-path"}, operatorActionStateBlocked, operatorActionOutcomeExecuting, "could not adopt", operatorActionBlockerWorkflows, true},
		{"other refusal", rec(5 * time.Minute), 85, operatorActionExecution{Refused: "adoption-stalled:contributor-lane:child-closed-twice"}, operatorActionStateFailed, operatorActionOutcomeExecuting, "could not adopt", "", true},
		{"nothing on record past window", rec(2 * time.Hour), 85, operatorActionExecution{}, operatorActionStateStuck, operatorActionOutcomeExecuting, "no adoption on record 2 h after the decision", "", true},
	}
	for _, c := range cases {
		got := stageTrackerOperatorActionExec(c.actions, c.stage, nil, nil, now, window, c.exec)
		if got == nil || got.State != c.wantSt || got.Outcome != c.wantOutc {
			t.Errorf("%s: got %+v, want state=%s outcome=%s", c.name, got, c.wantSt, c.wantOutc)
			continue
		}
		if c.progress != "" && !strings.Contains(got.Progress, c.progress) {
			t.Errorf("%s: progress %q missing %q", c.name, got.Progress, c.progress)
		}
		if got.Blocker != c.blocker {
			t.Errorf("%s: blocker %q want %q", c.name, got.Blocker, c.blocker)
		}
		if (got.Human != "") != c.human {
			t.Errorf("%s: human %q (want present=%v)", c.name, got.Human, c.human)
		}
		if got.Progress != "" && (strings.Contains(got.Progress, "contributor-lane") || strings.Contains(got.Progress, "adoption-stalled")) {
			t.Errorf("%s: bare slug in progress %q", c.name, got.Progress)
		}
	}
	// A `blocked` ledger record (capability-gap recommendation) carries the
	// blocker and the human step regardless of exec state.
	blocked := []OperatorAction{{Action: operatorActionLetHiveDecide, Actor: "o", At: now.Add(-time.Minute), Outcome: operatorActionOutcomeBlocked, Stage: 85}}
	got := stageTrackerOperatorAction(blocked, 85, nil, nil, now, window)
	if got == nil || got.State != operatorActionStateBlocked || got.Blocker != operatorActionBlockerWorkflows || got.Human == "" {
		t.Fatalf("blocked record = %+v", got)
	}
	// A no-effect record on the same stage is stuck with its own words; once
	// the item moved on it reads as superseded by the state change.
	noEffect := []OperatorAction{{Action: operatorActionLetHiveDecide, Actor: "o", At: now.Add(-time.Minute), Outcome: operatorActionOutcomeNoEffect, Detail: "the copy was closed", Stage: 85}}
	if got := stageTrackerOperatorAction(noEffect, 85, nil, nil, now, window); got == nil || got.State != operatorActionStateStuck || got.Progress != "the copy was closed" || got.Human == "" {
		t.Fatalf("no-effect same stage = %+v", got)
	}
	if got := stageTrackerOperatorAction(noEffect, 74, nil, nil, now, window); got == nil || got.State != operatorActionStateAddressed || got.Outcome != operatorActionOutcomeSuperseded {
		t.Fatalf("no-effect moved on = %+v", got)
	}
}

// operatorActionExecutionFor reads the adoption off the stage record over
// the tracker item and surfaces a terminal refusal for the current head.
func TestOperatorActionExecutionForReadsRecord(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	store := stagelabels.NewStore("", func() time.Time { return now })
	key := stagelabels.Key("testorg/testrepo", 8)
	store.Observe(key, stagelabels.NeedsDecision, "test")
	it := StageTrackerItem{Repo: "testorg/testrepo", Number: 8, Head: "aaa", Reason: "adoption-stalled:contributor-lane:workflow-path"}
	if got := operatorActionExecutionFor(it, stagelabels.Record{}, false); got.Refused != it.Reason || got.AdoptedBy != 0 {
		t.Fatalf("no record: %+v", got)
	}
	store.MarkAdopted(key, "aaa", 91, now)
	rec, _ := store.Get(key)
	if got := operatorActionExecutionFor(StageTrackerItem{Head: "aaa"}, rec, true); got.AdoptedBy != 91 || got.AdoptedByState != "" || got.Refused != "" {
		t.Fatalf("adopted: %+v", got)
	}
	store.MarkAdoptionEscalated(key, "aaa", "adoption-stalled:contributor-lane:workflow-path")
	rec, _ = store.Get(key)
	if got := operatorActionExecutionFor(StageTrackerItem{Head: "aaa"}, rec, true); !strings.Contains(got.Refused, "workflow-path") {
		t.Fatalf("escalated: %+v", got)
	}
	if got := operatorActionExecutionFor(StageTrackerItem{Head: "bbb"}, rec, true); got.Refused != "" {
		t.Fatalf("escalation for another head leaked: %+v", got)
	}
}

// An item the operator just closed from the dashboard leaves the tracker
// at once (hivecommons/hive#15436), for the grace window only, and only
// when the close actually applied.
func TestStageTrackerDropOperatorClosed(t *testing.T) {
	now := time.Date(2026, 10, 29, 17, 20, 0, 0, time.UTC)
	grace := 15 * time.Minute
	act := func(action, outcome, detail string, ago time.Duration) *StageTrackerOperatorAction {
		return &StageTrackerOperatorAction{Action: action, Outcome: outcome, Detail: detail, At: now.Add(-ago)}
	}
	items := []StageTrackerItem{
		{Repo: "o/r", Number: 1, Kind: "issue"},
		{Repo: "o/r", Number: 2, Kind: "issue", LastOperatorAction: act(operatorActionCloseRead, operatorActionOutcomeApplied, operatorActionClosedDetail, time.Minute)},
		{Repo: "o/r", Number: 3, Kind: "pr", LastOperatorAction: act(operatorActionCloseSuperseded, operatorActionOutcomeApplied, operatorActionClosedDetail, 2*time.Minute)},
		{Repo: "o/r", Number: 4, Kind: "issue", LastOperatorAction: act(operatorActionWontFix, operatorActionOutcomeApplied, operatorActionClosedDetail, grace+time.Second)},
		{Repo: "o/r", Number: 5, Kind: "issue", LastOperatorAction: act(operatorActionCloseRead, operatorActionOutcomeFailed, "Close refused: repo paused", time.Minute)},
		{Repo: "o/r", Number: 6, Kind: "issue", LastOperatorAction: act(operatorActionApprove, operatorActionOutcomeApplied, "unparked", time.Minute)},
		{Repo: "o/r", Number: 7, Kind: "issue", LastOperatorAction: act(operatorActionCloseIssue, operatorActionOutcomeApplied, operatorActionClosedDetail, time.Minute)},
		{Repo: "o/r", Number: 8, Kind: "issue", LastOperatorAction: act(operatorActionCloseIssue, operatorActionOutcomeFailed, "permission denied", time.Minute)},
		{Repo: "o/r", Number: 9, Kind: "pr", LinkedIssues: []StageTrackerLinkedIssue{{Number: 7, State: "open"}}},
	}
	got := stageTrackerDropOperatorClosed(items, now, grace)
	var nums []int
	for _, it := range got {
		nums = append(nums, it.Number)
	}
	want := []int{1, 4, 5, 6, 8, 9}
	if !slices.Equal(nums, want) {
		t.Fatalf("kept %v, want %v (just-closed rows 2 and 3 must leave; a refused close, an old close and a non-close stay)", nums, want)
	}
}
