package dashboard

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/inbox"
)

// Recording straight into the Inbox (● Record). The browser streams the
// MediaRecorder output as ordered chunks while it records; see
// pkg/inbox/recordings.go for storage and docs/inbox-recording.md for the
// user guide. Every route needs an owner or read-write session; read-only
// sessions get 403.
func (s *Server) registerInboxRecordingRoutes() {
	s.mux.HandleFunc("POST /api/inbox/recordings", s.handleInboxRecordingStart)
	s.mux.HandleFunc("POST /api/inbox/recordings/{id}/chunks", s.handleInboxRecordingChunk)
	s.mux.HandleFunc("POST /api/inbox/recordings/{id}/finalize", s.handleInboxRecordingFinalize)
	s.mux.HandleFunc("DELETE /api/inbox/recordings/{id}", s.handleInboxRecordingAbort)
	s.mux.HandleFunc("GET /api/inbox/recordings/{id}/audio", s.handleInboxRecordingAudio)
	s.mux.HandleFunc("DELETE /api/inbox/hive-recording/{id}", s.handleInboxRecordingDelete)
}

// inboxRecordingStartRequest is POST /api/inbox/recordings.
type inboxRecordingStartRequest struct {
	Title            string `json:"title"`
	MIME             string `json:"mime"`
	ConsentVersion   string `json:"consent_version"`
	IncludesTabAudio bool   `json:"includes_tab_audio"`
}

// InboxRecordingStartResponse is what a start returns: the id and the
// limits the recorder must respect.
type InboxRecordingStartResponse struct {
	ID            string `json:"id"`
	MaxChunkBytes int    `json:"max_chunk_bytes"`
	MaxSeconds    int    `json:"max_seconds"`
	RetentionDays int    `json:"retention_days"`
	// Transcribes reports whether the hive transcribes the recording when
	// it ends (transcription enabled with consent).
	Transcribes bool `json:"transcribes"`
}

// InboxRecordingChunkResponse is POST /api/inbox/recordings/{id}/chunks.
type InboxRecordingChunkResponse struct {
	ID string `json:"id"`
	inbox.RecordingChunk
}

// InboxRecordingFinalizeResponse is POST /api/inbox/recordings/{id}/finalize.
type InboxRecordingFinalizeResponse struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	Title      string `json:"title"`
	DurationMS int64  `json:"duration_ms"`
	Bytes      int64  `json:"bytes"`
	Chunks     int    `json:"chunks"`
	// Transcribes reports whether the hive transcribes it now.
	Transcribes bool `json:"transcribes"`
}

// inboxRecordingError is a recording error body; ExpectedSeq / ActiveID are
// set for an out-of-order chunk and an already-active recording.
type inboxRecordingError struct {
	Error       string `json:"error"`
	ExpectedSeq *int   `json:"expected_seq,omitempty"`
	ActiveID    string `json:"active_id,omitempty"`
}

// inboxRecordingOptions audits as the requesting user.
func (s *Server) inboxRecordingOptions(r *http.Request) inbox.RecordingOptions {
	return inbox.RecordingOptions{Actor: requestUser(r), Audit: func(a inbox.RecordingAudit) {
		if s.audit != nil {
			s.audit.Log(a.Actor, a.Action, auditDetail(a.Fields...), "")
		}
	}, TranscribeAudit: func(a inbox.TranscribeAudit) {
		if s.audit != nil {
			s.audit.Log(a.Actor, "inbox_transcribe", auditDetail(a.Fields()...), "")
		}
	}}
}

