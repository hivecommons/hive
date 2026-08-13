package hub

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func testChannelLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubChannelDigests points ghcrTagDigest at a fixed tag→digest table for the
// duration of one test, and clears the channel cache on both sides so tests
// cannot leak a resolved association into each other.
func stubChannelDigests(t *testing.T, byTag map[string]string) {
	t.Helper()
	resetChannelTargetCache()
	orig := ghcrTagDigest
	ghcrTagDigest = func(repo, tag string, _ *slog.Logger) string {
		if repo != ghcrRepoSpoke {
			t.Errorf("channel resolution must read the SPOKE repo, got %q", repo)
		}
		return byTag[tag]
	}
	t.Cleanup(func() {
		ghcrTagDigest = orig
		resetChannelTargetCache()
	})
}

func resetChannelTargetCache() {
	channelTargetMu.Lock()
	channelTargetCache = nil
	channelTargetCachedAt = time.Time{}
	channelTargetMu.Unlock()
}

func targetFor(targets []ChannelTarget, channel string) ChannelTarget {
	for _, t := range targets {
		if t.Channel == channel {
			return t
		}
	}
	return ChannelTarget{}
}

// TestResolveChannelTargetsMatchesByDigest is the everyday case: all three
// channels are retags of the v4 digest, so all three attribute to v4.
func TestResolveChannelTargetsMatchesByDigest(t *testing.T) {
	const v4Digest = "sha256:aaaa"
	stubChannelDigests(t, map[string]string{
		"v2-latest":             "sha256:bbbb",
		"v4-latest":             v4Digest,
		ReleaseChannelStable:    v4Digest,
		ReleaseChannelCandidate: v4Digest,
		ReleaseChannelEdge:      v4Digest,
	})

	got := resolveChannelTargets(map[string]string{"v2": "9a87c53", "v4": "3d31590"}, testChannelLogger())
	if len(got) != len(releaseChannels) {
		t.Fatalf("got %d targets, want %d", len(got), len(releaseChannels))
	}
	for _, ch := range releaseChannels {
		tgt := targetFor(got, ch)
		if tgt.Branch != "v4" {
			t.Errorf("channel %q: branch = %q, want v4", ch, tgt.Branch)
		}
		if tgt.SHA != "3d31590" {
			t.Errorf("channel %q: sha = %q, want 3d31590", ch, tgt.SHA)
		}
	}
}

// TestResolveChannelTargetsFollowsRepoint is the POSITIVE CONTROL for the
// live-association requirement. "stable" is re-pointed at the v2 digest while
// candidate/edge stay on v4. An implementation that hardcodes "channels are
// v4" — or that labels everything with a single branch — fails here.
func TestResolveChannelTargetsFollowsRepoint(t *testing.T) {
	const (
		v2Digest = "sha256:bbbb"
		v4Digest = "sha256:aaaa"
	)
	stubChannelDigests(t, map[string]string{
		"v2-latest":             v2Digest,
		"v4-latest":             v4Digest,
		ReleaseChannelStable:    v2Digest, // re-pointed back to v2
		ReleaseChannelCandidate: v4Digest,
		ReleaseChannelEdge:      v4Digest,
	})

	got := resolveChannelTargets(map[string]string{"v2": "9a87c53", "v4": "3d31590"}, testChannelLogger())

	if tgt := targetFor(got, ReleaseChannelStable); tgt.Branch != "v2" || tgt.SHA != "9a87c53" {
		t.Errorf("stable = %q/%q, want v2/9a87c53 — the channel→branch association must follow the DIGEST, not a hardcoded release branch", tgt.Branch, tgt.SHA)
	}
	for _, ch := range []string{ReleaseChannelCandidate, ReleaseChannelEdge} {
		if tgt := targetFor(got, ch); tgt.Branch != "v4" {
			t.Errorf("%s = %q, want v4", ch, tgt.Branch)
		}
	}
}

// TestResolveChannelTargetsUnmatchedDigestGetsNoBranch is the second positive
// control: a channel pointing at a digest no tracked branch carries must be
// reported WITHOUT a branch, not silently attributed to one. This is what
// stops the UI from labelling a mid-promotion build as "v4".
func TestResolveChannelTargetsUnmatchedDigestGetsNoBranch(t *testing.T) {
	stubChannelDigests(t, map[string]string{
		"v4-latest":             "sha256:aaaa",
		ReleaseChannelStable:    "sha256:cccc", // an older pinned build
		ReleaseChannelCandidate: "sha256:aaaa",
		ReleaseChannelEdge:      "sha256:aaaa",
	})

	got := resolveChannelTargets(map[string]string{"v4": "3d31590"}, testChannelLogger())

	tgt := targetFor(got, ReleaseChannelStable)
	if tgt.Branch != "" || tgt.SHA != "" {
		t.Errorf("stable = branch %q sha %q, want both empty — an unmatched digest must not be attributed to a branch", tgt.Branch, tgt.SHA)
	}
	if tgt.Digest != "sha256:cccc" {
		t.Errorf("stable digest = %q, want sha256:cccc so the UI can still identify the build", tgt.Digest)
	}
}

