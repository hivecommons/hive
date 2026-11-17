package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// Mail-in files (hivecommons/hive#18395, #18564): Markdown, audio, video
// and image attachments and text bodies mailed to hive@<host> become Inbox
// items of the email source.
//
//   - POST /api/inbox/email                      relay-signed, from the hub
//   - POST /api/inbox/email/chunk                relay-signed piece of a large delivery
//   - POST /api/inbox/email/{id}/approve|decline owner: the approval queue
//   - GET  /api/inbox/email/{id}/file            the stored file
func (s *Server) registerInboxEmailRoutes() {
	s.mux.HandleFunc("POST /api/inbox/email", s.handleInboxEmail)
	s.mux.HandleFunc("POST /api/inbox/email/chunk", s.handleInboxEmailChunk)
	s.mux.HandleFunc("POST /api/inbox/email/{id}/approve", s.handleInboxEmailApprove)
	s.mux.HandleFunc("POST /api/inbox/email/{id}/decline", s.handleInboxEmailDecline)
	s.mux.HandleFunc("GET /api/inbox/email/{id}/file", s.handleInboxEmailFile)
}

func (s *Server) inboxEmailSource(w http.ResponseWriter, code int) (*inbox.EmailSource, bool) {
	if s.deps != nil && s.deps.Inbox != nil {
		if e := s.deps.Inbox.Email(); e != nil {
			return e, true
		}
	}
	jsonError(w, "mail-in files are not available on this hive", code)
	return nil, false
}

func (s *Server) handleInboxEmail(w http.ResponseWriter, r *http.Request) {
	d, ok := s.readRelayedDelivery(w, r, wire.KindFiles)
	if !ok {
		return
	}
	// 409 is permanent for the sender (hive-mx bounces the mail).
	if !s.meetingsEmailConfig().FilesOn() {
		jsonError(w, "this hive does not accept mail-in files", http.StatusConflict)
		return
	}
	src, ok := s.inboxEmailSource(w, http.StatusConflict)
	if !ok {
		return
	}
	s.storeMailFiles(w, src, d)
}

// storeMailFiles adds a verified files delivery to the email source.
func (s *Server) storeMailFiles(w http.ResponseWriter, src *inbox.EmailSource, d wire.Delivery) {
	trusted := s.mailTrusted(d.Auth)
	items, err := src.Add(d, trusted)
	switch {
	case errors.Is(err, inbox.ErrEmailQuota):
		jsonError(w, err.Error(), http.StatusInsufficientStorage)
		return
	case err != nil:
		jsonError(w, "could not store the mail-in files", http.StatusInternalServerError)
		return
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	s.webhookLogger().Info("mail-in files received", "delivery", d.ID, "files", len(ids), "trusted", trusted)
	jsonResponse(w, map[string]any{"items": ids, "awaiting_approval": !trusted})
}

func (s *Server) handleInboxEmailApprove(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	src, ok := s.inboxEmailSource(w, http.StatusServiceUnavailable)
	if !ok {
		return
	}
	it, err := src.Approve(r.PathValue("id"), requestUser(r))
	if err != nil {
		inboxEmailError(w, err)
		return
	}
	s.auditFromRequest(r, "inbox-email-approve", it.ID, "")
	jsonResponse(w, it)
}

func (s *Server) handleInboxEmailDecline(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	src, ok := s.inboxEmailSource(w, http.StatusServiceUnavailable)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := src.Decline(id); err != nil {
		inboxEmailError(w, err)
		return
	}
	s.auditFromRequest(r, "inbox-email-decline", id, "")
	jsonResponse(w, map[string]any{"declined": true})
}

func inboxEmailError(w http.ResponseWriter, err error) {
	if errors.Is(err, inbox.ErrEmailNotFound) {
		jsonError(w, "no such email item", http.StatusNotFound)
		return
	}
	jsonError(w, "email item unavailable", http.StatusInternalServerError)
}

// handleInboxEmailFile serves a stored attachment for the Inbox reader: an
// image inline, anything else as a download. The type is the one sniffed
// on receipt, never the sender's claim, and the response may not run script.
func (s *Server) handleInboxEmailFile(w http.ResponseWriter, r *http.Request) {
	if !requireInboxPlaybackRole(w, r) {
		return
	}
	src, ok := s.inboxEmailSource(w, http.StatusServiceUnavailable)
	if !ok {
		return
	}
	rc, m, err := src.Open(r.PathValue("id"))
	if err != nil {
		inboxEmailError(w, err)
		return
	}
	defer rc.Close()
	// A video is a download like audio: CSP sandbox and nosniff below.
	disposition := "attachment"
	if m.Kind == wire.AttachImage {
		disposition = "inline"
	}
	ct := m.ContentType
	if m.Kind == wire.AttachMarkdown {
		ct = "text/plain; charset=utf-8"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Content-Length", strconv.FormatInt(m.Size, 10))
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": m.Filename}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, no-store")
	_, _ = io.Copy(w, rc)
}

// envInboxEmailMaxBytes overrides inboxEmailDefaultMaxBytes, the bound of
// one chunked delivery as a whole (the hub's own cap for this route).
const (
	envInboxEmailMaxBytes     = "HIVE_INBOX_EMAIL_MAX_BYTES"
	inboxEmailDefaultMaxBytes = 48 << 20
)

func inboxEmailMaxBytes() int64 {
	if n, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(envInboxEmailMaxBytes)), 10, 64); err == nil && n > 0 {
		return n
	}
	return inboxEmailDefaultMaxBytes
}

