package agentsmd

import "testing"

func resetStatuses() {
	statusMu.Lock()
	statuses = map[string]InjectionStatus{}
	statusMu.Unlock()
}

func TestRecordStatusRoundTripsCaseInsensitively(t *testing.T) {
	resetStatuses()
	t.Cleanup(resetStatuses)
	sections := []string{"Style guide"}
	RecordStatus("Acme/Widgets", InjectionStatus{
		SourceBytes: 40000, RenderedBytes: 30000, CapBytes: 32768,
		Truncated: true, OmittedSections: sections, OmittedBytes: 9000,
	})
	sections[0] = "mutated"
	st, ok := StatusFor("acme/widgets")
	if !ok {
		t.Fatal("status not found by case-folded key")
	}
	if !st.Truncated || st.CapBytes != 32768 || st.SourceBytes != 40000 || st.RenderedBytes != 30000 || st.OmittedBytes != 9000 {
		t.Errorf("status round-trip lost fields: %+v", st)
	}
	if len(st.OmittedSections) != 1 || st.OmittedSections[0] != "Style guide" {
		t.Errorf("recorded sections must be copied, got %v", st.OmittedSections)
	}
	if st.UpdatedAt == "" {
		t.Error("UpdatedAt should be stamped")
	}
	st.OmittedSections[0] = "changed by reader"
	again, _ := StatusFor("ACME/WIDGETS")
	if again.OmittedSections[0] != "Style guide" {
		t.Errorf("reader mutation leaked into the registry: %v", again.OmittedSections)
	}
}

func TestRecordStatusIgnoresBlankRepo(t *testing.T) {
	resetStatuses()
	t.Cleanup(resetStatuses)
	RecordStatus("  ", InjectionStatus{CapBytes: 1})
	if _, ok := StatusFor(""); ok {
		t.Error("blank repo must not be recorded or found")
	}
	if _, ok := StatusFor("acme/none"); ok {
		t.Error("unknown repo must not be found")
	}
	RecordStatus("acme/kept", InjectionStatus{UpdatedAt: "2026-01-01T00:00:00Z"})
	if st, _ := StatusFor("acme/kept"); st.UpdatedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("explicit UpdatedAt overwritten: %q", st.UpdatedAt)
	}
}
