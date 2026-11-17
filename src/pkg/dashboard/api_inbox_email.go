package dashboard

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// Mail-in files (hivecommons/hive#18395): Markdown, audio and image
// attachments mailed to hive@<host> become Inbox items of the email source.
//
//   - POST /api/inbox/email                      relay-signed, from the hub
//   - POST /api/inbox/email/{id}/approve|decline owner: the approval queue
//   - GET  /api/inbox/email/{id}/file            the stored file
func (s *Server) registerInboxEmailRoutes() {
	s.mux.HandleFunc("POST /api/inbox/email", s.handleInboxEmail)
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
