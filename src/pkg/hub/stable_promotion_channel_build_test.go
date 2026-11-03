package hub

import "testing"

// A stable record must describe ONE build. When the live revision label has
// moved past the cached ChannelTarget, the target's digest and commit date
// belong to the older build and must not be published next to the newer SHA
// (that mix read as "a03ccd0, digest of 58c09b1, promoted 2026-10-24").
func TestChannelBuildDropsStaleDigestWhenRevisionMoved(t *testing.T) {
	target := ChannelTarget{Channel: ReleaseChannelStable, SHA: "58c09b1", Digest: "sha256:6fa5182e", CommittedAt: "2026-10-24T23:11:41Z"}

	got := channelBuild(target, "a03ccd0")
	if got.SHA != "a03ccd0" {
		t.Fatalf("SHA = %q, want the live revision a03ccd0", got.SHA)
	}
	if got.Digest != "" || got.BuiltAt != "" {
		t.Fatalf("stale digest/built_at leaked onto the newer SHA: digest=%q built_at=%q", got.Digest, got.BuiltAt)
	}
}

func TestChannelBuildKeepsSnapshotWhenRevisionAgrees(t *testing.T) {
	target := ChannelTarget{Channel: ReleaseChannelStable, SHA: "58c09b13", Digest: "sha256:6fa5182e", CommittedAt: "2026-10-24T23:11:41Z"}

	for _, live := range []string{"", "58c09b1"} {
		got := channelBuild(target, live)
		if got.SHA != "58c09b13" || got.Digest != "sha256:6fa5182e" || got.BuiltAt != "2026-10-24T23:11:41Z" {
			t.Fatalf("live=%q: snapshot altered: %+v", live, got)
		}
	}
}
