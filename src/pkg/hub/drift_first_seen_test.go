package hub

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// ============================================================
// server.go — sanitizeProseField vs sanitizeHeartbeatField
//
// The drift hover rendered "TheGitHubAppisconfiguredbuthasnoinstallation."
// because spoke-reported PROSE (App-permission diagnoses, upgrade errors)
// went through the identifier sanitizer, which deletes spaces. Prose fields
// now take sanitizeProseField, which keeps the sentence readable while still
// neutralizing control characters and markup.
// ============================================================

func TestSanitizeProseFieldPreservesSentencesVerbatim(t *testing.T) {
	cases := []string{
		"The GitHub App is configured but has no installation. Install the app on your org to enable agents.",
		"This hive is authenticating as GitHub App installation 150350880 which belongs to another org.",
		"missing contents:write (have: read)",
		"Upgrade to v2-latest failed: image pull back-off",
	}
	for _, in := range cases {
		if got := sanitizeProseField(in); got != in {
			t.Errorf("sanitizeProseField(%q) = %q, want the sentence verbatim", in, got)
		}
	}
}

func TestSanitizeProseFieldNeutralizesDangerousInput(t *testing.T) {
	in := "line one\r\nline\ttwo \x1b[31mred\x00 <script>alert('x')</script> a & b `tick`"
	got := sanitizeProseField(in)
	for _, bad := range []string{"<", ">", "&", "`", "\n", "\r", "\t", "\x1b", "\x00"} {
		if strings.Contains(got, bad) {
			t.Errorf("sanitizeProseField left %q in %q", bad, got)
		}
	}
	// Newlines/tabs become spaces and runs collapse, so the words stay separated.
	if !strings.Contains(got, "line one line two") {
		t.Errorf("newlines should become single spaces, got %q", got)
	}
	// No tag survives even if a render path forgets to escape.
	if strings.Contains(got, "<script") {
		t.Errorf("markup survived sanitization: %q", got)
	}
}

func TestSanitizeProseFieldCapsLength(t *testing.T) {
	long := strings.Repeat("word ", maxProseFieldRunes)
	if n := len([]rune(sanitizeProseField(long))); n > maxProseFieldRunes {
		t.Errorf("sanitized prose is %d runes, want <= %d", n, maxProseFieldRunes)
	}
}

// Identifier-shaped fields must NOT be loosened by the prose fix: an org, a
// branch or an agent state still may not carry spaces or markup at all.
func TestSanitizeHeartbeatFieldStaysStrictForIdentifiers(t *testing.T) {
	if got := sanitizeHeartbeatField("my org name"); got != "myorgname" {
		t.Errorf("identifier sanitizer must still strip spaces, got %q", got)
	}
	if got := sanitizeHeartbeatField("v2 <b>"); got != "v2b" {
		t.Errorf("identifier sanitizer must still strip markup, got %q", got)
	}
}

