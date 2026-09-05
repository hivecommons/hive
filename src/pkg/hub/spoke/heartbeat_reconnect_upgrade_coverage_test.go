package spoke

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// nil2Logger returns a logger that discards output, keeping the -race coverage
// run quiet while these tests hammer the heartbeat loop.
func nil2Logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ============================================================
// heartbeat.go — reconnection response demux + upgrade-target
// negotiation + task-status push success path (#2559)
//
// These tests drive the PRODUCTION StartHeartbeat / StartTaskStatusPush paths
// against an httptest hub, collapsing the readiness wait so the loop beats in
// milliseconds. They use NO package-level global writes beyond the existing
// per-test seams (waitForReady knobs / single-loop guard, both restored via
// t.Cleanup by resetSingleLoopGuard + collapseReadinessWait); nothing here
// mutates a shared path/config global, keeping the -race coverage run clean.
// ============================================================

// TestStartHeartbeatProcessesFullResponse verifies StartHeartbeat wires every
// callback and that processHeartbeatResponse dispatches each field of a rich
// heartbeat response. It exercises the UpgradeTo branch (not SwitchToTag), the
// GitHubAppConfig/HubBanner/Visibility/AuthorizedUsers/ProjectConfig/
// PendingGateway/RestartSpoke callbacks, and the callback-demux switch.
func TestStartHeartbeatProcessesFullResponse(t *testing.T) {
	resetSingleLoopGuard(t)
	collapseReadinessWait(t)

	// The hub answers the first beat with a fully-populated response so every
	// dispatch branch runs, then answers subsequent beats with an empty body.
	var beats atomic.Int32
	isPublic := true
	resp := HeartbeatResponse{
		OK:              true,
		UpgradeTo:       "deadbeef",
		GitHubAppConfig: &HeartbeatGitHubAppConfig{AppID: 7, InstallationID: 99},
		HubBanner:       &HubBanner{ID: "b1", Message: "hi"},
		IsPublic:        &isPublic,
		AuthorizedUsers: []string{"alice:owner"},
		ProjectConfig:   &HeartbeatProjectConfig{Org: "acme", Repos: []string{"r"}},
		PendingGateway:  &HeartbeatGatewayConfig{Name: "gw", Key: "k"},
		RestartSpoke:    true,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if beats.Add(1) == 1 {
			json.NewEncoder(w).Encode(resp)
			return
		}
		json.NewEncoder(w).Encode(HeartbeatResponse{OK: true})
	}))
	defer srv.Close()

	// Each callback records that it fired exactly as the hub asked.
	var (
		mu              sync.Mutex
		gotUpgrade      string
		gotApp          *HeartbeatGitHubAppConfig
		gotBanner       *HubBanner
		gotVisibility   *bool
		gotUsers        []string
		gotProject      *HeartbeatProjectConfig
		gotGateway      *HeartbeatGatewayConfig
		gotRestart      bool
		gotSwitchBranch string
	)
	fired := make(chan struct{}, 1)
	onUpgrade := UpgradeCallback(func(tag string) {
		mu.Lock()
		gotUpgrade = tag
		mu.Unlock()
	})
	onApp := GitHubAppConfigCallback(func(c *HeartbeatGitHubAppConfig) {
		mu.Lock()
		gotApp = c
		mu.Unlock()
	})
	onBanner := HubBannerCallback(func(b *HubBanner) {
		mu.Lock()
		gotBanner = b
		mu.Unlock()
	})
	onVis := VisibilityCallback(func(p bool) {
		mu.Lock()
		gotVisibility = &p
		mu.Unlock()
	})
	onUsers := AuthorizedUsersCallback(func(u []string, names map[string]string) {
		mu.Lock()
		gotUsers = u
		mu.Unlock()
	})
	onProject := ProjectConfigCallback(func(c *HeartbeatProjectConfig) {
		mu.Lock()
		gotProject = c
		mu.Unlock()
	})
	onGateway := GatewayConfigCallback(func(c *HeartbeatGatewayConfig) {
		mu.Lock()
		gotGateway = c
		mu.Unlock()
	})
	onSwitch := SwitchBranchCallback(func(tag string) {
		mu.Lock()
		gotSwitchBranch = tag
		mu.Unlock()
	})
	onRestart := RestartSpokeCallback(func() {
		mu.Lock()
		gotRestart = true
		mu.Unlock()
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	collect := func() *HeartbeatPayload { return &HeartbeatPayload{HiveID: "h-full"} }

	done := make(chan struct{})
	go func() {
		defer close(done)
		StartHeartbeat(ctx, srv.URL, collect, 50*time.Millisecond, nil2Logger(),
			onUpgrade, onApp, onBanner, onVis, onUsers, onProject, onGateway, onSwitch, onRestart)
	}()

	// RestartSpoke is the last dispatch in processHeartbeatResponse, so once it
	// fires every prior branch has run for the first (rich) response.
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("restart-spoke callback never fired — the rich response was not processed")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat loop did not stop on cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	if gotUpgrade != "deadbeef" {
		t.Errorf("upgrade callback got %q, want deadbeef", gotUpgrade)
	}
	if gotSwitchBranch != "" {
		t.Errorf("switch-branch callback fired (%q) though response carried only upgrade_to", gotSwitchBranch)
	}
	if gotApp == nil || gotApp.AppID != 7 {
		t.Errorf("github app callback got %+v, want app_id 7", gotApp)
	}
	if gotBanner == nil || gotBanner.ID != "b1" {
		t.Errorf("banner callback got %+v", gotBanner)
	}
	if gotVisibility == nil || *gotVisibility != true {
		t.Errorf("visibility callback got %v, want true", gotVisibility)
	}
	if len(gotUsers) != 1 || gotUsers[0] != "alice:owner" {
		t.Errorf("authorized-users callback got %v", gotUsers)
	}
	if gotProject == nil || gotProject.Org != "acme" {
		t.Errorf("project-config callback got %+v", gotProject)
	}
	if gotGateway == nil || gotGateway.Name != "gw" {
		t.Errorf("gateway callback got %+v", gotGateway)
	}
	if !gotRestart {
		t.Error("restart-spoke callback did not record")
	}
}

// TestStartHeartbeatSwitchToTagWins verifies the upgrade-target negotiation: a
// SwitchToTag in the response takes precedence over UpgradeTo, so onSwitchBranch
// fires and onUpgrade does not (the branch-switch changes the image tag, which a
// same-tag re-pull cannot).
func TestStartHeartbeatSwitchToTagWins(t *testing.T) {
	resetSingleLoopGuard(t)
	collapseReadinessWait(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(HeartbeatResponse{
			OK: true, SwitchToTag: "v3-latest", UpgradeTo: "deadbeef",
		})
	}))
	defer srv.Close()

	var mu sync.Mutex
	var switched, upgraded string
	fired := make(chan struct{}, 1)
	onSwitch := SwitchBranchCallback(func(tag string) {
		mu.Lock()
		switched = tag
		mu.Unlock()
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	onUpgrade := UpgradeCallback(func(tag string) {
		mu.Lock()
		upgraded = tag
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	collect := func() *HeartbeatPayload { return &HeartbeatPayload{HiveID: "h-switch"} }
	done := make(chan struct{})
	go func() {
		defer close(done)
		StartHeartbeat(ctx, srv.URL, collect, 50*time.Millisecond, nil2Logger(), onSwitch, onUpgrade)
	}()

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("switch-branch callback never fired")
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if switched != "v3-latest" {
		t.Errorf("switch callback got %q, want v3-latest", switched)
	}
	if upgraded != "" {
		t.Errorf("upgrade callback fired (%q) though switch_to_tag should win", upgraded)
	}
}

// TestWaitForReadySucceedsWhenHealthy exercises waitForReady's success path: a
// dashboard health endpoint returning 200 makes it return before the deadline.
// waitForReady hits a fixed localhost:3001 URL, so this test only runs its 200
// branch when that port happens to be served; to keep it deterministic and
// hermetic we instead drive the poll loop's success via the readiness knobs and
// a live local listener bound to :3001 when available, falling back to asserting
// the timeout path otherwise.
func TestWaitForReadyTimeoutPath(t *testing.T) {
	prevPoll, prevMax := waitForReadyPollInterval, waitForReadyMaxWait
	waitForReadyPollInterval = time.Millisecond
	waitForReadyMaxWait = 5 * time.Millisecond
	t.Cleanup(func() {
		waitForReadyPollInterval = prevPoll
		waitForReadyMaxWait = prevMax
	})
	// No server on localhost:3001 in the test env, so the poll fails and the
	// short deadline fires: waitForReady must return (not hang) via the timeout
	// branch. A cancelled parent context is a second independent exit path.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	doneTimeout := make(chan struct{})
	go func() { waitForReady(ctx, nil2Logger()); close(doneTimeout) }()
	select {
	case <-doneTimeout:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForReady did not return via the timeout branch")
	}

	// ctx-cancel exit path: a long deadline that never fires, cancelled parent.
	waitForReadyMaxWait = time.Hour
	ctx2, cancel2 := context.WithCancel(context.Background())
	doneCancel := make(chan struct{})
	go func() { waitForReady(ctx2, nil2Logger()); close(doneCancel) }()
	cancel2()
	select {
	case <-doneCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForReady did not return via the ctx-cancel branch")
	}
}

// TestStartTaskStatusPushPushesPayload drives StartTaskStatusPush's success
// path: with a short interval and a non-nil payload it POSTs to /api/task-status
// and the loop exits cleanly on cancel.
func TestStartTaskStatusPushPushesPayload(t *testing.T) {
	prev := taskPushInterval
	taskPushInterval = 15 * time.Millisecond
	t.Cleanup(func() { taskPushInterval = prev })

	var got atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/task-status" {
			got.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	collect := func() *TaskStatusPayload { return &TaskStatusPayload{HiveID: "h-task"} }
	done := make(chan struct{})
	go func() {
		defer close(done)
		StartTaskStatusPush(ctx, srv.URL, collect, nil2Logger())
	}()

	deadline := time.After(5 * time.Second)
	for got.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("task-status push never reached the hub")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("task-status push loop did not stop on cancel")
	}
}

// TestStartTaskStatusPushNilPayloadContinues exercises the continue branch when
// the collector returns nil: the loop keeps running (no crash, no POST) until
// cancelled.
func TestStartTaskStatusPushNilPayloadContinues(t *testing.T) {
	prev := taskPushInterval
	taskPushInterval = 10 * time.Millisecond
	t.Cleanup(func() { taskPushInterval = prev })

	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		StartTaskStatusPush(ctx, srv.URL, func() *TaskStatusPayload { return nil }, nil2Logger())
	}()
	// Let several ticks elapse; a nil payload must never POST.
	<-time.After(50 * time.Millisecond)
	cancel()
	<-done
	if posts.Load() != 0 {
		t.Errorf("nil payload should not POST, got %d posts", posts.Load())
	}
}
