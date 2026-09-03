package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/worksource"
)

type fakeWorkSource struct {
	issues []worksource.Issue
	err    error
	calls  int
}

func (f *fakeWorkSource) SourceType() string { return "fake" }
func (f *fakeWorkSource) ListIssues(context.Context) ([]worksource.Issue, error) {
	f.calls++
	return f.issues, f.err
}

// TestWorkSourceIssuesForCycle guards the non-GitHub overlay (#4187, #4731,
// #4975): both failure modes fail CLOSED for issues, and the success path
// applies the GitHub exempt/require-label gates and SLA summary.
func TestWorkSourceIssuesForCycle(t *testing.T) {
	old := time.Now().Add(-3 * time.Hour) // past the SLA threshold
	cases := []struct {
		name       string
		ws         *fakeWorkSource
		wsErr      error
		exempt     []string
		filter     config.IssueFilterConfig
		wantTitles []string
		wantSLA    int
		wantCalls  int
	}{
		{ // positive control
			name:       "success projects every item",
			ws:         &fakeWorkSource{issues: []worksource.Issue{{Title: "a", CreatedAt: old}, {Title: "b"}}},
			wantTitles: []string{"a", "b"}, wantSLA: 1, wantCalls: 1,
		},
		{ // #4731: Linear items held to the same exempt policy as GitHub
			name:       "exempt labels drop items",
			ws:         &fakeWorkSource{issues: []worksource.Issue{{Title: "keep"}, {Title: "hold", Labels: []string{"hold"}}}},
			exempt:     []string{"hold"},
			wantTitles: []string{"keep"}, wantCalls: 1,
		},
		{ // #4731: require_labels applies to the overlay too
			name:       "issue filter admits only required labels",
			ws:         &fakeWorkSource{issues: []worksource.Issue{{Title: "yes", Labels: []string{"hive"}}, {Title: "no"}}},
			filter:     config.IssueFilterConfig{RequireLabels: []string{"hive"}},
			wantTitles: []string{"yes"}, wantCalls: 1,
		},
		{ // constructor failure: fail closed, never consult the source
			name:       "config error fails closed and skips ListIssues",
			ws:         &fakeWorkSource{issues: []worksource.Issue{{Title: "stale"}}},
			wsErr:      errors.New("bad work_source"),
			wantTitles: []string{}, wantCalls: 0,
		},
		{ // #4975: enumeration failure empties issues, never a stale list
			name:       "list error fails closed",
			ws:         &fakeWorkSource{err: errors.New("linear 502")},
			wantTitles: []string{}, wantCalls: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := workSourceIssuesForCycle(context.Background(), tc.ws, tc.wsErr, tc.exempt, tc.filter, testLogger())
			titles := make([]string, 0, len(got.Items))
			for _, it := range got.Items {
				titles = append(titles, it.Title)
			}
			if !reflect.DeepEqual(titles, tc.wantTitles) || got.Count != len(tc.wantTitles) {
				t.Fatalf("items = %v (count %d), want %v", titles, got.Count, tc.wantTitles)
			}
			if got.SLAViolations != tc.wantSLA {
				t.Fatalf("SLAViolations = %d, want %d", got.SLAViolations, tc.wantSLA)
			}
			if got.Items == nil {
				t.Fatalf("Items must never be nil (fail-closed result is an empty list)")
			}
			if tc.ws.calls != tc.wantCalls {
				t.Fatalf("ListIssues calls = %d, want %d", tc.ws.calls, tc.wantCalls)
			}
		})
	}
}

