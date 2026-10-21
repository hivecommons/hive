package hivectl

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	runtimeHive     = "https://hive.example"
	runtimeOther    = "https://other.example"
	runtimeStatus   = `{"pid":4242,"started_at":"start","session":"session-a","adopted_scope_version":8,"adopted_at":"adopted","subscriptions":[{"hive":"https://hive.example","readiness":"connected","acknowledged_scope_version":8,"observed_at":"now"}],"current_task":null,"idle_reason":null}`
	runtimePID      = `{"pid":4242,"started_at":"start","session":"session-a"}`
	runtimeCtrPID   = `{"pid":4242,"started_at":"start","session":"session-a","container_name":"hive-relay"}`
	runtimeTaskJSON = `"current_task":{"hive":"https://hive.example","repository":"org/repo","task_id":"t1","issue":7,"started_at":"now","draining":true}`
)

func runtimeStore(t *testing.T) *ProfileStore {
	t.Helper()
	t.Setenv("HIVE_RELAY_STATUS_FILE", "")
	original, originalRun := signalRelayPID, runRelayCommand
	t.Cleanup(func() { signalRelayPID, runRelayCommand = original, originalRun })
	signalRelayPID = func(int, syscall.Signal) error { return nil }
	return NewProfileStore(t.TempDir())
}

func writeRuntimeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runtimeSet(version uint64) *ProfileSet {
	return &ProfileSet{RepoScopeVersion: version, Profiles: []Profile{
		{Name: "one", Hub: runtimeHive, Selection: &RepositorySelection{Mode: SelectionSelected, Repositories: []string{treeRepo}}},
		{Name: "two", Hub: runtimeOther, Selection: &RepositorySelection{Mode: SelectionSelected, Repositories: []string{treeRepo}}},
		{Name: "off", Hub: "https://off.example", Disabled: true, Selection: &RepositorySelection{Mode: SelectionSelected, Repositories: []string{treeRepo}}},
	}}
}

func TestLiveTreeRuntimeUnavailableReasons(t *testing.T) {
	store := runtimeStore(t)
	set := runtimeSet(8)
	check := func(want string) {
		t.Helper()
		live := store.LiveTreeRuntime(set)
		if live.Available || live.Reason != want || live.Acknowledged != "unknown" || live.SavedScopeVersion != 8 {
			t.Fatalf("live = %+v, want reason %q", live, want)
		}
		if live.Summary != live.Label() || !strings.HasPrefix(live.Summary, TreeRuntimeUnavailable+" — "+want) {
			t.Fatalf("summary = %q", live.Summary)
		}
	}
	check("relay has not published a status snapshot")
	writeRuntimeFile(t, store.RelayStatusPath(), "{")
	check("relay status snapshot is unreadable")
	writeRuntimeFile(t, store.RelayStatusPath(), runtimeStatus)
	check("no managed relay PID record")
	writeRuntimeFile(t, store.RelayPIDPath(), `{"pid":4242,"started_at":"start","session":"old"}`)
	check("status snapshot is from a previous relay session")
	writeRuntimeFile(t, store.RelayPIDPath(), runtimePID)
	signalRelayPID = func(int, syscall.Signal) error { return syscall.ESRCH }
	check("relay process is not running")
	writeRuntimeFile(t, store.RelayPIDPath(), runtimeCtrPID)
	runRelayCommand = func(context.Context, string, ...string) ([]byte, error) { return []byte("false\n"), nil }
	check("relay container is not running")
	runRelayCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "docker" || strings.Join(args, " ") != "inspect -f {{.State.Running}} hive-relay" {
			t.Fatalf("inspect = %s %v", name, args)
		}
		return []byte("true\n"), nil
	}
	if live := store.LiveTreeRuntime(set); !live.Available {
		t.Fatalf("running container should be live: %+v", live)
	}
	if live := store.LiveTreeRuntime(nil); !live.Available || live.SavedScopeVersion != 0 || live.Acknowledged != "acknowledged" {
		t.Fatalf("nil set = %+v", live)
	}
}

