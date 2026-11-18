package dashboard

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

type umbrellaStatusCounts struct {
	ActionableItems   int `json:"actionable_items"`
	CandidateItems    int `json:"candidate_items"`
	CandidateItemsRaw int `json:"candidate_items_raw"`
	UmbrellaItems     int `json:"umbrella_items"`
	HumanOnlyItems    int `json:"human_only_items"`
}

type umbrellaQueueCounts struct {
	Queue          []ReadyQueueItem `json:"queue"`
	QueueTotal     int              `json:"queue_total"`
	UmbrellaTotal  int              `json:"umbrella_total"`
	HumanOnlyTotal int              `json:"human_only_total"`
}

// umbrellaSeed is the mixed backlog the counting tests share: two workable
// issues, three umbrellas by three different rules and one owner-only issue.
func umbrellaSeed() *StatusPayload {
	issue := func(n int, title string, labels []any, extra map[string]any) map[string]any {
		m := map[string]any{"number": float64(n), "title": title, "labels": labels}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	return &StatusPayload{Repos: []FrontendRepo{{
		Name: "acme/repo", Full: "acme/repo",
		ActionableIssues: []any{
			issue(1, "fix the paginator", []any{}, nil),
			issue(2, "add a flag", []any{"help wanted"}, nil),
			issue(3, "audit: fail-closed defaults", []any{"epic"}, nil),
			issue(4, "roadmap for q4", []any{}, map[string]any{"body": "<!-- hive:umbrella -->"}),
			issue(5, "decomposed work", []any{}, map[string]any{"children": map[string]any{"total": float64(4), "completed": float64(1)}}),
			issue(6, "verify on a real call", []any{"human-only"}, nil),
		},
	}}}
}

func TestContributeStatusCountsUmbrellaAndOwnerOnlySeparately(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))
	s.statusMu.Lock()
	s.status = umbrellaSeed()
	s.statusMu.Unlock()

	status := decodeContributeResponse[umbrellaStatusCounts](t, doGet(s, "/api/contribute/status"))
	want := umbrellaStatusCounts{ActionableItems: 2, CandidateItems: 2, CandidateItemsRaw: 6, UmbrellaItems: 3, HumanOnlyItems: 1}
	if status != want {
		t.Fatalf("status = %+v, want %+v", status, want)
	}

	queue := decodeContributeResponse[umbrellaQueueCounts](t, doGet(s, "/api/contribute/queue"))
	if queue.QueueTotal != 2 || queue.UmbrellaTotal != 3 || queue.HumanOnlyTotal != 1 {
		t.Fatalf("queue = total:%d umbrella:%d human_only:%d, want 2, 3, 1", queue.QueueTotal, queue.UmbrellaTotal, queue.HumanOnlyTotal)
	}
	for _, item := range queue.Queue {
		if item.Number > 2 {
			t.Errorf("issue #%d is excluded work but is in the ready queue", item.Number)
		}
	}

	fleet := decodeContributeResponse[struct {
		UmbrellaCount  int `json:"umbrella_count"`
		HumanOnlyCount int `json:"human_only_count"`
	}](t, doGet(s, "/api/contribute/fleet"))
	if fleet.UmbrellaCount != 3 || fleet.HumanOnlyCount != 1 {
		t.Fatalf("fleet counts = %+v, want umbrella 3 and human-only 1", fleet)
	}
}

// Turning the classification off restores exactly the old behaviour: only the
// enumerator's own tracker flag excludes, and the raw and workable candidate
// counts agree again.
func TestContributorExclusionPerRepoOverride(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	deps := testDeps(t)
	disabled := false
	deps.Config.Hub.ContributeUmbrella.PerRepo = map[string]config.ContributeUmbrellaRepo{
		"Acme/Repo": {Enabled: &disabled},
	}
	s.RegisterAPI(deps)
	payload := umbrellaSeed()
	payload.Repos[0].ActionableIssues = append(payload.Repos[0].ActionableIssues, map[string]any{
		"number": float64(7), "title": "legacy tracker", "labels": []any{}, "is_tracker": true,
	})
	s.statusMu.Lock()
	s.status = payload
	s.statusMu.Unlock()

	status := decodeContributeResponse[umbrellaStatusCounts](t, doGet(s, "/api/contribute/status"))
	// #3, #4, #5 and #6 are ordinary issues again; only the legacy flag (#7)
	// excludes. ActionableItems also passes the pre-existing admission gates,
	// so it is bounded by the candidate count and well above the 2 that the
	// enabled rules leave, rather than pinned to an exact figure.
	if status.UmbrellaItems != 1 || status.HumanOnlyItems != 0 || status.CandidateItems != 6 || status.ActionableItems <= 2 || status.ActionableItems > status.CandidateItems || status.CandidateItemsRaw != 7 {
		t.Fatalf("status with the rules disabled = %+v", status)
	}
}