// TestMergeResumeKicks guards #2573/#2627 (e4589b66): a crash-restarted agent
// is resume-kicked only through the governor gate, never duplicated, never
// reordered ahead of the governor's own due list.
func TestMergeResumeKicks(t *testing.T) {
	cases := []struct {
		name            string
		due, restarted  []string
		allow           map[string]bool
		want, wantAsked []string
	}{
		{name: "no restarts is a no-op", due: []string{"scanner"}, want: []string{"scanner"}},
		{ // positive control
			name: "allowed restarts appended in order",
			due:  []string{"scanner"}, restarted: []string{"reviewer", "fixer"},
			allow: map[string]bool{"reviewer": true, "fixer": true},
			want:  []string{"scanner", "reviewer", "fixer"}, wantAsked: []string{"reviewer", "fixer"},
		},
		{ // #2627: the crash-looping CLI — gate closed, due list untouched
			name: "gated restart is not kicked",
			due:  []string{"scanner"}, restarted: []string{"looping"},
			allow: map[string]bool{},
			want:  []string{"scanner"}, wantAsked: []string{"looping"},
		},
		{ // already due keeps ONE slot and does not spend a resume allowance
			name: "already-due restart is not duplicated or re-gated",
			due:  []string{"scanner"}, restarted: []string{"scanner"},
			allow: map[string]bool{"scanner": true},
			want:  []string{"scanner"}, wantAsked: nil,
		},
		{
			name:      "empty due list still admits allowed restarts",
			restarted: []string{"reviewer"}, allow: map[string]bool{"reviewer": true},
			want: []string{"reviewer"}, wantAsked: []string{"reviewer"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			allow := func(a string) bool { asked = append(asked, a); return tc.allow[a] }
			got := mergeResumeKicks(append([]string(nil), tc.due...), tc.restarted, allow, testLogger())
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("due = %v, want %v", got, tc.want)
			}
			if !reflect.DeepEqual(asked, tc.wantAsked) {
				t.Fatalf("gate consulted for %v, want %v", asked, tc.wantAsked)
			}
		})
	}
}

// TestFilterKickableAgents guards #808/#815 (d225de17) and #2573: on-demand
// agents from config OR any ACMM pack are never kicked on cadence, and
// operator-paused agents consume nothing.
func TestFilterKickableAgents(t *testing.T) {
	agents := map[string]config.AgentConfig{"scanner": {}, "brainstorm": {OnDemand: true}}
	cases := []struct {
		name             string
		due              []string
		onDemand, paused map[string]bool
		want, wantPaused []string
		wantNil          bool
	}{
		{name: "positive control passes everything", due: []string{"scanner", "reviewer"},
			want: []string{"scanner", "reviewer"}, wantPaused: []string{"scanner", "reviewer"}},
		{name: "config on_demand dropped before the pause check", due: []string{"brainstorm", "scanner"},
			want: []string{"scanner"}, wantPaused: []string{"scanner"}},
		{ // #815: pack-declared on-demand agent absent from cfg.Agents is still dropped
			name: "pack on_demand dropped even when not in config", due: []string{"planner", "scanner"},
			onDemand: map[string]bool{"planner": true},
			want:     []string{"scanner"}, wantPaused: []string{"scanner"}},
		{ // #2573: filtered here, not left for SendKick to reject every cycle
			name: "paused dropped", due: []string{"scanner", "reviewer"},
			paused: map[string]bool{"reviewer": true},
			want:   []string{"scanner"}, wantPaused: []string{"scanner", "reviewer"}},
		{name: "everything filtered yields nil", due: []string{"brainstorm"}, wantNil: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var pausedAsked []string
			isPaused := func(a string) bool { pausedAsked = append(pausedAsked, a); return tc.paused[a] }
			got := filterKickableAgents(tc.due, agents, tc.onDemand, isPaused)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			if !reflect.DeepEqual(pausedAsked, tc.wantPaused) {
				t.Fatalf("IsPaused consulted for %v, want %v", pausedAsked, tc.wantPaused)
			}
		})
	}
}

func kickMsgs(agents ...string) []scheduler.KickMessage {
	out := make([]scheduler.KickMessage, 0, len(agents))
	for _, a := range agents {
		out = append(out, scheduler.KickMessage{Agent: a, Message: "kick " + a})
	}
	return out
}

