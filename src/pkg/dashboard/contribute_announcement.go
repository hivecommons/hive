package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/config"
)

const contributeAnnouncementMaxChars = 500

func sanitizeContributeAnnouncement(in config.ContributeAnnouncement, previous config.ContributeAnnouncement, now time.Time) (config.ContributeAnnouncement, error) {
	text := sanitizeContributeAnnouncementText(in.Text)
	if text == "" {
		return config.ContributeAnnouncement{}, nil
	}
	level := strings.ToLower(strings.TrimSpace(in.Level))
	if level != "warning" {
		level = "info"
	}
	expiresAt := strings.TrimSpace(in.ExpiresAt)
	if expiresAt != "" {
		when, err := time.Parse(time.RFC3339, expiresAt)
		if err != nil {
			return config.ContributeAnnouncement{}, fmt.Errorf("expires_at must be RFC3339")
		}
		// Canonicalize so equivalent timestamps do not churn the config overlay.
		expiresAt = when.UTC().Format(time.RFC3339)
	}
	id := strings.TrimSpace(previous.ID)
	if previous.Text != text || id == "" {
		id = fmt.Sprintf("ann-%d", now.UnixNano())
	}
	return config.ContributeAnnouncement{ID: id, Text: text, Level: level, ExpiresAt: expiresAt}, nil
}

func sanitizeContributeAnnouncementText(s string) string {
	var b strings.Builder
	lastSpace := false
	for _, r := range strings.TrimSpace(s) {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if lastSpace {
				continue
			}
			lastSpace = true
			b.WriteRune(' ')
			continue
		}
		lastSpace = false
		b.WriteRune(r)
	}
	out := []rune(strings.TrimSpace(b.String()))
	if len(out) > contributeAnnouncementMaxChars {
		out = out[:contributeAnnouncementMaxChars]
	}
	return string(out)
}

func activeContributeAnnouncementFromConfig(cfg *config.Config, now time.Time) *config.ContributeAnnouncement {
	if cfg == nil {
		return nil
	}
	ann := cfg.Hub.ContributeAnnouncement
	ann.Text = sanitizeContributeAnnouncementText(ann.Text)
	if ann.Text == "" || strings.TrimSpace(ann.ID) == "" {
		return nil
	}
	if ann.Level != "warning" {
		ann.Level = "info"
	}
	if ann.ExpiresAt != "" {
		when, err := time.Parse(time.RFC3339, ann.ExpiresAt)
		if err != nil || !when.After(now) {
			return nil
		}
		ann.ExpiresAt = when.UTC().Format(time.RFC3339)
	}
	return &ann
}

func (s *Server) activeContributeAnnouncement() *config.ContributeAnnouncement {
	if s == nil || s.deps == nil {
		return nil
	}
	return activeContributeAnnouncementFromConfig(s.deps.Config, time.Now())
}

func sameContributeAnnouncement(a, b *config.ContributeAnnouncement) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.ID == b.ID && a.Text == b.Text && a.Level == b.Level && a.ExpiresAt == b.ExpiresAt
}

func (s *Server) publishContributeAnnouncementChange(ann *config.ContributeAnnouncement) {
	if s == nil || s.contributeHub == nil {
		return
	}
	s.contributeHub.broadcastAnnouncement(ann)
}

func (h *ContributeWSHub) broadcastAnnouncement(ann *config.ContributeAnnouncement) {
	if h == nil {
		return
	}
	if h.sse != nil {
		h.sse.broadcast(sseEvent{Type: "announcement", Announcement: ann})
	}
	if ann == nil || ann.Text == "" || ann.ID == "" {
		return
	}
	h.mu.RLock()
	targets := make([]*ContributorConnection, 0, len(h.connections))
	for _, c := range h.connections {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	for _, c := range targets {
		if c.ws == nil {
			continue
		}
		msg := WSMessage{Type: "notice", Seq: h.nextSeq(), Message: ann.Text, Announcement: ann}
		if err := c.send(msg); err != nil && h.logger != nil {
			h.logger.Warn("[contribute-ws] failed to send announcement notice", "error", err)
		}
	}
}

func (s *Server) handleContributeAnnouncement(w http.ResponseWriter, r *http.Request) {
	role := r.Header.Get("X-Hive-Role")
	if role == "" {
		jsonError(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if !s.requireContributorWrite(w, r) {
		return
	}
	if s == nil || s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body config.ContributeAnnouncement
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	oldAnnouncement := s.activeContributeAnnouncement()
	ann, err := sanitizeContributeAnnouncement(body, s.deps.Config.Hub.ContributeAnnouncement, time.Now())
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.deps.Config.Hub.ContributeAnnouncement = ann
	s.auditFromRequest(r, "config_governor_hub", auditDetail("section", "contribute_announcement"), "")
	s.refreshAndPersist()
	newAnnouncement := s.activeContributeAnnouncement()
	if !sameContributeAnnouncement(oldAnnouncement, newAnnouncement) {
		s.publishContributeAnnouncementChange(newAnnouncement)
	}
	jsonResponse(w, map[string]any{"ok": true, "announcement": newAnnouncement})
}

func (s *Server) handleContributeAnnouncementDismiss(w http.ResponseWriter, r *http.Request) {
	username := s.resolveContributeCaller(r)
	if username == "" {
		jsonError(w, "Sign in with GitHub to sync announcement dismissal.", http.StatusUnauthorized)
		return
	}
	profile := findContributor(username)
	if profile == nil {
		jsonError(w, "You need a contributor profile on this hive before dismissal can be synced.", http.StatusForbidden)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(body.ID)
	if id == "" || !utf8.ValidString(id) || len(id) > 128 {
		jsonError(w, "announcement id is required", http.StatusBadRequest)
		return
	}
	profile.DismissedContributeAnnouncementID = id
	if err := saveContributorProfile(profile); err != nil {
		jsonError(w, "could not save announcement dismissal", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "id": id})
}