// End-to-end through handleHeartbeat: the prose fields reach the registry with
// their spaces intact, while an identifier field on the same payload is still
// stripped strictly.
func TestHeartbeatProseFieldsKeepSpaces(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	const permIssue = "The GitHub App is configured but has no installation. Install the app on your org to enable agents."
	body := fmt.Sprintf(`{"hive_id":"prose-hive","org":"hivecommons","primary_repo":"hive",`+
		`"github_app_perm_issue":%q,"advisory_error":"advisory post failed: HTTP 403 from GitHub",`+
		`"git_branch":"v 2"}`, permIssue)
	rec := postHeartbeat(t, s, body)
	if rec.Code != 200 {
		t.Fatalf("heartbeat status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	var entry *RegistryEntry
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == "prose-hive" {
			entry = &s.registry.Hives[i]
		}
	}
	if entry == nil {
		t.Fatal("heartbeat did not register the hive")
	}
	if entry.GitHubAppPermIssue != permIssue {
		t.Errorf("GitHubAppPermIssue = %q, want the sentence verbatim %q", entry.GitHubAppPermIssue, permIssue)
	}
	if entry.AdvisoryError != "advisory post failed: HTTP 403 from GitHub" {
		t.Errorf("AdvisoryError = %q, want spaces preserved", entry.AdvisoryError)
	}
	// The identifier on the same payload stays strict.
	if entry.GitBranch != "v2" {
		t.Errorf("GitBranch = %q, want the strict sanitizer to still strip the space", entry.GitBranch)
	}
}

// Health-check details are composed into drift reasons from an unsanitized
// free-form map, so they are neutralized at composition — with the prose
// sanitizer, keeping "disk almost full" readable.
func TestHealthFailingChecksSanitizesButKeepsSpaces(t *testing.T) {
	health := map[string]any{
		"status": "degraded",
		"checks": []any{
			map[string]any{"name": "disk space", "status": "warn", "detail": "disk almost full <b>93%</b>"},
		},
	}
	got := healthFailingChecks(health)
	if len(got) != 1 {
		t.Fatalf("expected 1 failing check, got %v", got)
	}
	if got[0] != "disk space (warn: disk almost full b93%/b)" {
		t.Errorf("failing check = %q, want spaces kept and markup stripped", got[0])
	}
}

// ============================================================
// drift.go — first-seen tracking
//
// stampDriftFirstSeen keeps one instant per (hive, kind): stable while the
// signal persists across recomputes, cleared when it clears, fresh when it
// recurs. The hover renders it as "since 3:42 PM".
// ============================================================

// firstSeenFleet returns a norm-sized fleet plus one broken hive whose only
// deliberate fault is ACMM unset (and the no-agents that rides along with
// AgentCount forced to keep healthyEntry defaults elsewhere).
func firstSeenFleet(id string, acmmLevel int) []MyHiveEntry {
	broken := healthyEntry(id)
	broken.ACMMLevel = acmmLevel
	return append(normFleet(), broken)
}

func TestDriftFirstSeenStableWhileSignalPersists(t *testing.T) {
	const hive = "first-seen-stable-hive"
	latest := map[string]string{"v2": "abc1234"}
	t0 := driftNow

	hives := firstSeenFleet(hive, 0)
	annotateDrift(hives, latest, t0)
	sig, ok := driftKindsOf(hives[3].Drift)[DriftKindACMMUnset]
	if !ok {
		t.Fatal("test setup: acmm-unset signal expected")
	}
	if sig.FirstSeen != rfc(t0) {
		t.Fatalf("first observation: FirstSeen = %q, want %q", sig.FirstSeen, rfc(t0))
	}

	// Recompute from scratch five minutes later with the signal still present:
	// the stamp must not move.
	hives2 := firstSeenFleet(hive, 0)
	annotateDrift(hives2, latest, t0.Add(5*time.Minute))
	sig2 := driftKindsOf(hives2[3].Drift)[DriftKindACMMUnset]
	if sig2.FirstSeen != rfc(t0) {
		t.Fatalf("FirstSeen drifted on recompute: %q, want stable %q", sig2.FirstSeen, rfc(t0))
	}
}

func TestDriftFirstSeenResetsAfterSignalClears(t *testing.T) {
	const hive = "first-seen-reset-hive"
	latest := map[string]string{"v2": "abc1234"}
	t0 := driftNow

	annotateDrift(firstSeenFleet(hive, 0), latest, t0)

	// The fault is fixed: the signal clears, and the tracker forgets it.
	fixed := firstSeenFleet(hive, 3)
	annotateDrift(fixed, latest, t0.Add(10*time.Minute))
	if _, ok := driftKindsOf(fixed[3].Drift)[DriftKindACMMUnset]; ok {
		t.Fatal("acmm-unset should have cleared at level 3")
	}

	// It recurs later: the stamp must be the recurrence time, not the
	// resurrected original.
	t2 := t0.Add(20 * time.Minute)
	again := firstSeenFleet(hive, 0)
	annotateDrift(again, latest, t2)
	sig := driftKindsOf(again[3].Drift)[DriftKindACMMUnset]
	if sig.FirstSeen != rfc(t2) {
		t.Fatalf("recurrence FirstSeen = %q, want fresh %q", sig.FirstSeen, rfc(t2))
	}
}
