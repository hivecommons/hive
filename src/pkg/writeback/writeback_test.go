package writeback

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func intp(i int) *int    { return &i }
func boolp(b bool) *bool { return &b }

var (
	t0      = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	humanPR = Item{Number: 10, IsPR: true, Author: "maintainer", CreatedAt: t0.Add(time.Hour)}
	optedPR = Item{Number: 11, IsPR: true, Author: "maintainer", Labels: []string{"Hive-Review"}, CreatedAt: t0.Add(time.Hour)}
	ownPR   = Item{Number: 12, IsPR: true, Author: "hive[bot]", HiveAuthored: true, CreatedAt: t0.Add(time.Hour)}
	// clankerSuperseded is a contributor's clanker PR the hive re-authored
	// as its copy #90 (hivecommons/hive#15526); humanSuperseded the same
	// for a person's PR.
	clankerSuperseded = Item{Number: 13, IsPR: true, Author: "odttlnt", AgentAuthored: true, SupersededBy: 90, CreatedAt: t0.Add(time.Hour)}
	humanSuperseded   = Item{Number: 14, IsPR: true, Author: "maintainer", SupersededBy: 90, CreatedAt: t0.Add(time.Hour)}
	clankerAdopting   = Item{Number: 15, IsPR: true, Author: "odttlnt", AgentAuthored: true, CreatedAt: t0.Add(time.Hour)}
)

func newGate(t *testing.T, cfg Config, level int) (*Gate, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	g := New(cfg, Deps{
		LevelFor: func(string) int { return level },
		Logger:   slog.New(slog.NewTextHandler(&buf, nil)),
		Now:      func() time.Time { return t0 },
	}, "")
	g.ObserveRepo("org/repo")
	return g, &buf
}

