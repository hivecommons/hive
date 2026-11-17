package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/mailallow"
	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// Email in (hivecommons/hive#18395, plan #18391 "Email invites"): mail sent
// to hive@<this hive's host> reaches the cluster's hive-mx receiver, which
// parses it and posts it to the hub; the hub signs it with this hive's
// relay key and forwards it here.
//
//   - POST /api/meetings/invites           calendar invites -> the scheduler
//   - POST /api/meetings/calendar-emails/proof  proof mails -> the allowlist
//   - POST /api/inbox/email                mail-in files (api_inbox_email.go)
//   - POST /api/inbox/email/chunk          one piece of a large files delivery
//   - POST /api/inbox/email/report         a mail the receiver refused (#18699)
//
// All of them are relay-signed (verifyRelayedRequest) and listed in
// isPublicPath: no dashboard session, fail closed without a relay key.
//
// GET/POST/DELETE /api/meetings/calendar-emails is each signed-in user's
// own list of calendar email addresses, the proof codes that prove them,
// and (for the owner, ?all=1) the hive's whole allowlist.
const (
	// Spelled out like the route patterns (the docs citation guard
	// resolves routes by literal); TestMailRelayPathsMatchWire pins the
	// relay paths to wire.
	meetingsInvitesPath        = "/api/meetings/invites"
	meetingsCalendarEmailsPath = "/api/meetings/calendar-emails"
	meetingsCalendarProofPath  = "/api/meetings/calendar-emails/proof"
	inboxEmailPath             = "/api/inbox/email"
	inboxEmailChunkPath        = "/api/inbox/email/chunk"
	inboxEmailReportPath       = "/api/inbox/email/report"
	// mailRelayMaxBody bounds one delivery relayed in a single request; a
	// larger one arrives in pieces on inboxEmailChunkPath.
	mailRelayMaxBody = 8 << 20
	// mailAddressLocalPart is the mailbox hive-mx accepts.
	mailAddressLocalPart = wire.LocalPart
)

// MeetingsInviteIngester is implemented by the meeting scheduler service
// (cmd/hive) when it can take email invites.
type MeetingsInviteIngester interface {
	// IngestInvites schedules the invites of one delivery. trusted is the
	// allowlist decision (authenticated, allowlisted organizer); the
	// implementation also refuses changes to an existing meeting unless the
	// delivery is authenticated as that meeting's organizer.
	IngestInvites(ctx context.Context, d wire.Delivery, trusted bool) (any, error)
}

func (s *Server) registerMeetingsEmailRoutes() {
	s.mux.HandleFunc("POST /api/meetings/invites", s.handleMeetingsInvites)
	s.mux.HandleFunc("POST /api/meetings/calendar-emails/proof", s.handleCalendarEmailProof)
	s.mux.HandleFunc("GET /api/meetings/calendar-emails", s.handleCalendarEmailsList)
	s.mux.HandleFunc("POST /api/meetings/calendar-emails", s.handleCalendarEmailsAdd)
	s.mux.HandleFunc("DELETE /api/meetings/calendar-emails", s.handleCalendarEmailsRemove)
}

// SetMailAllowlist installs the email allowlist store (nil removes it).
// It also feeds Settings, Meeting attendee its Email section.
func (s *Server) SetMailAllowlist(st *mailallow.Store) {
	s.mailAllow.Store(st)
	if st == nil {
		s.SetMeetingsAttendeeEmail(nil)
		return
	}
	s.SetMeetingsAttendeeEmail(mailAttendeeEmail{s: s})
}

func (s *Server) mailAllowlist() *mailallow.Store { return s.mailAllow.Load() }

// dashboardConfig is the live dashboard config, zero when none is wired.
func (s *Server) dashboardConfig() config.DashboardConfig {
	if s.deps == nil || s.deps.Config == nil {
		return config.DashboardConfig{}
	}
	return s.deps.Config.Dashboard
}

func (s *Server) meetingsEmailConfig() config.MeetingsEmailConfig {
	if s.deps == nil || s.deps.Config == nil {
		return config.MeetingsEmailConfig{}
	}
	return s.deps.Config.Meetings.Email
}

