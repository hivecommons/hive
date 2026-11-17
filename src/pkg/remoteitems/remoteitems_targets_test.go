package remoteitems

import (
	"context"
	"strings"
	"testing"
	"time"
)

// hivecommons/hive#18516: with project.remote_write_identity a hive can
// write to one remote repo as its App bot and to another as the owner. Each
// repo is listed under its own login, its items are tracked under it, and
// feedback from that login (the hive's own replies) is never routed back.
func TestRunTargets_PerRepoLogin(t *testing.T) {
	const bot = "hivecommons-hive[bot]"
	forge := newForge()
	forge.items["hivecommons/docs"] = []RemoteItem{
		{Repo: "hivecommons/docs", Number: 3, IsPR: true, Body: "Docs fix" + marker, Author: bot, CreatedAt: t0},
		// The owner's own item on a bot-identity repo is not the hive's.
		{Repo: "hivecommons/docs", Number: 4, Body: "owner by hand" + marker, Author: owner, CreatedAt: t0},
	}
	forge.items["acme/site"] = []RemoteItem{
		{Repo: "acme/site", Number: 9, Body: "Findings" + marker, Author: owner, CreatedAt: t0},
		{Repo: "acme/site", Number: 10, Body: "bot elsewhere" + marker, Author: bot, CreatedAt: t0},
	}
	forge.comments[itemKey("hivecommons/docs", 3)] = []Comment{
		{Kind: "review", Author: "maintainer", Body: "please split this", CreatedAt: t0.Add(time.Minute)},
		{Kind: "comment", Author: bot, Body: "hive reply", CreatedAt: t0.Add(2 * time.Minute)},
	}
	forge.comments[itemKey("acme/site", 9)] = []Comment{
		{Kind: "comment", Author: "reviewer", Body: "thanks, one question", CreatedAt: t0.Add(time.Minute)},
		{Kind: "comment", Author: owner, Body: "owner reply", CreatedAt: t0.Add(2 * time.Minute)},
	}
	p, store := testPoller(t, forge)
	r := &fakeResumer{sessions: map[string]string{"docs-assessor": "s1"}}
	out := p.RunTargets(context.Background(), []Target{
		{Repo: "hivecommons/docs", Login: bot},
		{Repo: "acme/site", Login: owner},
		{Repo: "ignored/no-login"},
	}, r, t0.Add(time.Hour))
	if out.Repos != 2 || out.Added != 2 || out.Tracked != 2 || out.Routed != 2 {
		t.Fatalf("outcome = %+v", out)
	}
	forge.mu.Lock()
	calls := strings.Join(forge.calls, "|")
	forge.mu.Unlock()
	for _, want := range []string{"list hivecommons/docs " + bot, "list acme/site " + owner} {
		if !strings.Contains(calls, want) {
			t.Fatalf("calls %q lack %q", calls, want)
		}
	}
	if strings.Contains(calls, "ignored/no-login") {
		t.Fatalf("a target without a login must be skipped: %q", calls)
	}
	items, err := store.Items()
	if err != nil {
		t.Fatal(err)
	}
	tracked := map[string]bool{}
	for _, it := range items {
		tracked[it.Key()] = true
	}
	if !tracked[itemKey("hivecommons/docs", 3)] {
		t.Fatalf("the bot's PR must be tracked: %+v", items)
	}
	if !tracked[itemKey("acme/site", 9)] {
		t.Fatalf("the owner's issue must be tracked: %+v", items)
	}
	if len(items) != 2 {
		t.Fatalf("only items by each repo's own login are tracked: %+v", items)
	}
	sent := strings.Join(r.sent, "\n")
	if !strings.Contains(sent, "please split this") || !strings.Contains(sent, "thanks, one question") {
		t.Fatalf("feedback not routed: %s", sent)
	}
	if strings.Contains(sent, "hive reply") || strings.Contains(sent, "owner reply") {
		t.Fatalf("the hive's own replies must never be routed back: %s", sent)
	}

	// Run (one login for every repo) is RunTargets with that login.
	forge2 := newForge()
	p2, _ := testPoller(t, forge2)
	p2.Run(context.Background(), []string{"hivecommons/docs"}, owner, nil, t0)
	if len(forge2.calls) != 1 || forge2.calls[0] != "list hivecommons/docs "+owner {
		t.Fatalf("Run calls = %v", forge2.calls)
	}
}
