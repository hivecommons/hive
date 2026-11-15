package dashboard

import (
	"encoding/json"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// A label someone adds to a PR in a native-merge-queue repo wakes the lanes
// for that PR at once, so a hold reaches the dequeue before GitHub's queue
// merges it (projectbluefin/testsuite#1235). The hive's own bookkeeping
// labels, other actions, closed PRs and repos or bases outside the queue
// scope do not; a refusing label the hive adds itself does.
func TestNudgeQueueLabelEvent(t *testing.T) {
	const app = "hivecommons-hive[bot]"
	labelled := func(action, sender, label, state, base string, merged bool) []byte {
		b, err := json.Marshal(map[string]any{
			"action": action,
			"sender": map[string]any{"login": sender},
			"label":  map[string]any{"name": label},
			"pull_request": map[string]any{
				"number": 1235, "state": state, "merged": merged,
				"base": map[string]any{"ref": base},
			},
			"repository": map[string]any{"full_name": "projectbluefin/testsuite"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	payload := func(action, sender, state, base string, merged bool) []byte {
		return labelled(action, sender, "hold", state, base, merged)
	}
	queue := config.NativeMergeQueueAutoMergeConfig{Enabled: true, Repos: []string{"projectbluefin/testsuite"}, Bases: []string{"projectbluefin/testsuite#main"}}
	for _, tc := range []struct {
		name  string
		event string
		body  []byte
		queue config.NativeMergeQueueAutoMergeConfig
		want  bool
	}{
		{"person adds a label", "pull_request", payload("labeled", "Danathar", "open", "main", false), queue, true},
		{"another bot adds a label", "pull_request", payload("labeled", "mergeraptor[bot]", "open", "main", false), queue, true},
		{"the hive's own stage label", "pull_request", labelled("labeled", app, "stage/100-merge-queue", "open", "main", false), queue, false},
		{"the hive's own role label", "pull_request", labelled("labeled", app, "role/trusted", "open", "main", false), queue, false},
		{"the hive adds hold", "pull_request", payload("labeled", app, "open", "main", false), queue, true},
		{"the hive adds the sentinel label", "pull_request", labelled("labeled", app, config.DefaultSentinelLabel, "open", "main", false), queue, true},
		{"the hive adds needs-human", "pull_request", labelled("labeled", app, "needs-human", "open", "main", false), queue, true},
		{"the hive adds an owner-merge label", "pull_request", labelled("labeled", app, "needs-owner-merge", "open", "main", false), queue, true},
		{"the hive adds do-not-merge/hold", "pull_request", labelled("labeled", app, "do-not-merge/hold", "open", "main", false), queue, true},
		{"a person adds any label", "pull_request", labelled("labeled", "Danathar", "kind/bug", "open", "main", false), queue, true},
		{"label removed", "pull_request", payload("unlabeled", "Danathar", "open", "main", false), queue, false},
		{"other action", "pull_request", payload("synchronize", "Danathar", "open", "main", false), queue, false},
		{"closed PR", "pull_request", payload("labeled", "Danathar", "closed", "main", false), queue, false},
		{"merged PR", "pull_request", payload("labeled", "Danathar", "closed", "main", true), queue, false},
		{"base outside the queue scope", "pull_request", payload("labeled", "Danathar", "open", "release", false), queue, false},
		{"native queue off", "pull_request", payload("labeled", "Danathar", "open", "main", false), config.NativeMergeQueueAutoMergeConfig{}, false},
		{"issue event", "issues", payload("labeled", "Danathar", "open", "main", false), queue, false},
		{"malformed payload", "pull_request", []byte("{"), queue, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.AutoMerge.NativeMergeQueue = tc.queue
			client := github.NewClient("token", "projectbluefin", []string{"testsuite"}, nil, "http://127.0.0.1:0")
			client.SetAppBotLogin(app)
			var nudged []string
			s := &Server{deps: &Dependencies{Config: cfg, GHClient: client, SweepNudge: func(repo string, number int) {
				nudged = append(nudged, repo)
				if number != 1235 {
					t.Errorf("nudged #%d, want #1235", number)
				}
			}}}
			res := github.WebhookEventResult{Event: tc.event, Repo: "projectbluefin/testsuite", KnownRepo: true}
			got := s.nudgeQueueLabelEvent(tc.event, tc.body, res)
			if got != tc.want || (len(nudged) == 1) != tc.want || len(nudged) > 1 {
				t.Fatalf("nudge = %v, nudged %v; want %v", got, nudged, tc.want)
			}
			if tc.want && nudged[0] != "projectbluefin/testsuite" {
				t.Fatalf("nudged %q, want projectbluefin/testsuite", nudged[0])
			}
		})
	}
}

// Without a nudge hook or an App login the event still decides safely: no
// hook means nothing to call; an unknown App login nudges rather than skips.
func TestNudgeQueueLabelEventWithoutHookOrAppLogin(t *testing.T) {
	cfg := &config.Config{}
	cfg.AutoMerge.NativeMergeQueue = config.NativeMergeQueueAutoMergeConfig{Enabled: true}
	body, _ := json.Marshal(map[string]any{
		"action":       "labeled",
		"sender":       map[string]any{"login": "Danathar"},
		"pull_request": map[string]any{"number": 7, "state": "open", "base": map[string]any{"ref": "main"}},
	})
	res := github.WebhookEventResult{Repo: "acme/widget", KnownRepo: true}
	if (&Server{deps: &Dependencies{Config: cfg}}).nudgeQueueLabelEvent("pull_request", body, res) {
		t.Fatal("no SweepNudge hook must not report a nudge")
	}
	calls := 0
	s := &Server{deps: &Dependencies{Config: cfg, SweepNudge: func(string, int) { calls++ }}}
	if !s.nudgeQueueLabelEvent("pull_request", body, res) || calls != 1 {
		t.Fatalf("calls = %d, want one nudge with no App login known", calls)
	}
}