// hasMailAccess is the live access list check behind the allowlist: the
// login must hold owner, read-write or read on this hive right now.
func (s *Server) hasMailAccess(login string) bool {
	role, ok := s.dashboardConfig().AuthorizedRole(login)
	return ok && config.RoleAtLeast(role, config.RoleRead)
}

// mailTrusted is the allowlist decision for one delivery's sender.
func (s *Server) mailTrusted(auth wire.Auth) bool {
	_, ok := s.mailAllowlist().Trusted(auth, s.hasMailAccess)
	return ok
}

// readRelayedDelivery reads and authenticates one hub-relayed delivery of
// the given kind, writing the error response itself when it fails.
func (s *Server) readRelayedDelivery(w http.ResponseWriter, r *http.Request, kind wire.Kind) (wire.Delivery, bool) {
	var d wire.Delivery
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mailRelayMaxBody))
	if err != nil {
		jsonError(w, "delivery too large or unreadable", http.StatusRequestEntityTooLarge)
		return d, false
	}
	key := github.WebhookRelayKey()
	if key == "" {
		jsonError(w, "email relay disabled: this hive holds no hub relay key", http.StatusServiceUnavailable)
		return d, false
	}
	if reason := s.verifyRelayedRequest(r.Header, body, key, time.Now()); reason != "" {
		s.webhookLogger().Warn("hub-relayed mail rejected", "reason", reason, "delivery", r.Header.Get(webhookRelayDeliveryHeader))
		jsonError(w, "relay rejected: "+reason, http.StatusUnauthorized)
		return d, false
	}
	if err := json.Unmarshal(body, &d); err != nil || d.Kind != kind || d.ID != r.Header.Get(webhookRelayDeliveryHeader) {
		jsonError(w, "invalid delivery", http.StatusBadRequest)
		return d, false
	}
	return d, true
}