func TestContributorExclusionRules(t *testing.T) {
	cases := []struct {
		name   string
		issue  map[string]any
		reason string
	}{
		{"plain", map[string]any{"title": "fix", "labels": []any{"bug"}}, ""},
		{"umbrella label", map[string]any{"title": "x", "labels": []any{"Umbrella"}}, withheldReasonUmbrella},
		{"title prefix", map[string]any{"title": "[Tracking] x"}, withheldReasonUmbrella},
		{"task list", map[string]any{"title": "x", "body": "- [ ] #1\n- [x] #2"}, withheldReasonUmbrella},
		{"legacy tracker flag", map[string]any{"title": "x", "is_tracker": true}, withheldReasonTracker},
		{"needs-human", map[string]any{"title": "x", "labels": []any{"needs-human"}}, withheldReasonHumanOnly},
		{"human-only marker", map[string]any{"title": "x", "body": "<!-- hive:human-only -->"}, withheldReasonHumanOnly},
		{"umbrella wins over human-only", map[string]any{"title": "x", "labels": []any{"epic", "human-only"}}, withheldReasonUmbrella},
	}
	hub := &ContributeWSHub{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := hub.contributorExclusion("acme/repo", tc.issue); got != tc.reason {
				t.Fatalf("reason = %q, want %q", got, tc.reason)
			}
		})
	}
}

func TestContributePageShowsWorkableAndExcludedLine(t *testing.T) {
	body := contributePageBody(t)
	for _, want := range []string{
		`workable`,
		`id="queue-excluded-note"`,
		`not offered to contributors`,
		`data.umbrella_count`,
		`data.human_only_count`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("contributor page missing %q", want)
		}
	}
}

func TestStageTrackerOwnerFlags(t *testing.T) {
	cfg := &config.Config{}
	cases := []struct {
		name      string
		issue     github.Issue
		umbrella  string
		ownerOnly string
	}{
		{"plain", github.Issue{Title: "fix"}, "", ""},
		{"epic label", github.Issue{Title: "x", Labels: []string{"epic"}}, "label:epic", ""},
		{"prose-free marker", github.Issue{Title: "x", Body: "<!-- hive:umbrella -->"}, "marker", ""},
		{"sub-issues", github.Issue{Title: "x", Children: &github.IssueChildren{Total: 2}}, "sub-issues", ""},
		{"legacy detector", github.Issue{Title: "x", IsTracker: true}, "tracker-detector", ""},
		{"owner only", github.Issue{Title: "x", Labels: []string{"human-only"}}, "", "label:human-only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, o := stageTrackerOwnerFlags(cfg, "acme/repo", tc.issue)
			if u != tc.umbrella || o != tc.ownerOnly {
				t.Fatalf("flags = %q,%q want %q,%q", u, o, tc.umbrella, tc.ownerOnly)
			}
		})
	}
	// The pill is driven by the JSON the tracker emits.
	b, err := json.Marshal(StageTrackerItem{Umbrella: "marker", OwnerOnly: "label:needs-human"})
	if err != nil || !strings.Contains(string(b), `"umbrella":"marker"`) || !strings.Contains(string(b), `"owner_only":"label:needs-human"`) {
		t.Fatalf("tracker item json = %s (%v)", b, err)
	}
	src, err := os.ReadFile("static-src/js/tracker-row-render.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "Umbrella · owner") || !strings.Contains(string(src), "Owner only") {
		t.Fatal("tracker row render must carry the Umbrella · owner and Owner only pills")
	}
}
