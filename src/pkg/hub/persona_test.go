package hub

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/persona"
)

func TestHubPersonaStoredOnSaaSUserRecord(t *testing.T) {
	oldDir := saasUsersDir
	saasUsersDir = t.TempDir()
	t.Cleanup(func() { saasUsersDir = oldDir })

	if err := saveUserPersona("github:alice", persona.Record{
		Depth:         "technical",
		SummaryLength: "detailed",
		Notes:         "prefers links to receipts",
	}); err != nil {
		t.Fatalf("saveUserPersona: %v", err)
	}

	got, ok := loadUserPersona("github:alice")
	if !ok {
		t.Fatal("loadUserPersona did not find saved record")
	}
	if got.Depth != persona.DepthTechnical || got.SummaryLength != persona.SummaryDetailed || got.Notes != "prefers links to receipts" {
		t.Fatalf("persona = %#v", got)
	}
	u := loadSaaSUser("github:alice")
	if u == nil || len(u.Hives) != 0 || u.SaaSQuota != 0 {
		t.Fatalf("persona write changed autonomy/access fields: %#v", u)
	}
}

// TestHubPersonaLearningStateRoundTripsAsCountersOnly pins #8363 on the hub
// record: signal counters, pending suggestions, the last adjustment, and the
// pin survive the SaaS user save/load path, a learning-only record is still
// found, and none of it touches hives, quota, or any autonomy field.
func TestHubPersonaLearningStateRoundTripsAsCountersOnly(t *testing.T) {
	oldDir := saasUsersDir
	saasUsersDir = t.TempDir()
	t.Cleanup(func() { saasUsersDir = oldDir })

	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	cfg := persona.LearningConfig{Enabled: true, Threshold: 2}
	record := persona.Record{Depth: persona.DepthOutcomes, SummaryLength: persona.SummaryStandard}
	var err error
	for i := 0; i < cfg.Threshold; i++ {
		if record, err = record.RecordSignal(persona.SignalExpanded, now, cfg); err != nil {
			t.Fatalf("RecordSignal: %v", err)
		}
	}
	if err := saveUserPersona("github:alice", record); err != nil {
		t.Fatalf("saveUserPersona: %v", err)
	}
	got, ok := loadUserPersona("github:alice")
	if !ok {
		t.Fatal("loadUserPersona did not find the record")
	}
	pending := got.Suggestions()
	if len(pending) != 1 || pending[0].Key != persona.SuggestionKeyDepth || pending[0].To != persona.DepthTechnical || pending[0].Evidence != "2 expansions in 7 days" {
		t.Fatalf("suggestions after reload = %#v", pending)
	}
	if got.Depth != persona.DepthOutcomes {
		t.Fatalf("suggestion applied on save: depth = %q", got.Depth)
	}

	accepted, adj, err := got.AcceptSuggestion(1, now)
	if err != nil {
		t.Fatalf("AcceptSuggestion: %v", err)
	}
	undone, _, err := accepted.UndoAdjustment(now)
	if err != nil {
		t.Fatalf("UndoAdjustment: %v", err)
	}
	if err := saveUserPersona("github:alice", undone); err != nil {
		t.Fatalf("saveUserPersona undone: %v", err)
	}
	got, ok = loadUserPersona("github:alice")
	if !ok || !got.Pinned || got.Depth != persona.DepthOutcomes || len(got.Suggestions()) != 0 {
		t.Fatalf("undone record after reload = %#v (ok=%v), adjustment was %#v", got, ok, adj)
	}

	learningOnly := persona.Record{Learning: &persona.Learning{Signals: persona.Signals{WindowStart: now, Skipped: 1}}}
	if err := saveUserPersona("github:bob", learningOnly); err != nil {
		t.Fatalf("saveUserPersona learning-only: %v", err)
	}
	if got, ok := loadUserPersona("github:bob"); !ok || got.Learning == nil || got.Learning.Signals.Skipped != 1 {
		t.Fatalf("learning-only record lost on reload: %#v (ok=%v)", got, ok)
	}
	for _, identity := range []string{"github:alice", "github:bob"} {
		u := loadSaaSUser(identity)
		if u == nil || len(u.Hives) != 0 || u.SaaSQuota != 0 || u.Blocked {
			t.Fatalf("persona learning write changed autonomy/access fields for %s: %#v", identity, u)
		}
	}
}
