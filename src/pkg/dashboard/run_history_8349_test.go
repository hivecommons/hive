package dashboard

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/timeline"
)

const runHistoryTestToken = "ghp_abcdefghijklmnopqrstuvwxyz1234567890"

// TestStatusRunHistoryIncludesRunsAndScrubsTitles proves the status/snapshot
// payload carries the bounded run history (#8349): the live lease lands under
// Active, the finished journey under Recent, and neither title keeps the
// token that was fed in.
func TestStatusRunHistoryIncludesRunsAndScrubsTitles(t *testing.T) {
	s, _ := runsTestServer(t)
	now := time.Now()
	const (
		activeKey   = "myorg/repo1#834901"
		finishedKey = "myorg/repo1#834902"
	)
	if err := s.contributeHub.recordLeaseForKeyStage("alice", "task-8349", "myorg/repo1", 834901, activeKey, "contributor", StagePlan, 3, now); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	s.contributeHub.mu.Lock()
	s.contributeHub.connections["conn-8349"] = &ContributorConnection{
		profile:        &ContributorProfile{ContributorID: "alice"},
		currentTask:    &WSTaskAssign{TaskID: "task-8349", Repo: "myorg/repo1", Number: 834901, Title: "active run " + runHistoryTestToken},
		taskAssignedAt: now,
	}
	s.contributeHub.mu.Unlock()
	s.LifecycleTimeline().Record(timeline.Event{
		IssueRef: finishedKey,
		Kind:     timeline.KindStageCompleted,
		Agent:    "alice",
		At:       now.Add(-time.Hour).UnixMilli(),
		Attrs:    map[string]string{"stage_to": StagePlan, "gen": "7", "title": "finished run " + runHistoryTestToken},
	})

	status := &StatusPayload{}
	s.UpdateStatus(status)
	if status.RunHistory == nil {
		t.Fatal("status runHistory = nil, want populated history")
	}
	if status.RunHistory.Limit != SnapshotRunHistoryLimit {
		t.Fatalf("limit = %d, want %d", status.RunHistory.Limit, SnapshotRunHistoryLimit)
	}
	var active, recent *RunHistoryEntry
	for i := range status.RunHistory.Active {
		if status.RunHistory.Active[i].Key == activeKey {
			active = &status.RunHistory.Active[i]
		}
	}
	for i := range status.RunHistory.Recent {
		if status.RunHistory.Recent[i].Key == finishedKey {
			recent = &status.RunHistory.Recent[i]
		}
		if status.RunHistory.Recent[i].Key == activeKey {
			t.Fatal("live run was also listed as finished history")
		}
	}
	if active == nil {
		t.Fatalf("active run missing: %+v", status.RunHistory.Active)
	}
	if active.Outcome != RunOutcomeActive || active.Stage != StagePlan || active.Gen != 3 || active.WaitingOn == "" {
		t.Fatalf("active entry = %+v", *active)
	}
	if recent == nil {
		t.Fatalf("finished run missing: %+v", status.RunHistory.Recent)
	}
	if recent.Outcome != RunOutcomeCompleted || recent.Stage != StagePlan || recent.Gen != 7 || recent.WaitingOn != RunWaitingOnNone || recent.CompletedAt == "" {
		t.Fatalf("recent entry = %+v", *recent)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"runHistory"`) {
		t.Fatalf("status json lacks runHistory: %s", raw)
	}
	if strings.Contains(string(raw), runHistoryTestToken) {
		t.Fatalf("status json leaked the token: active=%q recent=%q", active.Title, recent.Title)
	}
	if !strings.HasPrefix(active.Title, "active run ") || !strings.HasPrefix(recent.Title, "finished run ") {
		t.Fatalf("scrubbing ate the non-secret title text: active=%q recent=%q", active.Title, recent.Title)
	}
}

// TestRunHistoryAbsentWhenRegistryUnavailable: a spoke that cannot project
// runs omits the field entirely so consumers render unknown, never zero.
func TestRunHistoryAbsentWhenRegistryUnavailable(t *testing.T) {
	srv := &Server{}
	if got := srv.StatusRunHistory(); got != nil {
		t.Fatalf("StatusRunHistory without a registry = %+v, want nil", got)
	}
	raw, err := json.Marshal(&StatusPayload{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"runHistory"`) {
		t.Fatalf("empty status json carries runHistory: %s", raw)
	}
}

