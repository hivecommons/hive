package dashboard

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/fixqueue"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15038: a PR waiting on a contributor's clanker offers
// the operator "Adopt now" (forced adoption, no grace / presence / policy
// wait) and "Reroute to another clanker" (the follow-up work goes to an
// online clanker's fix queue, falling back to Adopt now when it is not
// picked up within governor.reroute.pickup_timeout).

func takeoverServer(t *testing.T, mock *repoItemDecisionMock) (*Server, *stagelabels.Store) {
	t.Helper()
	srv := newRepoItemDecisionServer(t, mock)
	store := stagelabels.NewStore("", nil)
	srv.stageStore = store
	srv.adoptNowWait = 50 * time.Millisecond
	srv.contributeHub.fixAckGrace = 10 * time.Millisecond
	return srv, store
}

func recForced(rec stagelabels.Record) bool {
	_, ok := rec.AdoptionForced()
	return ok
}

func decodeDecision(t *testing.T, body string) repoItemDecisionResponse {
	t.Helper()
	var out repoItemDecisionResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

func TestRepoItemAdoptNowForcesAdoptionAndAudits(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true}
	srv, store := takeoverServer(t, mock)
	key := stagelabels.Key("testorg/testrepo", 8)
	var nudged []int
	srv.deps.SweepNudge = func(repo string, number int) {
		nudged = append(nudged, number)
		// The nudged sweep adopts: the engine records hive's copy.
		store.MarkAdopted(key, "feedfacefeedfacefeedfacefeedfacefeedface", 90, time.Now())
	}

	w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"adopt","force":true,"type":"pr","comment":"CI has been red for a day"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("adopt now: code=%d body=%s", w.Code, w.Body.String())
	}
	out := decodeDecision(t, w.Body.String())
	if out.Status != "adopted-now" || out.Result == nil || out.Result.Action != rowActionAdopted {
		t.Fatalf("response = %+v", out)
	}
	if out.Result.Outcome != "Adopted → #90 (hive's copy), contributor PR will close when #90 merges." {
		t.Fatalf("toast = %q", out.Result.Outcome)
	}
	if nudged == nil || nudged[0] != 8 {
		t.Fatalf("sweep nudged = %v", nudged)
	}
	if len(mock.comments) != 1 || !strings.HasPrefix(mock.comments[0], "<!-- hive:adoption-requested by=writer force=true -->") || !strings.Contains(mock.comments[0], "> CI has been red for a day") {
		t.Fatalf("comment = %q", mock.comments)
	}
	if !slices.Contains(mock.added, delegatedecision.LabelAdoptSupersede) {
		t.Fatalf("labels added = %v", mock.added)
	}
	acts := decisionAuditActions(srv)
	if !slices.Contains(acts, auditRepoItemDecisionAdoptNow) {
		t.Fatalf("audit = %v", acts)
	}
	for _, e := range srv.GetAudit().Recent(20) {
		if e.Action == auditRepoItemDecisionAdoptNow && !strings.Contains(e.Detail, "action=adopt-now") {
			t.Fatalf("audit detail %q lacks action=adopt-now", e.Detail)
		}
	}
	// MarkAdopted settles the forced mark once hive's copy exists.
	rec, _ := store.Get(key)
	if rec.AdoptedBy != 90 || recForced(rec) {
		t.Fatalf("record = %+v", rec)
	}

	// A second Adopt now on the adopted head is a no-op toast.
	w = postRepoItemDecision(t, srv, 8, "writer", `{"decision":"adopt","force":true,"type":"pr"}`)
	out = decodeDecision(t, w.Body.String())
	if w.Code != http.StatusOK || out.Status != "already-adopted" || out.Result.Action != rowActionNoop {
		t.Fatalf("repeat = %d %+v", w.Code, out)
	}
}

