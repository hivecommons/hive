package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// fakeVerdictStore is an in-memory NousVerdictStore.
type fakeVerdictStore struct {
	verdict NousVerdict
	ok      bool
	err     error
	loads   int
}

func (f *fakeVerdictStore) LoadVerdict() (NousVerdict, bool, error) {
	f.loads++
	return f.verdict, f.ok, f.err
}

// fakePromotionStore is an in-memory NousPromotionStore.
type fakePromotionStore struct {
	promotions []NousPromotion
	recordErr  error
	listErr    error
}

func (f *fakePromotionStore) RecordPromotion(p NousPromotion) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.promotions = append(f.promotions, p)
	return nil
}

func (f *fakePromotionStore) Promotions() ([]NousPromotion, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]NousPromotion(nil), f.promotions...), nil
}

// fakeNousFiler records CreateIssue calls.
type fakeNousFiler struct {
	calls  int
	repo   string
	title  string
	body   string
	labels []string
	err    error
}

func (f *fakeNousFiler) CreateIssue(_ context.Context, repo, title, body string, labels []string) (ghpkg.CreateIssueResult, error) {
	f.calls++
	f.repo, f.title, f.body, f.labels = repo, title, body, labels
	if f.err != nil {
		return ghpkg.CreateIssueResult{}, f.err
	}
	return ghpkg.CreateIssueResult{Number: 77, URL: "https://github.com/" + repo + "/issues/77"}, nil
}

func sampleVerdict() NousVerdict {
	return NousVerdict{
		Campaign:                "governor",
		Recommendation:          NousVerdictWithCaveats,
		Score:                   0.82,
		Baseline:                0.75,
		MeasuredAgainstBaseline: true,
		Citations:               []string{"ledger#12", "ledger#15"},
		Caveats:                 []string{"only two rounds"},
		Summary:                 "Lower kick cap helped.",
		TargetRepo:              "myorg/repo1",
		Report:                  "/data/nous/governor/report.md",
	}
}

// verdictTestServer is a runs-capable server with a Nous state holding a
// verdict and a fake issue filer; the Spektacular runner is enabled.
func verdictTestServer(t *testing.T) (*Server, *fakeNousFiler, *fakePromotionStore) {
	t.Helper()
	s, deps := runsTestServer(t)
	deps.Config.Runs.Spektacular.Enabled = true
	disableSpekHubExecutorForRelayTests(s)
	v := sampleVerdict()
	promos := &fakePromotionStore{}
	deps.Nous = &NousState{
		Mode:           "active",
		Status:         map[string]interface{}{"ok": true},
		Config:         map[string]interface{}{"output": map[string]interface{}{"autoHold": true}},
		Verdict:        &v,
		PromotionStore: promos,
		Now:            func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) },
	}
	filer := &fakeNousFiler{}
	s.nousIssueFiler = filer
	return s, filer, promos
}

func decodePromote(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	return out
}

func TestNousPromoteRequiresOwner(t *testing.T) {
	s, filer, _ := verdictTestServer(t)
	rec := doPostNoOwner(s, "/api/nous/promote", map[string]string{"target": NousPromoteTargetIssue})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner = %d, want 403", rec.Code)
	}
	if filer.calls != 0 {
		t.Fatalf("non-owner must not open issues")
	}
}

func TestNousPromoteWithoutNous(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/nous/promote", map[string]string{"target": NousPromoteTargetIssue})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no nous = %d, want 404", rec.Code)
	}
}

