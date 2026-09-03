package github

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
)

// Tests for advisory-digest repeat suppression (#5507).
//
// The bug these guard: ~250 digest comments accumulated on a third-party
// issue, the vast majority reading "0 findings", because the pre-existing
// #4818 body hash included FormatDigestMarkdown's GeneratedAt stamps and so
// differed on every render. The load-bearing assertion in this file is
// therefore the timestamp-exclusion proof (TestMaterialFingerprint_*): if that
// regresses, every other guard here silently stops firing.

// renderZeroFindingDigest renders an empty digest through the REAL formatter at
// the given generation time. Using the production renderer (rather than a
// hand-written fixture) is deliberate: a fixture would only prove the
// fingerprint ignores stamps the test author remembered to include, whereas
// this proves it against the exact bytes the governor posts.
func renderZeroFindingDigest(t *testing.T, at time.Time) string {
	t.Helper()
	md := advisory.FormatDigestMarkdown(&advisory.Digest{
		GeneratedAt: at,
		Mode:        "advisory",
		ByAgent:     map[string][]advisory.Finding{},
		TotalCount:  0,
	}, advisory.DigestOptions{ShowEmpty: true, Org: "testorg", PrimaryRepo: "testrepo"})
	if md == "" {
		t.Fatal("renderer returned empty digest; test fixture is vacuous")
	}
	return md
}

// renderFindingDigest renders a digest carrying n findings at the given time.
func renderFindingDigest(t *testing.T, at time.Time, n int) string {
	t.Helper()
	findings := make([]advisory.Finding, 0, n)
	for i := 0; i < n; i++ {
		findings = append(findings, advisory.Finding{
			Agent:     "quality",
			Timestamp: at,
			Type:      "coverage-gap",
			Severity:  "high",
			Title:     "uncovered branch in handler " + string(rune('A'+i)),
			Detail:    "add a test for the error path",
		})
	}
	md := advisory.FormatDigestMarkdown(&advisory.Digest{
		GeneratedAt: at,
		Mode:        "advisory",
		ByAgent:     map[string][]advisory.Finding{"quality": findings},
		TotalCount:  n,
	}, advisory.DigestOptions{Org: "testorg", PrimaryRepo: "testrepo"})
	if md == "" {
		t.Fatal("renderer returned empty digest; test fixture is vacuous")
	}
	return md
}

// TestMaterialFingerprint_ExcludesTimestamps is THE proof that the fix is not
// a no-op. Two digests with an identical (empty) finding set are rendered a
// full 37 hours apart, which forces a different calendar DATE, a different
// clock time and a different RFC3339 stamp — so this can never pass by
// accident because both renders happened in the same second.
func TestMaterialFingerprint_ExcludesTimestamps(t *testing.T) {
	t1 := time.Date(2026, 8, 30, 4, 0, 11, 0, time.UTC)
	t2 := t1.Add(37 * time.Hour)

	a := renderZeroFindingDigest(t, t1)
	b := renderZeroFindingDigest(t, t2)

	// Guard the guard: if the renderer ever stops stamping the body, this test
	// would pass trivially while proving nothing about exclusion.
	if a == b {
		t.Fatal("rendered bodies are byte-identical across a 37h gap — the renderer no longer stamps a timestamp, so this test cannot prove timestamp EXCLUSION; re-point it at whatever volatile field replaced it")
	}
	if !strings.Contains(a, "2026-08-30") || !strings.Contains(b, "2026-08-31") {
		t.Fatalf("expected distinct calendar dates in the two bodies:\nA=%q\nB=%q", a, b)
	}

	if got, want := materialDigestFingerprint(a), materialDigestFingerprint(b); got != want {
		t.Errorf("material fingerprint differs across renders that changed only the timestamp:\n got=%s\nwant=%s\nnormalized A:\n%s\nnormalized B:\n%s",
			got, want, materialDigestContent(a), materialDigestContent(b))
	}
}

