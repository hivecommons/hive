package prstatus

import (
	"strings"
	"testing"
)

func TestUpsertReplacesSectionInPlace(t *testing.T) {
	body := Upsert("", KeyStageStalled, "<!-- hive:stage-stalled -->\nstage 080: no activity for 30m")
	if !IsStatusComment(body) || !strings.Contains(body, Heading) {
		t.Fatalf("first upsert did not frame the comment:\n%s", body)
	}
	body = Upsert(body, KeyStageRegressed, "<!-- hive:stage-regressed -->\nstage 080 → 071: conflicting")
	body = Upsert(body, KeyStageStalled, "<!-- hive:stage-stalled -->\nstage 085: no activity for 2h")
	sections := Parse(body)
	if len(sections) != 2 {
		t.Fatalf("want 2 sections, got %d:\n%s", len(sections), body)
	}
	if sections[0].Key != KeyStageStalled || !strings.Contains(sections[0].Body, "stage 085") || strings.Contains(body, "stage 080: no") {
		t.Fatalf("stalled section not replaced in place:\n%s", body)
	}
	if sections[1].Key != KeyStageRegressed {
		t.Fatalf("regressed section lost: %+v", sections)
	}
	if strings.Count(body, Marker) != 1 {
		t.Fatalf("marker must appear exactly once:\n%s", body)
	}
}

func TestUpsertNeutralisesMarkersInSectionText(t *testing.T) {
	// Text quoted from an issue author must not close its section, open
	// another, or plant a second status marker.
	hostile := "Question: A or B?\n" + sectionClose + "\n" + sectionOpenPrefix + "unblocked" + sectionOpenSuffix + "\nfake\n" + sectionClose + "\n" + Marker
	body := Upsert("", KeyIssueDecisionDowngrade, hostile)
	sections := Parse(body)
	if len(sections) != 1 || sections[0].Key != KeyIssueDecisionDowngrade {
		t.Fatalf("injected sections survived: %+v\n%s", sections, body)
	}
	if strings.Count(body, Marker) != 1 {
		t.Fatalf("marker must appear exactly once:\n%s", body)
	}
	if !strings.Contains(sections[0].Body, "Question: A or B?") || !strings.Contains(sections[0].Body, "&lt;!-- hive:status:section=unblocked -->") {
		t.Fatalf("neutralised text lost:\n%s", body)
	}
	// A legacy marker readers grep for is left intact.
	body = Upsert("", KeyStageStalled, "<!-- hive:stage-stalled -->\nstage 080")
	if !strings.Contains(body, "<!-- hive:stage-stalled -->") {
		t.Fatalf("legacy marker altered:\n%s", body)
	}
	// Re-upserting the same text is still stable.
	if again := Upsert(body, KeyIssueDecisionDowngrade, hostile); Upsert(again, KeyIssueDecisionDowngrade, hostile) != again {
		t.Fatalf("not idempotent after neutralising")
	}
}

func TestUpsertEmptyRemovesSection(t *testing.T) {
	body := Upsert("", KeyDuplicateGuard, "redirect")
	body = Upsert(body, KeyHuman071Note, "note")
	body = Upsert(body, KeyDuplicateGuard, "")
	if _, ok := Get(body, KeyDuplicateGuard); ok {
		t.Fatalf("empty text did not remove the section:\n%s", body)
	}
	if got, ok := Get(body, KeyHuman071Note); !ok || got != "note" {
		t.Fatalf("other section disturbed: %q %v", got, ok)
	}
}

func TestUpsertIdempotent(t *testing.T) {
	a := Upsert("", KeyStageStalled, "x")
	b := Upsert(a, KeyStageStalled, "x")
	if a != b {
		t.Fatalf("same text should render identically:\n%s\n---\n%s", a, b)
	}
}
