package blocker

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const scannerReport = "needs-human: the remaining retry wrappers live in `.github/workflows/go-security-analysis.yml` and `.github/workflows/v2-ci.yml`, which the App token cannot push (no `workflows` permission); a maintainer must land them.\n\n🐝 hive · scanner\n<!-- hive:origin hive=hive at=2026-10-20T19:33:00Z -->"

func TestParseMarker(t *testing.T) {
	k, ok := ParseMarker("some text\n" + Marker(CapabilityWorkflowsPermission) + "\n")
	if !ok || k != CapabilityWorkflowsPermission {
		t.Fatalf("ParseMarker = %q, %v", k, ok)
	}
	if _, ok := ParseMarker("<!-- hive:blocker kind=capability:unknown-thing -->"); ok {
		t.Fatal("unknown kind parsed as valid")
	}
	if _, ok := ParseMarker("<!-- hive:blocked-on pr=o/r#1 -->"); ok {
		t.Fatal("blocked-on marker must not parse as a blocker")
	}
}

func TestInfer(t *testing.T) {
	cases := map[string]Kind{
		scannerReport: CapabilityWorkflowsPermission,
		"the remainder touches workflow files the App token cannot push, so the remainder needs a human": CapabilityWorkflowsPermission,
		"The hive lacks the `workflows` permission for this repo.":                                       CapabilityWorkflowsPermission,
		"needs-human: this requires a branch-protection change the App cannot make":                      CapabilityBranchProtection,
		"needs-human: the App cannot set the `NPM_TOKEN` secret; a maintainer must add it":               CapabilitySecret,
		"needs-human: the contributor must sign the CLA first":                                           HumanOnlyCLA,
		"needs-human: the App cannot push to the upstream repository":                                    CapabilityRepoWrite,
	}
	for text, want := range cases {
		got, ok := Infer(text)
		if !ok || got != want {
			t.Errorf("Infer(%q) = %q, %v; want %q", text, got, ok, want)
		}
	}
	for _, text := range []string{
		"needs-human: which of the two retry strategies do you prefer?",
		"The CI workflow in .github/workflows/v2-ci.yml is flaky; retry wrappers would help.",
		"",
	} {
		if k, ok := Infer(text); ok {
			t.Errorf("Infer(%q) = %q; want none", text, k)
		}
	}
}

func TestDetectAndReasonSentence(t *testing.T) {
	at := time.Date(2026, 10, 20, 19, 33, 0, 0, time.UTC)
	b, ok := Detect(scannerReport, at)
	if !ok {
		t.Fatal("Detect: no blocker")
	}
	if b.Kind != CapabilityWorkflowsPermission || b.Source != SourceInferred || !b.Since.Equal(at) {
		t.Fatalf("Detect = %+v", b)
	}
	if b.Detail == "" || b.Detail[:12] != "needs-human:" {
		t.Fatalf("Detail = %q", b.Detail)
	}
	for _, s := range []string{"🐝", "hive:origin"} {
		if contains(b.Detail, s) {
			t.Fatalf("Detail leaks footer %q: %q", s, b.Detail)
		}
	}
	m, ok := Detect("Needs a human.\n"+Marker(HumanOnlyCLA), at)
	if !ok || m.Kind != HumanOnlyCLA || m.Source != SourceMarker {
		t.Fatalf("marker Detect = %+v, %v", m, ok)
	}
}

func TestNormalizeReasonIgnoresNumbersAndPunctuation(t *testing.T) {
	a := NormalizeReason("needs-human: see #14990, the `workflows` permission is missing (2026-10-20T19:33:00Z).")
	b := NormalizeReason("needs-human: see #15001, the workflows permission is missing (2026-10-21T08:00:00Z)")
	if a == "" || a != b {
		t.Fatalf("NormalizeReason differs:\n%q\n%q", a, b)
	}
}