// handleInboxEmailChunk takes one relay-signed piece of a large files
// delivery (#18564). The hub signs each piece with the relay delivery id
// <delivery id>.<index>.<count> (wire.ChunkDelivery), so the order and the
// count are covered by the signature and replays are refused per piece.
// Pieces are staged on disk; the last one triggers decoding the whole body
// (multipart, or JSON) and storing its files, and its answer is the
// delivery's answer.
func (s *Server) handleInboxEmailChunk(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, wire.MaxRelayChunkBytes))
	if err != nil {
		jsonError(w, "piece too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	key := github.WebhookRelayKey()
	if key == "" {
		jsonError(w, "email relay disabled: this hive holds no hub relay key", http.StatusServiceUnavailable)
		return
	}
	if reason := s.verifyRelayedRequest(r.Header, body, key, time.Now()); reason != "" {
		s.webhookLogger().Warn("hub-relayed mail piece rejected", "reason", reason, "delivery", r.Header.Get(webhookRelayDeliveryHeader))
		jsonError(w, "relay rejected: "+reason, http.StatusUnauthorized)
		return
	}
	id, i, n, ok := wire.ParseChunkDelivery(r.Header.Get(webhookRelayDeliveryHeader))
	if !ok {
		jsonError(w, "invalid delivery", http.StatusBadRequest)
		return
	}
	if !s.meetingsEmailConfig().FilesOn() {
		jsonError(w, "this hive does not accept mail-in files", http.StatusConflict)
		return
	}
	src, ok := s.inboxEmailSource(w, http.StatusConflict)
	if !ok {
		return
	}
	path, err := src.StageChunk(id, i, n, body, inboxEmailMaxBytes())
	switch {
	case errors.Is(err, inbox.ErrEmailQuota):
		jsonError(w, err.Error(), http.StatusInsufficientStorage)
		return
	case errors.Is(err, inbox.ErrEmailTooLarge):
		jsonError(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	case errors.Is(err, inbox.ErrEmailChunk):
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	case err != nil:
		jsonError(w, "could not stage the mail-in piece", http.StatusInternalServerError)
		return
	}
	if path == "" {
		jsonResponse(w, map[string]any{"piece": i, "pieces": n})
		return
	}
	defer src.DropStage(id)
	d, err := decodeStagedDelivery(src, id, path, r.Header.Get(wire.HeaderBodyType))
	if err != nil || d.Kind != wire.KindFiles || d.ID != id {
		s.webhookLogger().Warn("hub-relayed mail body rejected", "delivery", id, "err", err)
		jsonError(w, "invalid delivery", http.StatusBadRequest)
		return
	}
	s.storeMailFiles(w, src, d)
}

// decodeStagedDelivery decodes a joined body: multipart (its files are
// streamed into the staging directory, size and digest checked) or JSON.
func decodeStagedDelivery(src *inbox.EmailSource, id, path, bodyType string) (wire.Delivery, error) {
	var d wire.Delivery
	f, err := os.Open(path)
	if err != nil {
		return d, err
	}
	defer f.Close()
	mediaType, params, err := mime.ParseMediaType(bodyType)
	if err != nil {
		return d, err
	}
	switch mediaType {
	case wire.ContentTypeMultipart:
		if params["boundary"] == "" {
			return d, wire.ErrMultipart
		}
		return wire.ReadMultipart(f, params["boundary"], func(i int, _ wire.Attachment, body io.Reader) (string, error) {
			return src.StageFile(id, i, body)
		})
	case wire.ContentTypeJSON:
		err := json.NewDecoder(io.LimitReader(f, inboxEmailMaxBytes())).Decode(&d)
		return d, err
	}
	return d, fmt.Errorf("unsupported body type %q", mediaType)
}
