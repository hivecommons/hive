package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// recordingDeleteOtherUser is a second read-write member: not the recorder.
const recordingDeleteOtherUser = "someone-else@example.com"

// doRecordingAs sends one request as role with an explicit user.
func doRecordingAs(s *Server, method, path, role, user string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(nil))
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
		req.Header.Set("X-Hive-User", user)
		if role == config.RoleOwner {
			req.Header.Set(ownerRoleVerifiedHeader, "true")
		}
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// finishTestRecording records one chunk as role and finalizes it.
func finishTestRecording(t *testing.T, s *Server, role string) string {
	t.Helper()
	id := startTestRecording(t, s, role)
	if rec := doRecording(s, http.MethodPost, chunkPath(id, 0), role, recordingWebM, nil); rec.Code != http.StatusOK {
		t.Fatalf("chunk = %d %s", rec.Code, rec.Body)
	}
	if rec := doRecording(s, http.MethodPost, "/api/inbox/recordings/"+id+"/finalize", role, []byte(`{"duration_ms":4000}`), nil); rec.Code != http.StatusOK {
		t.Fatalf("finalize = %d %s", rec.Code, rec.Body)
	}
	return id
}

func recordingDeletePath(id string) string { return "/api/inbox/" + inbox.RecordingType + "/" + id }

func recordingFactPath(src *inbox.RecordingSource, id string) string {
	return filepath.Join(src.FactDir, connector.Slug(inbox.RecordingType, inbox.RecordingConnector, id)+".md")
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestInboxRecordingDeleteRoleMatrix(t *testing.T) {
	s, svc, src := newRecordingTestServer(t, config.InboxRecording{})
	id := finishTestRecording(t, s, config.RoleReadWrite)
	creator := config.RoleReadWrite + "@example.com"
	dir := filepath.Join(src.Store.Dir, id)
	if !exists(dir) || !exists(recordingFactPath(src, id)) {
		t.Fatal("finalized recording has no audio dir or fact")
	}
	for _, tc := range []struct{ name, role, user string }{
		{"anonymous", "", ""},
		{"read", config.RoleRead, config.RoleRead + "@example.com"},
		{"read as the recorder", config.RoleRead, creator},
		{"read-write non-creator", config.RoleReadWrite, recordingDeleteOtherUser},
	} {
		if rec := doRecordingAs(s, http.MethodDelete, recordingDeletePath(id), tc.role, tc.user); rec.Code != http.StatusForbidden {
			t.Fatalf("%s delete = %d %s, want 403", tc.name, rec.Code, rec.Body)
		}
	}
	if !exists(dir) || !exists(recordingFactPath(src, id)) {
		t.Fatal("a refused delete removed something")
	}
	rec := doRecordingAs(s, http.MethodDelete, recordingDeletePath(id), config.RoleReadWrite, creator)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"deleted":true`) {
		t.Fatalf("creator delete = %d %s", rec.Code, rec.Body)
	}
	if rec := doRecordingAs(s, http.MethodDelete, recordingDeletePath(id), config.RoleReadWrite, creator); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", rec.Code)
	}

	// An owner deletes a read-write member's recording.
	id2 := finishTestRecording(t, s, config.RoleReadWrite)
	if rec := doRecordingAs(s, http.MethodDelete, recordingDeletePath(id2), config.RoleOwner, config.RoleOwner+"@example.com"); rec.Code != http.StatusOK {
		t.Fatalf("owner delete = %d %s", rec.Code, rec.Body)
	}
	if e := s.audit.Recent(1); len(e) != 1 || e[0].Action != "inbox_recording_delete" || !strings.Contains(e[0].Detail, "recording="+id2) || !strings.Contains(e[0].Detail, "recorded_by="+creator) {
		t.Fatalf("delete not audited: %+v", e)
	}
	items, err := svc.Items(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Source == inbox.RecordingType {
			t.Fatalf("deleted recording still listed: %+v", it)
		}
	}
}

// Delete removes the stored audio, the Knowledge fact with its machine
// transcript, and the transcript record; the share history count is
// reported and what was shared is not touched.
func TestInboxRecordingDeleteRemovesAudioAndFact(t *testing.T) {
	s, svc, src := newRecordingTestServer(t, config.InboxRecording{})
	store := inbox.NewTranscriptStore(filepath.Join(t.TempDir(), "transcripts"))
	svc.EnableTranscription(store)
	id := finishTestRecording(t, s, config.RoleReadWrite)
	audio := filepath.Join(src.Store.Dir, id)
	fact := recordingFactPath(src, id)
	if err := store.Put(inbox.TranscriptRecord{Source: inbox.RecordingType, ID: id, Status: inbox.TranscriptionDone, Provider: "whisper", Model: "base.en", Text: "ship it", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.AppendShare(inbox.Key(inbox.RecordingType, id), inbox.ShareRecord{Destination: inbox.DestDibs, Actor: "owner@example.com", StartedAt: time.Now(), Status: inbox.StatusFiled, Ref: &inbox.ShareRef{URL: "https://dibs.example/1"}}); err != nil {
		t.Fatal(err)
	}

	// Refused while its transcription runs: nothing is removed.
	if err := store.Put(inbox.TranscriptRecord{Source: inbox.RecordingType, ID: id, Status: inbox.TranscriptionRunning, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if rec := doRecording(s, http.MethodDelete, recordingDeletePath(id), config.RoleReadWrite, nil, nil); rec.Code != http.StatusConflict {
		t.Fatalf("delete while transcribing = %d %s, want 409", rec.Code, rec.Body)
	}
	if !exists(audio) || !exists(fact) {
		t.Fatal("a refused delete removed the audio or the fact")
	}
	if err := store.Put(inbox.TranscriptRecord{Source: inbox.RecordingType, ID: id, Status: inbox.TranscriptionDone, Text: "ship it", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	rec := doRecording(s, http.MethodDelete, recordingDeletePath(id), config.RoleReadWrite, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Deleted     bool `json:"deleted"`
		FactRemoved bool `json:"fact_removed"`
		SharesKept  int  `json:"shares_kept"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Deleted || !body.FactRemoved || body.SharesKept != 1 {
		t.Fatalf("delete response = %+v", body)
	}
	if exists(audio) {
		t.Fatal("audio directory survived the delete")
	}
	if exists(fact) {
		t.Fatal("knowledge fact survived the delete")
	}
	if _, ok := store.Get(inbox.RecordingType, id); ok {
		t.Fatal("transcript record survived the delete")
	}
	if h := svc.Store.History(inbox.Key(inbox.RecordingType, id)); len(h) != 0 {
		t.Fatalf("item state survived: %+v", h)
	}
	if rec := doRecording(s, http.MethodGet, "/api/inbox/recordings/"+id+"/audio", config.RoleReadWrite, nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("audio after delete = %d, want 404", rec.Code)
	}
}

func TestInboxRecordingDeleteInProgressAndUnknown(t *testing.T) {
	s, _, _ := newRecordingTestServer(t, config.InboxRecording{})
	if rec := doRecording(s, http.MethodDelete, recordingDeletePath("nope"), config.RoleOwner, nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown = %d, want 404", rec.Code)
	}
	id := startTestRecording(t, s, config.RoleReadWrite)
	if rec := doRecording(s, http.MethodDelete, recordingDeletePath(id), config.RoleReadWrite, nil, nil); rec.Code != http.StatusConflict {
		t.Fatalf("in-progress delete = %d %s, want 409", rec.Code, rec.Body)
	}
}