// writeInboxRecordingError maps recording errors to statuses.
func writeInboxRecordingError(w http.ResponseWriter, err error) {
	var gap *inbox.RecordingGapError
	var active *inbox.RecordingActiveError
	body := inboxRecordingError{Error: err.Error()}
	code := http.StatusInternalServerError
	switch {
	case errors.As(err, &gap):
		code, body.ExpectedSeq = http.StatusConflict, &gap.Expected
	case errors.As(err, &active):
		code, body.ActiveID = http.StatusConflict, active.ID
	case errors.Is(err, inbox.ErrRecordingDisabled):
		code = http.StatusPreconditionFailed
	case errors.Is(err, inbox.ErrRecordingNotFound):
		code = http.StatusNotFound
	case errors.Is(err, inbox.ErrRecordingNotYours):
		code = http.StatusForbidden
	case errors.Is(err, inbox.ErrRecordingClosed), errors.Is(err, inbox.ErrRecordingMismatch),
		errors.Is(err, inbox.ErrRecordingNotFinished), errors.Is(err, inbox.ErrRecordingTranscribing):
		code = http.StatusConflict
	case errors.Is(err, inbox.ErrRecordingConsent), errors.Is(err, inbox.ErrRecordingBadRequest), errors.Is(err, inbox.ErrRecordingEmpty):
		code = http.StatusBadRequest
	case errors.Is(err, inbox.ErrRecordingMIME), errors.Is(err, inbox.ErrRecordingNotMedia):
		code = http.StatusUnsupportedMediaType
	case errors.Is(err, inbox.ErrRecordingTooLarge), errors.Is(err, inbox.ErrRecordingTooLong), errors.Is(err, inbox.ErrRecordingChunkSize):
		code = http.StatusRequestEntityTooLarge
	case errors.Is(err, inbox.ErrRecordingQuota):
		code = http.StatusInsufficientStorage
	case errors.Is(err, inbox.ErrRecordingExpired):
		code = http.StatusGone
	case errors.Is(err, inbox.ErrNoAudio):
		code = http.StatusNotFound
	default:
		body.Error = "recording failed: " + err.Error()
	}
	jsonStatusResponse(w, code, body)
}

// handleInboxRecordingStart (owner or read-write) starts a recording. The
// request must acknowledge the current consent notice; the notice version,
// the user and whether tab audio is included are stored with it. One
// recording per user may be in progress (409 with active_id).
func (s *Server) handleInboxRecordingStart(w http.ResponseWriter, r *http.Request) {
	if !requireInboxPlaybackRole(w, r) {
		return
	}
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	var req inboxRecordingStartRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	m, err := svc.StartRecording(inbox.RecordingStart{
		Title: sanitizeString(req.Title), MIME: req.MIME, ConsentVersion: req.ConsentVersion,
		IncludesTabAudio: req.IncludesTabAudio, User: requestUser(r), Role: r.Header.Get("X-Hive-Role"),
	}, s.inboxRecordingOptions(r))
	if err != nil {
		writeInboxRecordingError(w, err)
		return
	}
	st := svc.RecordingSettings()
	w.Header().Set("Cache-Control", "no-store")
	jsonStatusResponse(w, http.StatusCreated, InboxRecordingStartResponse{
		ID: m.ID, MaxChunkBytes: st.MaxChunkBytes, MaxSeconds: st.MaxSeconds, RetentionDays: st.RetentionDays, Transcribes: st.Transcribes,
	})
}

