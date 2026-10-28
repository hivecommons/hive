package dashboard

import (
	"testing"
	"time"
)

func TestStageTrackerActorEvidence(t *testing.T) {
	for _, tc := range []struct {
		name               string
		item               StageTrackerItem
		agents             []FrontendAgent
		kind, login, state string
	}{
		{name: "conflicting nightly bot", item: StageTrackerItem{Kind: "pr", Stage: 70, Author: "github-actions[bot]", AuthorKind: "bot", Modifiers: []string{"needs-rebase"}}, kind: "none", login: "github-actions[bot]", state: "Conflicting · bot PR"},
		{name: "idle hive fixing classification", item: StageTrackerItem{Kind: "pr", Stage: 70, AuthorKind: "hive"}, kind: "none", state: "Waiting for fixes"},
		{name: "human return", item: StageTrackerItem{Kind: "pr", Stage: 71, AuthorKind: "human", Author: "sam"}, kind: "human", login: "sam"},
		{name: "contributor return", item: StageTrackerItem{Kind: "pr", Stage: 72, AuthorKind: "clanker", Author: "relay"}, kind: "clanker", login: "relay"},
		{name: "decision gate", item: StageTrackerItem{Kind: "pr", Stage: 85}, kind: "human"},
		{name: "unknown", item: StageTrackerItem{Kind: "pr", Stage: 30}, kind: "none"},
		{name: "current worker", item: StageTrackerItem{Repo: "hivecommons/hive", Number: 14921, Kind: "pr", Stage: 70}, agents: []FrontendAgent{{Name: "fixer", Busy: "working", LastAction: &FrontendAgentLastAction{Repo: "hive", Number: 14921, Current: true, At: "2026-10-28T12:00:00Z"}}}, kind: "hive", login: "fixer"},
		{name: "stale round", item: StageTrackerItem{Repo: "hivecommons/hive", Number: 14921, Kind: "pr", Stage: 70}, agents: []FrontendAgent{{Name: "fixer", Busy: "working", LastAction: &FrontendAgentLastAction{Repo: "hive", Number: 14921}}}, kind: "none", state: "Waiting for fixes"},
		{name: "other repository", item: StageTrackerItem{Repo: "hivecommons/docs", Number: 14921, Kind: "pr", Stage: 70}, agents: []FrontendAgent{{Name: "fixer", Busy: "working", LastAction: &FrontendAgentLastAction{Repo: "hive", Number: 14921, Current: true}}}, kind: "none", state: "Waiting for fixes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stageTrackerSetActor(&tc.item, tc.agents)
			a := tc.item.StageActor
			if a.Kind != tc.kind || a.Login != tc.login || a.Why == "" || tc.item.StageState != tc.state {
				t.Fatalf("actor=%+v state=%q", a, tc.item.StageState)
			}
			if tc.name == "current worker" && (a.Since == nil || !a.Since.Equal(time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC))) {
				t.Fatalf("worker since=%v", a.Since)
			}
		})
	}
}

func TestStageTrackerSharedPRDrawingOwner(t *testing.T) {
	// #14921 closes #14906 (already closed), #14910 and #14911. Input
	// order must not change its drawing owner, and other repos are isolated.
	items := []StageTrackerItem{
		{Repo: "hivecommons/hive", Number: 14911, Kind: "issue"},
		{Repo: "hivecommons/hive", Number: 14921, Kind: "pr", Stage: 70, StageActor: StageTrackerActor{Kind: "hive", Login: "fixer"}, LinkedIssues: []StageTrackerLinkedIssue{{Number: 14906, State: "closed"}, {Number: 14910, State: "open"}, {Number: 14911, State: "open"}}},
		{Repo: "hivecommons/hive", Number: 14910, Kind: "issue"},
		{Repo: "hivecommons/docs", Number: 14921, Kind: "pr"},
	}
	stageTrackerResolveLinkedPRs(items)
	if items[1].DrawnUnder != 14910 || items[3].DrawnUnder != 0 {
		t.Fatalf("owners: %+v", items)
	}
	for _, i := range []int{0, 2} {
		lp := items[i].LinkedPRs
		if len(lp) != 1 || lp[0].DrawnUnder != 14910 || lp[0].StageActor.Login != "fixer" {
			t.Fatalf("issue %d: %+v", items[i].Number, lp)
		}
	}
}