// TestResolveChannelTargetsUnresolvableChannel: a channel tag that does not
// exist on GHCR yields an empty row rather than a wrong one.
func TestResolveChannelTargetsUnresolvableChannel(t *testing.T) {
	stubChannelDigests(t, map[string]string{
		"v4-latest":          "sha256:aaaa",
		ReleaseChannelStable: "sha256:aaaa",
		// candidate and edge are absent from the registry.
	})

	got := resolveChannelTargets(map[string]string{"v4": "3d31590"}, testChannelLogger())
	for _, ch := range []string{ReleaseChannelCandidate, ReleaseChannelEdge} {
		tgt := targetFor(got, ch)
		if tgt.Digest != "" || tgt.Branch != "" {
			t.Errorf("%s resolved to %q/%q though its tag does not exist", ch, tgt.Digest, tgt.Branch)
		}
	}
	if tgt := targetFor(got, ReleaseChannelStable); tgt.Branch != "v4" {
		t.Errorf("stable = %q, want v4 — one missing channel must not poison the others", tgt.Branch)
	}
}

// TestResolveChannelTargetsDeterministicOnDigestCollision: when two branches
// sit at the same commit, the attributed branch must be stable across calls
// rather than flapping with Go's map iteration order.
func TestResolveChannelTargetsDeterministicOnDigestCollision(t *testing.T) {
	const d = "sha256:aaaa"
	stubChannelDigests(t, map[string]string{
		"v2-latest":             d,
		"v4-latest":             d,
		ReleaseChannelStable:    d,
		ReleaseChannelCandidate: d,
		ReleaseChannelEdge:      d,
	})
	shas := map[string]string{"v2": "3d31590", "v4": "3d31590"}

	first := targetFor(resolveChannelTargets(shas, testChannelLogger()), ReleaseChannelStable).Branch
	for i := 0; i < 20; i++ {
		if got := targetFor(resolveChannelTargets(shas, testChannelLogger()), ReleaseChannelStable).Branch; got != first {
			t.Fatalf("attribution flapped: %q then %q", first, got)
		}
	}
}

// TestGetChannelTargetsKeepsStaleOnRegistryOutage: a GHCR blip must not blank
// the channel rows on the dashboard.
func TestGetChannelTargetsKeepsStaleOnRegistryOutage(t *testing.T) {
	stubChannelDigests(t, map[string]string{
		"v4-latest":             "sha256:aaaa",
		ReleaseChannelStable:    "sha256:aaaa",
		ReleaseChannelCandidate: "sha256:aaaa",
		ReleaseChannelEdge:      "sha256:aaaa",
	})
	shas := map[string]string{"v4": "3d31590"}

	warm := getChannelTargets(shas, testChannelLogger())
	if targetFor(warm, ReleaseChannelStable).Branch != "v4" {
		t.Fatalf("warm-up failed: %+v", warm)
	}

	// Registry goes dark and the cache is expired.
	ghcrTagDigest = func(string, string, *slog.Logger) string { return "" }
	channelTargetMu.Lock()
	channelTargetCachedAt = time.Now().Add(-2 * channelDigestTTL)
	channelTargetMu.Unlock()

	got := getChannelTargets(shas, testChannelLogger())
	if targetFor(got, ReleaseChannelStable).Branch != "v4" {
		t.Errorf("stale answer was dropped on registry outage: %+v", got)
	}
}

// TestUpgradeTargetTag: a channel is already a moving tag and must NOT get a
// "-latest" suffix; a branch must.
func TestUpgradeTargetTag(t *testing.T) {
	cases := map[string]string{
		ReleaseChannelStable:    "stable",
		ReleaseChannelCandidate: "candidate",
		ReleaseChannelEdge:      "edge",
		"v4":                    "v4-latest",
		"v2":                    "v2-latest",
		"feat/x":                "feat-x-latest",
	}
	for in, want := range cases {
		if got := upgradeTargetTag(in); got != want {
			t.Errorf("upgradeTargetTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsReleaseChannel(t *testing.T) {
	for _, c := range releaseChannels {
		if !isReleaseChannel(c) {
			t.Errorf("isReleaseChannel(%q) = false", c)
		}
	}
	for _, notCh := range []string{"v2", "v4", "", "stable-latest", "Stable"} {
		if isReleaseChannel(notCh) {
			t.Errorf("isReleaseChannel(%q) = true, want false", notCh)
		}
	}
}

func TestReleaseChannelsReturnsCopy(t *testing.T) {
	got := ReleaseChannels()
	if len(got) != len(releaseChannels) {
		t.Fatalf("got %d channels, want %d", len(got), len(releaseChannels))
	}
	got[0] = "mutated"
	if releaseChannels[0] != ReleaseChannelStable {
		t.Error("ReleaseChannels() exposed the package slice — a caller mutated it")
	}
}
