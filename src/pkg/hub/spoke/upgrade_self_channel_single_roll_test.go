package spoke

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ============================================================
// UpgradeSelfToSHA on a release-channel follower rolls once (#17978)
//
// The hub says, via UpgradeChannelTag, when the spoke's channel tag already
// carries the upgrade target. Then the spoke must roll once ON the channel tag
// (pod-template annotation, image unchanged) — pinning to the SHA tag made the
// hub switch it straight back, a second roll. Without the hint (channel lags,
// or an old hub) the #10719 pin stays.
// ============================================================

// fakeSelfDeployment serves GETs of a Deployment running image and records
// every PATCH body.
func fakeSelfDeployment(t *testing.T, image string) *[]string {
	t.Helper()
	var mu sync.Mutex
	var patches []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"` + image + `"}]}}}}`))
		case http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			patches = append(patches, string(body))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	t.Cleanup(srv.Close)
	withFakeK8sAPI(t, srv)
	return &patches
}

func withUpgradeChannelHint(t *testing.T, target, channel string) {
	t.Helper()
	setUpgradeChannelTag(target, channel)
	t.Cleanup(func() { upgradeChannel.Store(nil) })
}

func TestUpgradeSelfToSHA_ChannelCarriesTarget_AnnotatesChannelTag(t *testing.T) {
	patches := fakeSelfDeployment(t, "ghcr.io/hivecommons/hive:edge")
	withUpgradeChannelHint(t, "bf3c61a", "edge")

	needsRestart, err := UpgradeSelfToSHA(slog.Default(), "bf3c61a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if needsRestart {
		t.Fatal("the annotation patch rolls the deployment itself; needsRestart = true")
	}
	if len(*patches) != 1 {
		t.Fatalf("got %d patches, want exactly one (one roll): %v", len(*patches), *patches)
	}
	p := (*patches)[0]
	if !strings.Contains(p, selfUpgradeTargetAnnotation) || !strings.Contains(p, "bf3c61a") {
		t.Errorf("patch = %s, want the upgrade-target annotation for bf3c61a", p)
	}
	if strings.Contains(p, `"image"`) {
		t.Errorf("patch = %s, must not change the image when the channel carries the target", p)
	}
}

func TestUpgradeSelfToSHA_ChannelLags_PinsToSHATag(t *testing.T) {
	patches := fakeSelfDeployment(t, "ghcr.io/hivecommons/hive:edge")
	// The hub said nothing about the channel carrying this target.
	withUpgradeChannelHint(t, "bf3c61a", "")

	if _, err := UpgradeSelfToSHA(slog.Default(), "bf3c61a"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(*patches, "\n")
	if !strings.Contains(joined, "ghcr.io/hivecommons/hive:bf3c61a") {
		t.Errorf("patches = %s, want the #10719 pin to the immutable SHA image", joined)
	}
	if strings.Contains(joined, selfUpgradeTargetAnnotation) {
		t.Errorf("patches = %s, a lagging channel must pin, not annotate the channel tag", joined)
	}
}

// A hint sent with a different UpgradeTo must not steer this upgrade.
func TestUpgradeSelfToSHA_ChannelHintForOtherTarget_Ignored(t *testing.T) {
	patches := fakeSelfDeployment(t, "ghcr.io/hivecommons/hive:edge")
	withUpgradeChannelHint(t, "0000000", "edge")

	if _, err := UpgradeSelfToSHA(slog.Default(), "bf3c61a"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if joined := strings.Join(*patches, "\n"); !strings.Contains(joined, "ghcr.io/hivecommons/hive:bf3c61a") {
		t.Errorf("patches = %s, want the SHA pin when the hint names another target", joined)
	}
}

// A follower left on a SHA tag by an earlier lagging upgrade returns to its
// channel tag in the same roll that lands the next target.
func TestUpgradeSelfToSHA_PinnedFollower_ReturnsToChannelTag(t *testing.T) {
	patches := fakeSelfDeployment(t, "ghcr.io/hivecommons/hive:bf3c61a")
	withUpgradeChannelHint(t, "e242050", "edge")

	needsRestart, err := UpgradeSelfToSHA(slog.Default(), "e242050")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if needsRestart {
		t.Fatal("the image patch rolls the deployment itself; needsRestart = true")
	}
	joined := strings.Join(*patches, "\n")
	if !strings.Contains(joined, "ghcr.io/hivecommons/hive:edge") {
		t.Errorf("patches = %s, want the image switched back to the channel tag", joined)
	}
	if strings.Contains(joined, "ghcr.io/hivecommons/hive:e242050") {
		t.Errorf("patches = %s, must not pin again when the channel carries the target", joined)
	}
}

// Without a hint a pinned follower keeps today's SHA-to-SHA patch.
func TestUpgradeSelfToSHA_PinnedFollowerChannelLags_StaysPinned(t *testing.T) {
	patches := fakeSelfDeployment(t, "ghcr.io/hivecommons/hive:bf3c61a")
	withUpgradeChannelHint(t, "e242050", "")

	if _, err := UpgradeSelfToSHA(slog.Default(), "e242050"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if joined := strings.Join(*patches, "\n"); !strings.Contains(joined, "ghcr.io/hivecommons/hive:e242050") {
		t.Errorf("patches = %s, want the SHA-to-SHA pin while the channel lags", joined)
	}
}

// A hint naming something that is not a release channel is ignored.
func TestUpgradeChannelTagFor_RejectsNonChannel(t *testing.T) {
	withUpgradeChannelHint(t, "bf3c61a", "v4-latest")
	if got := upgradeChannelTagFor("bf3c61a"); got != "" {
		t.Errorf("upgradeChannelTagFor = %q, want empty for a non-channel tag", got)
	}
	setUpgradeChannelTag("bf3c61a", "edge")
	if got := upgradeChannelTagFor("bf3c61a"); got != "edge" {
		t.Errorf("upgradeChannelTagFor = %q, want edge", got)
	}
}
