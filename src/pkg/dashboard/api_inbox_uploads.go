package dashboard

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/inbox"
)

const (
	// inboxUploadFormName is the multipart field each file travels in.
	inboxUploadFormName = "file"
	// inboxUploadOverheadBytes is the multipart framing allowed on top of
	// the files themselves.
	inboxUploadOverheadBytes = 64 << 10
	// inboxUploadTimeout bounds one upload request's extraction and storage.
	inboxUploadTimeout = 2 * time.Minute
	// inboxUploadReadWindow replaces the dashboard's 30-second read timeout
	// for an upload body: a 25 MB document or a 200 MB recording on a slow
	// link needs longer to arrive. inboxUploadMediaTimeout bounds a request
	// that carries audio or video.
	inboxUploadReadWindow   = 20 * time.Minute
	inboxUploadMediaTimeout = 25 * time.Minute
)

// registerInboxUploadRoutes adds the drag-and-drop ingest routes.
func (s *Server) registerInboxUploadRoutes() {
	s.mux.HandleFunc("POST /api/inbox/uploads", s.handleInboxUpload)
	s.mux.HandleFunc("POST /api/inbox/uploads/url", s.handleInboxUploadURL)
}

// InboxUploadResult is one file of POST /api/inbox/uploads.
type InboxUploadResult struct {
	Filename string `json:"filename"`
	OK       bool   `json:"ok"`
	Source   string `json:"source,omitempty"`
	ItemID   string `json:"item_id,omitempty"`
	Title    string `json:"title,omitempty"`
	Kind     string `json:"kind,omitempty"`
	// Code is the machine-readable reason a file was refused (too_large,
	// unsupported_type, parse_failed, quota_exceeded, too_many_files,
	// empty, internal).
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// InboxUploadResponse is POST /api/inbox/uploads.
type InboxUploadResponse struct {
	Results []InboxUploadResult `json:"results"`
}

// inboxUploadStatus maps an upload error code to the HTTP status a
// single-file request answers with.
func inboxUploadStatus(code string) int {
	switch code {
	case inbox.UploadErrTooLarge:
		return http.StatusRequestEntityTooLarge
	case inbox.UploadErrUnsupported:
		return http.StatusUnsupportedMediaType
	case inbox.UploadErrParse:
		return http.StatusUnprocessableEntity
	case inbox.UploadErrQuota:
		return http.StatusInsufficientStorage
	case inbox.UploadErrEmpty, inbox.UploadErrTooMany, inbox.UploadErrLinkRefused:
		return http.StatusBadRequest
	case inbox.UploadErrFetch:
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}

// requireInboxAddRole admits owners (verified) and read-write sessions, and
// refuses a browser request another site initiated (the session cookie is
// SameSite=Lax already; this is the second lock).
func requireInboxAddRole(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		jsonError(w, "cross-site uploads are refused", http.StatusForbidden)
		return false
	}
	return requireInboxPlaybackRole(w, r)
}

// handleInboxUpload (owner or read-write) adds dropped files to the Inbox.
// Each multipart "file" part is sniffed by content (never by its name or
// client MIME type alone), extracted to markdown, stored under
// connector-state/uploads/<id>/ (0700/0600) and written as a Knowledge fact.
// Every file is audited as inbox_upload with its outcome. One file answers
// with the status of its outcome (201, or 400/413/415/422/507); several
// files answer 200 with per-file results unless all of them failed.
func (s *Server) handleInboxUpload(w http.ResponseWriter, r *http.Request) {
	if !requireInboxAddRole(w, r) {
		return
	}
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	if svc.Uploads() == nil {
		jsonError(w, inbox.ErrUploadsDisabled.Error(), http.StatusNotFound)
		return
	}
	lim := svc.Config().Uploads
	maxFile, maxFiles := lim.EffectiveMaxFileBytes(), lim.EffectiveMaxFilesPerDrop()
	// Audio and video stream to disk under the recording cap.
	maxMedia := svc.Config().Recording.EffectiveMaxBytes()
	r.Body = http.MaxBytesReader(w, r.Body, max(maxFile, maxMedia)*int64(maxFiles)+inboxUploadOverheadBytes)
	// Best effort: a server without per-request deadlines keeps its own.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(inboxUploadReadWindow))
	mr, err := r.MultipartReader()
	if err != nil {
		jsonError(w, "expected a multipart upload with one or more \"file\" fields", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), inboxUploadMediaTimeout)
	defer cancel()
	user := requestUser(r)
	var results []InboxUploadResult
	files := 0
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if isUploadTooLarge(err) {
				jsonError(w, "the upload is too large: at most "+strconv.Itoa(maxFiles)+" files of up to "+strconv.FormatInt(maxFile, 10)+" bytes each", http.StatusRequestEntityTooLarge)
				return
			}
			jsonError(w, "malformed multipart upload", http.StatusBadRequest)
			return
		}
		if part.FormName() != inboxUploadFormName || part.FileName() == "" {
			continue
		}
		files++
		name := inbox.CleanUploadName(part.FileName())
		if files > maxFiles {
			results = append(results, s.inboxUploadRefused(r, name, 0, inbox.UploadErrTooMany, "too many files in one drop: at most "+strconv.Itoa(maxFiles)))
			continue
		}
		br := bufio.NewReaderSize(part, inbox.MediaSniffBytes)
		head, _ := br.Peek(inbox.MediaSniffBytes)
		if _, media, uerr := inbox.SniffMedia(name, head); uerr != nil {
			results = append(results, s.inboxUploadRefused(r, name, 0, uerr.Code, uerr.Message))
			continue
		} else if media {
			results = append(results, s.inboxUploadMedia(ctx, r, svc, part.FileName(), name, br, len(head)))
			continue
		}
		data, err := io.ReadAll(io.LimitReader(br, maxFile+1))
		if err != nil {
			if isUploadTooLarge(err) {
				jsonError(w, "the upload is too large", http.StatusRequestEntityTooLarge)
				return
			}
			results = append(results, s.inboxUploadRefused(r, name, 0, inbox.UploadErrInternal, "reading the file failed"))
			continue
		}
		if int64(len(data)) > maxFile {
			results = append(results, s.inboxUploadRefused(r, name, int64(len(data)), inbox.UploadErrTooLarge, "the file is too large (the limit is "+strconv.FormatInt(maxFile, 10)+" bytes)"))
			continue
		}
		it, err := svc.AddUpload(ctx, inbox.UploadInput{Filename: part.FileName(), Data: data, AddedBy: user})
		if err != nil {
			var ue *inbox.UploadError
			if errors.As(err, &ue) {
				results = append(results, s.inboxUploadRefused(r, name, int64(len(data)), ue.Code, ue.Message))
				continue
			}
			if s.logger != nil {
				s.logger.Warn("inbox: upload failed", "error", err)
			}
			results = append(results, s.inboxUploadRefused(r, name, int64(len(data)), inbox.UploadErrInternal, "storing the file failed"))
			continue
		}
		s.auditFromRequest(r, "inbox_upload", auditDetail("source", it.Source, "item", it.ID, "kind", it.UploadKind, "bytes", strconv.Itoa(len(data)), "outcome", "added"), "")
		results = append(results, InboxUploadResult{Filename: name, OK: true, Source: it.Source, ItemID: it.ID, Title: it.Title, Kind: it.UploadKind})
	}
	if len(results) == 0 {
		jsonError(w, "no file in the upload: send one or more multipart \"file\" fields", http.StatusBadRequest)
		return
	}
	code := http.StatusOK
	allFailed := true
	for _, res := range results {
		if res.OK {
			allFailed = false
		}
	}
	switch {
	case len(results) == 1 && results[0].OK:
		code = http.StatusCreated
	case allFailed:
		code = inboxUploadStatus(results[0].Code)
	}
	jsonStatusResponse(w, code, InboxUploadResponse{Results: results})
}