func TestRepoItemAdoptNowWithoutSweepIsQueuedForm(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true}
	srv, store := takeoverServer(t, mock)
	srv.deps.SweepNudge, srv.deps.WebhookWake = nil, nil

	w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"adopt","force":true,"type":"pr"}`)
	out := decodeDecision(t, w.Body.String())
	if w.Code != http.StatusOK || out.Status != repoItemDecisionStatusAdoptionQueued || !strings.HasPrefix(out.Result.Outcome, "Adopting… (queued)") || out.Result.Action != rowActionAdopted {
		t.Fatalf("code=%d out=%+v", w.Code, out)
	}
	if strings.Contains(out.Result.Outcome, "pass now running") {
		t.Fatalf("queued adoption promised the running pass: %q", out.Result.Outcome)
	}
	rec, _ := store.Get(stagelabels.Key("testorg/testrepo", 8))
	if rec.AdoptForcedBy != "writer" || !recForced(rec) {
		t.Fatalf("forced mark missing: %+v", rec)
	}
}

func TestRepoItemAdoptNowGuards(t *testing.T) {
	// A reader cannot force adoption.
	srv, _ := takeoverServer(t, &repoItemDecisionMock{prForeign: true})
	if w := postRepoItemDecision(t, srv, 8, "reader", `{"decision":"adopt","force":true,"type":"pr"}`); w.Code != http.StatusForbidden {
		t.Fatalf("reader: code=%d body=%s", w.Code, w.Body.String())
	}
	// Hive's own PR is never adopted.
	srv, _ = takeoverServer(t, &repoItemDecisionMock{})
	if w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"adopt","force":true,"type":"pr"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("hive-authored: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRepoItemRerouteNoneOnlineIsConflict(t *testing.T) {
	srv, _ := takeoverServer(t, &repoItemDecisionMock{prForeign: true})
	w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"reroute","to":"any","type":"pr"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), RerouteNoneOnlineReason) {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	// Naming an offline clanker says so too.
	w = postRepoItemDecision(t, srv, 8, "writer", `{"decision":"reroute","to":"bee2","type":"pr"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "@bee2 is not online") {
		t.Fatalf("named offline: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRepoItemRerouteDispatchesFixPRAndMarks(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true}
	srv, store := takeoverServer(t, mock)
	hub := srv.contributeHub
	fixConn(hub, "bee2", fixCaps(capFixPRInterrupt), time.Now())
	got := captureFixDispatch(hub)
	var fallback func()
	srv.rerouteAfterFunc = func(d time.Duration, f func()) {
		if d != config.DefaultReroutePickupTimeout {
			t.Errorf("pickup window = %s, want %s", d, config.DefaultReroutePickupTimeout)
		}
		fallback = f
	}
	key := stagelabels.Key("testorg/testrepo", 8)
	store.Observe(key, stagelabels.ReturnedToContributor, "check-failing:lint")

	w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"reroute","to":"any","type":"pr","comment":"please take it"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("reroute: code=%d body=%s", w.Code, w.Body.String())
	}
	out := decodeDecision(t, w.Body.String())
	if out.Status != "rerouted" || out.Result == nil || out.Result.Action != rowActionRerouted || !strings.HasPrefix(out.Result.Outcome, "Rerouted → @bee2 (dispatched now") {
		t.Fatalf("response = %+v", out)
	}
	if !strings.Contains(out.Result.Next, "within 30m") {
		t.Fatalf("next = %q", out.Result.Next)
	}
	// Dispatch rides the existing fix-pr task_assign to the target relay.
	if len(*got) != 1 || (*got)[0].Kind != taskKindFixPR || len((*got)[0].FixPRs) != 1 {
		t.Fatalf("dispatched = %+v", *got)
	}
	fp := (*got)[0].FixPRs[0]
	if fp.Repo != "testorg/testrepo" || fp.Number != 8 || fp.RerouteFrom != "someone" || fp.RerouteBy != "writer" || !slices.Equal(fp.FailingChecks, []string{"lint"}) || !strings.Contains(fp.RerouteReason, "please take it") {
		t.Fatalf("fix-pr = %+v", fp)
	}
	if len(mock.comments) != 1 || !strings.HasPrefix(mock.comments[0], "<!-- hive:rerouted to=@bee2 by=@writer reason=check-failing:lint — please take it -->") || !strings.Contains(mock.comments[0], "@someone — @writer has rerouted") {
		t.Fatalf("comment = %q", mock.comments)
	}
	rec, _ := store.Get(key)
	if rec.Rerouted("feedfacefeedfacefeedfacefeedfacefeedface") != "bee2" || rec.RerouteBy != "writer" {
		t.Fatalf("record = %+v", rec)
	}
	if !slices.Contains(decisionAuditActions(srv), auditRepoItemDecisionReroute) {
		t.Fatalf("audit = %v", decisionAuditActions(srv))
	}
	if fallback == nil {
		t.Fatal("pickup timer not armed")
	}
	// The target's relay has the task in flight: the window lapsing does
	// not fall back.
	if hub.ReroutePickedUp("bee2", "testorg/testrepo", 8) != true {
		t.Fatal("dispatched reroute not in flight")
	}
	if srv.reroutePickupLapsed("testorg/testrepo", 8, "feedfacefeedfacefeedfacefeedfacefeedface", "bee2", "writer") {
		t.Fatal("fell back while the target held the task")
	}
	if rec, _ := store.Get(key); recForced(rec) {
		t.Fatalf("forced after pickup: %+v", rec)
	}
}

