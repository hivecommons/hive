package dashboard

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// createProposalBead creates a transcript-derived proposal bead the way the
// brainstorm prompt instructs (proposed=true, no confirmed).
func createProposalBead(t *testing.T, store *beads.Store, title string) *beads.Bead {
	t.Helper()
	b := createFactBead(t, store, title, "requirement")
	if err := store.SetMetadata(b.ID, "proposed", "true"); err != nil {
		t.Fatalf("set proposed: %v", err)
	}
	if err := store.SetMetadata(b.ID, "source_ref", "transcripts/meeting.txt:1-2"); err != nil {
		t.Fatalf("set source_ref: %v", err)
	}
	return b
}

// TestCheckForFactsProposalOnlyTickDoesNotAdvance reproduces #9171: a tick
// that sees only unconfirmed proposals must not advance to scaffold on the
// Q&A-derived facts, and must leave every proposal pending for review.
func TestCheckForFactsProposalOnlyTickDoesNotAdvance(t *testing.T) {
	w, eng, store := factWatcher(t)
	for i := 0; i < targetFactCount; i++ {
		createProposalBead(t, store, fmt.Sprintf("Proposal %c", 'A'+i))
	}

	w.checkForFacts(context.Background(), w.findInceptionBeads())

	st := eng.GetState()
	if st.Phase != knowledge.PhaseStructure {
		t.Fatalf("phase = %s after a proposal-only tick, want structure", st.Phase)
	}
	if len(st.ProposedFacts) != targetFactCount {
		t.Fatalf("pending proposals = %d, want %d", len(st.ProposedFacts), targetFactCount)
	}
	if !w.factGraceStart.IsZero() {
		t.Fatal("unconfirmed proposals started the enrichment grace period")
	}
}

// TestCheckForFactsProposalsDoNotCountTowardTarget: 3 confirmed facts plus 5
// proposals must not reach targetFactCount and advance immediately.
func TestCheckForFactsProposalsDoNotCountTowardTarget(t *testing.T) {
	w, eng, store := factWatcher(t)
	createFactBead(t, store, "Vision statement", "vision")
	createFactBead(t, store, "Requirement one", "requirement")
	createFactBead(t, store, "Constraint one", "constraint")
	for i := 0; i < targetFactCount-3; i++ {
		createProposalBead(t, store, fmt.Sprintf("Proposal %c", 'A'+i))
	}

	w.checkForFacts(context.Background(), w.findInceptionBeads())

	if st := eng.GetState(); st.Phase != knowledge.PhaseStructure {
		t.Fatalf("phase = %s: proposals were counted toward the target", st.Phase)
	}
	if w.factGraceStart.IsZero() {
		t.Fatal("3 confirmed facts should start the grace period")
	}
}

// TestCheckForFactsAdvanceKeepsUnconfirmedProposals: once the grace period
// elapses the watcher advances on confirmed facts, but the proposals nobody
// confirmed survive in state instead of being wiped.
func TestCheckForFactsAdvanceKeepsUnconfirmedProposals(t *testing.T) {
	w, eng, store := factWatcher(t)
	createFactBead(t, store, "Vision statement", "vision")
	createFactBead(t, store, "Requirement one", "requirement")
	createFactBead(t, store, "Constraint one", "constraint")
	createProposalBead(t, store, "Proposal pending")
	confirmed := createProposalBead(t, store, "Proposal confirmed")
	if err := store.SetMetadata(confirmed.ID, "confirmed", "true"); err != nil {
		t.Fatalf("set confirmed: %v", err)
	}

	w.checkForFacts(context.Background(), w.findInceptionBeads())
	w.factGraceStart = time.Now().Add(-factEnrichmentGracePeriod - time.Second)
	w.checkForFacts(context.Background(), w.findInceptionBeads())

	st := eng.GetState()
	if st.Phase != knowledge.PhaseScaffold {
		t.Fatalf("phase = %s, want scaffold after grace", st.Phase)
	}
	if len(st.ProposedFacts) != 1 || st.ProposedFacts[0].Title != "Proposal pending" {
		t.Fatalf("pending proposals = %+v, want only the unconfirmed one", st.ProposedFacts)
	}
}

// TestFactSetKeyDetectsSameSizeChange pins the dedupe key: an equal-size set
// with a different member (a confirmation replacing a supplemented fact) must
// not look unchanged, while reordering must.
func TestFactSetKeyDetectsSameSizeChange(t *testing.T) {
	a := []knowledge.IdeationFact{{Title: "one", Type: knowledge.FactVision}, {Title: "two", Type: knowledge.FactRequirement}}
	reordered := []knowledge.IdeationFact{a[1], a[0]}
	changed := []knowledge.IdeationFact{a[0], {Title: "three", Type: knowledge.FactRequirement, Confirmed: true}}
	if factSetKey(a) != factSetKey(reordered) {
		t.Fatal("fact set key depends on order")
	}
	if factSetKey(a) == factSetKey(changed) {
		t.Fatal("same-size change produced an identical fact set key")
	}
}

func postTranscriptUpload(t *testing.T, s *Server, filename string, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("source", "transcript"); err != nil {
		t.Fatalf("source field: %v", err)
	}
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/inception/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func errorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	msg, _ := resp["error"].(string)
	return msg
}

// TestTranscriptImportSurfacesRealError: a plain transcript upload with no
// inception running must report that, not a bogus "must be a zip" error.
func TestTranscriptImportSurfacesRealError(t *testing.T) {
	s, _, _ := covFInceptionServer(t)

	rec := postTranscriptUpload(t, s, "meeting.txt", []byte("Agreed: ship it"))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if msg := errorMessage(t, rec); !strings.Contains(msg, "no inception in progress") {
		t.Fatalf("error = %q, want the ImportTranscript error", msg)
	}
}

// TestTranscriptImportZipWithNothingImportedIs400: a zip whose entries all
// fail (or that has no transcript entry) must not return 200 imported=0.
func TestTranscriptImportZipWithNothingImportedIs400(t *testing.T) {
	s, _, _ := covFInceptionServer(t)

	rec := postTranscriptUpload(t, s, "notes.zip", zipOf(t, map[string]string{"a.txt": "one", "b.md": "two"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("failing entries: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if msg := errorMessage(t, rec); !strings.Contains(msg, "no inception in progress") {
		t.Fatalf("failing entries: error = %q, want the entry's ImportTranscript error", msg)
	}

	if r := doPost(s, "/api/inception/start", map[string]interface{}{"idea": "zip transcript fixture"}); r.Code != http.StatusOK {
		t.Fatalf("start: %d body=%s", r.Code, r.Body.String())
	}
	rec = postTranscriptUpload(t, s, "images.zip", zipOf(t, map[string]string{"diagram.png": "binary"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no transcript entry: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	rec = postTranscriptUpload(t, s, "notes.zip", zipOf(t, map[string]string{"a.txt": "one", "skip.png": "x"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("valid zip: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}