// TestMaterialFingerprint_NormalizedContentHasNoTimestamps asserts on the
// normalization directly. Hash equality alone cannot show WHICH volatile field
// leaked, so this checks the stripped text contains no date-shaped residue.
func TestMaterialFingerprint_NormalizedContentHasNoTimestamps(t *testing.T) {
	at := time.Date(2026, 8, 30, 4, 0, 11, 0, time.UTC)
	normalized := materialDigestContent(renderZeroFindingDigest(t, at))
	for _, residue := range []string{"2026-08-30", "04:00", "T04:00:11Z"} {
		if strings.Contains(normalized, residue) {
			t.Errorf("normalized digest still contains volatile substring %q — it will leak into the hash and defeat suppression:\n%s", residue, normalized)
		}
	}
}

// TestMaterialFingerprint_DetectsRealContentChange is the counterweight: the
// exclusion must not be so aggressive that it erases the findings themselves.
func TestMaterialFingerprint_DetectsRealContentChange(t *testing.T) {
	at := time.Date(2026, 8, 30, 4, 0, 11, 0, time.UTC)
	zero := renderZeroFindingDigest(t, at)
	three := renderFindingDigest(t, at, 3)

	if materialDigestFingerprint(zero) == materialDigestFingerprint(three) {
		t.Fatal("0-finding and 3-finding digests fingerprint identically — the normalization is stripping material content, not just timestamps")
	}

	// Same count, different finding text must also differ.
	one := renderFindingDigest(t, at, 1)
	two := renderFindingDigest(t, at, 2)
	if materialDigestFingerprint(one) == materialDigestFingerprint(two) {
		t.Error("1-finding and 2-finding digests fingerprint identically")
	}
}

func TestIsZeroFindingDigest(t *testing.T) {
	at := time.Date(2026, 8, 30, 4, 0, 11, 0, time.UTC)
	if !isZeroFindingDigest(renderZeroFindingDigest(t, at)) {
		t.Error("empty digest not recognized as zero-finding")
	}
	if isZeroFindingDigest(renderFindingDigest(t, at, 3)) {
		t.Error("3-finding digest misread as zero-finding")
	}
	// Unreadable body: must fail SAFE (not zero-finding), so the zero-finding
	// cap never suppresses a digest whose count it could not parse.
	if isZeroFindingDigest("## 🐝 Advisory Digest\n\nsomething the parser does not know") {
		t.Error("unparseable digest treated as zero-finding; the cap would suppress content it cannot read")
	}
	// A count of 10 must not match a prefix-y "0".
	if isZeroFindingDigest("**Findings:** 10\n") {
		t.Error("**Findings:** 10 misread as zero-finding")
	}
}

// TestEvaluateDigestSuppression_FirstPostAlwaysAllowed pins the hasPosted
// short-circuit. The inputs are chosen so that WITHOUT that short-circuit the
// call would suppress: the pending body is passed as its own baseline, so the
// identical guard would fire on it. Only the "no digest comment on the target
// yet" check can let it through. (A naive version of this test that passed
// posted="" proves nothing — an empty baseline fingerprints differently from
// any real digest, so it is allowed through whether the guard exists or not.)
func TestEvaluateDigestSuppression_FirstPostAlwaysAllowed(t *testing.T) {
	at := time.Date(2026, 8, 30, 4, 0, 11, 0, time.UTC)
	pending := renderZeroFindingDigest(t, at)

	// Sanity: with hasPosted=true these same inputs MUST suppress, otherwise
	// the assertion below is not exercising the short-circuit.
	if sup := evaluateDigestSuppression(pending, pending, true, at, at); !sup.suppress {
		t.Fatal("control case did not suppress; this test cannot prove the first-post short-circuit")
	}

	if sup := evaluateDigestSuppression(pending, pending, false, at, at); sup.suppress {
		t.Fatalf("first digest on a target was suppressed (reason=%s); a fresh advisory issue would stay empty forever", sup.reason)
	}
}