// inboxUploadMedia streams one audio or video part into the store and
// queues its transcription when transcription is on.
func (s *Server) inboxUploadMedia(ctx context.Context, r *http.Request, svc *inbox.Service, raw, name string, br *bufio.Reader, headLen int) InboxUploadResult {
	head := make([]byte, headLen)
	if _, err := io.ReadFull(br, head); err != nil {
		return s.inboxUploadRefused(r, name, 0, inbox.UploadErrInternal, "reading the file failed")
	}
	user := requestUser(r)
	opts := inbox.TranscribeOptions{Actor: user, Audit: func(a inbox.TranscribeAudit) {
		if s.audit != nil {
			s.audit.Log(a.Actor, "inbox_transcribe", auditDetail(a.Fields()...), "")
		}
	}}
	it, err := svc.AddMedia(ctx, inbox.MediaInput{Filename: raw, Head: head, Body: br, AddedBy: user}, opts)
	if err != nil {
		if isUploadTooLarge(err) {
			return s.inboxUploadRefused(r, name, 0, inbox.UploadErrTooLarge, "the upload is too large")
		}
		var ue *inbox.UploadError
		if errors.As(err, &ue) {
			return s.inboxUploadRefused(r, name, 0, ue.Code, ue.Message)
		}
		if s.logger != nil {
			s.logger.Warn("inbox: media upload failed", "error", err)
		}
		return s.inboxUploadRefused(r, name, 0, inbox.UploadErrInternal, "storing the file failed")
	}
	s.auditFromRequest(r, "inbox_upload", auditDetail("source", it.Source, "item", it.ID, "kind", it.UploadKind, "outcome", "added"), "")
	return InboxUploadResult{Filename: name, OK: true, Source: it.Source, ItemID: it.ID, Title: it.Title, Kind: it.UploadKind}
}

