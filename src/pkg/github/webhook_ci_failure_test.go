package github

import (
	"reflect"
	"testing"
	"time"
)

// Failed check deliveries resolve their head SHA to open PRs from the PR
// detail cache (no GitHub call: setupWebhookCacheTest fails on any) and
// record one deduplicated signal per PR, head and check
// (hivecommons/hive#15291).
func TestHandleWebhookEventRecordsCIFailureSignals(t *testing.T) {
	c, clock := setupWebhookCacheTest(t)
	seedWebhookTestPRs(c, clock.now.Add(-time.Minute))

	deliver := func(event, payload string) WebhookEventResult {
		t.Helper()
		res, err := c.HandleWebhookEvent(event, []byte(payload))
		if err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		return res
	}
	checks := func(res WebhookEventResult) []string {
		var out []string
		for _, s := range res.CIFailures {
			out = append(out, s.Check)
		}
		return out
	}

	// A fork PR's check_run carries no pull_requests: the cached head wins.
	res := deliver("check_run", `{"action":"completed","check_run":{"name":"unit","conclusion":"timed_out","head_sha":"sha-2","html_url":"https://gh.example/runs/9","pull_requests":[]},"repository":{"full_name":"org/repo"}}`)
	want := []CIFailureSignal{{Repo: "org/repo", Number: 2, HeadSHA: "sha-2", Check: "unit", DetailsURL: "https://gh.example/runs/9", At: clock.now}}
	if !reflect.DeepEqual(res.CIFailures, want) {
		t.Fatalf("check_run signals = %+v, want %+v", res.CIFailures, want)
	}
	if again := deliver("check_run", `{"action":"completed","check_run":{"name":"unit","conclusion":"failure","head_sha":"sha-2","pull_requests":[{"number":2}]},"repository":{"full_name":"org/repo"}}`); len(again.CIFailures) != 0 {
		t.Fatalf("same head + check must record once, got %+v", again.CIFailures)
	}
	if got := checks(deliver("check_suite", `{"action":"completed","check_suite":{"conclusion":"failure","head_sha":"sha-1","app":{"name":"GitHub Actions"}},"repository":{"full_name":"org/repo"}}`)); !reflect.DeepEqual(got, []string{"GitHub Actions"}) {
		t.Fatalf("check_suite checks = %v", got)
	}
	if got := checks(deliver("status", `{"sha":"sha-3","state":"failure","context":"ci/dco","target_url":"https://ci.example/dco","repository":{"full_name":"org/repo"}}`)); !reflect.DeepEqual(got, []string{"ci/dco"}) {
		t.Fatalf("status checks = %v", got)
	}

	for _, ignored := range []struct{ event, payload string }{
		{"check_run", `{"action":"created","check_run":{"name":"lint","conclusion":"failure","head_sha":"sha-1","pull_requests":[{"number":1}]},"repository":{"full_name":"org/repo"}}`},
		{"check_run", `{"action":"completed","check_run":{"name":"lint","conclusion":"cancelled","head_sha":"sha-1","pull_requests":[{"number":1}]},"repository":{"full_name":"org/repo"}}`},
		{"check_suite", `{"action":"completed","check_suite":{"conclusion":"success","head_sha":"sha-1","pull_requests":[{"number":1}]},"repository":{"full_name":"org/repo"}}`},
		{"status", `{"sha":"sha-1","state":"pending","context":"ci/x","repository":{"full_name":"org/repo"}}`},
		{"status", `{"sha":"unknown-head","state":"failure","context":"ci/x","repository":{"full_name":"org/repo"}}`},
	} {
		if res := deliver(ignored.event, ignored.payload); len(res.CIFailures) != 0 {
			t.Fatalf("%s %s recorded %+v, want none", ignored.event, ignored.payload, res.CIFailures)
		}
	}

	snap := WebhookHealthSnapshot().CIFailures
	if snap.Count != 3 || snap.Last == nil || snap.Last.Check != "ci/dco" || snap.Last.Number != 3 {
		t.Fatalf("ci_failures = %+v, want count 3, last #3 ci/dco", snap)
	}
	clock.now = clock.now.Add(ciFailureRetention)
	if got := CIFailureSignalsSnapshot().Count; got != 0 {
		t.Fatalf("signals older than the retention must be pruned, count = %d", got)
	}
}
