package stagelabels

import (
	"path/filepath"
	"testing"
	"time"
)

// A person's Approve admits one head (hivecommons/hive#15136): noted on an
// existing record only, read back per head, and the login recoverable from
// the admitted_by value.
func TestStoreAdmission(t *testing.T) {
	now := time.Date(2026, 11, 2, 18, 0, 0, 0, time.UTC)
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), func() time.Time { return now })
	key := Key("hivecommons/hotshot", 317)

	if got := OperatorApprovalAdmission(" @Octo "); got != "operator-approval:octo" {
		t.Fatalf("OperatorApprovalAdmission = %q", got)
	}
	if OperatorApprovalAdmission("") != "" || AdmissionLogin("") != "" || AdmissionLogin("registry:trusted") != "" {
		t.Fatal("empty/foreign admission values must not yield a login")
	}
	if got := AdmissionLogin("operator-approval:octo"); got != "octo" {
		t.Fatalf("AdmissionLogin = %q", got)
	}

	// Unknown key, empty head or empty by: no-op.
	if st.NoteAdmission(key, "aaa", "operator-approval:octo", now) {
		t.Fatal("note on unknown key reported a change")
	}
	if _, ok := st.Get(key); ok {
		t.Fatal("note created a record")
	}
	st.Observe(key, ReadyForHumanMerge, "contributor-lane:untrusted-author")
	if st.NoteAdmission(key, "", "operator-approval:octo", now) || st.NoteAdmission(key, "aaa", "", now) {
		t.Fatal("empty note reported a change")
	}

	if !st.NoteAdmission(key, "aaa", "operator-approval:octo", now) {
		t.Fatal("first note must report a change")
	}
	r, _ := st.Get(key)
	if r.Admission("aaa") != "operator-approval:octo" || !r.AdmittedAt.Equal(now) {
		t.Fatalf("admission: %+v", r)
	}
	if r.Admission("bbb") != "" || r.Admission("") != "" {
		t.Fatal("admission leaked to another head")
	}
	// Same note again: unchanged. A new head replaces it.
	if st.NoteAdmission(key, "aaa", "operator-approval:octo", now.Add(time.Hour)) {
		t.Fatal("identical note reported a change")
	}
	if !st.NoteAdmission(key, "bbb", "operator-approval:mallory", now.Add(time.Hour)) {
		t.Fatal("new head must report a change")
	}
	if r, _ = st.Get(key); r.Admission("aaa") != "" || r.Admission("bbb") != "operator-approval:mallory" {
		t.Fatalf("replacement: %+v", r)
	}
}