// handleInboxRecordingChunk (the recorder) stores chunk ?seq=N, the raw
// MediaRecorder bytes as the body (at most max_chunk_bytes). Chunks are
// accepted in order only: a gap answers 409 with expected_seq, and a re-send
// of a stored chunk with the same bytes answers 200 again (duplicate: true).
func (s *Server) handleInboxRecordingChunk(w http.ResponseWriter, r *http.Request) {
	if !requireInboxPlaybackRole(w, r) {
		return
	}
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	defer closeHTTPBody(r.Body)
	seq, err := strconv.Atoi(r.URL.Query().Get("seq"))
	if err != nil || seq < 0 {
		jsonError(w, "seq must be a non-negative integer", http.StatusBadRequest)
		return
	}
	body := http.MaxBytesReader(w, r.Body, inbox.RecordingMaxChunkBytes)
	data, err := io.ReadAll(body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			jsonError(w, fmt.Sprintf("a chunk must be at most %d bytes", inbox.RecordingMaxChunkBytes), http.StatusRequestEntityTooLarge)
			return
		}
		jsonError(w, "reading chunk: "+err.Error(), http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	res, err := svc.AppendRecordingChunk(id, requestUser(r), seq, data)
	if err != nil {
		writeInboxRecordingError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, InboxRecordingChunkResponse{ID: id, RecordingChunk: res})
}

// inboxRecordingFinalizeRequest is the optional finalize body.
type inboxRecordingFinalizeRequest struct {
	// DurationMS is the recorder's own count, pauses excluded.
	DurationMS int64 `json:"duration_ms"`
}

// handleInboxRecordingFinalize (the recorder) ends a recording: the chunks
// are concatenated into one audio file and the item appears in the Inbox.
func (s *Server) handleInboxRecordingFinalize(w http.ResponseWriter, r *http.Request) {
	if !requireInboxPlaybackRole(w, r) {
		return
	}
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	var req inboxRecordingFinalizeRequest
	if r.ContentLength != 0 {
		if err := decodeBody(r, &req); err != nil && !errors.Is(err, io.EOF) {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
	}
	m, err := svc.FinalizeRecording(strings.TrimSpace(r.PathValue("id")), requestUser(r), req.DurationMS, s.inboxRecordingOptions(r))
	if err != nil {
		writeInboxRecordingError(w, err)
		return
	}
	jsonResponse(w, InboxRecordingFinalizeResponse{
		ID: m.ID, State: m.State, Title: m.Title, DurationMS: m.DurationMS, Bytes: m.Bytes, Chunks: m.Chunks,
		Transcribes: svc.RecordingSettings().Transcribes,
	})
}

// handleInboxRecordingAbort discards an in-progress recording: the
// recorder's own, or anyone's for an owner.
func (s *Server) handleInboxRecordingAbort(w http.ResponseWriter, r *http.Request) {
	if !requireInboxPlaybackRole(w, r) {
		return
	}
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	asOwner := isOwnerRole(r.Header.Get("X-Hive-Role"))
	m, err := svc.AbortRecording(strings.TrimSpace(r.PathValue("id")), requestUser(r), asOwner, s.inboxRecordingOptions(r))
	if err != nil {
		writeInboxRecordingError(w, err)
		return
	}
	jsonResponse(w, map[string]any{"id": m.ID, "aborted": true})
}

// handleInboxRecordingDelete deletes a finished hive recording: the stored
// audio and chunks, the Inbox item and its Knowledge fact (the machine
// transcript). Owners may delete any recording, read-write members only
// their own, read members none (403). What was already shared from it is
// not touched. 409 while it is in progress or being transcribed.
func (s *Server) handleInboxRecordingDelete(w http.ResponseWriter, r *http.Request) {
	if !requireInboxPlaybackRole(w, r) {
		return
	}
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	asOwner := isOwnerRole(r.Header.Get("X-Hive-Role"))
	res, err := svc.DeleteRecording(strings.TrimSpace(r.PathValue("id")), requestUser(r), asOwner, s.inboxRecordingOptions(r))
	if err != nil && res.ID == "" {
		writeInboxRecordingError(w, err)
		return
	}
	if err != nil {
		// The audio and metadata are gone; a leftover (fact, transcript
		// record or item state) is logged, not reported as a failure.
		s.logger.Warn("inbox: recording delete left something behind", "recording", res.ID, "error", err)
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, map[string]any{"id": res.ID, "deleted": true, "fact_removed": res.FactRemoved, "shares_kept": res.SharesKept})
}

// handleInboxRecordingAudio (owner or read-write) serves a finished
// recording from this hive's disk, on the dashboard origin, with Range
// support (http.ServeContent) and no-store. After retention the audio is
// gone: 410. Opening a playback (no Range, or a range from byte 0) is
// audited as inbox_recording_audio; the player's seeks are not.
func (s *Server) handleInboxRecordingAudio(w http.ResponseWriter, r *http.Request) {
	if !requireInboxPlaybackRole(w, r) {
		return
	}
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	a, err := svc.OpenRecordingAudio(id)
	if err != nil {
		writeInboxLocalAudioError(w, svc, err)
		return
	}
	s.serveLocalAudio(w, r, a, "inbox_recording_audio", auditDetail("recording", id))
}

// serveLocalAudio writes a hive-local audio file with Range support.
func (s *Server) serveLocalAudio(w http.ResponseWriter, r *http.Request, a inbox.LocalAudio, action, detail string) {
	defer a.File.Close()
	if rng := r.Header.Get("Range"); rng == "" || strings.HasPrefix(rng, "bytes=0-") {
		s.auditFromRequest(r, action, detail, "")
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", a.MIME)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", "inline")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	http.ServeContent(w, r, a.Name, a.ModTime, a.File)
}

// writeInboxLocalAudioError answers a failed local audio open; expired
// audio is 410 with the retention named.
func writeInboxLocalAudioError(w http.ResponseWriter, svc *inbox.Service, err error) {
	w.Header().Set("Cache-Control", "no-store")
	if errors.Is(err, inbox.ErrRecordingExpired) {
		days := svc.RecordingSettings().RetentionDays
		jsonError(w, fmt.Sprintf("audio expired (kept %d days); the transcript and the Knowledge fact are kept", days), http.StatusGone)
		return
	}
	writeInboxRecordingError(w, err)
}
