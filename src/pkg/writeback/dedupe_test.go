package writeback

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clock is the injected fake time for the content layer tests.
type clock struct{ now time.Time }

func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newDedupeGate(t *testing.T, cfg Config, path string) (*Gate, *clock, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	clk := &clock{now: t0.Add(2 * time.Hour)}
	g := New(cfg, Deps{
		LevelFor: func(string) int { return 6 },
		Logger:   slog.New(slog.NewTextHandler(&buf, nil)),
		Now:      func() time.Time { return clk.now },
	}, path)
	g.ObserveRepo("org/repo")
	return g, clk, &buf
}

func foreignPR(n int) Item {
	return Item{Number: n, IsPR: true, Author: "contributor", Labels: []string{"Hive-Review"}, CreatedAt: t0.Add(time.Hour)}
}

const adoptBody = "<!-- hive:contributor-adopt sha=0123456789abcdef0123456789abcdef01234567 at=2026-10-28T17:20:06Z -->\n" +
	"🐝 This PR was **auto-promoted for contributor-lane merging** by @alice at 2026-10-28T17:20:06Z (head `0123456789abcdef`, waited 30m0s)."

func TestSameItemDedupe(t *testing.T) {
	cases := []struct {
		name   string
		first  string
		second string
		want   bool // second allowed?
	}{
		{"identical body", adoptBody, adoptBody, false},
		{"marker args differ", adoptBody, strings.Replace(adoptBody, "at=2026-10-28T17:20:06Z", "at=2026-10-28T17:25:00Z nonce=9", 1), false},
		{"sha differs", adoptBody, strings.ReplaceAll(adoptBody, "0123456789abcdef", "fedcba9876543210"), false},
		{"timestamp differs", adoptBody, strings.ReplaceAll(adoptBody, "2026-10-28T17:20:06Z", "2026-10-29T03:01:59+02:00"), false},
		{"duration differs", adoptBody, strings.Replace(adoptBody, "30m0s", "3h", 1), false},
		{"whitespace differs", adoptBody, strings.Replace(adoptBody, "This PR was", "This  PR\n was", 1), false},
		{"genuinely different", adoptBody, "🐝 Adoption failed: the copy went red twice.", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, _, _ := newDedupeGate(t, Config{}, "")
			if d := g.AllowedPreview("org/repo", foreignPR(3925), KindContributorAdopt, tc.first); !d.Allowed {
				t.Fatalf("first write refused: %+v", d)
			}
			d := g.AllowedPreview("org/repo", foreignPR(3925), KindContributorAdopt, tc.second)
			if d.Allowed != tc.want {
				t.Fatalf("second write allowed=%v want %v (%+v)", d.Allowed, tc.want, d)
			}
			if !tc.want {
				if d.Reason != ReasonDuplicate || !strings.Contains(d.Detail, "already written to #3925") {
					t.Fatalf("decision %+v", d)
				}
				rec := g.Recent()
				if len(rec) != 1 || rec[0].Reason != ReasonDuplicate || rec[0].Number != 3925 {
					t.Fatalf("recent %+v", rec)
				}
				if st := g.Snapshot(); st.Dedupe.SameItem != 1 || st.Suppressed24h != 1 {
					t.Fatalf("status %+v", st.Dedupe)
				}
			}
		})
	}
}

func TestSameItemDedupeTTLExpiry(t *testing.T) {
	g, clk, _ := newDedupeGate(t, Config{Dedupe: DedupeConfig{SameItemTTL: 2 * time.Hour}}, "")
	g.AllowedPreview("org/repo", foreignPR(1), KindSentinel, "finding A")
	clk.advance(time.Hour)
	if d := g.AllowedPreview("org/repo", foreignPR(1), KindSentinel, "finding A"); d.Allowed {
		t.Fatalf("inside TTL should dedupe: %+v", d)
	}
	clk.advance(time.Hour + time.Second)
	if d := g.AllowedPreview("org/repo", foreignPR(1), KindSentinel, "finding A"); !d.Allowed {
		t.Fatalf("after TTL should pass: %+v", d)
	}
}

