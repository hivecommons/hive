package stagelabels

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hivecommons/hive#15020: who held an item and why rides the record so the
// tracker, the 085 comment and the popover can say it.

func TestHoldProvenanceWords(t *testing.T) {
	hive := &HoldProvenance{By: HoldByHive, Source: HoldSourceReporterTrust, Reason: ReporterTrustReason(15017, "Karibhou", "CONTRIBUTOR"), Issue: 15017, Reporter: "Karibhou", Association: "CONTRIBUTOR"}
	if got := hive.Pill(); got != "Held by hive · reporter trust (issue #15017 by @Karibhou, contributor)" {
		t.Fatalf("Pill = %q", got)
	}
	if got := hive.Clause(); got != "the `hold` label was set by the hive itself: reporter trust (issue #15017 by @Karibhou, contributor)" {
		t.Fatalf("Clause = %q", got)
	}
	if !hive.SelfImposed() || !hive.ReporterTrust() {
		t.Fatal("hive reporter-trust provenance must report both")
	}
	person := &HoldProvenance{By: "clubanderson", Source: HoldSourceLabel}
	if got := person.Pill(); got != "Held by @clubanderson" {
		t.Fatalf("Pill = %q", got)
	}
	if got := person.Clause(); got != "the `hold` label was set by @clubanderson" {
		t.Fatalf("Clause = %q", got)
	}
	if person.SelfImposed() || person.ReporterTrust() {
		t.Fatal("a person's hold is neither self-imposed nor reporter trust")
	}
	var none *HoldProvenance
	if none.Pill() != "" || none.Clause() != "" || none.SelfImposed() || none.ReporterTrust() {
		t.Fatal("nil provenance must say nothing")
	}
	if (&HoldProvenance{}).Pill() != "" {
		t.Fatal("an unattributed provenance must say nothing")
	}
	for _, tc := range []struct{ issue, reporter, assoc, want string }{
		{"15017", "Karibhou", "CONTRIBUTOR", "reporter trust (issue #15017 by @Karibhou, contributor)"},
		{"7", "x", "FIRST_TIME_CONTRIBUTOR", "reporter trust (issue #7 by @x, first time contributor)"},
		{"0", "", "", "reporter trust"},
		{"9", "", "", "reporter trust (issue #9)"},
	} {
		var n int
		for _, ch := range tc.issue {
			n = n*10 + int(ch-'0')
		}
		if got := ReporterTrustReason(n, tc.reporter, tc.assoc); got != tc.want {
			t.Fatalf("ReporterTrustReason(%s, %q, %q) = %q, want %q", tc.issue, tc.reporter, tc.assoc, got, tc.want)
		}
	}
}

func TestHoldProvenanceEqual(t *testing.T) {
	at := time.Date(2026, 10, 20, 17, 14, 14, 0, time.UTC)
	a := &HoldProvenance{By: HoldByHive, Source: HoldSourceReporterTrust, Reason: "r", Since: at, Issue: 1}
	b := *a
	b.Since = at.Add(300 * time.Millisecond)
	if !a.Equal(&b) {
		t.Fatal("sub-second drift must compare equal")
	}
	b.By = "pat"
	if a.Equal(&b) || a.Equal(nil) || (*HoldProvenance)(nil).Equal(a) {
		t.Fatal("different provenance / nil must not compare equal")
	}
	if !(*HoldProvenance)(nil).Equal(nil) {
		t.Fatal("nil equals nil")
	}
}

func TestStoreNoteHold(t *testing.T) {
	now := time.Date(2026, 10, 20, 17, 40, 0, 0, time.UTC)
	s := NewStore(filepath.Join(t.TempDir(), "stage.json"), func() time.Time { return now })
	key := Key("acme/widget", 15020)
	hold := &HoldProvenance{By: HoldByHive, Source: HoldSourceReporterTrust, Reason: "reporter trust (issue #15017 by @Karibhou, contributor)", Since: now, Issue: 15017, Reporter: "Karibhou", Association: "CONTRIBUTOR"}
	s.NoteHold(key, hold) // no record yet: nothing to annotate
	if _, ok := s.Get(key); ok {
		t.Fatal("NoteHold must not create a record")
	}
	s.Observe(key, NeedsDecision, "label:hold")
	s.NoteHold(key, hold)
	rec, _ := s.Get(key)
	if rec.Hold == nil || !rec.Hold.Equal(hold) {
		t.Fatalf("record hold = %+v, want %+v", rec.Hold, hold)
	}
	hold.Reason = "mutated after the call"
	if rec, _ := s.Get(key); rec.Hold.Reason != "reporter trust (issue #15017 by @Karibhou, contributor)" {
		t.Fatal("the store must copy the provenance, not alias the caller's pointer")
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.dirty = false
	s.NoteHold(key, &HoldProvenance{By: HoldByHive, Source: HoldSourceReporterTrust, Reason: "reporter trust (issue #15017 by @Karibhou, contributor)", Since: now, Issue: 15017, Reporter: "Karibhou", Association: "CONTRIBUTOR"})
	if s.dirty {
		t.Fatal("re-noting the same provenance must not dirty the store")
	}
	reloaded := NewStore(s.path, func() time.Time { return now })
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if rec, ok := reloaded.Get(key); !ok || rec.Hold == nil || rec.Hold.By != HoldByHive || rec.Hold.Issue != 15017 {
		t.Fatalf("reloaded hold = %+v (ok=%v)", rec.Hold, ok)
	}
	s.ClearHold(key)
	if rec, _ := s.Get(key); rec.Hold != nil {
		t.Fatalf("ClearHold left %+v", rec.Hold)
	}
}

func TestRouteHumanNamesWhoHeld(t *testing.T) {
	base := Evidence{Head: "abc1234", Verdict: VerdictApproved, Checks: CheckSummary{Total: 3, Green: 3, Known: true}, DCO: DCOYes, Holds: []string{"hold"}, Cause: CauseDecision, SelfAuthored: true}
	plain := RouteHuman(base)
	if plain.Stage != NeedsDecision || strings.Join(plain.Reasons, "|") != "the `hold` label is set|the automerge lane will not merge this unattended" || plain.Hold != nil {
		t.Fatalf("plain route = %+v", plain)
	}
	hive := base
	hive.Hold = &HoldProvenance{By: HoldByHive, Source: HoldSourceReporterTrust, Reason: ReporterTrustReason(15017, "Karibhou", "CONTRIBUTOR")}
	route := RouteHuman(hive)
	if route.Reasons[0] != "the `hold` label was set by the hive itself: reporter trust (issue #15017 by @Karibhou, contributor)" || route.Hold != hive.Hold {
		t.Fatalf("hive-held route = %+v", route)
	}
	comment := DecisionComment(route)
	if !strings.Contains(comment, "set by the hive itself: reporter trust (issue #15017 by @Karibhou, contributor)") || strings.Contains(comment, "the `hold` label is set") {
		t.Fatalf("085 comment:\n%s", comment)
	}
	person := base
	person.Holds = []string{"do-not-merge", "hold"}
	person.Hold = &HoldProvenance{By: "clubanderson", Source: HoldSourceLabel}
	route = RouteHuman(person)
	if strings.Join(route.Reasons, "|") != "the `do-not-merge` label is set|the `hold` label was set by @clubanderson|the automerge lane will not merge this unattended" {
		t.Fatalf("person-held route reasons = %q", route.Reasons)
	}
}