func TestLiveTreeRuntimeAcknowledgement(t *testing.T) {
	store := runtimeStore(t)
	writeRuntimeFile(t, store.RelayStatusPath(), runtimeStatus)
	writeRuntimeFile(t, store.RelayPIDPath(), runtimePID)

	// "two" is enabled but the relay never adopted it, so the saved
	// generation is pending even though "one" acknowledged it.
	live := store.LiveTreeRuntime(runtimeSet(8))
	if !live.Available || live.PID != 4242 || live.AdoptedScopeVersion != 8 || live.Acknowledged != "pending" {
		t.Fatalf("live = %+v", live)
	}
	if sub := live.Subscriptions[runtimeHive]; sub.Readiness != "connected" || sub.Acknowledged != "acknowledged" || sub.ObservedAt != "now" {
		t.Fatalf("subscription = %+v", sub)
	}
	if !strings.Contains(live.Label(), "relay pid 4242 · scope v8 pending · idle") {
		t.Fatalf("label = %q", live.Label())
	}

	single := &ProfileSet{RepoScopeVersion: 8, Profiles: []Profile{{Name: "one", Hub: runtimeHive + "/contribute"}, {Name: "off", Hub: runtimeOther, Disabled: true}, {Name: "bad", Hub: "::not a url"}}}
	if live := store.LiveTreeRuntime(single); live.Acknowledged != "acknowledged" {
		t.Fatalf("normalised, disabled and malformed hubs: %+v", live)
	}
	if live := store.LiveTreeRuntime(&ProfileSet{RepoScopeVersion: 9, Profiles: single.Profiles[:1]}); live.Acknowledged != "pending" || live.Subscriptions[runtimeHive].Acknowledged != "pending" {
		t.Fatalf("newer saved generation = %+v", live)
	}

	writeRuntimeFile(t, store.RelayStatusPath(), strings.Replace(runtimeStatus, `"current_task":null`, runtimeTaskJSON, 1))
	live = store.LiveTreeRuntime(single)
	if live.CurrentTask == nil || live.CurrentTask.Repository != "org/repo" || !strings.Contains(live.Label(), "working on org/repo (draining)") {
		t.Fatalf("task label = %q (%+v)", live.Label(), live.CurrentTask)
	}
	writeRuntimeFile(t, store.RelayStatusPath(), strings.Replace(runtimeStatus, `"idle_reason":null`, `"idle_reason":"no-repositories-selected"`, 1))
	if live := store.LiveTreeRuntime(single); live.IdleReason != "no-repositories-selected" || !strings.HasSuffix(live.Label(), "idle — no-repositories-selected") {
		t.Fatalf("idle label = %q", live.Label())
	}
}

func TestDurableTreeRuntime(t *testing.T) {
	store := runtimeStore(t)
	if d := store.DurableTreeRuntime(nil); d.Available || d.Reason != DurableActivityNotShipped || d.Summary != TreeRuntimeUnavailable+" — "+DurableActivityNotShipped {
		t.Fatalf("nil source = %+v", d)
	}
	if d := UnavailableDurable("  "); d.Reason != DurableActivityNotShipped {
		t.Fatalf("blank reason = %+v", d)
	}
	failing := func(*ProfileStore) (TreeDurable, error) { return TreeDurable{}, errors.New("store locked") }
	if d := store.DurableTreeRuntime(failing); d.Available || d.Reason != "store locked" {
		t.Fatalf("failing source = %+v", d)
	}
	declined := func(*ProfileStore) (TreeDurable, error) { return TreeDurable{Reason: "retention expired"}, nil }
	if d := store.DurableTreeRuntime(declined); d.Available || d.Reason != "retention expired" {
		t.Fatalf("declined source = %+v", d)
	}
	empty := func(s *ProfileStore) (TreeDurable, error) {
		if s != store {
			t.Fatal("source must receive the store")
		}
		return TreeDurable{Available: true, Retention: "30d"}, nil
	}
	if d := store.DurableTreeRuntime(empty); !d.Available || d.Summary != treeHistoryNone+" within 30d" {
		t.Fatalf("empty store = %+v", d)
	}
	last := time.Date(2026, 10, 21, 6, 0, 0, 0, time.UTC)
	full := func(*ProfileStore) (TreeDurable, error) {
		return TreeDurable{Available: true, Retention: "30d", Attempts: 3, Completed: 2, LastActivity: last}, nil
	}
	if d := store.DurableTreeRuntime(full); d.Summary != "3 attempt(s) · 2 completed · last 2026-10-21T06:00:00Z · retention 30d" {
		t.Fatalf("summary = %q", d.Summary)
	}
	if d := (TreeDurable{Available: true}); d.Label() != treeHistoryNone {
		t.Fatalf("label = %q", d.Label())
	}
}