func TestBurstAcrossItems(t *testing.T) {
	g, clk, buf := newDedupeGate(t, Config{}, "")
	body := func(n int) string {
		return fmt.Sprintf("<!-- hive:contributor-adopt pr=%d -->\n🐝 #%d was auto-promoted for contributor-lane merging by @user%d.", n, n, n)
	}
	var allowed, held int
	for i, n := range []int{3925, 3928, 3931} {
		clk.advance(time.Duration(i*6) * time.Second)
		d := g.AllowedPreview("org/repo", foreignPR(n), KindContributorAdopt, body(n))
		if d.Allowed {
			allowed++
			continue
		}
		held++
		if d.Reason != ReasonBurst || !strings.Contains(d.Detail, "already sent to #3925 within 1m0s") {
			t.Fatalf("item %d: %+v", n, d)
		}
	}
	if allowed != 1 || held != 2 {
		t.Fatalf("allowed=%d held=%d", allowed, held)
	}
	if n := strings.Count(buf.String(), "notification burst held"); n != 1 {
		t.Fatalf("want exactly one WARN per burst, got %d:\n%s", n, buf.String())
	}
	if st := g.Snapshot(); st.Dedupe.Burst != 2 {
		t.Fatalf("status %+v", st.Dedupe)
	}
	// Outside the window the same notice to a fourth item passes.
	clk.advance(2 * time.Minute)
	if d := g.AllowedPreview("org/repo", foreignPR(3940), KindContributorAdopt, body(3940)); !d.Allowed {
		t.Fatalf("after window: %+v", d)
	}
	// A different body in the same second is not a burst.
	if d := g.AllowedPreview("org/repo", foreignPR(3941), KindContributorAdopt, "🐝 Adoption failed on #3941."); !d.Allowed {
		t.Fatalf("different body: %+v", d)
	}
}

func TestBurstItemsThreshold(t *testing.T) {
	g, _, _ := newDedupeGate(t, Config{Dedupe: DedupeConfig{BurstItems: 3}}, "")
	for _, n := range []int{1, 2} {
		if d := g.AllowedPreview("org/repo", foreignPR(n), KindWelcomeComment, "Welcome @someone!"); !d.Allowed {
			t.Fatalf("item %d: %+v", n, d)
		}
	}
	if d := g.AllowedPreview("org/repo", foreignPR(3), KindWelcomeComment, "Welcome @other!"); d.Allowed || d.Reason != ReasonBurst {
		t.Fatalf("third item: %+v", d)
	}
}

func TestHourlyCap(t *testing.T) {
	g, clk, _ := newDedupeGate(t, Config{Dedupe: DedupeConfig{HourlyCap: 3}}, "")
	for n := 1; n <= 3; n++ {
		clk.advance(2 * time.Minute)
		if d := g.AllowedPreview("org/repo", foreignPR(n), KindSentinel, fmt.Sprintf("finding %d on #%d: rule r%d", n*7, n, n)); !d.Allowed {
			t.Fatalf("write %d: %+v", n, d)
		}
	}
	clk.advance(2 * time.Minute)
	d := g.AllowedPreview("org/repo", foreignPR(99), KindSentinel, "an unrelated finding about secrets")
	if d.Allowed || d.Reason != ReasonRateCap {
		t.Fatalf("over cap: %+v", d)
	}
	if st := g.Snapshot(); st.Dedupe.RateCapped != 1 {
		t.Fatalf("status %+v", st.Dedupe)
	}
	// Another repo has its own budget.
	g.ObserveRepo("org/other")
	if d := g.AllowedPreview("org/other", foreignPR(5), KindSentinel, "an unrelated finding about secrets"); !d.Allowed {
		t.Fatalf("other repo: %+v", d)
	}
	// The window slides.
	clk.advance(time.Hour)
	if d := g.AllowedPreview("org/repo", foreignPR(100), KindSentinel, "yet another finding"); !d.Allowed {
		t.Fatalf("after window: %+v", d)
	}
}

func TestExemptKindNeverCapped(t *testing.T) {
	for k := range RateCapExemptKinds {
		if !k.ContentDeduped() {
			continue
		}
		t.Run(string(k), func(t *testing.T) {
			g, _, _ := newDedupeGate(t, Config{Dedupe: DedupeConfig{HourlyCap: 1}}, "")
			for n := 1; n <= 5; n++ {
				body := fmt.Sprintf("decision %c needed on #%d", 'a'+n, n)
				if d := g.AllowedPreview("org/repo", foreignPR(n), k, body); !d.Allowed && d.Reason == ReasonRateCap {
					t.Fatalf("exempt kind %s capped: %+v", k, d)
				}
			}
		})
	}
}