func TestEvaluateDigestSuppression_IdenticalContentSuppressed(t *testing.T) {
	t1 := time.Date(2026, 8, 30, 4, 0, 11, 0, time.UTC)
	t2 := t1.Add(4 * time.Hour) // the observed ~4h repost cadence
	posted := renderZeroFindingDigest(t, t1)
	pending := renderZeroFindingDigest(t, t2)

	sup := evaluateDigestSuppression(pending, posted, true, t1, t2)
	if !sup.suppress {
		t.Fatal("a materially identical re-post was NOT suppressed — this is the exact 250-comment bug")
	}
	if sup.reason != suppressReasonIdentical {
		t.Errorf("reason = %q, want %q", sup.reason, suppressReasonIdentical)
	}
}

// TestEvaluateDigestSuppression_ChangedContentPostsInsideWindow is the design
// caution from the issue: 0 findings → 3 findings is news and must post even
// well inside the 48h zero-finding window.
func TestEvaluateDigestSuppression_ChangedContentPostsInsideWindow(t *testing.T) {
	t1 := time.Date(2026, 8, 30, 4, 0, 11, 0, time.UTC)
	t2 := t1.Add(1 * time.Hour) // deep inside zeroFindingDigestMinInterval
	posted := renderZeroFindingDigest(t, t1)
	pending := renderFindingDigest(t, t2, 3)

	if sup := evaluateDigestSuppression(pending, posted, true, t1, t2); sup.suppress {
		t.Fatalf("0-findings → 3-findings was suppressed (reason=%s) — real news would be swallowed", sup.reason)
	}

	// And the reverse transition, 3 findings → 0 (everything fixed), is also
	// news: it must post rather than be caught by the zero-finding cap.
	postedThree := renderFindingDigest(t, t1, 3)
	pendingZero := renderZeroFindingDigest(t, t2)
	if sup := evaluateDigestSuppression(pendingZero, postedThree, true, t1, t2); sup.suppress {
		t.Fatalf("3-findings → 0-findings was suppressed (reason=%s) — the all-clear would never be posted", sup.reason)
	}
}

// TestEvaluateDigestSuppression_ZeroFindingCap covers guard 2: two zero-finding
// digests whose text DRIFTS materially (so the identical guard cannot catch
// them) are still capped to one per 48h.
func TestEvaluateDigestSuppression_ZeroFindingCap(t *testing.T) {
	t1 := time.Date(2026, 8, 30, 4, 0, 11, 0, time.UTC)
	posted := renderZeroFindingDigest(t, t1)
	// Force material drift while keeping the count at zero, so this test
	// exercises the CAP and not the identical guard.
	pending := renderZeroFindingDigest(t, t1.Add(4*time.Hour)) + "\n_scanned 41 files_\n"
	if materialDigestFingerprint(pending) == materialDigestFingerprint(posted) {
		t.Fatal("fixtures fingerprint identically; this test would exercise the identical guard, not the zero-finding cap")
	}

	// Pin the window to the value the issue specifies. Expressing the
	// boundaries below only in terms of the constant would make this test move
	// WITH any change to it — including a change to 1s that disables the cap in
	// practice — so the contract is asserted against a literal here and the
	// boundary cases are anchored on absolute offsets.
	if zeroFindingDigestMinInterval != 48*time.Hour {
		t.Fatalf("zeroFindingDigestMinInterval = %v, want 48h (#5507: at most one zero-finding digest per target per 48h)", zeroFindingDigestMinInterval)
	}

	// Just under 48h: capped.
	inside := t1.Add(47 * time.Hour)
	sup := evaluateDigestSuppression(pending, posted, true, t1, inside)
	if !sup.suppress {
		t.Fatal("drifting zero-finding digest inside the 48h window was not capped")
	}
	if sup.reason != suppressReasonZeroFindingCap {
		t.Errorf("reason = %q, want %q", sup.reason, suppressReasonZeroFindingCap)
	}

	// Just past 48h: posts again, so the freshness signal never dies.
	outside := t1.Add(49 * time.Hour)
	if sup := evaluateDigestSuppression(pending, posted, true, t1, outside); sup.suppress {
		t.Fatalf("zero-finding digest suppressed 49h after the last post (reason=%s); the digest would freeze permanently", sup.reason)
	}
}