func TestKindWords(t *testing.T) {
	if !CapabilityWorkflowsPermission.IsCapability() || HumanOnlyCLA.IsCapability() {
		t.Fatal("IsCapability")
	}
	if CapabilityWorkflowsPermission.Permission() != "workflows" || HumanOnlyCLA.Permission() != "" {
		t.Fatal("Permission")
	}
	if CapabilityWorkflowsPermission.Short() != "App lacks workflows permission" {
		t.Fatalf("Short = %q", CapabilityWorkflowsPermission.Short())
	}
	if !contains(CapabilityWorkflowsPermission.CantDo(), "no `workflows` permission") {
		t.Fatalf("CantDo = %q", CapabilityWorkflowsPermission.CantDo())
	}
	opts := OptionsFor(CapabilityWorkflowsPermission)
	if len(opts) != 5 || opts[0].Action != ActionAppSettings || opts[1].Action != ActionRecheck || opts[3].Action != ActionClosePartial || opts[4].Action != ActionWait {
		t.Fatalf("OptionsFor = %+v", opts)
	}
	if got := OptionsFor(HumanOnlyCLA); len(got) != 2 {
		t.Fatalf("human-only options = %+v", got)
	}
	if got := MissingPermissions([]Blocker{{Kind: CapabilityWorkflowsPermission}, {Kind: CapabilityWorkflowsPermission}, {Kind: HumanOnlyCLA}, {Kind: CapabilitySecret}}); len(got) != 2 || got[0] != "secrets" || got[1] != "workflows" {
		t.Fatalf("MissingPermissions = %v", got)
	}
}

func TestLedgerRecordAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "item-blockers.json")
	l := NewLedger(path)
	at := time.Date(2026, 10, 20, 19, 33, 0, 0, time.UTC)
	e, changed := l.Observe("hivecommons/hive", 14976, scannerReport, at)
	if !changed || !e.Blocks() || e.Blocker.Kind != CapabilityWorkflowsPermission {
		t.Fatalf("Observe = %+v, %v", e, changed)
	}
	// A comment with no needs-human content does not touch the record.
	if _, changed := l.Observe("hivecommons/hive", 14976, "Thanks, looking.", at.Add(time.Minute)); changed {
		t.Fatal("plain comment changed the ledger")
	}
	reloaded := NewLedger(path)
	got, ok := reloaded.Blocking("hivecommons/hive", 14976)
	if !ok || got.Kind != CapabilityWorkflowsPermission || !got.Since.Equal(at) {
		t.Fatalf("reloaded = %+v, %v", got, ok)
	}
	caps := reloaded.Capabilities()
	if caps.BlockedItems != 1 || len(caps.Missing) != 1 || caps.Missing[0] != "workflows" {
		t.Fatalf("Capabilities = %+v", caps)
	}
	reloaded.SetNow(func() time.Time { return at.Add(time.Hour) })
	if n := reloaded.ClearKind(CapabilityWorkflowsPermission); n != 1 {
		t.Fatalf("ClearKind = %d", n)
	}
	if _, ok := reloaded.Blocking("hivecommons/hive", 14976); ok {
		t.Fatal("blocker survived ClearKind")
	}
	if NewLedger(path).Capabilities().BlockedItems != 0 {
		t.Fatal("ClearKind not persisted")
	}
	// The same old report re-read (the delegated path re-observes every
	// comment) does not re-park a cleared item; a fresh one does.
	if _, changed := reloaded.Observe("hivecommons/hive", 14976, scannerReport, at); changed {
		t.Fatal("stale report re-parked a cleared item")
	}
	if _, ok := reloaded.Blocking("hivecommons/hive", 14976); ok {
		t.Fatal("stale report re-blocked")
	}
	if e, changed := reloaded.Observe("hivecommons/hive", 14976, scannerReport, at.Add(2*time.Hour)); !changed || !e.Blocks() {
		t.Fatalf("fresh report after clear = %+v, %v", e, changed)
	}
}

