package rotation

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A failed probe must NEVER publish a healthy reading, no matter what the
// fail-open Available flag says. This is the single most important correctness
// property of #6967 (kubestellar/hive#6909, #6951, #6833).
func TestHeadroomToContributorReading_ProbeErrorIsUnknownNotHealthy(t *testing.T) {
	// failOpen() sets Available:true — the fail-open shape rotation relies on.
	h := failOpen("openai", errors.New("codex app-server unreachable"))
	if !h.Available {
		t.Fatalf("precondition: failOpen should mark Available true, got %+v", h)
	}
	r := HeadroomToContributorReading(h)
	if r.State != "unknown" {
		t.Errorf("probe error published state %q, want \"unknown\" — a failed measurement must HOLD, never admit", r.State)
	}
	if len(r.Limits) != 0 {
		t.Errorf("probe error carried %d windows, want none", len(r.Limits))
	}
	if !strings.Contains(r.Error, "codex app-server unreachable") {
		t.Errorf("probe error text not published: %+v", r)
	}
}

// TestHeadroomToContributorReading_UnknownCarriesCause pins that the published
// reading distinguishes WHY it is unknown (kubestellar/hive#6986): a dead
// adapter (unrecognized_schema) must publish a different cause than a host with
// no credentials (no_credentials) and than a missing CLI (not_installed), so a
// silently-broken adapter is not invisible behind an `unknown` that also means
// "no credentials on this host". The state stays "unknown" (a hold) in every
// case — the cause never softens the fail-safe direction.
func TestHeadroomToContributorReading_UnknownCarriesCause(t *testing.T) {
	cases := []struct {
		name  string
		cause ProbeErrorCause
		want  string
	}{
		{"dead adapter", ProbeCauseUnrecognizedSchema, "unrecognized_schema"},
		{"no credentials", ProbeCauseNoCredentials, "no_credentials"},
		{"not installed", ProbeCauseNotInstalled, "not_installed"},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		h := failOpen("google", errors.New("boom"))
		h.ProbeErrCause = tc.cause
		r := HeadroomToContributorReading(h)
		if r.State != "unknown" {
			t.Errorf("%s: state = %q, want unknown (must still hold)", tc.name, r.State)
		}
		if r.Cause != tc.want {
			t.Errorf("%s: cause = %q, want %q", tc.name, r.Cause, tc.want)
		}
		seen[r.Cause] = true
	}
	if len(seen) != len(cases) {
		t.Errorf("distinct causes collapsed: %v — the #6986 states are not distinguishable", seen)
	}

	// An uncategorized failure defaults to the generic probe_failed rather than
	// claiming a cause it does not know.
	r := HeadroomToContributorReading(failOpen("openai", errors.New("codex app-server unreachable")))
	if r.Cause != "probe_failed" {
		t.Errorf("uncategorized failure cause = %q, want probe_failed", r.Cause)
	}

	// A healthy reading carries no cause.
	if got := HeadroomToContributorReading(Headroom{Provider: "openai", Available: true}); got.Cause != "" {
		t.Errorf("healthy reading cause = %q, want empty", got.Cause)
	}
}

func TestHeadroomToContributorReading_HealthyCarriesWindows(t *testing.T) {
	reset := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	h := Headroom{
		Provider:  "openai",
		Available: true,
		Limits: []LimitWindow{
			{ID: "five_hour", Kind: "five_hour", PctRemaining: 82, ResetAt: reset},
			{ID: "weekly", Kind: "weekly", PctRemaining: 40},
		},
	}
	r := HeadroomToContributorReading(h)
	if r.State != "available" {
		t.Fatalf("state = %q, want available", r.State)
	}
	if len(r.Limits) != 2 {
		t.Fatalf("limits = %d, want 2", len(r.Limits))
	}
	if r.Limits[0].PctRemaining != 82 || r.Limits[0].Kind != "five_hour" || r.Limits[0].ID != "five_hour" {
		t.Errorf("window 0 = %+v", r.Limits[0])
	}
	if r.Limits[0].ResetEpoch != reset.UnixMilli() {
		t.Errorf("reset_epoch = %d, want %d", r.Limits[0].ResetEpoch, reset.UnixMilli())
	}
	if r.Limits[0].ResetsAt != "2026-01-02T03:04:05Z" {
		t.Errorf("resets_at = %q", r.Limits[0].ResetsAt)
	}
	// A window with no reset time must not fabricate one.
	if r.Limits[1].ResetEpoch != 0 || r.Limits[1].ResetsAt != "" {
		t.Errorf("window without reset carried one: %+v", r.Limits[1])
	}
}