func TestKindSets(t *testing.T) {
	for _, k := range Kinds() {
		switch {
		case k.IsLabelKind():
			if k.ContentDeduped() {
				t.Errorf("%s: label kinds have no body and must not be content-deduped", k)
			}
			if !k.RateCapExempt() {
				t.Errorf("%s: label kinds must be exempt from the hourly cap", k)
			}
		case k.IsIssueFileKind(), k == KindSupersessionClose, k == KindIssueClose, k == KindHoldGateNotice, k == KindParentIssueEdit, k == KindCIRerun:
			if k.ContentDeduped() {
				t.Errorf("%s re-checks the gate with a constant preview; it must not be content-deduped", k)
			}
		default:
			if !k.ContentDeduped() {
				t.Errorf("%s posts a comment body and should be content-deduped", k)
			}
		}
	}
	// Merge-path kinds listed in the PR body.
	for _, k := range []Kind{KindVerdictLabel, KindNeedsDecisionComment, KindContributorAdopt, KindHoldGateNotice, KindCIRerun, KindIssueClose, KindSupersessionClose} {
		if !k.RateCapExempt() {
			t.Errorf("%s is on the merge path and must be exempt from the hourly cap", k)
		}
	}
	for k := range RateCapExemptKinds {
		if !IsKind(string(k)) {
			t.Errorf("RateCapExemptKinds names unknown kind %q", k)
		}
	}
	for k := range ContentDedupeKinds {
		if !IsKind(string(k)) {
			t.Errorf("ContentDedupeKinds names unknown kind %q", k)
		}
	}
}

func TestContentLayerSkipsNoPreviewAndNonCommentKinds(t *testing.T) {
	g, _, _ := newDedupeGate(t, Config{}, "")
	for i := 0; i < 3; i++ {
		if d := g.Allowed("org/repo", foreignPR(1), KindContributorAdopt); !d.Allowed {
			t.Fatalf("no preview must stay policy-only: %+v", d)
		}
		if d := g.AllowedPreview("org/repo", ownPR, KindHoldGateNotice, "hold-gate upkeep"); !d.Allowed {
			t.Fatalf("hold_gate_notice must not be content-deduped: %+v", d)
		}
		if d := g.AllowedPreview("org/repo", foreignPR(1), KindSupersessionClose, "close superseded PR"); !d.Allowed {
			t.Fatalf("closes must not be content-deduped: %+v", d)
		}
	}
	off := Config{Dedupe: DedupeConfig{Enabled: boolp(false)}}
	g2, _, _ := newDedupeGate(t, off, "")
	for i := 0; i < 2; i++ {
		if d := g2.AllowedPreview("org/repo", foreignPR(1), KindSentinel, "same"); !d.Allowed {
			t.Fatalf("disabled layer refused: %+v", d)
		}
	}
	if st := g2.Snapshot(); st.Dedupe.Enabled {
		t.Fatalf("status should report dedupe disabled: %+v", st.Dedupe)
	}
}

func TestDedupePersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writeback.json")
	g, clk, _ := newDedupeGate(t, Config{}, path)
	if d := g.AllowedPreview("org/repo", foreignPR(15021), KindSentinel, "selfreport: 3 findings"); !d.Allowed {
		t.Fatalf("first: %+v", d)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"same_item"`) || !strings.Contains(string(data), `"repos"`) {
		t.Fatalf("state file lacks dedupe table:\n%s", data)
	}

	var buf bytes.Buffer
	g2 := New(Config{}, Deps{LevelFor: func(string) int { return 6 }, Logger: slog.New(slog.NewTextHandler(&buf, nil)), Now: func() time.Time { return clk.now.Add(time.Minute) }}, path)
	if d := g2.AllowedPreview("org/repo", foreignPR(15021), KindSentinel, "selfreport: 3 findings"); d.Allowed || d.Reason != ReasonDuplicate {
		t.Fatalf("after reload the duplicate must still be held: %+v", d)
	}
	if st := g2.Snapshot(); !st.Repos["org/repo"].Writable && st.Repos["org/repo"].FirstSeen.IsZero() {
		t.Fatalf("repo records lost on reload: %+v", st.Repos)
	}
}

func TestLegacyStateFileStillLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writeback.json")
	legacy := `{"org/repo":{"first_seen":"2026-10-28T10:00:00Z","writable_at":"2026-10-28T10:00:00Z"}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	g, _, _ := newDedupeGate(t, Config{}, path)
	st := g.Snapshot()
	if rs, ok := st.Repos["org/repo"]; !ok || !rs.Writable {
		t.Fatalf("legacy repo record not loaded: %+v", st.Repos)
	}
}

func TestDedupeTableBounded(t *testing.T) {
	g, _, _ := newDedupeGate(t, Config{}, "")
	g.mu.Lock()
	for i := 0; i < maxDedupeEntries+100; i++ {
		g.dedupe.SameItem[fmt.Sprintf("k%d", i)] = t0.Add(time.Duration(i) * time.Second)
	}
	g.pruneDedupeLocked(t0.Add(time.Duration(maxDedupeEntries+100) * time.Second))
	n := len(g.dedupe.SameItem)
	_, oldestKept := g.dedupe.SameItem["k100"]
	_, oldestDropped := g.dedupe.SameItem["k99"]
	g.mu.Unlock()
	if n != maxDedupeEntries || !oldestKept || oldestDropped {
		t.Fatalf("n=%d kept(k100)=%v dropped(k99)=%v", n, oldestKept, oldestDropped)
	}
}

func TestDedupeConfigValidate(t *testing.T) {
	bad := []Config{
		{Dedupe: DedupeConfig{SameItemTTL: -time.Second}},
		{Dedupe: DedupeConfig{BurstWindow: -time.Second}},
		{Dedupe: DedupeConfig{BurstItems: -1}},
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("case %d: want error", i)
		}
	}
	ok := Config{Dedupe: DedupeConfig{HourlyCap: -1}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if ok.Dedupe.HourlyCapOrDefault() != 0 {
		t.Fatal("negative hourly_cap should disable the cap")
	}
	var zero DedupeConfig
	if !zero.EnabledOrDefault() || zero.SameItemTTLOrDefault() != DefaultSameItemTTL || zero.BurstItemsOrDefault() != DefaultBurstItems || zero.BurstWindowOrDefault() != DefaultBurstWindow || zero.HourlyCapOrDefault() != DefaultHourlyCap {
		t.Fatal("zero config must resolve to defaults")
	}
}

func TestNormalizers(t *testing.T) {
	a := normalizeSameItem(adoptBody)
	if strings.Contains(a, "0123456789abcdef") || strings.Contains(a, "2026-10-28") || strings.Contains(a, "30m0s") || !strings.Contains(a, "<!-- hive:contributor-adopt -->") {
		t.Fatalf("same-item normalization: %q", a)
	}
	if !strings.Contains(a, "@alice") {
		t.Fatalf("same-item normalization must keep mentions: %q", a)
	}
	b := normalizeBurst("🐝 #3925 by @alice on org/repo#12: 7 files")
	if strings.Contains(b, "3925") || strings.Contains(b, "@alice") || strings.Contains(b, "12") || strings.Contains(b, "7 ") {
		t.Fatalf("burst normalization: %q", b)
	}
	if SameItemHash("x") == SameItemHash("y") || len(SameItemHash("x")) != hashLen {
		t.Fatal("hash")
	}
}

func TestIsContentReason(t *testing.T) {
	for _, r := range []string{ReasonDuplicate, ReasonBurst, ReasonRateCap} {
		if !IsContentReason(r) {
			t.Errorf("%s should be a content reason", r)
		}
	}
	for _, r := range []string{ReasonRepoPaused, ReasonBelowLevel, ReasonNeedsLabel, ""} {
		if IsContentReason(r) {
			t.Errorf("%s is a policy reason", r)
		}
	}
}

func TestDedupeConfigOverridesAndCounterWindow(t *testing.T) {
	cfg := Config{Dedupe: DedupeConfig{BurstWindow: 5 * time.Second, SameItemTTL: time.Minute, BurstItems: 4, HourlyCap: 7}}
	g, clk, _ := newDedupeGate(t, cfg, "")
	st := g.Snapshot().Dedupe
	if st.BurstWindowS != 5 || st.SameItemTTLS != 60 || st.BurstItems != 4 || st.HourlyCap != 7 {
		t.Fatalf("status echoes overrides: %+v", st)
	}
	g.AllowedPreview("org/repo", foreignPR(1), KindSentinel, "same")
	g.AllowedPreview("org/repo", foreignPR(1), KindSentinel, "same")
	if g.Snapshot().Dedupe.SameItem != 1 {
		t.Fatal("counter should see the suppression")
	}
	// Counters are a one-hour window; the drill-down keeps 24h.
	clk.advance(61 * time.Minute)
	if d := g.Snapshot().Dedupe; d.SameItem != 0 {
		t.Fatalf("counter must age out after an hour: %+v", d)
	}
	if len(g.Recent()) != 1 {
		t.Fatal("drill-down keeps the entry for 24h")
	}
}

func TestDedupeStateInitFromPartialFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writeback.json")
	partial := `{"repos":{"org/repo":{"first_seen":"2026-10-28T10:00:00Z","writable_at":"2026-10-28T10:00:00Z"}},"dedupe":{"same_item":{"org/repo|1|sentinel|abc":"2026-10-28T13:59:00Z"}}}`
	if err := os.WriteFile(path, []byte(partial), 0o644); err != nil {
		t.Fatal(err)
	}
	g, _, _ := newDedupeGate(t, Config{}, path)
	// Missing burst/hourly maps are created; a fresh write must not panic.
	if d := g.AllowedPreview("org/repo", foreignPR(2), KindSentinel, "fresh"); !d.Allowed {
		t.Fatalf("%+v", d)
	}
}
