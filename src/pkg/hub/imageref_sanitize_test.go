package hub

import "testing"

// TestSanitizeImageRefRoundTrip pins the bug that made 11 healthy hives render
// a CRITICAL "pinned-image" badge: the shared heartbeat sanitizer's allowlist
// omitted the colon, so "ghcr.io/hivecommons/hive:v2-latest" arrived at the
// drift rule as "ghcr.io/hivecommons/hivev2-latest". With no tag separator left
// imageTagIsMutable said "not mutable", and the hub told the operator a rolling
// hive could never be upgraded.
//
// An image ref must survive the hub verbatim, while still being sanitized: a
// hostile spoke must not be able to smuggle markup into a hub-rendered string.
func TestSanitizeImageRefRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		why  string
	}{
		{
			name: "mutable branch tag survives verbatim",
			in:   "ghcr.io/hivecommons/hive:v2-latest",
			want: "ghcr.io/hivecommons/hive:v2-latest",
			why:  "the live ref on every vllm-d and hive-oke deployment",
		},
		{
			name: "digest pin survives verbatim",
			in:   "ghcr.io/hivecommons/hive@sha256:9f2c1ab34de5",
			want: "ghcr.io/hivecommons/hive@sha256:9f2c1ab34de5",
			why:  "both @ and : are legal in an OCI digest reference",
		},
		{
			name: "sha tag pin survives verbatim",
			in:   "ghcr.io/hivecommons/hive:63d8902",
			want: "ghcr.io/hivecommons/hive:63d8902",
			why:  "a genuine pin must reach the drift rule intact so it is still flagged",
		},
		{
			name: "registry port survives verbatim",
			in:   "registry.internal:5000/kubestellar/hive:v2-latest",
			want: "registry.internal:5000/kubestellar/hive:v2-latest",
			why:  "a port colon must not be confused with a tag colon",
		},
		{
			name: "markup is still stripped",
			in:   "ghcr.io/x/hive:<script>alert(1)</script>",
			want: "ghcr.io/x/hive:scriptalert1/script",
			why:  "sanitization must keep working — a hostile spoke cannot inject markup",
		},
		{
			name: "quotes and spaces are stripped",
			in:   `ghcr.io/x/hive:v2-latest" onload="x`,
			want: "ghcr.io/x/hive:v2-latestonloadx",
			why:  "attribute-breaking characters have no place in an image ref",
		},
		{
			name: "empty stays empty",
			in:   "",
			want: "",
			why:  "unknown image must stay unknown, not become a bogus value",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeImageRef(c.in); got != c.want {
				t.Errorf("sanitizeImageRef(%q) = %q, want %q (%s)", c.in, got, c.want, c.why)
			}
		})
	}
}

// TestSanitizeHeartbeatFieldStillRejectsColon guards the scope decision: the
// colon was added to a DEDICATED image-ref sanitizer, not to the shared one.
// sanitizeHeartbeatField is applied to ~12 fields (hive IDs, agent names,
// governor mode, owner, leaderboard usernames); none of those may contain a
// colon, and widening the shared allowlist would have loosened all of them.
func TestSanitizeHeartbeatFieldStillRejectsColon(t *testing.T) {
	if got := sanitizeHeartbeatField("agent:evil"); got != "agentevil" {
		t.Errorf("sanitizeHeartbeatField must still strip ':' from non-image fields; got %q", got)
	}
}

// TestImageRefTagParseFailureIsSilent is the defence-in-depth half of the fix.
//
// A ref with no tag separator at all is a MALFORMED value, not a legitimate
// immutable pin — exactly what the mangling produced. imageTagIsMutable
// answers "is a restart useful?" and correctly says false there (the caller
// must take the safe image-patch path). The drift rule asks a different
// question — "is this hive definitively pinned?" — and an unparseable ref is
// not evidence of a pin. It must degrade to silence, as an empty ref already
// does, so a future mangling can never again produce a critical alert about a
// healthy hive.
func TestImageRefTagParseFailureIsSilent(t *testing.T) {
	cases := []struct {
		name   string
		ref    string
		pinned bool
		why    string
	}{
		{"mutable tag", "ghcr.io/hivecommons/hive:v2-latest", false, "rolling tag, no signal"},
		{"mangled colonless ref", "ghcr.io/hivecommons/hivev2-latest", false, "unparseable — unknown, not pinned"},
		{"bare repo, no tag", "ghcr.io/hivecommons/hive", false, "implicit :latest — do not guess"},
		{"registry port but no tag", "registry.internal:5000/kubestellar/hive", false, "port is not a tag"},
		{"empty", "", false, "unknown image"},
		{"sha tag pin", "ghcr.io/hivecommons/hive:63d8902", true, "genuine pin — MUST still be flagged"},
		{"release tag pin", "ghcr.io/hivecommons/hive:v2.1.0", true, "immutable release tag is a real pin"},
		{"digest pin", "ghcr.io/hivecommons/hive@sha256:abc123", true, "digest pin is a real pin"},
		{"port plus sha pin", "registry.internal:5000/kubestellar/hive:63d8902", true, "genuine pin behind a port"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := imageRefIsPinned(c.ref); got != c.pinned {
				t.Errorf("imageRefIsPinned(%q) = %v, want %v (%s)", c.ref, got, c.pinned, c.why)
			}
		})
	}
}

// TestComputeDriftNoPinnedSignalForMangledRef is the end-to-end regression: a
// healthy rolling hive whose ref lost its colon in transit must produce NO
// pinned-image signal at all, let alone a critical one.
func TestComputeDriftNoPinnedSignalForMangledRef(t *testing.T) {
	norm := computeFleetNorm(normFleet())
	latest := map[string]string{"v2": "abc1234"}

	cases := []struct {
		name       string
		ref        string
		wantSignal bool
	}{
		{"healthy rolling tag", "ghcr.io/hivecommons/hive:v2-latest", false},
		{"mangled colonless ref", "ghcr.io/hivecommons/hivev2-latest", false},
		{"genuine sha pin", "ghcr.io/hivecommons/hive:63d8902", true},
		{"genuine digest pin", "ghcr.io/hivecommons/hive@sha256:abc123", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := healthyEntry("h1")
			h.ImageRef = c.ref
			kinds := driftKindsOf(computeDrift(h, norm, latest, driftNow))
			sig, got := kinds[DriftKindPinnedImage]
			if got != c.wantSignal {
				t.Fatalf("ref %q: pinned-image signal present = %v, want %v (signals: %+v)",
					c.ref, got, c.wantSignal, kinds)
			}
			if got && sig.Severity != DriftCritical {
				t.Errorf("ref %q: a genuine pin should stay critical, got %q", c.ref, sig.Severity)
			}
		})
	}
}

// TestHeartbeatImageRefSurvivesIngest proves the fix at the boundary the bug
// actually lived at: the ref written into the registry entry by the heartbeat
// handler's sanitization must still parse as a rolling tag.
func TestHeartbeatImageRefSurvivesIngest(t *testing.T) {
	const live = "ghcr.io/hivecommons/hive:v2-latest"
	stored := sanitizeImageRef(live)
	if stored != live {
		t.Fatalf("ingest mangled the ref: %q -> %q", live, stored)
	}
	if imageRefIsPinned(stored) {
		t.Errorf("stored ref %q must not read as pinned", stored)
	}
	if !imageTagIsMutable(stored) {
		t.Errorf("stored ref %q must still read as mutable for the upgrade path", stored)
	}
}
