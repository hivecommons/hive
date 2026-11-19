package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Desktop companion meetings (hivecommons/hive#18596). The companion app
// captures a person's own meeting client audio and microphone after a
// consent prompt and streams it here as a live meeting Betty takes part in.
// Every route needs an owner or read-write session; a companion meeting can
// be used only by the person who started it (owners may use any). The
// meetings feature arrives through MeetingsService; companion support is the
// optional CompanionMeetingsService on the same value.

// CompanionStartRequest is POST /api/companion/meetings.
type CompanionStartRequest struct {
	Title          string `json:"title"`
	App            string `json:"app"`
	HostName       string `json:"host_name"`
	ConsentVersion string `json:"consent_version"`
	// ConsentAttested is the person's confirmation that the participants
	// were told and agreed.
	ConsentAttested bool `json:"consent_attested"`
}

// CompanionMeetingsService is the companion side of the meetings feature.
type CompanionMeetingsService interface {
	// CompanionInfo is what the companion needs before a capture: the
	// consent notice and its version, the PCM format and the chunk limit.
	CompanionInfo() any
	StartCompanion(ctx context.Context, req CompanionStartRequest, user string) (any, error)
	CompanionAudio(ctx context.Context, id, user string, asOwner bool, channel string, seq int, pcm []byte) (any, error)
	CompanionReplies(ctx context.Context, id, user string, asOwner bool, after int64) (any, error)
	EndCompanion(ctx context.Context, id, user string, asOwner bool) (any, error)
}

// ErrMeetingForbidden is another person's companion meeting.
var ErrMeetingForbidden = errors.New("this companion meeting belongs to someone else")

// ErrMeetingUnavailable means companion meetings cannot run on this hive.
var ErrMeetingUnavailable = errors.New("companion meetings are not available on this hive")

// CompanionGapError is an out-of-order audio chunk.
type CompanionGapError struct{ Expected int }

func (e *CompanionGapError) Error() string {
	return fmt.Sprintf("chunk out of order; expected seq %d", e.Expected)
}

// companionMaxChunkBytes bounds one audio chunk body read here; the
// meetings service applies the exact limit it reports in CompanionInfo.
const companionMaxChunkBytes = 131072

func (s *Server) registerCompanionRoutes() {
	s.mux.HandleFunc("GET /api/companion/info", s.handleCompanionInfo)
	s.mux.HandleFunc("POST /api/companion/meetings", s.handleCompanionStart)
	s.mux.HandleFunc("POST /api/companion/meetings/{id}/audio", s.handleCompanionAudio)
	s.mux.HandleFunc("GET /api/companion/meetings/{id}/replies", s.handleCompanionReplies)
	s.mux.HandleFunc("POST /api/companion/meetings/{id}/end", s.handleCompanionEnd)
}

// companionService admits owner or read-write and returns the companion
// side of the meetings feature, answering the error itself otherwise.
func (s *Server) companionService(w http.ResponseWriter, r *http.Request) (CompanionMeetingsService, bool) {
	if !requireInboxPlaybackRole(w, r) {
		return nil, false
	}
	svc, ok := s.meetingsService(w)
	if !ok {
		return nil, false
	}
	cs, ok := svc.(CompanionMeetingsService)
	if !ok {
		jsonError(w, ErrMeetingUnavailable.Error(), http.StatusServiceUnavailable)
		return nil, false
	}
	return cs, true
}

// requestIsOwner reports a verified owner.
func requestIsOwner(r *http.Request) bool {
	return isOwnerRole(r.Header.Get("X-Hive-Role")) && r.Header.Get(ownerRoleVerifiedHeader) == "true"
}

func (s *Server) companionError(w http.ResponseWriter, r *http.Request, err error) {
	var gap *CompanionGapError
	switch {
	case errors.As(err, &gap):
		expected := gap.Expected
		jsonStatusResponse(w, http.StatusConflict, inboxRecordingError{Error: err.Error(), ExpectedSeq: &expected})
	case errors.Is(err, ErrMeetingForbidden):
		jsonError(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, ErrMeetingUnavailable):
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
	default:
		s.meetingsError(w, r, err)
	}
}

func (s *Server) handleCompanionInfo(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.companionService(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, cs.CompanionInfo())
}

func (s *Server) handleCompanionStart(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.companionService(w, r)
	if !ok {
		return
	}
	var req CompanionStartRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Title, req.App, req.HostName = sanitizeString(req.Title), sanitizeString(req.App), sanitizeString(req.HostName)
	ctx, cancel := context.WithTimeout(r.Context(), meetingsCallTimeout)
	defer cancel()
	m, err := cs.StartCompanion(ctx, req, requestUser(r))
	if err != nil {
		s.companionError(w, r, err)
		return
	}
	s.auditFromRequest(r, "meeting_companion_start", auditDetail("app", req.App, "consent_version", req.ConsentVersion), "")
	w.Header().Set("Cache-Control", "no-store")
	jsonStatusResponse(w, http.StatusCreated, m)
}

// handleCompanionAudio stores chunk ?channel=call|mic&seq=N: the body is
// raw 16 kHz mono s16le PCM. Chunks are accepted in order per channel; a
// gap answers 409 with expected_seq and a resend is acknowledged again.
func (s *Server) handleCompanionAudio(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.companionService(w, r)
	if !ok {
		return
	}
	defer closeHTTPBody(r.Body)
	channel := r.URL.Query().Get("channel")
	seq, err := strconv.Atoi(r.URL.Query().Get("seq"))
	if err != nil || seq < 0 {
		jsonError(w, "seq must be a non-negative integer", http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, companionMaxChunkBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			jsonError(w, fmt.Sprintf("a chunk must be at most %d bytes", companionMaxChunkBytes), http.StatusRequestEntityTooLarge)
			return
		}
		jsonError(w, "reading chunk: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), meetingsCallTimeout)
	defer cancel()
	res, err := cs.CompanionAudio(ctx, strings.TrimSpace(r.PathValue("id")), requestUser(r), requestIsOwner(r), channel, seq, data)
	if err != nil {
		s.companionError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, res)
}

// handleCompanionReplies returns Betty's lines after ?after=N and the
// meeting's state.
func (s *Server) handleCompanionReplies(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.companionService(w, r)
	if !ok {
		return
	}
	var after int64
	if raw := r.URL.Query().Get("after"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			jsonError(w, "after must be a non-negative integer", http.StatusBadRequest)
			return
		}
		after = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), meetingsCallTimeout)
	defer cancel()
	res, err := cs.CompanionReplies(ctx, strings.TrimSpace(r.PathValue("id")), requestUser(r), requestIsOwner(r), after)
	if err != nil {
		s.companionError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, res)
}

func (s *Server) handleCompanionEnd(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.companionService(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	ctx, cancel := context.WithTimeout(r.Context(), meetingsCallTimeout)
	defer cancel()
	m, err := cs.EndCompanion(ctx, id, requestUser(r), requestIsOwner(r))
	if err != nil {
		s.companionError(w, r, err)
		return
	}
	s.auditFromRequest(r, "meeting_companion_end", auditDetail("meeting", id), "")
	jsonResponse(w, m)
}