// inboxUploadRefused audits a refused file and returns its result. The
// audit records the reason code and size, not the file's content.
func (s *Server) inboxUploadRefused(r *http.Request, name string, size int64, code, msg string) InboxUploadResult {
	s.auditFromRequest(r, "inbox_upload", auditDetail("source", inbox.UploadType, "bytes", strconv.FormatInt(size, 10), "outcome", "refused", "code", code), "")
	return InboxUploadResult{Filename: name, Code: code, Error: msg}
}

type inboxUploadURLRequest struct {
	URL string `json:"url"`
}

// handleInboxUploadURL (owner or read-write) adds a link: the hive fetches it
// with the SSRF guard (http/https only, no credentials, its own DNS lookup
// with private, loopback, link-local, CGNAT, multicast, ULA and metadata
// ranges refused, every redirect re-checked, at most 3, size and time
// capped) and runs the body through the same type pipeline as a dropped
// file. 201 with one result, or 400 link_refused, 413, 415, 422, 502
// fetch_failed, 507. Audited as inbox_upload with origin=link and the host.
func (s *Server) handleInboxUploadURL(w http.ResponseWriter, r *http.Request) {
	if !requireInboxAddRole(w, r) {
		return
	}
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	if svc.Uploads() == nil {
		jsonError(w, inbox.ErrUploadsDisabled.Error(), http.StatusNotFound)
		return
	}
	var req inboxUploadURLRequest
	if err := decodeBody(r, &req); err != nil || strings.TrimSpace(req.URL) == "" {
		jsonError(w, "url is required", http.StatusBadRequest)
		return
	}
	host := ""
	if u, err := inbox.ValidateLinkURL(req.URL); err == nil {
		host = u.Hostname()
	}
	ctx, cancel := context.WithTimeout(r.Context(), inboxUploadTimeout)
	defer cancel()
	it, err := svc.AddLink(ctx, inbox.LinkInput{URL: req.URL, AddedBy: requestUser(r)})
	if err != nil {
		code, msg := inbox.UploadErrInternal, "adding the link failed"
		var ue *inbox.UploadError
		if errors.As(err, &ue) {
			code, msg = ue.Code, ue.Message
		} else if s.logger != nil {
			s.logger.Warn("inbox: link failed", "error", err)
		}
		s.auditFromRequest(r, "inbox_upload", auditDetail("source", inbox.UploadType, "origin", inbox.UploadOriginLink, "host", host, "outcome", "refused", "code", code), "")
		jsonStatusResponse(w, inboxUploadStatus(code), InboxUploadResponse{Results: []InboxUploadResult{{Filename: host, Code: code, Error: msg}}})
		return
	}
	s.auditFromRequest(r, "inbox_upload", auditDetail("source", it.Source, "item", it.ID, "origin", inbox.UploadOriginLink, "host", host, "kind", it.UploadKind, "outcome", "added"), "")
	jsonStatusResponse(w, http.StatusCreated, InboxUploadResponse{Results: []InboxUploadResult{{Filename: host, OK: true, Source: it.Source, ItemID: it.ID, Title: it.Title, Kind: it.UploadKind}}})
}
