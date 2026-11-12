package hub

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"testing"
)

// ============================================================
// A release-channel follower rolls once per upgrade (#17978)
// ============================================================
//
// The spoke pinned every channel upgrade to the target's SHA tag (#10719) and
// the next beat's tracked-channel re-arm switched it straight back: two rolls
// per upgrade. Now the hub tells the spoke when its channel tag already carries
// the target (UpgradeChannelTag, one roll on the channel tag), and leaves a pin
// to the SHA it just sent alone (the channel lagged; the next upgrade returns
// the spoke to the channel in the same roll).

const (
	singleRollOld  = "aaa1111"
	singleRollNew  = "bbb2222"
	singleRollHive = "edge-follower"
)

// setupEdgeFollower builds a hub whose spoke-managed hive follows :edge, with
// the edge tag at channelSHA and per-SHA images published for both builds, so
// the reachable target is always singleRollNew.
func setupEdgeFollower(t *testing.T, channelSHA string, pin *DigestPin) *HubServer {
	t.Helper()
	resetCommitOrderState(t)
	stubCommitCompare(func(string, string, *slog.Logger) (string, error) {
		return "", errors.New("no compare API in tests")
	})
	// The edge tag lags the target when it still carries the old build.
	seedCommitOrder(singleRollOld, singleRollNew, false)
	stubBranchHead(t, "v2", singleRollNew)
	stubChannelRevisions(t, map[string]string{ReleaseChannelEdge: channelSHA})
	stubChannelImageSelection(t,
		[]branchSHAInfo{{SHA: singleRollNew}, {SHA: singleRollOld}},
		map[string]bool{singleRollNew: true, singleRollOld: true}, true)
	t.Cleanup(helperSetupTempDirs(t))
	srv := newHubServerForTest(t)
	srv.setHubSecret("")
	if err := saveSaaSHive(&SaaSHive{ID: singleRollHive, TrackedChannel: ReleaseChannelEdge, DigestPin: pin}); err != nil {
		t.Fatalf("saveSaaSHive: %v", err)
	}
	return srv
}

func edgeFollowerBeat(t *testing.T, srv *HubServer, gitHash, tag string, upgrading bool) HeartbeatResponse {
	t.Helper()
	up := "false"
	if upgrading {
		up = "true"
	}
	rec := postHeartbeat(t, srv,
		`{"hive_id":"`+singleRollHive+`","git_hash":"`+gitHash+`","git_branch":"v2","auto_upgrade":true,`+
			`"upgrading":`+up+`,"image_ref":"ghcr.io/hivecommons/hive:`+tag+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp HeartbeatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("heartbeat: decoding response: %v", err)
	}
	return resp
}

func TestHeartbeatUpgradeCarriesChannelTagWhenChannelHasTarget(t *testing.T) {
	srv := setupEdgeFollower(t, singleRollNew, nil)

	resp := edgeFollowerBeat(t, srv, singleRollOld, ReleaseChannelEdge, false)
	if resp.UpgradeTo != singleRollNew {
		t.Fatalf("upgrade_to = %q, want %q", resp.UpgradeTo, singleRollNew)
	}
	if resp.UpgradeChannelTag != ReleaseChannelEdge {
		t.Errorf("upgrade_channel_tag = %q, want %q — :edge already carries the target", resp.UpgradeChannelTag, ReleaseChannelEdge)
	}
}

func TestHeartbeatUpgradeOmitsChannelTagWhenChannelLags(t *testing.T) {
	srv := setupEdgeFollower(t, singleRollOld, nil)

	resp := edgeFollowerBeat(t, srv, singleRollOld, ReleaseChannelEdge, false)
	if resp.UpgradeTo != singleRollNew {
		t.Fatalf("upgrade_to = %q, want %q", resp.UpgradeTo, singleRollNew)
	}
	if resp.UpgradeChannelTag != "" {
		t.Errorf("upgrade_channel_tag = %q, want empty — :edge lags the target, so the spoke must pin", resp.UpgradeChannelTag)
	}
}

// The follower reports the SHA tag it was just sent: no switch back.
func TestHeartbeatNoReArmForFollowerPinnedToSentTarget(t *testing.T) {
	srv := setupEdgeFollower(t, singleRollOld, nil)
	edgeFollowerBeat(t, srv, singleRollOld, ReleaseChannelEdge, false)

	resp := edgeFollowerBeat(t, srv, singleRollNew, singleRollNew, false)
	if resp.SwitchToTag != "" {
		t.Errorf("switch_to_tag = %q, want none — the pin is the target the hub sent", resp.SwitchToTag)
	}
	if got := armedSwitchTag(srv, singleRollHive); got != "" {
		t.Errorf("heartbeatSwitchTag = %q, want empty", got)
	}
}

