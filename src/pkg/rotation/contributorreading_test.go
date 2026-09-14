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
	for _, p := range []string{"anthropic", "openai", "google"} {
		if !contributorGuardProviders[p] {
			t.Errorf("%q should be a guard-supported provider", p)
		}
	}
	if contributorGuardProviders["deepseek"] {
		t.Errorf("deepseek is metered and must not be guard-supported")
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