func TestHeadroomToContributorReading_StaleRateLimitCarriesLastGoodWindows(t *testing.T) {
	captured := time.Date(2026, 9, 24, 19, 50, 0, 0, time.UTC)
	reset := time.Date(2026, 9, 24, 23, 0, 0, 0, time.UTC)
	h := Headroom{
		Provider:      "anthropic",
		Available:     true,
		CapturedAt:    captured,
		Stale:         true,
		ProbeErr:      errors.New("claude usage HTTP 429"),
		ProbeErrCause: ProbeCauseRateLimited,
		Limits:        []LimitWindow{{ID: "weekly_all", Kind: "weekly", PctRemaining: 61, ResetAt: reset}},
	}
	r := HeadroomToContributorReading(h)
	if r.State != "available" {
		t.Fatalf("state = %q, want available from stale last-good", r.State)
	}
	if r.Cause != "rate_limited" || r.Error == "" {
		t.Fatalf("rate-limit metadata missing: %+v", r)
	}
	if r.CapturedAt != "2026-09-24T19:50:00Z" {
		t.Fatalf("CapturedAt = %q, want last-good capture time", r.CapturedAt)
	}
	if len(r.Limits) != 1 || r.Limits[0].PctRemaining != 61 || r.Limits[0].ResetEpoch != reset.UnixMilli() {
		t.Fatalf("limits = %+v, want retained last-good window", r.Limits)
	}
}

func TestHeadroomToContributorReading_CarriesCapturedAt(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)
	r := HeadroomToContributorReading(Headroom{Provider: "openai", Available: true})
	after := time.Now().UTC().Add(time.Second)
	if r.CapturedAt == "" {
		t.Fatal("CapturedAt is empty; relay cannot judge freshness without captured_at")
	}
	captured, err := time.Parse(time.RFC3339, r.CapturedAt)
	if err != nil {
		t.Fatalf("CapturedAt = %q, want RFC3339: %v", r.CapturedAt, err)
	}
	if captured.Before(before) || captured.After(after) {
		t.Errorf("CapturedAt = %s, want between %s and %s", captured, before, after)
	}
}

func TestContributorReadingCapturedAt_WireFormatMatchesRelay(t *testing.T) {
	r := ContributorReading{State: "available", CapturedAt: "2026-01-02T03:04:05Z", Limits: []ContributorLimitWindow{}}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["captured_at"] != "2026-01-02T03:04:05Z" {
		t.Errorf("captured_at = %#v, want relay RFC3339 field/value", got["captured_at"])
	}
	legacy, err := json.Marshal(ContributorReading{State: "available", Limits: []ContributorLimitWindow{}})
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if strings.Contains(string(legacy), "captured_at") {
		t.Errorf("empty CapturedAt should omit captured_at for version-skew safety, got %s", legacy)
	}
}

// DefaultContributorPoolDir makes publishing default-on (kubestellar/hive#6987):
// no explicit HIVE_CONTRIBUTOR_QUOTA_POOL_DIR, yet a supported backend still gets
// a reading. XDG_CONFIG_HOME is honoured first (matching the hivectl session
// cache), so this vector is the shared parity point with defaultContributorPool
// Dir() in bin/contributor-relay.js — both MUST produce the identical path or
// the publisher writes where the relay never reads. Keep this in step with the
// JS test '#6987 defaultContributorPoolDir matches Go under XDG_CONFIG_HOME'.
func TestDefaultContributorPoolDir_XDGParity(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/shared/base")
	got := DefaultContributorPoolDir()
	want := filepath.Join("/shared/base", "hive", "contributor-quota")
	if got != want {
		t.Errorf("DefaultContributorPoolDir() = %q, want %q (drifted from contributor-relay.js)", got, want)
	}
}

// With XDG_CONFIG_HOME unset the path still resolves under the platform user
// config dir's hive tree, so a default install has a route. The exact prefix is
// platform-specific; the invariant we pin is the trailing hive/contributor-quota
// segment and a non-empty result on a host with a resolvable config dir.
func TestDefaultContributorPoolDir_FallsBackToUserConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	ucd, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("no user config dir on this host: %v", err)
	}
	got := DefaultContributorPoolDir()
	want := filepath.Join(ucd, "hive", "contributor-quota")
	if got != want {
		t.Errorf("DefaultContributorPoolDir() = %q, want %q", got, want)
	}
}