func TestContributionTreeApplyRuntime(t *testing.T) {
	var nilTree *ContributionTree
	nilTree.ApplyRuntime(TreeRuntime{})

	set := runtimeSet(8)
	repos := map[string]TreeRepositorySnapshot{
		"one": {Discovery: TreeComplete, Repositories: []string{treeRepo, treeNewRepo}},
		"two": {Discovery: TreeComplete, Repositories: []string{treeRepo}},
		"off": {Discovery: TreeComplete, Repositories: []string{treeRepo}},
	}
	build := func() *ContributionTree {
		t.Helper()
		tree, err := BuildContributionTree(set, nil, repos)
		if err != nil {
			t.Fatal(err)
		}
		return tree
	}
	subs := func(tree *ContributionTree) map[string]TreeSubscription {
		out := map[string]TreeSubscription{}
		for _, s := range tree.Subscriptions {
			out[s.ID] = s
		}
		return out
	}

	// Nothing available: every column keeps its unavailable label.
	tree := build()
	tree.ApplyRuntime(TreeRuntime{Live: TreeLive{Reason: "x"}, Durable: UnavailableDurable("")})
	for _, s := range tree.Subscriptions {
		wantReadiness := "unknown"
		if s.Disabled {
			wantReadiness = "disabled"
		}
		if s.Readiness != wantReadiness || s.Activity != TreeRuntimeUnavailable || s.History != TreeRuntimeUnavailable {
			t.Fatalf("%s = %+v", s.ID, s)
		}
		for _, r := range s.Repositories {
			if r.Activity != TreeRuntimeUnavailable || r.History != TreeRuntimeUnavailable {
				t.Fatalf("%s/%s = %+v", s.ID, r.ID, r)
			}
		}
	}

	last := time.Date(2026, 10, 21, 6, 0, 0, 0, time.UTC)
	rt := TreeRuntime{
		Live: TreeLive{Available: true, PID: 1, IdleReason: "quota held", Subscriptions: map[string]TreeLiveSubscription{
			runtimeHive: {Readiness: "connected", Acknowledged: "acknowledged"},
		}},
		Durable: TreeDurable{Available: true, Repositories: map[string]TreeDurableRepository{
			treeRepo: {Attempts: 2, Completed: 1, LastActivity: last},
		}},
	}
	tree = build()
	tree.ApplyRuntime(rt)
	got := subs(tree)
	if s := got["one"]; s.Readiness != "connected" || s.Activity != "idle — quota held" || s.History != "2 attempt(s) · 1 completed · last 2026-10-21T06:00:00Z" {
		t.Fatalf("one = %+v", s)
	}
	if s := got["two"]; s.Readiness != treeReadinessNotAdopted || s.Activity != "idle — quota held" {
		t.Fatalf("two = %+v", s)
	}
	if s := got["off"]; s.Readiness != "disabled" {
		t.Fatalf("disabled = %+v", s)
	}
	for _, r := range got["one"].Repositories {
		switch r.ID {
		case treeRepo:
			if r.Activity != treeActivityIdle || r.History != "2 attempt(s) · 1 completed · last 2026-10-21T06:00:00Z" {
				t.Fatalf("repo = %+v", r)
			}
		case treeNewRepo:
			if r.Activity != treeActivityIdle || r.History != treeHistoryNone {
				t.Fatalf("new repo = %+v", r)
			}
		}
	}

	// A current task marks its Hive and repository; another Hive stays idle.
	rt.Live.IdleReason = ""
	rt.Live.CurrentTask = &RelayCurrentTask{Hive: runtimeHive, Repository: treeRepo, Draining: true}
	rt.Durable.Repositories = map[string]TreeDurableRepository{treeRepo: {Attempts: 1}}
	tree = build()
	tree.ApplyRuntime(rt)
	got = subs(tree)
	if s := got["one"]; s.Activity != "working on "+treeRepo+" (draining)" || s.History != "1 attempt(s) · 0 completed" {
		t.Fatalf("working = %+v", s)
	}
	if s := got["two"]; s.Activity != treeActivityIdle {
		t.Fatalf("other hive = %+v", s)
	}
	for _, r := range got["one"].Repositories {
		if r.ID == treeRepo && (r.Activity != treeActivityInProgress || r.History != "1 attempt(s) · 0 completed") {
			t.Fatalf("in-progress repo = %+v", r)
		}
	}
	if sub := got["two"]; sub.Repositories[0].Activity != treeActivityIdle {
		t.Fatalf("idle repo = %+v", sub.Repositories[0])
	}
}
