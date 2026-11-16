package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inbox"
)

// recordingWebM is a first chunk that starts with the WebM (EBML) header.
var recordingWebM = append([]byte{0x1A, 0x45, 0xDF, 0xA3}, []byte("opus-frames-0123456789")...)

// newRecordingTestServer wires an Inbox with recording enabled.
func newRecordingTestServer(t *testing.T, rec config.InboxRecording) (*Server, *inbox.Service, *inbox.RecordingSource) {
	t.Helper()
	s, deps := apiServer(t)
	if rec.Enabled == nil {
		on := true
		rec.Enabled = &on
	}
	deps.Config.Inbox = config.InboxConfig{Recording: rec}
	dir := t.TempDir()
	svc := inbox.NewService(deps.Config.Inbox, inbox.NewStore(filepath.Join(dir, "_inbox.json")), nil, nil, nil)
	svc.Run = func(fn func()) { fn() }
	svc.EnableTranscription(inbox.NewTranscriptStore(filepath.Join(dir, "transcripts")))
	src := &inbox.RecordingSource{Store: inbox.NewRecordingStore(filepath.Join(dir, "recordings")), FactDir: filepath.Join(dir, "connectors", inbox.RecordingLayer)}
	svc.EnableRecordings(src)
	deps.Inbox = svc
	return s, svc, src
}

// doRecording sends one request as role (user <role>@example.com).
func doRecording(s *Server, method, path, role string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
		req.Header.Set("X-Hive-User", role+"@example.com")
		if role == config.RoleOwner {
			req.Header.Set(ownerRoleVerifiedHeader, "true")
		}
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func recordingStartBody(t *testing.T, consent string) []byte {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"title": "Weekly sync <b>x</b>", "mime": "audio/webm;codecs=opus", "consent_version": consent, "includes_tab_audio": false})
	return b
}

func startTestRecording(t *testing.T, s *Server, role string) string {
	t.Helper()
	rec := doRecording(s, http.MethodPost, "/api/inbox/recordings", role, recordingStartBody(t, inbox.RecordingConsentVersion), map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("start as %s = %d %s", role, rec.Code, rec.Body)
	}
	var resp InboxRecordingStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.MaxChunkBytes != inbox.RecordingMaxChunkBytes || resp.MaxSeconds != config.DefaultInboxRecordingMaxSeconds || resp.RetentionDays != config.DefaultInboxRecordingRetentionDays {
		t.Fatalf("start response = %+v", resp)
	}
	return resp.ID
}

func chunkPath(id string, seq int) string {
	return "/api/inbox/recordings/" + id + "/chunks?seq=" + strconv.Itoa(seq)
}

func TestInboxRecordingRoleMatrix(t *testing.T) {
	s, _, _ := newRecordingTestServer(t, config.InboxRecording{})
	body := recordingStartBody(t, inbox.RecordingConsentVersion)
	for _, role := range []string{"", config.RoleRead} {
		if rec := doRecording(s, http.MethodPost, "/api/inbox/recordings", role, body, nil); rec.Code != http.StatusForbidden {
			t.Fatalf("start as %q = %d, want 403", role, rec.Code)
		}
	}
	// An owner header without the verified marker is not an owner.
	req := httptest.NewRequest(http.MethodPost, "/api/inbox/recordings", bytes.NewReader(body))
	req.Header.Set("X-Hive-Role", config.RoleOwner)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unverified owner = %d, want 403", rec.Code)
	}
	id := startTestRecording(t, s, config.RoleReadWrite)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, chunkPath(id, 0)},
		{http.MethodPost, "/api/inbox/recordings/" + id + "/finalize"},
		{http.MethodDelete, "/api/inbox/recordings/" + id},
		{http.MethodGet, "/api/inbox/recordings/" + id + "/audio"},
	} {
		if rec := doRecording(s, tc.method, tc.path, config.RoleRead, recordingWebM, nil); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s as read-only = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
	startTestRecording(t, s, config.RoleOwner)
	if e := s.audit.Recent(1); len(e) != 1 || e[0].Action != "inbox_recording_start" || !strings.Contains(e[0].Detail, "consent_version="+inbox.RecordingConsentVersion) {
		t.Fatalf("start not audited: %+v", e)
	}
}