// A default install (no explicit pool dir) still publishes and the reading lands
// exactly where the relay's derived path looks for it — the end-to-end proof of
// #6987 on the producer side. We drive the Manager publish path with the derived
// dir and assert the pool-keyed file the relay would read is present and healthy.
func TestManager_PublishesToDefaultPoolDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	poolDir := DefaultContributorPoolDir()

	m := NewManager(rotationTestConfig())
	m.EnableContributorReadingPublish(poolDir, "")
	m.SetHeadroom(Headroom{
		Provider:  "anthropic",
		Available: true,
		Limits:    []LimitWindow{{ID: "weekly", Kind: "weekly", PctRemaining: 42}},
	})

	path := ContributorReadingPath(poolDir, "claude", "")
	var r ContributorReading
	readJSONFile(t, path, &r)
	if r.State != "available" || len(r.Limits) != 1 || r.Limits[0].PctRemaining != 42 {
		t.Errorf("published reading = %+v, want available/weekly/42 at derived default path %q", r, path)
	}
}

// The Go pool key must match derivePoolKey() in bin/lib/quota-pool-store.js
// exactly, or the publisher writes to a path the relay never reads. These
// vectors were produced by the JS implementation; keep both sides in step.
func TestDeriveContributorPoolKey_MatchesJS(t *testing.T) {
	cases := map[string]string{
		"claude":  "1f49f53cdfcecfbc",
		"codex":   "d6316145f173a075",
		"agy":     "77e6ed5ee0f8c5f2",
		"copilot": "72733404fc10338c",
	}
	for backend, want := range cases {
		if got := deriveContributorPoolKey(backend, ""); got != want {
			t.Errorf("deriveContributorPoolKey(%q,\"\") = %q, want %q (drifted from quota-pool-store.js)", backend, got, want)
		}
	}
	// Trimming/lowercasing must match too.
	if a, b := deriveContributorPoolKey("  Claude  ", ""), deriveContributorPoolKey("claude", ""); a != b {
		t.Errorf("key not normalized: %q vs %q", a, b)
	}
}

// The write must be atomic: a reader either sees the whole new file or the old
// one, never a partial. We assert the property indirectly by proving no stray
// temp file is left behind and the final file is complete valid JSON.
func TestPublishContributorReadingAtomic_WritesCompleteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "12ab.reading.json")
	r := ContributorReading{State: "available", Limits: []ContributorLimitWindow{{ID: "weekly", Kind: "weekly", PctRemaining: 30}}}
	if err := publishContributorReadingAtomic(path, r); err != nil {
		t.Fatalf("publish: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got ContributorReading
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("published file is not complete JSON: %v", err)
	}
	if got.State != "available" || len(got.Limits) != 1 || got.Limits[0].PctRemaining != 30 {
		t.Errorf("round-trip = %+v", got)
	}
	// No temp sibling may survive a successful write (rename consumed it).
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("stray temp file left behind: %s", e.Name())
		}
	}
}

// End-to-end on the Go side: a Manager with publishing enabled writes a real
// reading, at the pool-keyed path, for a supported backend after a probe — and
// a failed probe publishes "unknown", not a healthy reading.
func TestManager_PublishesReadingForSupportedBackend(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(rotationTestConfig())
	m.EnableContributorReadingPublish(dir, "")

	// Healthy probe for anthropic (fronts "claude","pi").
	m.SetHeadroom(Headroom{
		Provider:  "anthropic",
		Available: true,
		Limits:    []LimitWindow{{ID: "weekly", Kind: "weekly", PctRemaining: 55}},
	})
	claudePath := ContributorReadingPath(dir, "claude", "")
	var reading ContributorReading
	readJSONFile(t, claudePath, &reading)
	if reading.State != "available" || len(reading.Limits) != 1 || reading.Limits[0].PctRemaining != 55 {
		t.Errorf("claude reading = %+v", reading)
	}
	// Same reading published for every backend fronting the provider.
	readJSONFile(t, ContributorReadingPath(dir, "pi", ""), &reading)
	if reading.State != "available" {
		t.Errorf("pi reading state = %q", reading.State)
	}

	// A failed probe must overwrite with "unknown", never leave/emit healthy.
	m.SetHeadroom(failOpen("anthropic", errors.New("usage API 500")))
	readJSONFile(t, claudePath, &reading)
	if reading.State != "unknown" || len(reading.Limits) != 0 {
		t.Errorf("after failed probe, published reading = %+v, want unknown/no-limits", reading)
	}
}