// TestGateKickMessagesForProviderBudget guards #4294 (and #4583): a fresh
// spend rebuff withholds EVERY assembled kick, a stale one releases exactly
// one probe, and a healthy provider is inert.
func TestGateKickMessagesForProviderBudget(t *testing.T) {
	cases := []struct {
		name               string
		msgs               []scheduler.KickMessage
		suppress, latched  bool
		wantKept, wantHeld []string
		wantProbe          bool
	}{
		{name: "not latched passes through", msgs: kickMsgs("a", "b"), wantKept: []string{"a", "b"}},
		{ // #4294: total — a CEL or review kick is withheld like any other
			name: "fresh latch withholds all", msgs: kickMsgs("governor", "cel", "review"),
			suppress: true, latched: true, wantHeld: []string{"governor", "cel", "review"}},
		{name: "stale latch releases exactly one probe", msgs: kickMsgs("first", "second", "third"),
			latched: true, wantKept: []string{"first"}, wantHeld: []string{"second", "third"}, wantProbe: true},
		{name: "stale latch with a single kick releases it as the probe", msgs: kickMsgs("only"),
			latched: true, wantKept: []string{"only"}, wantProbe: true},
		{ // the probe stamp must not advance without a real kick going out
			name: "stale latch with no kicks releases nothing", latched: true},
		{name: "fresh latch with no kicks is inert", suppress: true, latched: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gateKickMessagesForProviderBudget(tc.msgs, tc.suppress, tc.latched)
			kept := make([]string, 0, len(got.Kept))
			for _, m := range got.Kept {
				kept = append(kept, m.Agent)
			}
			if !reflect.DeepEqual(kept, orEmpty(tc.wantKept)) {
				t.Fatalf("kept = %v, want %v", kept, tc.wantKept)
			}
			if !reflect.DeepEqual(orEmpty(got.Withheld), orEmpty(tc.wantHeld)) {
				t.Fatalf("withheld = %v, want %v", got.Withheld, tc.wantHeld)
			}
			if got.ReleaseProbe != tc.wantProbe || (got.ReleaseProbe && len(got.Kept) != 1) {
				t.Fatalf("ReleaseProbe = %v (kept %d), want %v with exactly one kick", got.ReleaseProbe, len(got.Kept), tc.wantProbe)
			}
		})
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// TestSelectSLABreachNotifications guards 915bb96a: at most three 2x-SLA
// pages per cycle, in enumeration order, with the cap reported.
func TestSelectSLABreachNotifications(t *testing.T) {
	aged := func(mins ...int) []github.Issue {
		out := make([]github.Issue, 0, len(mins))
		for i, m := range mins {
			out = append(out, github.Issue{Number: i + 1, AgeMinutes: m})
		}
		return out
	}
	cases := []struct {
		name       string
		items      []github.Issue
		want       []int
		wantCapped bool
	}{
		{name: "nothing breached", items: aged(5, 60), want: []int{}},
		{name: "boundary: exactly 60 is not a 2x breach", items: aged(60), want: []int{}},
		{name: "positive control: two breaches, both paged", items: aged(61, 10, 120), want: []int{1, 3}},
		{name: "exactly three is not capped", items: aged(90, 90, 90), want: []int{1, 2, 3}},
		{ // 915bb96a: only the first three go out; the fourth trips the cap
			name: "fourth breach trips the cap", items: aged(90, 90, 90, 90, 90), want: []int{1, 2, 3}, wantCapped: true},
		{name: "interleaved healthy issues neither count nor cap early",
			items: aged(90, 1, 90, 1, 90, 1, 90), want: []int{1, 3, 5}, wantCapped: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, capped := selectSLABreachNotifications(tc.items)
			nums := make([]int, 0, len(got))
			for _, it := range got {
				nums = append(nums, it.Number)
			}
			if !reflect.DeepEqual(nums, tc.want) || capped != tc.wantCapped {
				t.Fatalf("selected = %v capped = %v, want %v capped = %v", nums, capped, tc.want, tc.wantCapped)
			}
		})
	}
}

// TestClassifyAdvisoryPostError guards the App-banner verdict ordering on a
// failed digest post: #1699 (e22e3530) a rate-limited 403 is NOT "App not
// installed"; #2353 (c60969f2) a write-forbidden 403 is attributed honestly;
// 106a95fc/#2301 everything else goes to the shared auth probe.
func TestClassifyAdvisoryPostError(t *testing.T) {
	cases := []struct {
		name string
		err  string
		want advisoryPostFailure
	}{
		{"403 resource not accessible is write-forbidden",
			"POST https://api.github.com/repos/o/r/issues/1/comments: 403 Resource not accessible by integration []", advisoryPostWriteForbidden},
		// #1699: the exact regression — a rate-limited 403 must not reach the banner path.
		{"403 rate limit is rate-limited, not write-forbidden", "403 API rate limit exceeded for installation ID 1", advisoryPostRateLimited},
		{"secondary rate limit without a status code is rate-limited", "You have exceeded a secondary Rate Limit", advisoryPostRateLimited},
		// 106a95fc: a 401 must reach the auth probe, not be swallowed.
		{"401 bad credentials goes to the auth probe", "401 Bad credentials", advisoryPostAuthProbe},
		{"resource not accessible without 403 goes to the auth probe", "Resource not accessible by integration", advisoryPostAuthProbe},
		{"5xx goes to the auth probe", "502 Bad Gateway", advisoryPostAuthProbe},
		// Historical check order: write-forbidden wins when both texts appear.
		{"write-forbidden is checked before rate limit", "403 Resource not accessible by integration (rate limit headers present)", advisoryPostWriteForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAdvisoryPostError(errors.New(tc.err)); got != tc.want {
				t.Fatalf("classifyAdvisoryPostError(%q) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