func TestRepoItemReroutePickupLapseFallsBackToAdoptNow(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true}
	srv, store := takeoverServer(t, mock)
	hub := srv.contributeHub
	hub.fixDispatched = func(_ *ContributorConnection, _ WSMessage) {}
	// bee2 is online but already holds a task: the reroute queues behind it.
	c := fixConn(hub, "bee2", fixCaps(capFixPRInterrupt), time.Now())
	c.mu.Lock()
	c.currentTask = &WSTaskAssign{TaskID: "busy"}
	c.mu.Unlock()
	hub.fixQueue().Enqueue("bee2", fixqueue.Item{Repo: "org/other", Number: 1, HeadSHA: "x"})
	hub.rememberFixOwner("bee2", fixqueue.Key("org/other", 1))
	hub.dispatchFixes("bee2", []*ContributorConnection{c}, false)
	var fallback func()
	srv.rerouteAfterFunc = func(_ time.Duration, f func()) { fallback = f }
	var nudged []int
	srv.deps.SweepNudge = func(_ string, n int) { nudged = append(nudged, n) }
	key := stagelabels.Key("testorg/testrepo", 8)

	w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"reroute","to":"bee2","type":"pr"}`)
	out := decodeDecision(t, w.Body.String())
	if w.Code != http.StatusOK || !strings.Contains(out.Result.Outcome, "Rerouted → @bee2 (queued") {
		t.Fatalf("code=%d out=%+v", w.Code, out)
	}
	if !hub.RerouteQueued("bee2", "testorg/testrepo", 8) || hub.ReroutePickedUp("bee2", "testorg/testrepo", 8) {
		t.Fatalf("queue state: queued=%v inflight=%v", hub.RerouteQueued("bee2", "testorg/testrepo", 8), hub.ReroutePickedUp("bee2", "testorg/testrepo", 8))
	}

	mock.mu.Lock()
	mock.comments = nil
	mock.mu.Unlock()
	fallback()

	rec, _ := store.Get(key)
	if rec.RerouteTo != "" || rec.RerouteFallbackAt.IsZero() || rec.AdoptForcedBy != "writer" || !recForced(rec) {
		t.Fatalf("record after lapse = %+v", rec)
	}
	if hub.RerouteQueued("bee2", "testorg/testrepo", 8) {
		t.Fatal("lapsed item still queued on bee2")
	}
	if len(mock.comments) != 1 || !strings.HasPrefix(mock.comments[0], "<!-- hive:rerouted to=@bee2 by=@writer lapsed=true -->") {
		t.Fatalf("fallback comment = %q", mock.comments)
	}
	if !slices.Contains(mock.added, delegatedecision.LabelAdoptSupersede) {
		t.Fatalf("labels = %v", mock.added)
	}
	if !slices.Contains(decisionAuditActions(srv), auditRepoItemRerouteFallback) {
		t.Fatalf("audit = %v", decisionAuditActions(srv))
	}
	if nudged == nil || nudged[0] != 8 {
		t.Fatalf("sweep not nudged: %v", nudged)
	}
	found := false
	srv.systemAlertsMu.Lock()
	alerts := append([]SystemAlert(nil), srv.systemAlerts...)
	srv.systemAlertsMu.Unlock()
	for _, a := range alerts {
		if strings.HasPrefix(a.ID, repoItemRerouteFallbackAlert) && strings.Contains(a.Message, "falling back to Adopt now") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no fallback alert: %+v", alerts)
	}
	// Running the lapse again finds nothing to fall back from.
	if srv.reroutePickupLapsed("testorg/testrepo", 8, "feedfacefeedfacefeedfacefeedfacefeedface", "bee2", "writer") {
		t.Fatal("second lapse fell back again")
	}
}

func TestRerouteCandidatesExcludeAuthorAndOffline(t *testing.T) {
	hub, _ := covK2Hub(t)
	now := time.Now()
	fixConn(hub, "bee2", fixCaps(capFixPRInterrupt), now)
	fixConn(hub, "someone", fixCaps(capFixPRInterrupt), now)
	fixConn(hub, "stale", fixCaps(capFixPRInterrupt), now.Add(-2*time.Hour))
	cands := hub.RerouteCandidates(now, "someone")
	if len(cands) != 1 || cands[0].Login != "bee2" || !cands[0].Capable {
		t.Fatalf("candidates = %+v", cands)
	}
	if _, err := hub.Reroute(RerouteRequest{Repo: "o/r", Number: 1, Author: "someone", To: "someone"}); err == nil || !strings.Contains(err.Error(), "is the PR's author") {
		t.Fatalf("reroute to author err = %v", err)
	}
}

// Tracker: the row carries takeover{} with the menu's facts.

func TestStageTrackerTakeoverOffersAdoptNowAndReroute(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, status := adoptionFixture(t, now, config.AdoptionMaintainerPolicyNever, offlinePresence(now.Add(-26*time.Hour)))
	since := now.Add(-time.Hour)
	s.rerouteCandidates = func(author string) []RerouteCandidate {
		if author != "hanthor" {
			t.Errorf("candidates asked excluding %q", author)
		}
		return []RerouteCandidate{{Login: "bee2", HiveName: "bee2-hive", OnlineSince: &since, Load: 1, Capable: true}}
	}
	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524)
	tk := it.Takeover
	if tk == nil || !tk.AdoptNow || !tk.Reroute || tk.Author != "hanthor" || tk.AuthorOnline || len(tk.Clankers) != 1 || tk.Clankers[0].Login != "bee2" {
		t.Fatalf("takeover = %+v", tk)
	}
	if !tk.GraceElapsed {
		t.Fatalf("26h past the nudge is past the offline grace: %+v", tk)
	}
	// Grace elapsed under never: the popover asks a person (Adopt now /
	// Reroute lead; Hold closes).
	d := it.DecisionNeeded
	if d == nil || len(d.Options) != 3 || d.Options[0].Action != decisionActionAdoptNow || !d.Options[0].Enabled || d.Options[1].Action != decisionActionReroute || !d.Options[1].Enabled {
		t.Fatalf("decision_needed = %+v", d)
	}
	if !strings.Contains(d.Question, "@hanthor has been offline for 26h") || !strings.Contains(d.Question, "reroute") {
		t.Fatalf("question = %q", d.Question)
	}
	raw, _ := json.Marshal(it)
	for _, want := range []string{`"takeover":{`, `"adopt_now":true`, `"reroute":true`, `"clankers":[{"login":"bee2"`, `"grace_elapsed":true`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("payload lacks %s: %s", want, raw)
		}
	}
}

// Approve, then Adopt now (hive#15312): the moment Adopt now is recorded
// the row is in its acted state — "Adopting… (queued by @x)", no "not
// adopted: a maintainer approved this head", no Adopt now action, no
// decision popover asking the person to take it over again.
func TestStageTrackerTakeoverQueuedAfterAdoptNow(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, status := adoptionFixture(t, now, config.AdoptionMaintainerPolicyNever, offlinePresence(now.Add(-26*time.Hour)))
	s.rerouteCandidates = func(string) []RerouteCandidate { return []RerouteCandidate{{Login: "bee2", Capable: true}} }
	key := stagelabels.Key("hivecommons/hive", 14524)
	store.NoteAdmission(key, "feed", stagelabels.OperatorApprovalAdmission("andan02"), now.Add(-10*time.Minute))
	store.NoteAdoptionBlocked(key, "feed", stagelabels.AdoptBlockedOperatorAdmission, now.Add(-9*time.Minute))
	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524)
	if it.Adoption == nil || it.Adoption.BlockedBy != stagelabels.AdoptBlockedOperatorAdmission || !strings.Contains(it.Adoption.Text, "a maintainer approved this head") || it.Takeover == nil || !it.Takeover.AdoptNow {
		t.Fatalf("precondition (approved, blocked, Adopt now offered): adoption=%+v takeover=%+v", it.Adoption, it.Takeover)
	}

	store.NoteAdoptionForced(key, "andan02", now.Add(-time.Minute))
	it = trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524)
	a, tk := it.Adoption, it.Takeover
	if a == nil || a.QueuedBy != "andan02" || a.BlockedBy != "" || a.Action != "" || !strings.HasPrefix(a.Text, "Adopting… (queued by @andan02") {
		t.Fatalf("adoption after Adopt now = %+v", a)
	}
	if tk == nil || tk.AdoptNow || tk.AdoptNowReason != takeoverAdoptNowQueued || tk.ForcedBy != "andan02" || !tk.Reroute {
		t.Fatalf("takeover after Adopt now = %+v", tk)
	}
	if it.DecisionNeeded != nil {
		t.Fatalf("queued adoption still asks a person: %+v", it.DecisionNeeded)
	}
	if strings.Contains(it.Caption, "not adopted") {
		t.Fatalf("caption still says not adopted: %q", it.Caption)
	}
	for _, o := range takeoverDecisionOptions(tk, false) {
		if o.Action == decisionActionAdoptNow {
			t.Fatalf("Adopt now still offered in the popover: %+v", o)
		}
	}
	raw, _ := json.Marshal(it)
	for _, want := range []string{`"queued_by":"andan02"`, `"forced_by":"andan02"`, `"adopt_now":false`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("payload lacks %s: %s", want, raw)
		}
	}
}

// A hold added after Adopt now: the engine stops at the hold without
// consuming the request, so the row must not keep saying "queued".
func TestStageTrackerTakeoverHoldAfterAdoptNowNotQueued(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, status := adoptionFixture(t, now, config.AdoptionMaintainerPolicyNever, offlinePresence(now.Add(-26*time.Hour)))
	s.rerouteCandidates = func(string) []RerouteCandidate { return []RerouteCandidate{{Login: "bee2", Capable: true}} }
	store.NoteAdoptionForced(stagelabels.Key("hivecommons/hive", 14524), "andan02", now.Add(-time.Minute))
	pr := status.Repos[0].OpenPrs[0].(FrontendPR)
	pr.Labels = append(pr.Labels, "hold")
	status.Repos[0].OpenPrs[0] = pr
	s.UpdateStatus(status)
	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524)
	if tk := it.Takeover; tk == nil || tk.AdoptNow || tk.AdoptNowReason != takeoverAdoptNowDisabledHold {
		t.Fatalf("takeover = %+v", it.Takeover)
	}
	if a := it.Adoption; a != nil && (a.QueuedBy != "" || strings.HasPrefix(a.Text, "Adopting…")) {
		t.Fatalf("adoption still queued under a hold: %+v", a)
	}
}

func TestStageTrackerTakeoverDisabledReasons(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, status := adoptionFixture(t, now, config.AdoptionMaintainerPolicyNever, offlinePresence(now.Add(-26*time.Hour)))
	s.rerouteCandidates = func(string) []RerouteCandidate { return nil }
	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524)
	if tk := it.Takeover; tk == nil || !tk.AdoptNow || tk.Reroute || tk.RerouteReason != RerouteNoneOnlineReason {
		t.Fatalf("no clankers: %+v", it.Takeover)
	}
	if d := it.DecisionNeeded; d == nil || d.Options[1].Enabled || d.Options[1].ReasonIfDisabled != RerouteNoneOnlineReason {
		t.Fatalf("reroute option = %+v", d)
	}

	// A hold disables both with "release the hold first".
	pr := status.Repos[0].OpenPrs[0].(FrontendPR)
	pr.Labels = append(pr.Labels, "hold")
	status.Repos[0].OpenPrs[0] = pr
	s.UpdateStatus(status)
	s.rerouteCandidates = func(string) []RerouteCandidate { return []RerouteCandidate{{Login: "bee2", Capable: true}} }
	it = trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524)
	if tk := it.Takeover; tk == nil || tk.AdoptNow || tk.Reroute || tk.AdoptNowReason != takeoverAdoptNowDisabledHold || tk.RerouteReason != takeoverAdoptNowDisabledHold {
		t.Fatalf("held: %+v", it.Takeover)
	}
}

func TestStageTrackerTakeoverShowsReroutePill(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, status := adoptionFixture(t, now, config.AdoptionMaintainerPolicyNever, offlinePresence(now.Add(-26*time.Hour)))
	s.rerouteCandidates = func(string) []RerouteCandidate { return []RerouteCandidate{{Login: "bee2", Capable: true}} }
	store.NoteReroute(stagelabels.Key("hivecommons/hive", 14524), "feed", "bee2", "andan02", "check-failing:ratchet", now.Add(-5*time.Minute))
	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524)
	r := it.Takeover.Rerouted
	if r == nil || r.To != "bee2" || r.By != "andan02" || !r.TargetOnline || r.Text != "Rerouted to @bee2 by @andan02 — check-failing:ratchet" {
		t.Fatalf("rerouted = %+v", r)
	}
	if !strings.Contains(it.Caption, "Rerouted to @bee2") {
		t.Fatalf("caption = %q", it.Caption)
	}
}

// Store: the marks the two actions leave.

func TestStageStoreForcedAndRerouteMarks(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	store := stagelabels.NewStore("", func() time.Time { return now })
	key := stagelabels.Key("o/r", 1)
	store.Observe(key, stagelabels.ReturnedToContributor, "x")
	store.NoteReroute(key, "h1", "bee2", "op", "why", now)
	rec, _ := store.Get(key)
	if rec.Rerouted("h1") != "bee2" || rec.Rerouted("h2") != "" || rec.RerouteAt != now {
		t.Fatalf("after reroute: %+v", rec)
	}
	// Adopt now supersedes a pending reroute.
	store.NoteAdoptionForced(key, "op", now.Add(time.Minute))
	rec, _ = store.Get(key)
	if rec.Rerouted("h1") != "" || !recForced(rec) || rec.AdoptForcedBy != "op" {
		t.Fatalf("after force: %+v", rec)
	}
	store.ClearAdoptionForced(key)
	if rec, _ = store.Get(key); recForced(rec) {
		t.Fatalf("clear left %+v", rec)
	}
	store.NoteReroute(key, "h1", "bee2", "op", "why", now)
	store.ClearReroute(key, true, now.Add(2*time.Minute))
	rec, _ = store.Get(key)
	if rec.RerouteTo != "" || !rec.RerouteFallbackAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("after fallback clear: %+v", rec)
	}
	// Adoption settles everything.
	store.NoteAdoptionForced(key, "op", now)
	store.MarkAdopted(key, "h1", 9, now)
	rec, _ = store.Get(key)
	if recForced(rec) || rec.RerouteTo != "" || rec.AdoptedBy != 9 {
		t.Fatalf("after adopted: %+v", rec)
	}
}

// Delegated "Let hive decide": grace elapsed → adopt now.

func TestDelegatedDecisionGraceElapsedAdoptsNow(t *testing.T) {
	d := delegatedecision.Decide(delegatedecision.Input{Kind: delegatedecision.KindPR, AdoptionGraceElapsed: true})
	if d.Rule != delegatedecision.RuleAdoptNow || d.Verdict != delegatedecision.VerdictAdoptSupersede || !slices.ContainsFunc(d.Actions, func(a delegatedecision.Action) bool { return a.Kind == delegatedecision.ActionAdoptSupersede }) {
		t.Fatalf("decision = %+v", d)
	}
}