func TestManager_DoesNotPublishWhenDisabledOrUnsupported(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(rotationTestConfig())
	// Publishing not enabled: nothing written.
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: true, Limits: []LimitWindow{{Kind: "weekly", PctRemaining: 10}}})
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("publish happened while disabled: %v", entries)
	}
	// Enabled, but a metered/unsupported provider is never published as a
	// windowed quota reading.
	m.EnableContributorReadingPublish(dir, "")
	m.SetHeadroom(Headroom{Provider: "deepseek", Available: true, PctRemaining: 90})
	if _, err := os.Stat(ContributorReadingPath(dir, "litellm", "")); !os.IsNotExist(err) {
		t.Errorf("deepseek/litellm was published; metered providers must not be")
	}
}

func readJSONFile(t *testing.T, path string, v interface{}) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}

// Guard against an accidental drift in which providers are considered
// guard-supported.
func TestContributorGuardProviders(t *testing.T) {
	for _, p := range []string{"anthropic", "openai", "google", "aws-kiro"} {
		if !contributorGuardProviders[p] {
			t.Errorf("%q should be a guard-supported provider", p)
		}
	}
	if contributorGuardProviders["deepseek"] {
		t.Errorf("deepseek is metered and must not be guard-supported")
	}
}

// The publish-only default backend map (kubestellar/hive#6987) must flatten to
// exactly QUOTA_GUARD_SUPPORTED_BACKENDS in bin/contributor-relay.js
// ({claude, pi, codex, agy, gemini, kiro}): a backend the relay guards but this map
// omits gets no reading and silently stays on the unprovisioned admit; a
// backend here the relay does not guard writes files nothing reads. Every
// provider in the map must itself be guard-supported.
func TestContributorGuardDefaultBackends_MatchesRelaySupportedSet(t *testing.T) {
	want := map[string]bool{"claude": true, "pi": true, "codex": true, "agy": true, "gemini": true, "kiro": true}
	got := map[string]bool{}
	for provider, backends := range contributorGuardDefaultBackends {
		if !contributorGuardProviders[provider] {
			t.Errorf("default backends name provider %q that is not guard-supported", provider)
		}
		for _, b := range backends {
			if got[b] {
				t.Errorf("backend %q listed under more than one provider", b)
			}
			got[b] = true
		}
	}
	for b := range want {
		if !got[b] {
			t.Errorf("backend %q is in the relay's QUOTA_GUARD_SUPPORTED_BACKENDS but has no default provider mapping", b)
		}
	}
	for b := range got {
		if !want[b] {
			t.Errorf("backend %q mapped here but not in the relay's QUOTA_GUARD_SUPPORTED_BACKENDS (drifted from contributor-relay.js)", b)
		}
	}
}

// A publish-only manager (rotation disabled, kubestellar/hive#6987 condition
// (a)) publishes a healthy reading to every default backend fronting the
// provider — with no operator-authored rotation config at all.
func TestContributorReadingPublisher_PublishesForDefaultBackends(t *testing.T) {
	dir := t.TempDir()
	m := NewContributorReadingPublisher(dir, "")
	m.SetHeadroom(Headroom{
		Provider:  "google",
		Available: true,
		Limits:    []LimitWindow{{ID: "weekly", Kind: "weekly", PctRemaining: 63}},
	})
	for _, backend := range []string{"agy", "gemini"} {
		var r ContributorReading
		readJSONFile(t, ContributorReadingPath(dir, backend, ""), &r)
		if r.State != "available" || len(r.Limits) != 1 || r.Limits[0].PctRemaining != 63 {
			t.Errorf("%s reading = %+v, want available/weekly/63", backend, r)
		}
	}
}