func (s *Server) handleMeetingsInvites(w http.ResponseWriter, r *http.Request) {
	d, ok := s.readRelayedDelivery(w, r, wire.KindInvite)
	if !ok {
		return
	}
	// 409 is permanent for the sender (the hub answers hive-mx 422, and
	// hive-mx bounces): this hive does not take invites.
	if !s.meetingsEmailConfig().InvitesOn() {
		jsonError(w, "this hive does not accept email invites", http.StatusConflict)
		return
	}
	var ing MeetingsInviteIngester
	if h := s.meetingsSchedule.Load(); h != nil && h.svc != nil {
		ing, _ = h.svc.(MeetingsInviteIngester)
	}
	if ing == nil {
		jsonError(w, "meetings are not enabled on this hive", http.StatusConflict)
		return
	}
	trusted := false
	for _, inv := range d.Invites {
		if inv.Organizer != nil && strings.EqualFold(inv.Organizer.Email, d.Auth.From) {
			trusted = s.mailTrusted(d.Auth)
			break
		}
	}
	out, err := ing.IngestInvites(r.Context(), d, trusted)
	if err != nil {
		if errors.Is(err, ErrMeetingInvalid) {
			jsonError(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		s.meetingsError(w, r, err)
		return
	}
	s.recordMail(inbox.MailEvent{Outcome: inbox.MailAccepted, Kind: string(wire.KindInvite), SenderDomain: mailSenderDomain(d.Auth), Auth: inbox.MailAuthOf(d.Auth)})
	s.webhookLogger().Info("email invite received", "delivery", d.ID, "invites", len(d.Invites), "trusted", trusted)
	jsonResponse(w, out)
}

func (s *Server) handleCalendarEmailProof(w http.ResponseWriter, r *http.Request) {
	d, ok := s.readRelayedDelivery(w, r, wire.KindProof)
	if !ok {
		return
	}
	st := s.mailAllowlist()
	if st == nil || d.Proof == nil {
		jsonError(w, "calendar emails are not available on this hive", http.StatusConflict)
		return
	}
	e, err := st.ProveAuth(d.Proof.Code, d.Auth)
	switch {
	case err == nil:
		s.webhookLogger().Info("calendar email proven", "delivery", d.ID)
		jsonResponse(w, map[string]any{"proven": true, "email": e.Email})
	case errors.Is(err, mailallow.ErrUnknownCode), errors.Is(err, mailallow.ErrNotProven):
		// Permanent: the sender's server bounces it. The reason is also on
		// the entry (Settings row) or, for an unknown code, in the owner's
		// recent proof attempts (#18579).
		reason := mailallow.AttemptCodeNotRecognized
		if e.LastFailure != nil {
			reason = e.LastFailure.Reason
		}
		s.webhookLogger().Info("calendar email proof refused", "delivery", d.ID, "reason", reason)
		jsonError(w, err.Error(), http.StatusUnprocessableEntity)
	default:
		jsonError(w, "could not record the proof", http.StatusInternalServerError)
	}
}

// requireSignedInRole admits any user holding at least read access.
func requireSignedInRole(w http.ResponseWriter, r *http.Request) bool {
	role := r.Header.Get("X-Hive-Role")
	if !config.RoleAtLeast(role, config.RoleRead) {
		jsonError(w, "sign-in required", http.StatusForbidden)
		return false
	}
	if isOwnerRole(role) && r.Header.Get(ownerRoleVerifiedHeader) != "true" {
		jsonError(w, "owner access not verified", http.StatusForbidden)
		return false
	}
	return true
}

// mailAddress is hive@<this hive's dashboard host>.
func (s *Server) mailAddress(r *http.Request) string {
	host := ""
	if s.deps != nil && s.deps.Config != nil {
		if u, err := url.Parse(strings.TrimSpace(s.deps.Config.Hub.DashboardURL)); err == nil {
			host = u.Hostname()
		}
	}
	if host == "" {
		host = r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
	}
	return mailAddressLocalPart + "@" + strings.ToLower(host)
}

// calendarEmailView is one address as the dashboard shows it.
type calendarEmailView struct {
	Login         string     `json:"login,omitempty"`
	Email         string     `json:"email"`
	Proven        bool       `json:"proven"`
	ProvenAt      *time.Time `json:"proven_at,omitempty"`
	Code          string     `json:"code,omitempty"`
	CodeExpiresAt *time.Time `json:"code_expires_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	// LastFailure is the latest failed proof: reason code, time, the SPF,
	// DKIM and alignment results, and Message, the plain-language reason
	// with what to do (#18579).
	LastFailure *calendarEmailFailureView `json:"last_failure,omitempty"`
	// Active is false when the user no longer has access to this hive: the
	// address is then not on the effective allowlist.
	Active bool `json:"active"`
}

// calendarEmailFailureView is a failed proof as the Settings row shows it.
type calendarEmailFailureView struct {
	mailallow.ProofFailure
	Message string `json:"message"`
}

// proofFromNone stands in for the From address of a from-mismatch proof
// whose From header the receiver could not read as one address.
const proofFromNone = "(none: the From header was missing, repeated or unparsable)"

// proofFailureMessage says in plain words why a proof of email failed and
// what to do about it.
func proofFailureMessage(f mailallow.ProofFailure, email string) string {
	switch f.Reason {
	case mailallow.ReasonCodeNotAligned:
		domain := f.FromDomain
		if domain == "" {
			domain = email[strings.LastIndexByte(email, '@')+1:]
		}
		return "Your mail server did not authenticate this address: neither SPF nor DKIM passed for " + domain +
			". Turn on DKIM for the domain, add the sending server to SPF, or prove an address from a provider that signs mail."
	case mailallow.ReasonCodeFromMismatch:
		seen := f.From
		if seen == "" {
			seen = proofFromNone
		}
		return "The proof came from a different address than the one registered: the mail's From was " + seen + ". Send it from " + email + " itself."
	case mailallow.ReasonCodeExpired:
		return "The code expired. Remove and add the address again."
	default:
		return "The proof mail did not count. Send it again."
	}
}

func (s *Server) calendarEmailView(e mailallow.Entry, withLogin bool) calendarEmailView {
	v := calendarEmailView{Email: e.Email, Proven: e.Proven(), ProvenAt: e.ProvenAt, Code: e.Code, LastError: e.LastError, Active: s.hasMailAccess(e.Login)}
	if f := e.LastFailure; f != nil && !e.Proven() {
		v.LastFailure = &calendarEmailFailureView{ProofFailure: *f, Message: proofFailureMessage(*f, e.Email)}
	}
	if e.Code != "" {
		t := e.CodeExpiresAt
		v.CodeExpiresAt = &t
	}
	if withLogin {
		v.Login = e.Login
		v.Code, v.CodeExpiresAt = "", nil
	}
	return v
}

func proofInstruction(address, code string) string {
	return "Send an email from that address to " + address + " with " + code + " in the subject."
}

func (s *Server) calendarEmailsStore(w http.ResponseWriter) (*mailallow.Store, bool) {
	st := s.mailAllowlist()
	if st == nil {
		jsonError(w, "calendar emails are not available on this hive", http.StatusServiceUnavailable)
		return nil, false
	}
	return st, true
}

func (s *Server) handleCalendarEmailsList(w http.ResponseWriter, r *http.Request) {
	if !requireSignedInRole(w, r) {
		return
	}
	st, ok := s.calendarEmailsStore(w)
	if !ok {
		return
	}
	user := requestUser(r)
	mine := []calendarEmailView{}
	for _, e := range st.List(user) {
		mine = append(mine, s.calendarEmailView(e, false))
	}
	em := s.meetingsEmailConfig()
	out := map[string]any{
		"address":         s.mailAddress(r),
		"emails":          mine,
		"invites_enabled": em.InvitesOn(),
		"files_enabled":   em.FilesOn(),
	}
	owner := isOwnerRole(r.Header.Get("X-Hive-Role"))
	if owner {
		// Proof mails whose code matched nothing (a typo), masked: the
		// owner's view of the hive's mail activity (#18579).
		out["recent_proof_attempts"] = st.RecentAttempts()
	}
	if r.URL.Query().Get("all") == "1" && owner {
		all := []calendarEmailView{}
		for _, e := range st.All() {
			all = append(all, s.calendarEmailView(e, true))
		}
		out["allowlist"] = all
	}
	jsonResponse(w, out)
}

type calendarEmailRequest struct {
	Email string `json:"email"`
}

func (s *Server) handleCalendarEmailsAdd(w http.ResponseWriter, r *http.Request) {
	if !requireSignedInRole(w, r) {
		return
	}
	st, ok := s.calendarEmailsStore(w)
	if !ok {
		return
	}
	var req calendarEmailRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	e, err := st.Add(requestUser(r), req.Email)
	switch {
	case errors.Is(err, mailallow.ErrInvalidEmail):
		jsonError(w, "not a valid email address", http.StatusBadRequest)
		return
	case errors.Is(err, mailallow.ErrTooMany), errors.Is(err, mailallow.ErrFull):
		jsonError(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		jsonError(w, "could not save the calendar email", http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "calendar-email-add", e.Email, "")
	address := s.mailAddress(r)
	out := map[string]any{"email": s.calendarEmailView(e, false), "address": address}
	if e.Code != "" {
		out["instruction"] = proofInstruction(address, e.Code)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleCalendarEmailsRemove(w http.ResponseWriter, r *http.Request) {
	if !requireSignedInRole(w, r) {
		return
	}
	st, ok := s.calendarEmailsStore(w)
	if !ok {
		return
	}
	email := r.URL.Query().Get("email")
	login := requestUser(r)
	// The owner may remove anyone's address from the allowlist.
	if other := r.URL.Query().Get("login"); other != "" && isOwnerRole(r.Header.Get("X-Hive-Role")) {
		login = other
	}
	switch err := st.Remove(login, email); {
	case errors.Is(err, mailallow.ErrNotFound):
		jsonError(w, "no such calendar email", http.StatusNotFound)
	case err != nil:
		jsonError(w, "could not remove the calendar email", http.StatusInternalServerError)
	default:
		s.auditFromRequest(r, "calendar-email-remove", strings.ToLower(strings.TrimSpace(email)), "")
		jsonResponse(w, map[string]any{"removed": true})
	}
}
