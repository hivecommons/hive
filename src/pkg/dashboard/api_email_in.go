package dashboard

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The email-in address in the Inbox, Calendar and Meetings sections
// (hivecommons/hive#18693). Each hive has one address, hive@<dashboard
// host> (#18395): mail to it reaches the cluster's hive-mx receiver, the
// hub relays it here, attachments and text bodies become Inbox items and
// calendar invites become Calendar entries. Each section's own state
// endpoint carries the address as `email_in`, so the section can show it
// where people look for it:
//
//   - GET /api/inbox              (Inbox)
//   - GET /api/meetings/schedule  (Calendar)
//   - GET /api/meetings           (Meetings)

// emailInDocsURL explains how mail reaches a hive and how to set it up.
const emailInDocsURL = "https://github.com/hivecommons/hive/blob/v5/src/docs/meetings.md#is-the-address-set-up"

// Why EmailInView.Ready is false.
const (
	// emailInNoRelay: this hive holds no hub relay key, so no hub relays
	// mail to it (a hive that is not registered with a hub that runs the
	// mail receiver).
	emailInNoRelay = "no_relay"
	// emailInNoHost: the hive's public dashboard host is not known.
	emailInNoHost = "no_host"
)

// EmailInView is the `email_in` block of a section's state.
type EmailInView struct {
	// Address is hive@<dashboard host>; empty unless Ready.
	Address string `json:"address,omitempty"`
	// Ready reports that mail to Address can reach this hive: it holds the
	// hub relay key that every relayed delivery is verified with.
	Ready bool `json:"ready"`
	// Reason is why Ready is false: "no_relay" or "no_host".
	Reason string `json:"reason,omitempty"`
	// Files: mailed-in files and text become Inbox items
	// (meetings.email.files_enabled, and the email source is wired).
	Files bool `json:"files_enabled"`
	// Invites: calendar invites become Calendar entries
	// (meetings.email.invites_enabled, and the meeting scheduler runs).
	Invites bool `json:"invites_enabled"`
	// DocsURL is the setup guide.
	DocsURL string `json:"docs_url"`
}

// emailInHost is the host mail for this hive is addressed to: the hub's
// dashboard URL for it, else the host the dashboard is reached on.
func (s *Server) emailInHost(r *http.Request) string {
	if s.deps != nil && s.deps.Config != nil {
		if u, err := url.Parse(strings.TrimSpace(s.deps.Config.Hub.DashboardURL)); err == nil && u.Hostname() != "" {
			return strings.ToLower(u.Hostname())
		}
	}
	return meetingsAttendeeHost(r)
}

// emailInView is the email-in block for the caller, or nil when the caller
// may not see it. Mail from a proven address of someone with access is
// filed without approval, so the address is shown to the roles that may
// add to the Inbox and the Calendar themselves: owner and read-write.
func (s *Server) emailInView(r *http.Request) *EmailInView {
	role := r.Header.Get("X-Hive-Role")
	if isOwnerRole(role) && r.Header.Get(ownerRoleVerifiedHeader) != "true" {
		return nil
	}
	if !config.RoleAtLeast(role, config.RoleReadWrite) {
		return nil
	}
	v := &EmailInView{DocsURL: emailInDocsURL}
	em := s.meetingsEmailConfig()
	v.Files = em.FilesOn() && s.deps != nil && s.deps.Inbox != nil && s.deps.Inbox.Email() != nil
	if h := s.meetingsSchedule.Load(); h != nil && h.svc != nil {
		_, takesInvites := h.svc.(MeetingsInviteIngester)
		v.Invites = em.InvitesOn() && takesInvites
	}
	host := s.emailInHost(r)
	switch {
	case github.WebhookRelayKey() == "":
		v.Reason = emailInNoRelay
	case host == "":
		v.Reason = emailInNoHost
	default:
		v.Ready, v.Address = true, mailAddressLocalPart+"@"+host
	}
	return v
}

// withEmailIn adds the caller's email_in block to a section state that is
// encoded as a JSON object (the scheduler's listing is opaque to this
// package). State that is not an object, or a caller who may not see the
// address, is returned unchanged.
func (s *Server) withEmailIn(r *http.Request, state any) any {
	v := s.emailInView(r)
	if v == nil {
		return state
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return state
	}
	obj := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return state
	}
	if obj["email_in"], err = json.Marshal(v); err != nil {
		return state
	}
	return obj
}