// In publish-only mode a not_installed probe failure publishes NOTHING: an
// absent CLI cannot spend quota on this host, and a published `unknown` would
// flip a co-located relay for a backend the host never had from the
// unprovisioned admit to a permanent hold — the #6951 fleet-wide stop. Any
// OTHER failure still publishes `unknown` (a hold): skipping those would be a
// fail-open.
func TestContributorReadingPublisher_SkipsNotInstalledButPublishesOtherFailures(t *testing.T) {
	dir := t.TempDir()
	m := NewContributorReadingPublisher(dir, "")

	h := failOpen("openai", errors.New("codex: command not found"))
	h.ProbeErrCause = ProbeCauseNotInstalled
	m.SetHeadroom(h)
	if _, err := os.Stat(ContributorReadingPath(dir, "codex", "")); !os.IsNotExist(err) {
		t.Errorf("not_installed probe published a reading; an absent CLI must stay unprovisioned, not become a hold")
	}

	m.SetHeadroom(failOpen("openai", errors.New("usage API 500")))
	var r ContributorReading
	readJSONFile(t, ContributorReadingPath(dir, "codex", ""), &r)
	if r.State != "unknown" || r.Cause != "probe_failed" {
		t.Errorf("generic probe failure reading = %+v, want unknown/probe_failed (must still hold)", r)
	}
}

// The not_installed skip is exclusive to the publish-only constructor: under
// operator-configured rotation the provider was named explicitly, so
// unknown/not_installed is deliberate signal and keeps publishing (the #6983
// behaviour, unchanged).
func TestManager_RotationModeStillPublishesNotInstalled(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(rotationTestConfig())
	m.EnableContributorReadingPublish(dir, "")

	h := failOpen("anthropic", errors.New("claude: command not found"))
	h.ProbeErrCause = ProbeCauseNotInstalled
	m.SetHeadroom(h)
	var r ContributorReading
	readJSONFile(t, ContributorReadingPath(dir, "claude", ""), &r)
	if r.State != "unknown" || r.Cause != "not_installed" {
		t.Errorf("rotation-mode not_installed reading = %+v, want unknown/not_installed published", r)
	}
}

// An empty dir leaves the publish-only manager publishing nothing — the same
// "no route ⇒ no hold" contract as EnableContributorReadingPublish, so an
// unresolvable config dir can never strand a relay.
func TestContributorReadingPublisher_EmptyDirPublishesNothing(t *testing.T) {
	m := NewContributorReadingPublisher("", "")
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: true, Limits: []LimitWindow{{Kind: "weekly", PctRemaining: 10}}})
	// No panic and no dir to inspect: the contract is simply that publishing is
	// off. Assert via the manager's own state.
	if m.contributorPublishDir != "" {
		t.Errorf("empty dir should leave publishing disabled")
	}
}

// Atomicity under a concurrent reader: a relay reading the file while the
// publisher rewrites it must never observe a partial document. rename() makes
// this hold; a naive direct write would let the reader catch a half-written
// file. The two readings differ enough in size that a torn large write is
// invalid JSON, so a non-atomic writer is caught here.
func TestPublishContributorReadingAtomic_ConcurrentReaderNeverSeesPartial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cc.reading.json")

	big := ContributorReading{State: "available"}
	for i := 0; i < 200; i++ {
		big.Limits = append(big.Limits, ContributorLimitWindow{ID: "w", Kind: "weekly", PctRemaining: 42, ResetsAt: "2026-01-02T03:04:05Z"})
	}
	small := ContributorReading{State: "unknown", Limits: []ContributorLimitWindow{}}

	if err := publishContributorReadingAtomic(path, small); err != nil {
		t.Fatalf("seed: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3000; i++ {
			r := big
			if i%2 == 0 {
				r = small
			}
			if err := publishContributorReadingAtomic(path, r); err != nil {
				t.Errorf("write: %v", err)
				return
			}
		}
	}()

	for i := 0; i < 6000; i++ {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // absent is fine; partial is not
			}
			t.Fatalf("read: %v", err)
		}
		var got ContributorReading
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("concurrent read saw a PARTIAL file (write was not atomic): %v", err)
		}
		if got.State != "available" && got.State != "unknown" {
			t.Fatalf("concurrent read saw an inconsistent state %q", got.State)
		}
	}
	<-done
}

// ── kubestellar/hive#6987 — condition (b): the publisher presence marker ──────