func TestNousPromoteValidation(t *testing.T) {
	s, filer, _ := verdictTestServer(t)
	if rec := doPostRaw(s, "/api/nous/promote", "{not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json = %d, want 400", rec.Code)
	}
	for _, target := range []string{"", "pr", "ISSUE"} {
		rec := doPost(s, "/api/nous/promote", map[string]string{"target": target})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("target %q = %d, want 400", target, rec.Code)
		}
	}
	if rec := doPost(s, "/api/nous/promote", map[string]string{"target": NousPromoteTargetIssue, "repo": "../x"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad repo = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if filer.calls != 0 {
		t.Fatalf("validation failures must not open issues")
	}
	if e := lastAudit(t, s, nousAuditPromoteFailed); !strings.Contains(e.Detail, "repo") {
		t.Fatalf("failed audit detail = %q", e.Detail)
	}
}

func TestNousPromoteNoVerdict(t *testing.T) {
	s, _, _ := verdictTestServer(t)
	s.deps.Nous.Verdict = nil
	rec := doPost(s, "/api/nous/promote", map[string]string{"target": NousPromoteTargetIssue})
	if rec.Code != http.StatusConflict {
		t.Fatalf("no verdict = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

func TestNousPromoteIssueOpensIssueOnceAndAudits(t *testing.T) {
	s, filer, promos := verdictTestServer(t)
	rec := doPost(s, "/api/nous/promote", map[string]string{"target": NousPromoteTargetIssue})
	if rec.Code != http.StatusOK {
		t.Fatalf("promote = %d: %s", rec.Code, rec.Body.String())
	}
	out := decodePromote(t, rec.Body.Bytes())
	if out["status"] != nousPromoteStatusDone {
		t.Fatalf("status = %v", out["status"])
	}
	p := out["promotion"].(map[string]interface{})
	if p["repo"] != "myorg/repo1" || p["number"] != float64(77) || p["target"] != NousPromoteTargetIssue || p["promoted_by"] != "local" {
		t.Fatalf("promotion = %+v", p)
	}
	if filer.calls != 1 || filer.repo != "myorg/repo1" {
		t.Fatalf("filer calls=%d repo=%q", filer.calls, filer.repo)
	}
	if !strings.HasPrefix(filer.title, nousIssueTitlePrefix+": deploy with caveats (governor)") {
		t.Fatalf("title = %q", filer.title)
	}
	if len(filer.labels) != 1 || filer.labels[0] != nousIssueLabelHold {
		t.Fatalf("labels = %v, want hold (autoHold on)", filer.labels)
	}
	for _, want := range []string{"0.82 vs. 0.75", "ledger#12", "only two rounds", "Lower kick cap helped.", "report.md", "Promoted by hand", "hive: agent=dashboard"} {
		if !strings.Contains(filer.body, want) {
			t.Fatalf("body missing %q:\n%s", want, filer.body)
		}
	}
	if len(promos.promotions) != 1 || promos.promotions[0].Number != 77 {
		t.Fatalf("store = %+v", promos.promotions)
	}
	if e := lastAudit(t, s, nousAuditPromote); !strings.Contains(e.Detail, "target="+NousPromoteTargetIssue) || !strings.Contains(e.Detail, "number=77") {
		t.Fatalf("audit = %q", e.Detail)
	}
	if e := lastAudit(t, s, ghpkg.AuditActionHiveIssueCreated); !strings.Contains(e.Detail, "flow="+nousIssueFlow) {
		t.Fatalf("issue audit = %q", e.Detail)
	}

	// Idempotent: the second promote to the same target is a 409 and does
	// not open another issue.
	rec = doPost(s, "/api/nous/promote", map[string]string{"target": NousPromoteTargetIssue})
	if rec.Code != http.StatusConflict {
		t.Fatalf("repeat = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	out = decodePromote(t, rec.Body.Bytes())
	if out["status"] != nousPromoteStatusAlready || out["promotion"].(map[string]interface{})["number"] != float64(77) {
		t.Fatalf("repeat body = %v", out)
	}
	if filer.calls != 1 || len(promos.promotions) != 1 {
		t.Fatalf("repeat must not re-open: calls=%d store=%d", filer.calls, len(promos.promotions))
	}
	if e := lastAudit(t, s, nousAuditPromoteFailed); !strings.Contains(e.Detail, nousPromoteStatusAlready) {
		t.Fatalf("repeat audit = %q", e.Detail)
	}
}

func TestNousPromoteNoHoldLabelWhenAutoHoldOff(t *testing.T) {
	s, filer, _ := verdictTestServer(t)
	s.deps.Nous.Config = map[string]interface{}{}
	if rec := doPost(s, "/api/nous/promote", map[string]string{"target": NousPromoteTargetIssue}); rec.Code != http.StatusOK {
		t.Fatalf("promote = %d: %s", rec.Code, rec.Body.String())
	}
	if len(filer.labels) != 0 {
		t.Fatalf("labels = %v, want none", filer.labels)
	}
}

func TestNousPromoteSpektacularRunAdmitsRunReusingIssue(t *testing.T) {
	s, filer, promos := verdictTestServer(t)
	if rec := doPost(s, "/api/nous/promote", map[string]string{"target": NousPromoteTargetIssue}); rec.Code != http.StatusOK {
		t.Fatalf("issue promote = %d: %s", rec.Code, rec.Body.String())
	}
	rec := doPost(s, "/api/nous/promote", map[string]string{"target": NousPromoteTargetSpektacularRun})
	if rec.Code != http.StatusOK {
		t.Fatalf("run promote = %d: %s", rec.Code, rec.Body.String())
	}
	p := decodePromote(t, rec.Body.Bytes())["promotion"].(map[string]interface{})
	if p["run_key"] != "myorg/repo1#77" || p["number"] != float64(77) {
		t.Fatalf("promotion = %+v", p)
	}
	if filer.calls != 1 {
		t.Fatalf("run target must reuse the issue, calls=%d", filer.calls)
	}
	stages, err := s.RunStageAccessor().PendingRunStages(context.Background())
	if err != nil {
		t.Fatalf("PendingRunStages: %v", err)
	}
	if len(stages) != 1 || stages[0].Repo != "myorg/repo1" || stages[0].RunKey != "myorg/repo1#77" || stages[0].Stage != StageSpec {
		t.Fatalf("stages = %+v", stages)
	}
	if len(promos.promotions) != 2 {
		t.Fatalf("store = %+v", promos.promotions)
	}
	if e := lastAudit(t, s, nousAuditPromote); !strings.Contains(e.Detail, "run_key=myorg/repo1#77") {
		t.Fatalf("audit = %q", e.Detail)
	}
}

func TestNousPromoteSpektacularRunOpensIssueWhenNone(t *testing.T) {
	s, filer, _ := verdictTestServer(t)
	rec := doPost(s, "/api/nous/promote", map[string]string{"target": NousPromoteTargetSpektacularRun})
	if rec.Code != http.StatusOK {
		t.Fatalf("run promote = %d: %s", rec.Code, rec.Body.String())
	}
	if filer.calls != 1 {
		t.Fatalf("calls = %d", filer.calls)
	}
	stages, err := s.RunStageAccessor().PendingRunStages(context.Background())
	if err != nil || len(stages) != 1 {
		t.Fatalf("stages = %+v err=%v", stages, err)
	}
}

func TestNousPromoteSpektacularRunPreconditions(t *testing.T) {
	s, filer, _ := verdictTestServer(t)
	s.deps.Config.Runs.Spektacular.Enabled = false
	rec := doPost(s, "/api/nous/promote", map[string]string{"target": NousPromoteTargetSpektacularRun})
	if rec.Code != http.StatusConflict {
		t.Fatalf("runner off = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	s.deps.Config.Runs.Spektacular.Enabled = true
	hub := s.contributeHub
	s.contributeHub = nil
	rec = doPost(s, "/api/nous/promote", map[string]string{"target": NousPromoteTargetSpektacularRun})
	s.contributeHub = hub
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no hub = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if filer.calls != 0 {
		t.Fatalf("precondition failures must not open issues")
	}
}

func TestNousPromoteRepoResolution(t *testing.T) {
	s, filer, _ := verdictTestServer(t)
	// Body repo wins and is qualified with the project org.
	if rec := doPost(s, "/api/nous/promote", map[string]string{"target": NousPromoteTargetIssue, "repo": "other"}); rec.Code != http.StatusOK {
		t.Fatalf("promote = %d: %s", rec.Code, rec.Body.String())
	}
	if filer.repo != "myorg/other" {
		t.Fatalf("repo = %q", filer.repo)
	}

	// No verdict repo, single configured repo -> used.
	s2, filer2, _ := verdictTestServer(t)
	s2.deps.Nous.Verdict.TargetRepo = ""
	s2.deps.Nous.Config["repos"] = map[string]interface{}{"repos": []interface{}{"acme/widgets"}}
	if rec := doPost(s2, "/api/nous/promote", map[string]string{"target": NousPromoteTargetIssue}); rec.Code != http.StatusOK {
		t.Fatalf("promote = %d: %s", rec.Code, rec.Body.String())
	}
	if filer2.repo != "acme/widgets" {
		t.Fatalf("repo = %q", filer2.repo)
	}

	// Several configured repos and no verdict repo -> the owner must name one.
	s3, filer3, _ := verdictTestServer(t)
	s3.deps.Nous.Verdict.TargetRepo = ""
	s3.deps.Nous.Config["repos"] = map[string]interface{}{"repos": []interface{}{"a/b", "c/d"}}
	if rec := doPost(s3, "/api/nous/promote", map[string]string{"target": NousPromoteTargetIssue}); rec.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous repo = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if filer3.calls != 0 {
		t.Fatalf("ambiguous repo must not open issues")
	}
}

func TestNousPromoteFilerFailures(t *testing.T) {
	s, filer, promos := verdictTestServer(t)
	filer.err = errors.New("boom")
	rec := doPost(s, "/api/nous/promote", map[string]string{"target": NousPromoteTargetIssue})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("filer error = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if len(promos.promotions) != 0 {
		t.Fatalf("failed create must not be recorded")
	}

	s.nousIssueFiler = nil
	s.deps.GHClient = nil
	rec = doPost(s, "/api/nous/promote", map[string]string{"target": NousPromoteTargetIssue})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no client = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

func TestNousPromoteStoreFailure(t *testing.T) {
	s, _, promos := verdictTestServer(t)
	promos.recordErr = errors.New("disk full")
	rec := doPost(s, "/api/nous/promote", map[string]string{"target": NousPromoteTargetIssue})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("store error = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if e := lastAudit(t, s, nousAuditPromoteFailed); !strings.Contains(e.Detail, "disk full") {
		t.Fatalf("audit = %q", e.Detail)
	}
}

func TestNousApproveNoLongerAdmitsRuns(t *testing.T) {
	s, filer, _ := verdictTestServer(t)
	s.deps.Nous.Config["output"] = map[string]interface{}{"mode": "spektacular-run"}
	if rec := doPost(s, "/api/nous/approve", nil); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", rec.Code, rec.Body.String())
	}
	stages, err := s.RunStageAccessor().PendingRunStages(context.Background())
	if err != nil || len(stages) != 0 || filer.calls != 0 {
		t.Fatalf("approve must not promote: stages=%+v calls=%d err=%v", stages, filer.calls, err)
	}
}

func TestNousStatusIncludesVerdictAndPromotions(t *testing.T) {
	s, _, _ := verdictTestServer(t)
	if rec := doPost(s, "/api/nous/promote", map[string]string{"target": NousPromoteTargetIssue}); rec.Code != http.StatusOK {
		t.Fatalf("promote = %d: %s", rec.Code, rec.Body.String())
	}
	rec := doGet(s, "/api/nous/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	out := decodePromote(t, rec.Body.Bytes())
	if out["ok"] != true {
		t.Fatalf("existing status keys must survive: %v", out)
	}
	v := out[nousStatusKeyVerdict].(map[string]interface{})
	if v[nousVerdictKeyRecommend] != NousVerdictWithCaveats || v[nousVerdictKeyMeasured] != true || v[nousVerdictKeyReport] != "/data/nous/governor/report.md" {
		t.Fatalf("verdict = %v", v)
	}
	targets := v[nousVerdictKeyTargets].([]interface{})
	if len(targets) != 2 || targets[0] != NousPromoteTargetIssue || targets[1] != NousPromoteTargetSpektacularRun {
		t.Fatalf("targets = %v", targets)
	}
	promos := v[nousVerdictKeyPromotions].(map[string]interface{})
	issue := promos[NousPromoteTargetIssue].(map[string]interface{})
	if issue[nousPromotionKeyNumber] != float64(77) || issue[nousPromotionKeyURL] == "" {
		t.Fatalf("issue promotion = %v", issue)
	}
	if _, ok := promos[NousPromoteTargetSpektacularRun]; ok {
		t.Fatalf("run promotion must be absent until promoted")
	}

	// Not-measured verdicts carry the flag; no verdict drops the key.
	s.deps.Nous.Verdict.MeasuredAgainstBaseline = false
	out = decodePromote(t, doGet(s, "/api/nous/status").Body.Bytes())
	if out[nousStatusKeyVerdict].(map[string]interface{})[nousVerdictKeyMeasured] != false {
		t.Fatalf("measured flag not served")
	}
	s.deps.Nous.Verdict = nil
	s.deps.Nous.Status[nousStatusKeyVerdict] = "stale"
	out = decodePromote(t, doGet(s, "/api/nous/status").Body.Bytes())
	if _, ok := out[nousStatusKeyVerdict]; ok {
		t.Fatalf("stale verdict must be dropped: %v", out)
	}
}

func TestNousStateSyncVerdict(t *testing.T) {
	var nilState *NousState
	if err := nilState.SyncVerdict(); err != nil {
		t.Fatalf("nil state: %v", err)
	}
	ns := &NousState{}
	if err := ns.SyncVerdict(); err != nil || ns.Verdict != nil {
		t.Fatalf("no stores: err=%v verdict=%v", err, ns.Verdict)
	}

	vs := &fakeVerdictStore{verdict: sampleVerdict(), ok: true}
	ps := &fakePromotionStore{promotions: []NousPromotion{{Campaign: "governor", Target: NousPromoteTargetIssue, Number: 3}}}
	ns = &NousState{VerdictStore: vs, PromotionStore: ps}
	if err := ns.SyncVerdict(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if ns.Verdict == nil || ns.Verdict.Campaign != "governor" || len(ns.Promotions) != 1 {
		t.Fatalf("state = %+v", ns)
	}
	if p, ok := ns.promotionOf(NousPromoteTargetIssue); !ok || p.Number != 3 {
		t.Fatalf("promotionOf = %+v %v", p, ok)
	}
	if _, ok := ns.promotionOf(NousPromoteTargetSpektacularRun); ok {
		t.Fatalf("unexpected run promotion")
	}

	// Verdict withdrawn -> cleared; store errors keep the last good copy.
	vs.ok = false
	if err := ns.SyncVerdict(); err != nil || ns.Verdict != nil {
		t.Fatalf("withdrawn: err=%v verdict=%v", err, ns.Verdict)
	}
	vs.ok, vs.err = true, errors.New("bad json")
	ps.listErr = errors.New("unreadable")
	ns.Promotions = nil
	err := ns.SyncVerdict()
	if err == nil || !strings.Contains(err.Error(), "bad json") || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("err = %v", err)
	}
	if ns.Verdict != nil || ns.Promotions != nil {
		t.Fatalf("errors must not overwrite state: %+v", ns)
	}
	if _, ok := ns.promotionOf(NousPromoteTargetIssue); ok {
		t.Fatalf("promotionOf without verdict must be false")
	}
}

func TestSyncNousVerdictLogsStoreErrors(t *testing.T) {
	s, _ := apiServer(t)
	s.syncNousVerdict() // no Nous: no-op
	vs := &fakeVerdictStore{err: errors.New("corrupt")}
	s.deps.Nous = &NousState{VerdictStore: vs}
	s.syncNousVerdict()
	if vs.loads != 1 {
		t.Fatalf("loads = %d", vs.loads)
	}
	if rec := doGet(s, "/api/nous/status"); rec.Code != http.StatusOK {
		t.Fatalf("status with corrupt verdict = %d", rec.Code)
	}
}

func TestNousVerdictIssueContentNotMeasured(t *testing.T) {
	v := sampleVerdict()
	v.MeasuredAgainstBaseline = false
	v.Campaign = ""
	v.Recommendation = "custom_call"
	v.Citations, v.Caveats, v.Summary, v.Report = nil, nil, "", ""
	title, body := nousVerdictIssueContent(v, "alice")
	if title != nousIssueTitlePrefix+": custom call ("+nousIssueUnnamedCampaign+")" {
		t.Fatalf("title = %q", title)
	}
	if !strings.Contains(body, nousIssueNotMeasuredLabel) || strings.Contains(body, "vs.") || strings.Contains(body, "### Evidence") || !strings.Contains(body, "by alice") {
		t.Fatalf("body = %s", body)
	}
	for _, tc := range []struct{ in, want string }{
		{NousVerdictDeploy, "deploy"},
		{NousVerdictWithCaveats, "deploy with caveats"},
		{NousVerdictFallBack, "fall back to baseline"},
	} {
		if got := nousVerdictLabel(tc.in); got != tc.want {
			t.Fatalf("label(%s) = %q", tc.in, got)
		}
	}
	for _, target := range promoteTargetsForTest() {
		if !ValidNousPromoteTarget(target) {
			t.Fatalf("%q should be valid", target)
		}
	}
	if nousPromoteStatus(errors.New("plain")) != http.StatusInternalServerError {
		t.Fatalf("plain errors map to 500")
	}
}

// promoteTargetsForTest lists the known targets in UI order.
func promoteTargetsForTest() []string {
	return []string{NousPromoteTargetIssue, NousPromoteTargetSpektacularRun}
}

func TestNousPromoteFilerFallsBackToGHClient(t *testing.T) {
	s, _ := apiServer(t)
	s.nousIssueFiler = nil
	if s.deps.GHClient == nil {
		if s.nousIssueFilerFor() != nil {
			t.Fatalf("nil client must give nil filer")
		}
		return
	}
	if s.nousIssueFilerFor() == nil {
		t.Fatalf("GHClient should back the filer")
	}
}
