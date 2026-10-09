package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/retro"
)

func TestAutonomyDecisionSinkSatisfiesInterface(t *testing.T) {
	var _ retro.AutonomyDecisionSink = NewAutonomyDecisionSink(&Server{}, nil, nil)
}

func TestAutonomyDecisionSinkNilSafeWithoutServer(t *testing.T) {
	d := retro.AutonomyDecision{Repo: "org/repo", Direction: "promote", From: 2, To: 3}

	var nilSink *AutonomyDecisionSink
	nilSink.RecordAutonomyDecision(d) // must not panic

	called := false
	noServer := NewAutonomyDecisionSink(nil, func(string, string, bool) { called = true }, func(string, int) { called = true })
	noServer.RecordAutonomyDecision(d)
	if called {
		t.Fatal("sink without a server must not notify or apply")
	}

	// A server with no audit log, no deps and no callbacks is the minimal
	// wiring cmd/hive can produce before the dashboard finishes booting.
	NewAutonomyDecisionSink(&Server{}, nil, nil).RecordAutonomyDecision(d)
}

func TestAutonomyDecisionSinkRecordsAuditAppliesAndNotifies(t *testing.T) {
	srv := &Server{audit: &AuditLog{}}

	var gotTitle, gotMsg string
	var gotDemote bool
	notify := func(title, message string, demote bool) {
		gotTitle, gotMsg, gotDemote = title, message, demote
	}
	var appliedRepo string
	var appliedLevel int
	apply := func(repo string, level int) {
		appliedRepo, appliedLevel = repo, level
	}

	sink := NewAutonomyDecisionSink(srv, notify, apply)
	sink.RecordAutonomyDecision(retro.AutonomyDecision{
		Repo:        "org/repo",
		Direction:   "promote",
		From:        2,
		To:          3,
		EvidenceIDs: []string{"ev-1", "ev-2"},
		Reason:      "30 clean merges",
	})

	entries := srv.audit.Recent(10)
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Action != "autonomy_acmm_promote" {
		t.Errorf("Action = %q, want autonomy_acmm_promote", e.Action)
	}
	if e.User != "system" {
		t.Errorf("User = %q, want system", e.User)
	}
	if e.Agent != retro.Actor {
		t.Errorf("Agent = %q, want %q", e.Agent, retro.Actor)
	}
	for _, want := range []string{"repo=org/repo", "direction=promote", "from=2", "to=3", "evidence=ev-1|ev-2", "reason=30 clean merges"} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("Detail = %q, missing %q", e.Detail, want)
		}
	}

	if appliedRepo != "org/repo" || appliedLevel != 3 {
		t.Errorf("apply(%q, %d), want (org/repo, 3)", appliedRepo, appliedLevel)
	}
	if gotTitle != "Automatic ACMM promote" {
		t.Errorf("notify title = %q, want %q", gotTitle, "Automatic ACMM promote")
	}
	if !strings.Contains(gotMsg, "org/repo moved L2 → L3 (30 clean merges)") {
		t.Errorf("notify message = %q", gotMsg)
	}
	if gotDemote {
		t.Error("promote must not be flagged as a demotion")
	}
}

func TestAutonomyDecisionSinkDemoteFlagsNotification(t *testing.T) {
	srv := &Server{audit: &AuditLog{}}
	var gotDemote bool
	sink := NewAutonomyDecisionSink(srv, func(_, _ string, demote bool) { gotDemote = demote }, nil)

	sink.RecordAutonomyDecision(retro.AutonomyDecision{Repo: "org/repo", Direction: "demote", From: 4, To: 3, Reason: "regressions"})

	if !gotDemote {
		t.Error("demote decision must set the demote flag on the notification")
	}
	entries := srv.audit.Recent(1)
	if len(entries) != 1 || entries[0].Action != "autonomy_acmm_demote" {
		t.Fatalf("audit = %+v, want one autonomy_acmm_demote entry", entries)
	}
}