// Publishing a reading also refreshes the pool's presence marker, so its mtime
// stays fresh for as long as the publisher is alive. The relay treats a fresh
// marker with a missing reading as "publisher expected" and HOLDS; a marker
// nothing refreshes goes stale and the relay falls back to the non-stranding
// unprovisioned admit.
func TestPublishContributorReading_RefreshesPresenceMarker(t *testing.T) {
	dir := t.TempDir()
	m := NewContributorReadingPublisher(dir, "")
	m.SetHeadroom(Headroom{Provider: "openai", Available: true, Limits: []LimitWindow{{Kind: "weekly", PctRemaining: 50}}})

	var marker contributorPublisherMarker
	readJSONFile(t, ContributorPublisherMarkerPath(dir, "codex", ""), &marker)
	if marker.PID != os.Getpid() || marker.IntervalMS != pollInterval.Milliseconds() {
		t.Errorf("marker = %+v, want pid %d and interval %d", marker, os.Getpid(), pollInterval.Milliseconds())
	}
}

// announceContributorPublisher declares presence for every guard-supported pool
// BEFORE the first probe completes, so the relay holds through the startup
// "publisher expected, no reading yet" window instead of admitting blind.
func TestAnnounceContributorPublisher_WritesMarkersForAllGuardPools(t *testing.T) {
	dir := t.TempDir()
	m := NewContributorReadingPublisher(dir, "")
	m.announceContributorPublisher()

	for _, backend := range []string{"claude", "pi", "codex", "agy", "gemini"} {
		if _, err := os.Stat(ContributorPublisherMarkerPath(dir, backend, "")); err != nil {
			t.Errorf("no presence marker for %s after announce: %v", backend, err)
		}
		if _, err := os.Stat(ContributorReadingPath(dir, backend, "")); !os.IsNotExist(err) {
			t.Errorf("announce must declare presence only, never fabricate a reading for %s", backend)
		}
	}
}

// An empty dir announces nothing — the same "no route ⇒ no write" contract as
// publishing.
func TestAnnounceContributorPublisher_EmptyDirWritesNothing(t *testing.T) {
	m := NewContributorReadingPublisher("", "")
	m.announceContributorPublisher() // must not panic or create anything
}

// A not_installed probe REMOVES the announce-time marker in publish-only mode:
// the probe has established no reading will ever come, and a marker left behind
// would hold the pool until it went stale instead of restoring the
// unprovisioned admit an absent CLI must keep.
func TestContributorReadingPublisher_NotInstalledRemovesMarker(t *testing.T) {
	dir := t.TempDir()
	m := NewContributorReadingPublisher(dir, "")
	m.announceContributorPublisher()
	if _, err := os.Stat(ContributorPublisherMarkerPath(dir, "codex", "")); err != nil {
		t.Fatalf("precondition: announce marker missing: %v", err)
	}

	h := failOpen("openai", errors.New("codex: command not found"))
	h.ProbeErrCause = ProbeCauseNotInstalled
	m.SetHeadroom(h)

	for _, backend := range []string{"codex"} {
		if _, err := os.Stat(ContributorPublisherMarkerPath(dir, backend, "")); !os.IsNotExist(err) {
			t.Errorf("marker for %s survived a not_installed probe; it would hold a pool no reading will ever reach", backend)
		}
	}
	// Other providers' markers are untouched.
	if _, err := os.Stat(ContributorPublisherMarkerPath(dir, "claude", "")); err != nil {
		t.Errorf("not_installed for openai must not remove anthropic's marker: %v", err)
	}
}

// Under operator-configured rotation not_installed keeps publishing `unknown`
// (deliberate signal), so the marker is refreshed, not removed.
func TestManager_RotationModeNotInstalledKeepsMarker(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(rotationTestConfig())
	m.EnableContributorReadingPublish(dir, "")

	h := failOpen("anthropic", errors.New("claude: command not found"))
	h.ProbeErrCause = ProbeCauseNotInstalled
	m.SetHeadroom(h)
	if _, err := os.Stat(ContributorPublisherMarkerPath(dir, "claude", "")); err != nil {
		t.Errorf("rotation-mode not_installed must keep declaring presence (it published a holding reading): %v", err)
	}
}

// The marker path must match quotaDefaultPublisherMarkerFile() in
// bin/contributor-relay.js: same pool key, same `.publisher.json` suffix.
func TestContributorPublisherMarkerPath_MatchesJS(t *testing.T) {
	got := ContributorPublisherMarkerPath("/base", "claude", "")
	if want := "/base/1f49f53cdfcecfbc.publisher.json"; got != want {
		t.Errorf("marker path = %q, want %q (drifted from quotaDefaultPublisherMarkerFile in contributor-relay.js)", got, want)
	}
}