func TestDefaultsAndKinds(t *testing.T) {
	var c Config
	if c.MinLevelOrDefault() != DefaultMinLevel || !c.RequireLabelsEnabled() || c.BackfillEnabled() {
		t.Fatalf("defaults wrong: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(Kinds()) != len(DefaultKinds()) || !IsKind(" Sentinel ") || IsKind("nope") {
		t.Fatal("kinds helpers")
	}
	if !KindStageLabel.IsLabelKind() || KindSentinel.IsLabelKind() || !KindIssueFile.IsIssueFileKind() {
		t.Fatal("kind classification")
	}
	for _, m := range RepoModes() {
		if !m.Valid() {
			t.Fatalf("mode %q invalid", m)
		}
	}
	if RepoMode("weird").Valid() {
		t.Fatal("weird valid")
	}
	// Operator defaults.
	if p := c.Policy(KindSentinel); p.MinLevel != 5 || !p.RequireLabel || p.UnlabeledMinLevel != 6 {
		t.Fatalf("sentinel %+v", p)
	}
	if p := c.Policy(KindHoldGateNotice); p.MinLevel != 4 || !p.OwnOnly {
		t.Fatalf("hold gate %+v", p)
	}
	if p := c.Policy(KindStageLabel); p.MinLevel != 5 || !p.RequireLabel {
		t.Fatalf("stage %+v", p)
	}
	if p := c.Policy(Kind("unknown")); !p.OwnOnly || p.MinLevel != 99 {
		t.Fatalf("unknown kind must fail closed: %+v", p)
	}
	if rows := c.EffectiveKinds(); len(rows) != len(Kinds()) || rows[0].Kind != Kinds()[0] {
		t.Fatal("EffectiveKinds")
	}
}

func TestPolicyOverrides(t *testing.T) {
	c := Config{
		Unsolicited: UnsolicitedConfig{MinLevel: intp(6)},
		Kinds: map[string]KindPolicyOverride{
			"verdict_label": {MinLevel: intp(2), RequireLabel: boolp(false)},
			"sentinel":      {UnlabeledMinLevel: intp(5), OwnOnly: boolp(false)},
		},
	}
	if p := c.Policy(KindVerdictLabel); p.MinLevel != 6 || p.RequireLabel {
		t.Fatalf("global floor should raise: %+v", p)
	}
	if p := c.Policy(KindSentinel); p.UnlabeledMinLevel != 5 || p.MinLevel != 6 {
		t.Fatalf("sentinel override %+v", p)
	}
	if p := c.Policy(KindHoldGateNotice); p.MinLevel != 4 {
		t.Fatalf("floor must not touch own-only kinds: %+v", p)
	}
	if p := c.Policy(KindIssueFile); p.MinLevel != 3 {
		t.Fatalf("floor must not touch issue_file: %+v", p)
	}
	c2 := Config{Unsolicited: UnsolicitedConfig{RequireLabels: boolp(false)}}
	if p := c2.Policy(KindStageLabel); p.RequireLabel {
		t.Fatal("require_labels=false must clear RequireLabel")
	}
}

func TestRepoModeFor(t *testing.T) {
	c := Config{PerRepo: map[string]RepoOverride{"Torch-Spyre/torch-spyre": {Mode: RepoModeIssuesOnly}, "bare": {Mode: RepoModeOff}}}
	if c.RepoModeFor("torch-spyre/TORCH-SPYRE") != RepoModeIssuesOnly {
		t.Fatal("qualified")
	}
	if c.RepoModeFor("torch-spyre") != RepoModeIssuesOnly {
		t.Fatal("bare lookup of qualified key")
	}
	if c.RepoModeFor("org/bare") != RepoModeOff {
		t.Fatal("qualified lookup of bare key")
	}
	if c.RepoModeFor("other/repo") != RepoModeDefault || c.RepoModeFor("") != RepoModeDefault {
		t.Fatal("default")
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]Config{
		"min_level":         {Unsolicited: UnsolicitedConfig{MinLevel: intp(7)}},
		"unknown kind":      {Kinds: map[string]KindPolicyOverride{"bogus": {}}},
		"kind min_level":    {Kinds: map[string]KindPolicyOverride{"sentinel": {MinLevel: intp(-1)}}},
		"kind unlabeled":    {Kinds: map[string]KindPolicyOverride{"sentinel": {UnlabeledMinLevel: intp(9)}}},
		"empty repo":        {PerRepo: map[string]RepoOverride{" ": {}}},
		"unknown repo mode": {PerRepo: map[string]RepoOverride{"a/b": {Mode: "sometimes"}}},
	}
	for name, c := range cases {
		if err := c.Validate(); err == nil || !strings.HasPrefix(err.Error(), ConfigKey+".") {
			t.Errorf("%s: want prefixed error, got %v", name, err)
		}
	}
	ok := Config{Unsolicited: UnsolicitedConfig{MinLevel: intp(5)}, Kinds: map[string]KindPolicyOverride{"sentinel": {MinLevel: intp(6)}}, PerRepo: map[string]RepoOverride{"a/b": {Mode: RepoModeFull}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDecisionTable(t *testing.T) {
	cases := []struct {
		name   string
		cfg    Config
		level  int
		item   Item
		kind   Kind
		allow  bool
		reason string
	}{
		{"sentinel L4 human", Config{}, 4, humanPR, KindSentinel, false, ReasonNeedsLabel},
		{"sentinel L4 opted", Config{}, 4, optedPR, KindSentinel, false, ReasonBelowLevel},
		{"sentinel L5 opted", Config{}, 5, optedPR, KindSentinel, true, ReasonLabelOptIn},
		{"sentinel L5 human", Config{}, 5, humanPR, KindSentinel, false, ReasonNeedsLabel},
		{"sentinel L6 human", Config{}, 6, humanPR, KindSentinel, true, ReasonLevel},
		{"own item any kind", Config{}, 1, ownPR, KindSentinel, true, ReasonOwnItem},
		{"hold gate own L4", Config{}, 4, ownPR, KindHoldGateNotice, true, ReasonOwnItem},
		{"hold gate foreign L6", Config{}, 6, optedPR, KindHoldGateNotice, false, ReasonOwnOnly},
		{"stage label L5 human", Config{}, 5, humanPR, KindStageLabel, false, ReasonNeedsLabel},
		{"stage label L5 opted", Config{}, 5, optedPR, KindStageLabel, true, ReasonLabelOptIn},
		{"stage label L6 human", Config{}, 6, humanPR, KindStageLabel, false, ReasonNeedsLabel},
		{"level unknown", Config{}, 0, optedPR, KindStageLabel, false, ReasonLevelUnknown},
		{"issue file L4", Config{}, 4, Item{Number: 0}, KindIssueFile, true, ReasonIssueFile},
		{"issue file L2", Config{}, 2, Item{}, KindIssueFile, false, ReasonBelowLevel},
		{"issue file level unknown", Config{}, 0, Item{}, KindIssueFile, true, ReasonIssueFile},
		{"repo off", Config{PerRepo: map[string]RepoOverride{"org/repo": {Mode: RepoModeOff}}}, 6, ownPR, KindIssueFile, false, ReasonRepoOff},
		{"issues-only files", Config{PerRepo: map[string]RepoOverride{"org/repo": {Mode: RepoModeIssuesOnly}}}, 4, Item{}, KindIssueFile, true, ReasonIssueFile},
		{"issues-only blocks comment", Config{PerRepo: map[string]RepoOverride{"org/repo": {Mode: RepoModeIssuesOnly}}}, 6, optedPR, KindSentinel, false, ReasonRepoIssues},
		{"labels-only blocks file", Config{PerRepo: map[string]RepoOverride{"org/repo": {Mode: RepoModeLabelsOnly}}}, 6, Item{}, KindIssueFile, false, ReasonRepoLabels},
		{"labels-only blocks comment", Config{PerRepo: map[string]RepoOverride{"org/repo": {Mode: RepoModeLabelsOnly}}}, 6, optedPR, KindSentinel, false, ReasonRepoLabels},
		{"labels-only allows label", Config{PerRepo: map[string]RepoOverride{"org/repo": {Mode: RepoModeLabelsOnly}}}, 5, optedPR, KindStageLabel, true, ReasonLabelOptIn},
		{"full bypasses", Config{PerRepo: map[string]RepoOverride{"org/repo": {Mode: RepoModeFull}}}, 1, humanPR, KindSentinel, true, ReasonRepoFull},
		{"no-label kind by level", Config{Kinds: map[string]KindPolicyOverride{"claim_comment": {RequireLabel: boolp(false)}}}, 4, humanPR, KindClaimComment, true, ReasonLevel},
		{"no-label kind below", Config{Kinds: map[string]KindPolicyOverride{"claim_comment": {RequireLabel: boolp(false)}}}, 3, humanPR, KindClaimComment, false, ReasonBelowLevel},
		{"unknown kind fails closed", Config{}, 6, optedPR, Kind("mystery"), false, ReasonOwnOnly},
		// Adoption close of a superseded original (hivecommons/hive#15526):
		// agent-authored with the hive's copy on record closes by policy,
		// no opt-in label needed; a person's PR, or an agent PR before its
		// copy exists, still needs the label; other kinds are unchanged.
		{"adopt close superseded clanker L6", Config{}, 6, clankerSuperseded, KindContributorAdopt, true, ReasonSupersededAgentPR},
		{"adopt close superseded clanker L1", Config{}, 1, clankerSuperseded, KindContributorAdopt, true, ReasonSupersededAgentPR},
		{"adopt close superseded human L6", Config{}, 6, humanSuperseded, KindContributorAdopt, false, ReasonNeedsLabel},
		{"adopt clanker no copy yet L6", Config{}, 6, clankerAdopting, KindContributorAdopt, false, ReasonNeedsLabel},
		{"sentinel on superseded clanker", Config{}, 5, clankerSuperseded, KindSentinel, false, ReasonNeedsLabel},
		{"adopt close superseded repo off", Config{PerRepo: map[string]RepoOverride{"org/repo": {Mode: RepoModeOff}}}, 6, clankerSuperseded, KindContributorAdopt, false, ReasonRepoOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := newGate(t, tc.cfg, tc.level)
			d := g.Allowed("org/repo", tc.item, tc.kind)
			if d.Allowed != tc.allow || d.Reason != tc.reason {
				t.Fatalf("got %+v, want allow=%v reason=%s", d, tc.allow, tc.reason)
			}
		})
	}
}

func TestRepoListedAndNilGate(t *testing.T) {
	var nilGate *Gate
	if d := nilGate.Allowed("x/y", humanPR, KindSentinel); !d.Allowed || d.Reason != ReasonNoGate {
		t.Fatalf("nil gate %+v", d)
	}
	nilGate.ObserveRepo("x/y")
	nilGate.NoteWriteOutcome("x/y", 200)
	nilGate.SweepSummary("x/y")
	nilGate.SetConfig(Config{})
	if nilGate.Recent() != nil || nilGate.Snapshot().Enabled || !nilGate.FirstSeen("x/y").IsZero() || nilGate.Config().PerRepo != nil {
		t.Fatal("nil gate accessors")
	}
	g := New(Config{}, Deps{LevelFor: func(string) int { return 6 }, RepoListed: func(r string) bool { return r == "org/listed" }, Now: func() time.Time { return t0 }}, "")
	if d := g.Allowed("org/other", humanPR, KindSentinel); d.Allowed || d.Reason != ReasonRepoNotListed {
		t.Fatalf("unlisted %+v", d)
	}
	if d := g.Allowed("org/listed", humanPR, KindSentinel); !d.Allowed {
		t.Fatalf("listed %+v", d)
	}
}

func TestBackfillGuardAndPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "writeback.json")
	now := t0
	var buf bytes.Buffer
	deps := Deps{LevelFor: func(string) int { return 6 }, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	g := New(Config{}, deps, path)
	g.ObserveRepo("org/repo")
	g.ObserveRepo("org/repo") // idempotent
	if !g.FirstSeen("org/repo").Equal(t0) {
		t.Fatal("first_seen")
	}
	old := Item{Number: 1, IsPR: true, Author: "h", CreatedAt: t0.Add(-time.Hour)}
	fresh := Item{Number: 2, IsPR: true, Author: "h", CreatedAt: t0.Add(time.Hour)}
	if d := g.Allowed("org/repo", old, KindSentinel); d.Allowed || d.Reason != ReasonBackfill {
		t.Fatalf("old %+v", d)
	}
	if d := g.Allowed("org/repo", fresh, KindSentinel); !d.Allowed {
		t.Fatalf("fresh %+v", d)
	}
	// Opt-in label beats the backfill guard — the human asked.
	oldOpted := old
	oldOpted.Labels = []string{"hive-review"}
	if d := g.Allowed("org/repo", oldOpted, KindSentinel); !d.Allowed {
		t.Fatalf("old opted %+v", d)
	}
	// Own items and zero CreatedAt are never backfill-blocked.
	if d := g.Allowed("org/repo", Item{Number: 3, HiveAuthored: true, CreatedAt: t0.Add(-time.Hour)}, KindSentinel); !d.Allowed {
		t.Fatal("own")
	}
	if d := g.Allowed("org/repo", Item{Number: 4, Author: "h"}, KindSentinel); !d.Allowed {
		t.Fatal("zero created")
	}
	// App gaining write moves the cutoff forward.
	now = t0.Add(2 * time.Hour)
	g.NoteWriteOutcome("org/repo", 403)
	now = t0.Add(3 * time.Hour)
	g.NoteWriteOutcome("org/repo", 201)
	g.NoteWriteOutcome("org/repo", 200) // second 2xx must not move it
	if d := g.Allowed("org/repo", fresh, KindSentinel); d.Allowed || d.Reason != ReasonBackfill {
		t.Fatalf("fresh after writable_at %+v", d)
	}
	g.SweepSummary("org/repo")
	g.SweepSummary("org/repo")
	g.SweepSummary("org/never")
	if n := strings.Count(buf.String(), "writeback: skipped 2 pre-existing items on org/repo (backfill off)"); n != 1 {
		t.Fatalf("summary lines: %d\n%s", n, buf.String())
	}
	// Persistence round-trip.
	g2 := New(Config{}, deps, path)
	if !g2.FirstSeen("org/repo").Equal(t0) {
		t.Fatal("reload first_seen")
	}
	st := g2.Snapshot()
	rs := st.Repos["org/repo"]
	if !rs.Writable || rs.PreExisting != 2 || !rs.WritableAt.Equal(t0.Add(3*time.Hour)) || rs.Level != 6 {
		t.Fatalf("repo status %+v", rs)
	}
	if len(st.Banners) != 1 || !strings.Contains(st.Banners[0].Message, "hive can now write to org/repo; 2 existing PRs were left untouched") {
		t.Fatalf("banner %+v", st.Banners)
	}
	// Backfill on: everything flows and the banner goes away.
	g2.SetConfig(Config{Unsolicited: UnsolicitedConfig{Backfill: boolp(true)}})
	if d := g2.Allowed("org/repo", old, KindSentinel); !d.Allowed {
		t.Fatalf("backfill on %+v", d)
	}
	if b := g2.Snapshot().Banners; len(b) != 0 {
		t.Fatalf("banner with backfill on: %+v", b)
	}
	// Unreadable file starts empty.
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("{nope"), 0o644)
	if g3 := New(Config{}, deps, bad); !g3.FirstSeen("org/repo").IsZero() {
		t.Fatal("bad file should start empty")
	}
	// Unwritable path must not panic.
	g4 := New(Config{}, deps, filepath.Join(dir, "missing", "x", "wb.json"))
	g4.ObserveRepo("a/b")
}

func TestRecentAndSnapshot(t *testing.T) {
	now := t0
	g := New(Config{}, Deps{LevelFor: func(string) int { return 4 }, OptInLabels: func(string) []string { return []string{"needs-hive"} }, Now: func() time.Time { return now }}, "")
	g.ObserveRepo("org/repo")
	g.AllowedPreview("org/repo", humanPR, KindSentinel, "Sentinel alert — maintainer review required\nmore")
	now = now.Add(time.Minute)
	g.AllowedPreview("org/repo", Item{Number: 20, Author: "x", Labels: []string{"needs-hive"}, CreatedAt: t0.Add(time.Hour)}, KindStageLabel, "")
	rec := g.Recent()
	if len(rec) != 2 || rec[0].Number != 20 || rec[1].Preview != "Sentinel alert — maintainer review required" || rec[1].Kind != KindSentinel {
		t.Fatalf("recent %+v", rec)
	}
	if !strings.Contains(rec[1].Detail, "needs-hive") {
		t.Fatalf("detail should name the opt-in labels: %s", rec[1].Detail)
	}
	st := g.Snapshot()
	if st.Suppressed24h != 2 || st.Repos["org/repo"].Suppressed24 != 2 || st.OptInLabels[0] != "needs-hive" || st.MinLevel != 5 || !st.RequireLabels || st.Backfill {
		t.Fatalf("snapshot %+v", st)
	}
	now = now.Add(SuppressedRetention + time.Minute)
	if len(g.Recent()) != 0 || g.Snapshot().Suppressed24h != 0 {
		t.Fatal("prune after 24h")
	}
	for i := 0; i < maxRecent+10; i++ {
		g.Allowed("org/repo", Item{Number: i, Author: "x", CreatedAt: t0.Add(time.Hour)}, KindSentinel)
	}
	if len(g.Recent()) != maxRecent {
		t.Fatal("ring bound")
	}
	long := strings.Repeat("x", 300)
	if fl := firstLine(long); len(fl) <= 200 || !strings.HasSuffix(fl, "…") {
		t.Fatal("firstLine truncation")
	}
}

// A repo pause (#6203) refuses EVERY kind, own items and mode=full
// included, and is checked before any other rule; lifting it restores the
// ordinary decision.
func TestRepoPausedRefusesEveryKind(t *testing.T) {
	paused := map[string]RepoPauseInfo{"org/repo": {By: "clubanderson", Reason: "release freeze"}}
	g := New(Config{PerRepo: map[string]RepoOverride{"org/full": {Mode: RepoModeFull}}}, Deps{
		LevelFor: func(string) int { return 6 },
		RepoPause: func(repo string) (RepoPauseInfo, bool) {
			info, ok := paused[repo]
			return info, ok
		},
		Now: func() time.Time { return t0 },
	}, "")
	cases := []struct {
		name string
		repo string
		item Item
		kind Kind
	}{
		{"own item", "org/repo", ownPR, KindHoldGateNotice},
		{"foreign L6 sentinel", "org/repo", humanPR, KindSentinel},
		{"issue file", "org/repo", Item{}, KindIssueFile},
		{"stage label opted", "org/repo", optedPR, KindStageLabel},
		{"case-insensitive repo", "org/repo", ownPR, KindClaimComment},
		{"superseded agent PR close", "org/repo", clankerSuperseded, KindContributorAdopt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := g.AllowedPreview(tc.repo, tc.item, tc.kind, "preview")
			if d.Allowed || d.Reason != ReasonRepoPaused {
				t.Fatalf("got %+v, want refused with %s", d, ReasonRepoPaused)
			}
			for _, want := range []string{"clubanderson", "release freeze", "paused"} {
				if !strings.Contains(d.Detail, want) {
					t.Errorf("detail %q lacks %q", d.Detail, want)
				}
			}
		})
	}
	if n := len(g.Recent()); n != len(cases) {
		t.Fatalf("suppressions recorded = %d, want %d", n, len(cases))
	}
	// Unpaused repos are untouched: own items and mode=full still pass.
	if d := g.Allowed("org/other", ownPR, KindSentinel); !d.Allowed || d.Reason != ReasonOwnItem {
		t.Fatalf("unpaused own item: %+v", d)
	}
	if d := g.Allowed("org/full", humanPR, KindSentinel); !d.Allowed || d.Reason != ReasonRepoFull {
		t.Fatalf("unpaused full repo: %+v", d)
	}
	// Lifted → back to the normal decision.
	delete(paused, "org/repo")
	if d := g.Allowed("org/repo", ownPR, KindHoldGateNotice); !d.Allowed || d.Reason != ReasonOwnItem {
		t.Fatalf("after unpause: %+v", d)
	}
	if info, ok := g.RepoPaused("org/repo"); ok || info.By != "" {
		t.Fatalf("RepoPaused after unpause = %+v,%v", info, ok)
	}
	var nilGate *Gate
	if _, ok := nilGate.RepoPaused("org/repo"); ok {
		t.Fatal("nil gate must never report paused")
	}
}