func TestLedgerLoopGuard(t *testing.T) {
	l := NewLedger("")
	t0 := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	reason := "needs-human: which retry strategy should the remaining jobs use?"
	e := l.RecordReason("o/r", 7, NormalizeReason(reason), t0)
	if e.Blocks() || e.Reapplied {
		t.Fatalf("first reason must not block: %+v", e)
	}
	l.NoteDelegatedUnpark("o/r", 7, t0.Add(10*time.Minute))
	// A different reason after the un-park is a new question, not a loop.
	e = l.RecordReason("o/r", 7, NormalizeReason("needs-human: the fix needs a new dependency; approve adding it?"), t0.Add(20*time.Minute))
	if e.Blocks() || e.Reapplied {
		t.Fatalf("new reason tripped the guard: %+v", e)
	}
	l.NoteDelegatedUnpark("o/r", 7, t0.Add(30*time.Minute))
	// The same reason (re-phrased numbers) coming back trips it.
	e = l.RecordReason("o/r", 7, NormalizeReason("needs-human: the fix needs a new dependency; approve adding it? (#99)"), t0.Add(40*time.Minute))
	if !e.Reapplied || !e.Blocks() || e.Blocker.Kind != HumanOnlyReasked || e.Blocker.Source != SourceLoopGuard {
		t.Fatalf("guard did not trip: %+v", e)
	}
	// A capability blocker recorded after an un-park keeps its own kind and
	// still marks the loop.
	l2 := NewLedger("")
	l2.Observe("o/r", 8, scannerReport, t0)
	l2.NoteDelegatedUnpark("o/r", 8, t0.Add(time.Minute))
	e2, _ := l2.Observe("o/r", 8, scannerReport, t0.Add(2*time.Minute))
	if !e2.Reapplied || e2.Blocker.Kind != CapabilityWorkflowsPermission || !e2.Blocker.Since.Equal(t0) {
		t.Fatalf("capability re-report = %+v", e2)
	}
}