// A follower on a tag the hub did not send is still switched back.
func TestHeartbeatReArmsFollowerOnUnrelatedTag(t *testing.T) {
	for _, tag := range []string{"ccc3333", "v2-latest"} {
		t.Run(tag, func(t *testing.T) {
			srv := setupEdgeFollower(t, singleRollOld, nil)
			edgeFollowerBeat(t, srv, singleRollOld, ReleaseChannelEdge, false)

			resp := edgeFollowerBeat(t, srv, singleRollNew, tag, false)
			if resp.SwitchToTag != ReleaseChannelEdge {
				t.Errorf("switch_to_tag = %q, want %q for unrelated tag %q", resp.SwitchToTag, ReleaseChannelEdge, tag)
			}
		})
	}
}

// Without any upgrade sent, a SHA tag is drift as before.
func TestHeartbeatReArmsFollowerOnSHATagNeverSent(t *testing.T) {
	srv := setupEdgeFollower(t, singleRollNew, nil)

	resp := edgeFollowerBeat(t, srv, singleRollNew, singleRollNew, false)
	if resp.SwitchToTag != ReleaseChannelEdge {
		t.Errorf("switch_to_tag = %q, want %q", resp.SwitchToTag, ReleaseChannelEdge)
	}
}

func TestHeartbeatPauseStillBlocksFollowerReArm(t *testing.T) {
	srv := setupEdgeFollower(t, singleRollOld, nil)
	pauseSpokes(t, srv, true)

	resp := edgeFollowerBeat(t, srv, singleRollNew, "v2-latest", false)
	if resp.SwitchToTag != "" || resp.UpgradeTo != "" {
		t.Errorf("paused: switch_to_tag=%q upgrade_to=%q, want neither", resp.SwitchToTag, resp.UpgradeTo)
	}
	if got := armedSwitchTag(srv, singleRollHive); got != "" {
		t.Errorf("paused: heartbeatSwitchTag = %q, want empty", got)
	}
}

func TestHeartbeatDigestPinStillBlocksFollowerReArm(t *testing.T) {
	srv := setupEdgeFollower(t, singleRollOld, &DigestPin{
		Digest: "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		By:     "op",
	})

	resp := edgeFollowerBeat(t, srv, singleRollNew, "v2-latest", false)
	if resp.SwitchToTag != "" || resp.UpgradeTo != "" {
		t.Errorf("pinned: switch_to_tag=%q upgrade_to=%q, want neither", resp.SwitchToTag, resp.UpgradeTo)
	}
	if got := armedSwitchTag(srv, singleRollHive); got != "" {
		t.Errorf("pinned: heartbeatSwitchTag = %q, want empty", got)
	}
}

// imageChanges counts the image/template changes a spoke would make for a
// sequence of hub responses: every SwitchToTag, and each distinct UpgradeTo
// (the spoke acts on the first and ignores re-sends of the same target).
func imageChanges(resps []HeartbeatResponse) int {
	n := 0
	seen := map[string]bool{}
	for _, r := range resps {
		switch {
		case r.SwitchToTag != "":
			n++
		case r.UpgradeTo != "" && !seen[r.UpgradeTo]:
			seen[r.UpgradeTo] = true
			n++
		}
	}
	return n
}

// The live sequence: instruct, then the old pod mid-roll, the new pod, and the
// settled pod. At most one image or template change, in both cases.
func TestChannelFollowerUpgradeSequenceRollsOnce(t *testing.T) {
	cases := []struct {
		name       string
		channelSHA string
		landedTag  string // the tag the spoke's single roll leaves it on
	}{
		{"channel carries target", singleRollNew, ReleaseChannelEdge},
		{"channel lags target", singleRollOld, singleRollNew},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := setupEdgeFollower(t, tc.channelSHA, nil)
			var resps []HeartbeatResponse
			first := edgeFollowerBeat(t, srv, singleRollOld, ReleaseChannelEdge, false)
			if first.UpgradeTo != singleRollNew {
				t.Fatalf("upgrade_to = %q, want %q", first.UpgradeTo, singleRollNew)
			}
			wantTag := ""
			if tc.landedTag == ReleaseChannelEdge {
				wantTag = ReleaseChannelEdge
			}
			if first.UpgradeChannelTag != wantTag {
				t.Fatalf("upgrade_channel_tag = %q, want %q", first.UpgradeChannelTag, wantTag)
			}
			resps = append(resps,
				first,
				edgeFollowerBeat(t, srv, singleRollOld, ReleaseChannelEdge, true), // old pod mid-roll
				edgeFollowerBeat(t, srv, singleRollNew, tc.landedTag, false),      // new pod
				edgeFollowerBeat(t, srv, singleRollNew, tc.landedTag, false),      // settled pod
			)
			if n := imageChanges(resps); n != 1 {
				t.Errorf("image/template changes instructed = %d, want exactly 1: %+v", n, resps)
			}
		})
	}
}
