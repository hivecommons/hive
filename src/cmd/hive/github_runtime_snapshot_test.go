package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	hivegithub "github.com/kubestellar/hive/pkg/github"
)

func testLiveAppRuntimeSnapshot() liveGitHubRuntimeSnapshot {
	permissions := map[string]string{
		"actions": "write", "workflows": "write", "statuses": "write", "contents": "write",
		"issues": "write", "pull_requests": "write", "checks": "read", "metadata": "read",
	}
	return liveGitHubRuntimeSnapshot{
		Client: &hivegithub.Client{}, Token: func(context.Context) (string, error) { return "token", nil }, Mode: "app",
		Repository: "owner/repository", RepositoryID: 33,
		Writer: hivegithub.AuthenticatedUserIdentity{ID: 44, Login: "hive-test[bot]", Type: "Bot"},
		App: hivegithub.AppRuntimeIdentity{
			AppID: 11, InstallationID: 22, BotID: 44, BotLogin: "hive-test[bot]", BotType: "Bot",
			Repository: "owner/repository", RepositoryID: 33, Permissions: permissions,
			PermissionDigest: strings.Repeat("a", 64), BindingDigest: strings.Repeat("b", 64),
		},
	}
}

func TestLiveGitHubRuntimeStorePublishesClonesRotatesAndClears(t *testing.T) {
	store := &liveGitHubRuntimeStore{}
	input := testLiveAppRuntimeSnapshot()
	first, err := store.Publish(input)
	if err != nil {
		t.Fatal(err)
	}
	input.App.Permissions["actions"] = "read"
	current, ok := store.Current()
	if !ok || current.Revision != 1 || current.App.Permissions["actions"] != "write" || current.BindingDigest == "" {
		t.Fatalf("published runtime was not immutable: %+v", current)
	}
	current.App.Permissions["actions"] = "read"
	again, _ := store.Current()
	if again.App.Permissions["actions"] != "write" {
		t.Fatal("Current returned a mutable permission map")
	}
	unchanged, err := store.Publish(again)
	if err != nil || unchanged.Revision != first.Revision {
		t.Fatalf("identical runtime publication was not idempotent: first=%+v unchanged=%+v err=%v", first, unchanged, err)
	}
	secondInput := testLiveAppRuntimeSnapshot()
	second, err := store.Publish(secondInput)
	if err != nil || second.Revision != 2 || second.BindingDigest != first.BindingDigest {
		t.Fatalf("same structural identity rotation failed: first=%+v second=%+v err=%v", first, second, err)
	}
	store.Clear()
	if _, ok := store.Current(); ok {
		t.Fatal("cleared live runtime remained available")
	}
}

func TestLiveGitHubRuntimeStoreRejectsIncompleteAndIsConcurrencySafe(t *testing.T) {
	store := &liveGitHubRuntimeStore{}
	if _, err := store.Publish(liveGitHubRuntimeSnapshot{}); err == nil {
		t.Fatal("incomplete runtime was accepted")
	}
	const writers = 32
	var wait sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := store.Publish(testLiveAppRuntimeSnapshot())
			errs <- err
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	current, ok := store.Current()
	if !ok || current.Revision < 1 || current.Revision > writers {
		t.Fatalf("concurrent publication produced an invalid revision: ok=%t revision=%d", ok, current.Revision)
	}
}

func TestRefreshLiveGitHubAppRuntimeUpdatesPermissionsAndRejectsStructuralDrift(t *testing.T) {
	originalResolver := resolveLiveGitHubAppIdentity
	originalStore := dashboardLiveGitHubRuntime.Load()
	t.Cleanup(func() {
		resolveLiveGitHubAppIdentity = originalResolver
		dashboardLiveGitHubRuntime.Store(originalStore)
	})

	store := &liveGitHubRuntimeStore{}
	dashboardLiveGitHubRuntime.Store(store)
	initial := testLiveAppRuntimeSnapshot()
	if _, err := store.Publish(initial); err != nil {
		t.Fatal(err)
	}
	updated := initial.App
	updated.Permissions = cloneLiveGitHubRuntime(initial).App.Permissions
	updated.Permissions["workflows"] = "read"
	updated.PermissionDigest = strings.Repeat("c", 64)
	resolveLiveGitHubAppIdentity = func(context.Context, *hivegithub.Client, string) (hivegithub.AppRuntimeIdentity, error) {
		return updated, nil
	}
	refreshed, err := refreshLiveGitHubAppRuntime(context.Background(), initial)
	if err != nil || refreshed.Revision != 2 || refreshed.App.Permissions["workflows"] != "read" {
		t.Fatalf("live permission refresh failed: refreshed=%+v err=%v", refreshed, err)
	}
	if err := refreshed.App.RequireVisualHivePermissions(); err == nil || !strings.Contains(err.Error(), "workflows") {
		t.Fatalf("refreshed permission loss was not enforced: %v", err)
	}

	drifted := updated
	drifted.InstallationID++
	resolveLiveGitHubAppIdentity = func(context.Context, *hivegithub.Client, string) (hivegithub.AppRuntimeIdentity, error) {
		return drifted, nil
	}
	if _, err := refreshLiveGitHubAppRuntime(context.Background(), refreshed); err == nil || !strings.Contains(err.Error(), "managed rebind") {
		t.Fatalf("structural App drift was accepted: %v", err)
	}
}