// TestRunHistoryBoundedAndNewestFirst: history never exceeds the limit and is
// ordered by completion time, newest first; merged journeys report merged.
func TestRunHistoryBoundedAndNewestFirst(t *testing.T) {
	base := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	journeys := make([]timeline.Journey, 0, 5)
	for i := 0; i < 5; i++ {
		at := base.Add(time.Duration(i) * time.Minute).UnixMilli()
		j := timeline.Journey{
			Ref:     fmt.Sprintf("org/repo#%d", i),
			Current: timeline.KindStageCompleted,
			Stages: map[timeline.Kind]*timeline.Stage{
				timeline.KindStageCompleted: {FirstAt: at, LastAt: at, Count: 1, Agent: "a", Attrs: map[string]string{"stage_to": StageImplement}},
			},
		}
		if i == 4 {
			j.Current = timeline.KindMerged
		}
		journeys = append(journeys, j)
	}
	// A journey with no stage completion is not a run and must not appear.
	journeys = append(journeys, timeline.Journey{Ref: "org/repo#99", Stages: map[timeline.Kind]*timeline.Stage{timeline.KindMerged: {LastAt: 1}}})

	const limit = 3
	got := runHistoryFromProjection(nil, journeys, limit)
	if len(got.Recent) != limit || got.Limit != limit {
		t.Fatalf("recent = %d entries (limit %d), want %d", len(got.Recent), got.Limit, limit)
	}
	if got.Recent[0].Key != "org/repo#4" || got.Recent[0].Outcome != RunOutcomeMerged {
		t.Fatalf("newest entry = %+v, want org/repo#4 merged", got.Recent[0])
	}
	if got.Recent[2].Key != "org/repo#2" || got.Recent[2].Outcome != RunOutcomeCompleted || got.Recent[2].Repo != "org/repo" {
		t.Fatalf("third entry = %+v", got.Recent[2])
	}
	if got.Active == nil || len(got.Active) != 0 {
		t.Fatalf("active = %#v, want empty non-nil", got.Active)
	}
}

// TestStageAdvanceRecordsScrubbedTitle: the stage_completed event the lease
// registry emits carries the task title already scrubbed, so the timeline
// itself never retains a credential-shaped title.
func TestStageAdvanceRecordsScrubbedTitle(t *testing.T) {
	s, _ := runsTestServer(t)
	hub := s.contributeHub
	now := time.Now()
	const key = "myorg/repo1#834903"
	if err := hub.recordLeaseForKeyStage("c-title", "task-title", "myorg/repo1", 834903, key, "contributor", StageSpec, 30, now); err != nil {
		t.Fatalf("record staged lease: %v", err)
	}
	hub.mu.Lock()
	hub.connections["conn-title"] = &ContributorConnection{
		profile:        &ContributorProfile{ContributorID: "c-title"},
		currentTask:    &WSTaskAssign{TaskID: "task-title", Repo: "myorg/repo1", Number: 834903, Title: "spec " + runHistoryTestToken},
		taskAssignedAt: now,
	}
	hub.mu.Unlock()
	if _, err := hub.advanceLeaseStage("c-title", "task-title", StagePlan, now.Add(time.Minute)); err != nil {
		t.Fatalf("advance stage: %v", err)
	}
	j, ok := s.LifecycleTimeline().Journey(key)
	if !ok || j.Stages[timeline.KindStageCompleted] == nil {
		t.Fatalf("no stage_completed journey for %s", key)
	}
	st := j.Stages[timeline.KindStageCompleted]
	if st.Agent != "c-title" {
		t.Fatalf("stage agent = %q, want lease identity", st.Agent)
	}
	title := st.Attrs["title"]
	if title == "" || strings.Contains(title, runHistoryTestToken) || !strings.HasPrefix(title, "spec ") {
		t.Fatalf("recorded title = %q, want scrubbed title", title)
	}
}

// TestLeaderboardForHubCreditsStageCompletions: stage completions on the
// timeline are credited to the contributor on the lease (by contributor id or
// username), and a contributor with none reports a real zero, not nil.
func TestLeaderboardForHubCreditsStageCompletions(t *testing.T) {
	setupContributeEnv(t)
	for _, p := range []*ContributorProfile{
		{GitHubUsername: "alice-8349", ContributorID: "c-alice-8349", TrustTier: "trusted", TasksCompleted: 1},
		{GitHubUsername: "bob-8349", ContributorID: "c-bob-8349", TrustTier: "newcomer"},
	} {
		if err := saveContributorProfile(p); err != nil {
			t.Fatalf("seed %s: %v", p.GitHubUsername, err)
		}
	}
	s, _ := runsTestServer(t)
	base := time.Now().Add(-2 * time.Hour)
	for i, agent := range []string{"c-alice-8349", "c-alice-8349", "alice-8349"} {
		s.LifecycleTimeline().Record(timeline.Event{
			IssueRef: fmt.Sprintf("myorg/repo1#83491%d", i),
			Kind:     timeline.KindStageCompleted,
			Agent:    agent,
			At:       base.Add(time.Duration(i) * time.Minute).UnixMilli(),
			Attrs:    map[string]string{"stage_to": StagePlan},
		})
	}

	byUser := map[string]LeaderboardEntry{}
	for _, e := range s.LeaderboardForHub() {
		byUser[e.GitHubUsername] = e
	}
	alice, ok := byUser["alice-8349"]
	if !ok || alice.StagesCompleted == nil || *alice.StagesCompleted != 3 {
		t.Fatalf("alice = %+v (found %v), want stages_completed 3", alice, ok)
	}
	if alice.TasksCompleted != 1 {
		t.Fatalf("alice tasks_completed = %d, want ranking input unchanged", alice.TasksCompleted)
	}
	bob, ok := byUser["bob-8349"]
	if !ok || bob.StagesCompleted == nil || *bob.StagesCompleted != 0 {
		t.Fatalf("bob = %+v (found %v), want a reported zero", bob, ok)
	}
	raw, err := json.Marshal(alice)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"stages_completed":3`) {
		t.Fatalf("leaderboard json = %s", raw)
	}
}