func TestInboxRecordingDisabledAndConsent(t *testing.T) {
	off := false
	s, _, _ := newRecordingTestServer(t, config.InboxRecording{Enabled: &off})
	if rec := doRecording(s, http.MethodPost, "/api/inbox/recordings", config.RoleReadWrite, recordingStartBody(t, inbox.RecordingConsentVersion), nil); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("disabled start = %d, want 412", rec.Code)
	}
	s, _, _ = newRecordingTestServer(t, config.InboxRecording{})
	if rec := doRecording(s, http.MethodPost, "/api/inbox/recordings", config.RoleReadWrite, recordingStartBody(t, ""), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("start without consent = %d, want 400", rec.Code)
	}
	b, _ := json.Marshal(map[string]any{"mime": "audio/ogg", "consent_version": inbox.RecordingConsentVersion})
	if rec := doRecording(s, http.MethodPost, "/api/inbox/recordings", config.RoleReadWrite, b, nil); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("ogg start = %d, want 415", rec.Code)
	}
	id := startTestRecording(t, s, config.RoleReadWrite)
	rec := doRecording(s, http.MethodPost, "/api/inbox/recordings", config.RoleReadWrite, recordingStartBody(t, inbox.RecordingConsentVersion), nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"active_id":"`+id+`"`) {
		t.Fatalf("second start = %d %s, want 409 with active_id", rec.Code, rec.Body)
	}
}

func TestInboxRecordingChunksFinalizeAndAudio(t *testing.T) {
	s, svc, src := newRecordingTestServer(t, config.InboxRecording{})
	id := startTestRecording(t, s, config.RoleReadWrite)
	if m, _ := src.Store.Get(id); m.Title != "Weekly sync x" {
		t.Fatalf("title not sanitized: %q", m.Title)
	}
	if rec := doRecording(s, http.MethodPost, chunkPath(id, 0), config.RoleReadWrite, []byte("<html>"), nil); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-media first chunk = %d, want 415", rec.Code)
	}
	if rec := doRecording(s, http.MethodPost, "/api/inbox/recordings/"+id+"/chunks?seq=x", config.RoleReadWrite, recordingWebM, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad seq = %d, want 400", rec.Code)
	}
	if rec := doRecording(s, http.MethodPost, chunkPath(id, 0), config.RoleReadWrite, make([]byte, inbox.RecordingMaxChunkBytes+1), nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize chunk = %d, want 413", rec.Code)
	}
	if rec := doRecording(s, http.MethodPost, chunkPath(id, 0), config.RoleOwner, recordingWebM, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("someone else's chunk = %d, want 403", rec.Code)
	}
	if rec := doRecording(s, http.MethodPost, chunkPath(id, 0), config.RoleReadWrite, recordingWebM, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"next_seq":1`) {
		t.Fatalf("chunk 0 = %d %s", rec.Code, rec.Body)
	}
	rec := doRecording(s, http.MethodPost, chunkPath(id, 2), config.RoleReadWrite, []byte("gap"), nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"expected_seq":1`) {
		t.Fatalf("gap = %d %s, want 409 expected_seq 1", rec.Code, rec.Body)
	}
	rec = doRecording(s, http.MethodPost, chunkPath(id, 0), config.RoleReadWrite, recordingWebM, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"duplicate":true`) {
		t.Fatalf("idempotent resend = %d %s", rec.Code, rec.Body)
	}
	if rec := doRecording(s, http.MethodPost, chunkPath(id, 0), config.RoleReadWrite, append(append([]byte{}, recordingWebM...), 'x'), nil); rec.Code != http.StatusConflict {
		t.Fatalf("resend with other bytes = %d, want 409", rec.Code)
	}
	tail := []byte("cluster-two")
	if rec := doRecording(s, http.MethodPost, chunkPath(id, 1), config.RoleReadWrite, tail, nil); rec.Code != http.StatusOK {
		t.Fatalf("chunk 1 = %d %s", rec.Code, rec.Body)
	}
	if rec := doRecording(s, http.MethodGet, "/api/inbox/recordings/"+id+"/audio", config.RoleReadWrite, nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("audio before finalize = %d, want 404", rec.Code)
	}
	rec = doRecording(s, http.MethodPost, "/api/inbox/recordings/"+id+"/finalize", config.RoleReadWrite, []byte(`{"duration_ms":1500}`), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"finalized"`) {
		t.Fatalf("finalize = %d %s", rec.Code, rec.Body)
	}
	if e := s.audit.Recent(1); len(e) != 1 || e[0].Action != "inbox_recording_finalize" {
		t.Fatalf("finalize not audited: %+v", e)
	}
	whole := append(append([]byte{}, recordingWebM...), tail...)

	audioPath := "/api/inbox/recordings/" + id + "/audio"
	rec = doRecording(s, http.MethodGet, audioPath, config.RoleReadWrite, nil, nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), whole) {
		t.Fatalf("audio = %d %q", rec.Code, rec.Body.Bytes())
	}
	for k, want := range map[string]string{"Cache-Control": "no-store", "Content-Type": "audio/webm", "X-Content-Type-Options": "nosniff", "Accept-Ranges": "bytes", "Cross-Origin-Resource-Policy": "same-origin"} {
		if got := rec.Header().Get(k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
	if e := s.audit.Recent(1); len(e) != 1 || e[0].Action != "inbox_recording_audio" {
		t.Fatalf("playback not audited: %+v", e)
	}
	audited := len(s.audit.Recent(0))
	rec = doRecording(s, http.MethodGet, audioPath, config.RoleReadWrite, nil, map[string]string{"Range": "bytes=4-9"})
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), whole[4:10]) || rec.Header().Get("Content-Range") != "bytes 4-9/"+strconv.Itoa(len(whole)) {
		t.Fatalf("range = %d %q %q", rec.Code, rec.Body.Bytes(), rec.Header().Get("Content-Range"))
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("range response is cacheable")
	}
	if n := len(s.audit.Recent(0)); n != audited {
		t.Fatalf("a seek was audited again: %d entries, want %d", n, audited)
	}

	// The Inbox player's generic stream path serves the same file from disk.
	stream := doRecording(s, http.MethodGet, "/api/inbox/"+inbox.RecordingType+"/"+id+"/audio/stream", config.RoleReadWrite, nil, map[string]string{"Range": "bytes=0-3"})
	if stream.Code != http.StatusPartialContent || !bytes.Equal(stream.Body.Bytes(), whole[:4]) || stream.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("stream = %d %q", stream.Code, stream.Body.Bytes())
	}
	list := doRecording(s, http.MethodGet, "/api/inbox", config.RoleReadWrite, nil, nil)
	if !strings.Contains(list.Body.String(), `"source":"`+inbox.RecordingType+`"`) || !strings.Contains(list.Body.String(), `"recorded_by":"read-write@example.com"`) || !strings.Contains(list.Body.String(), `"recording":{"enabled":true`) {
		t.Fatalf("inbox listing lacks the recording: %s", list.Body)
	}

	// Retention: after the audio is deleted the endpoint answers 410.
	src.Store.Now = func() time.Time { return time.Now().Add(config.DefaultInboxRecordingRetentionDays * 24 * time.Hour) }
	if res, err := svc.RecordingSweep(inbox.RecordingOptions{}); err != nil || len(res.Expired) != 1 {
		t.Fatalf("sweep = %+v, %v", res, err)
	}
	rec = doRecording(s, http.MethodGet, audioPath, config.RoleReadWrite, nil, nil)
	if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "audio expired (kept 5 days)") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expired audio = %d %s", rec.Code, rec.Body)
	}
	if rec := doRecording(s, http.MethodGet, "/api/inbox/"+inbox.RecordingType+"/"+id+"/audio/stream", config.RoleReadWrite, nil, nil); rec.Code != http.StatusGone {
		t.Fatalf("expired stream = %d, want 410", rec.Code)
	}
}

func TestInboxRecordingAbortAndCaps(t *testing.T) {
	s, _, _ := newRecordingTestServer(t, config.InboxRecording{MaxBytes: 30, QuotaBytes: 40})
	id := startTestRecording(t, s, config.RoleReadWrite)
	if rec := doRecording(s, http.MethodPost, chunkPath(id, 0), config.RoleReadWrite, recordingWebM, nil); rec.Code != http.StatusOK {
		t.Fatalf("chunk 0 = %d %s", rec.Code, rec.Body)
	}
	if rec := doRecording(s, http.MethodPost, chunkPath(id, 1), config.RoleReadWrite, []byte("0123456789"), nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over max_bytes = %d, want 413", rec.Code)
	}
	other := startTestRecording(t, s, config.RoleOwner)
	if rec := doRecording(s, http.MethodPost, chunkPath(other, 0), config.RoleOwner, recordingWebM, nil); rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("over quota = %d %s, want 507", rec.Code, rec.Body)
	}
	if rec := doRecording(s, http.MethodDelete, "/api/inbox/recordings/"+id, config.RoleOwner, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("owner abort = %d %s", rec.Code, rec.Body)
	}
	if e := s.audit.Recent(1); len(e) != 1 || e[0].Action != "inbox_recording_abort" {
		t.Fatalf("abort not audited: %+v", e)
	}
	if rec := doRecording(s, http.MethodPost, "/api/inbox/recordings/"+id+"/finalize", config.RoleReadWrite, nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("finalize aborted = %d, want 404", rec.Code)
	}
	if rec := doRecording(s, http.MethodPost, "/api/inbox/recordings/"+other+"/finalize", config.RoleOwner, nil, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("finalize empty = %d, want 400", rec.Code)
	}
}
