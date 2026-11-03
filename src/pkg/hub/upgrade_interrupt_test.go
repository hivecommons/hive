package hub

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestUpgradeInterrupts_ArmPendingTTLClear(t *testing.T) {
	t.Parallel()
	u := newUpgradeInterrupts()
	now := time.Date(2026, 10, 28, 17, 0, 0, 0, time.UTC)

	if _, ok := u.pending("h1", now); ok {
		t.Fatal("nothing armed yet")
	}
	u.arm("h1", "abc1234", "alice", now)
	req, ok := u.pending("h1", now.Add(time.Minute))
	if !ok || req.By != "alice" || req.Target != "abc1234" {
		t.Fatalf("pending = %+v, %v", req, ok)
	}
	if _, ok := u.pending("h1", now.Add(upgradeInterruptTTL+time.Second)); ok {
		t.Fatal("expired interrupt must not be pending")
	}
	if _, ok := u.pending("h1", now); ok {
		t.Fatal("expired interrupt must be forgotten, not resurrected")
	}
	u.arm("h1", "abc1234", "alice", now)
	u.clear("h1")
	if _, ok := u.pending("h1", now); ok {
		t.Fatal("clear must drop the request")
	}
	u.arm("", "x", "y", now)
	if len(u.byID) != 0 {
		t.Fatal("empty hive id must be ignored")
	}
	var nilStore *upgradeInterrupts
	nilStore.arm("h", "t", "b", now)
	nilStore.clear("h")
	if _, ok := nilStore.pending("h", now); ok {
		t.Fatal("nil store is inert")
	}
}

func TestUpgradeInterruptReq_AppliesTo(t *testing.T) {
	t.Parallel()
	full := "551ffc34f0123456789abcdef0123456789abcde"
	cases := []struct {
		name, reqTarget, target string
		want                    bool
	}{
		{"same sha", full, full, true},
		{"short vs full", "551ffc34f", full, true},
		{"request before target known", "", full, true},
		{"instruction without target", full, "", true},
		{"different target", "deadbeef0", full, false},
	}
	for _, tc := range cases {
		if got := (upgradeInterruptReq{Target: tc.reqTarget}).appliesTo(tc.target); got != tc.want {
			t.Errorf("%s: appliesTo = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestUpgradeInterruptRequested(t *testing.T) {
	t.Parallel()
	for q, want := range map[string]bool{
		"": false, "?interrupt=1": true, "?interrupt=true": true, "?interrupt=YES": true,
		"?interrupt=0": false, "?interrupt=no": false, "?other=1": false,
	} {
		r := httptest.NewRequest("POST", "/api/saas/hives/h/upgrade"+q, nil)
		if got := upgradeInterruptRequested(r); got != want {
			t.Errorf("%q: got %v, want %v", q, got, want)
		}
	}
	if upgradeInterruptRequested(nil) {
		t.Fatal("nil request")
	}
}

// An armed "Upgrade now (interrupt turns)" rides on the heartbeat that
// carries the upgrade instruction, and only then — a hive that is not being
// told to upgrade never sees the flag, and a different hive never does.
func TestHeartbeatCarriesUpgradeInterruptOnlyWithUpgradeTo(t *testing.T) {
	resetCommitOrderState(t)
	stubBranchHead(t, "v2", "526ef71")
	stubChannelRevisions(t, map[string]string{"stable": "77ba848"})
	srv := newHubServerForTest(t)
	srv.setHubSecret("")
	now := time.Now()
	srv.upgradeInterrupts.arm("chan-spoke", "77ba848", "owner", now)
	srv.upgradeInterrupts.arm("chan-other", "77ba848", "owner", now)

	resp := postChannelHeartbeat(t, srv,
		`{"hive_id":"chan-spoke","git_hash":"old1111","git_branch":"v2","auto_upgrade":true,`+
			`"image_ref":"ghcr.io/hivecommons/hive:stable"}`)
	if resp.UpgradeTo != "77ba848" || !resp.UpgradeInterrupt {
		t.Fatalf("got upgrade_to=%q interrupt=%v, want 77ba848 with interrupt", resp.UpgradeTo, resp.UpgradeInterrupt)
	}

	// Already at the channel: no instruction, so no interrupt either.
	resp = postChannelHeartbeat(t, srv,
		`{"hive_id":"chan-other","git_hash":"77ba848","git_branch":"v2","auto_upgrade":true,`+
			`"image_ref":"ghcr.io/hivecommons/hive:stable"}`)
	if resp.UpgradeTo != "" || resp.UpgradeInterrupt {
		t.Fatalf("got upgrade_to=%q interrupt=%v, want neither", resp.UpgradeTo, resp.UpgradeInterrupt)
	}

	// Plain hive with nothing armed.
	resp = postChannelHeartbeat(t, srv,
		`{"hive_id":"chan-third","git_hash":"old1111","git_branch":"v2","auto_upgrade":true,`+
			`"image_ref":"ghcr.io/hivecommons/hive:stable"}`)
	if resp.UpgradeTo != "77ba848" || resp.UpgradeInterrupt {
		t.Fatalf("got upgrade_to=%q interrupt=%v, want instruction without interrupt", resp.UpgradeTo, resp.UpgradeInterrupt)
	}
}