func newAutonomyDecisionSinkWithStore(t *testing.T) (*AutonomyDecisionSink, *beads.Store) {
	t.Helper()
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	srv := &Server{deps: &Dependencies{BeadStores: map[string]*beads.Store{retro.Actor: store}}}
	return NewAutonomyDecisionSink(srv, nil, nil), store
}

func TestAutonomyDecisionSinkRecordsAdvisoryBead(t *testing.T) {
	sink, store := newAutonomyDecisionSinkWithStore(t)

	sink.RecordAutonomyDecision(retro.AutonomyDecision{
		Repo:        "org/repo",
		Direction:   "promote",
		From:        2,
		To:          3,
		EvidenceIDs: []string{"ev-1", "ev-2"},
		Reason:      "30 clean merges",
	})

	all := store.List(beads.ListFilter{})
	if len(all) != 1 {
		t.Fatalf("beads = %d, want 1", len(all))
	}
	b := all[0]
	if b.Title != "autonomy decision: org/repo promoted to L3" {
		t.Errorf("Title = %q", b.Title)
	}
	if b.Type != beads.TypeDecision {
		t.Errorf("Type = %q, want %q", b.Type, beads.TypeDecision)
	}
	if b.Priority != beads.PriorityMedium {
		t.Errorf("Priority = %d, want %d (promote is medium)", b.Priority, beads.PriorityMedium)
	}
	if b.Actor != retro.Actor {
		t.Errorf("Actor = %q, want %q", b.Actor, retro.Actor)
	}
	want := map[string]string{
		"autonomy_decision":     "promote",
		"autonomy_scope_type":   "repo",
		"autonomy_scope_value":  "org/repo",
		"autonomy_from_level":   "2",
		"autonomy_to_level":     "3",
		"autonomy_evidence_ids": "ev-1,ev-2",
		"detail":                "30 clean merges",
	}
	for k, v := range want {
		if got := b.Meta(k); got != v {
			t.Errorf("Meta(%q) = %q, want %q", k, got, v)
		}
	}
}

func TestAutonomyDecisionSinkDemoteBeadIsHighPriority(t *testing.T) {
	sink, store := newAutonomyDecisionSinkWithStore(t)

	sink.RecordAutonomyDecision(retro.AutonomyDecision{Repo: "org/repo", Direction: "demote", From: 4, To: 3, Reason: "regressions"})

	all := store.List(beads.ListFilter{})
	if len(all) != 1 {
		t.Fatalf("beads = %d, want 1", len(all))
	}
	if all[0].Priority != beads.PriorityHigh {
		t.Errorf("Priority = %d, want %d (demote is high)", all[0].Priority, beads.PriorityHigh)
	}
	if all[0].Title != "autonomy decision: org/repo demoted to L3" {
		t.Errorf("Title = %q", all[0].Title)
	}
}

func TestAutonomyDecisionSinkSkipsAdvisoryWithoutRetroStore(t *testing.T) {
	d := retro.AutonomyDecision{Repo: "org/repo", Direction: "promote", From: 2, To: 3}

	// nil deps: nothing to write to, must not panic.
	NewAutonomyDecisionSink(&Server{}, nil, nil).RecordAutonomyDecision(d)

	// deps without any stores.
	NewAutonomyDecisionSink(&Server{deps: &Dependencies{}}, nil, nil).RecordAutonomyDecision(d)

	// stores present but none for the retro actor: the scanner store must
	// stay untouched.
	other, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	srv := &Server{deps: &Dependencies{BeadStores: map[string]*beads.Store{"scanner": other, retro.Actor: nil}}}
	NewAutonomyDecisionSink(srv, nil, nil).RecordAutonomyDecision(d)
	if n := len(other.List(beads.ListFilter{})); n != 0 {
		t.Errorf("scanner store has %d beads, want 0", n)
	}
}