func TestSharedLedgerSwap(t *testing.T) {
	prev := SetShared(NewLedger(""))
	defer SetShared(prev)
	Shared().Record("o/r", 1, Blocker{Kind: CapabilitySecret}, "")
	if _, ok := Shared().Blocking("o/r", 1); !ok {
		t.Fatal("shared ledger lost the record")
	}
	Shared().Clear("o/r", 1)
	if len(Shared().Blockers()) != 0 {
		t.Fatal("Clear left an entry")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestKindWordsCoverEveryKind(t *testing.T) {
	for _, k := range Kinds {
		if k.Short() == "" || k.CantDo() == "" {
			t.Fatalf("%s has no words", k)
		}
		if k.IsCapability() != (k.Permission() != "") {
			t.Fatalf("%s: IsCapability=%v but Permission=%q", k, k.IsCapability(), k.Permission())
		}
	}
	if CapabilityRepoWrite.Permission() != "contents" || CapabilityBranchProtection.Permission() != "administration" || CapabilitySecret.Permission() != "secrets" {
		t.Fatal("Permission words")
	}
	if !contains(CapabilityRepoWrite.Short(), "push") || !contains(CapabilityBranchProtection.Short(), "branch protection") || !contains(CapabilitySecret.Short(), "secret") || !contains(HumanOnlyCLA.Short(), "CLA") || !contains(HumanOnlyReasked.Short(), "re-asked") {
		t.Fatal("Short words")
	}
	if !contains(CapabilityRepoWrite.CantDo(), "contents: write") || !contains(CapabilityBranchProtection.CantDo(), "administration") || !contains(CapabilitySecret.CantDo(), "secret") || !contains(HumanOnlyCLA.CantDo(), "CLA") || !contains(HumanOnlyReasked.CantDo(), "loop") {
		t.Fatal("CantDo words")
	}
	unknown := Kind("capability:nope")
	if unknown.Valid() || unknown.Short() != "hive cannot do this" || unknown.CantDo() != "Hive can't do this one." {
		t.Fatalf("unknown kind words: %q / %q", unknown.Short(), unknown.CantDo())
	}
}

func TestReasonSentenceSkipsFootersAndCaps(t *testing.T) {
	text := "<!-- hive:agent -->\n```\ncode\n```\n🐝 footer\n<sub>meta</sub>\n_This comment was written by a bot_\n- > " + repeat("word ", 80) + "needs-human: too long"
	got := ReasonSentence(text)
	if len([]rune(got)) > 280 || got[len(got)-len("…"):] != "…" {
		t.Fatalf("not capped: %d %q", len([]rune(got)), got)
	}
	if got[0] != 'w' {
		t.Fatalf("list/quote prefix kept: %q", got)
	}
	if ReasonSentence("🐝 only footer\n```\n```") != "" {
		t.Fatal("footer-only text must yield no sentence")
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func TestLedgerLoadIgnoresBadFileAndStaleRecords(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := len(NewLedger(bad).Blockers()); n != 0 {
		t.Fatalf("corrupt file yielded %d entries", n)
	}
	// A path whose parent cannot be created makes save fail without
	// losing the in-memory record.
	blocked := filepath.Join(bad, "nested", "ledger.json")
	l := NewLedger(blocked)
	l.Record("o/r", 1, Blocker{Kind: CapabilitySecret}, "")
	if _, ok := l.Blocking("o/r", 1); !ok {
		t.Fatal("unsaveable ledger lost the record")
	}
	// Record, RecordReason and Observe refuse reports older than the clear.
	l2 := NewLedger("")
	t0 := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	l2.SetNow(func() time.Time { return t0.Add(time.Hour) })
	l2.Observe("o/r", 2, scannerReport, t0)
	if l2.ClearKind(CapabilityWorkflowsPermission) != 1 {
		t.Fatal("ClearKind")
	}
	if e := l2.Record("o/r", 2, Blocker{Kind: CapabilityWorkflowsPermission, Since: t0}, "r"); e.Blocks() {
		t.Fatalf("stale Record re-blocked: %+v", e)
	}
	if e := l2.RecordReason("o/r", 2, "r", t0); e.Reason == "r" || !e.ReportedAt.Equal(t0) {
		t.Fatalf("stale RecordReason stored a reason: %+v", e)
	}
	// Zero times fall back to the ledger clock.
	e := l2.Record("o/r", 3, Blocker{Kind: CapabilitySecret}, "")
	if !e.Blocker.Since.Equal(t0.Add(time.Hour)) {
		t.Fatalf("Since not defaulted: %+v", e)
	}
	e = l2.RecordReason("o/r", 4, "why", time.Time{})
	if !e.ReportedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("ReportedAt not defaulted: %+v", e)
	}
	e = l2.NoteDelegatedUnpark("o/r", 4, time.Time{})
	if !e.DelegatedUnparkAt.Equal(t0.Add(time.Hour)) || e.ReasonAtUnpark != "why" {
		t.Fatalf("NoteDelegatedUnpark defaults: %+v", e)
	}
	// A needs-human comment with no kind is remembered as a reason only;
	// a comment on an unknown item that says nothing returns an empty entry.
	if e, changed := l2.Observe("o/r", 5, "needs-human: which colour?", t0); !changed || e.Blocks() || e.Reason == "" {
		t.Fatalf("plain reason = %+v, %v", e, changed)
	}
	if e, changed := l2.Observe("o/r", 6, "   ", t0); changed || e.Repo != "" {
		t.Fatalf("blank text = %+v, %v", e, changed)
	}
	if e, changed := l2.Observe("o/r", 6, "nothing here", t0); changed || e.Repo != "" {
		t.Fatalf("silent text = %+v, %v", e, changed)
	}
	// Reading back a ledger from a path, then writing it, persists the clear.
	path := filepath.Join(dir, "ok.json")
	l3 := NewLedger(path)
	l3.Record("o/r", 7, Blocker{Kind: CapabilityRepoWrite}, "")
	caps := NewLedger(path).Capabilities()
	if caps.BlockedItems != 1 || caps.ByPermission["contents"] != 1 {
		t.Fatalf("Capabilities = %+v", caps)
	}
	if NewLedger(path).ClearKind(CapabilityRepoWrite) != 1 || NewLedger(path).ClearKind(CapabilityRepoWrite) != 0 {
		t.Fatal("ClearKind persistence")
	}
}
