package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	operatorMessageMaxRunes      = 1000
	operatorMessageRateWindow    = time.Minute
	operatorMessageRatePerSender = 10
)

var operatorMessageRateLimiter = struct {
	sync.Mutex
	buckets map[string][]time.Time
}{buckets: map[string][]time.Time{}}

func sanitizeOperatorMessageText(in string) string {
	in = strings.TrimSpace(in)
	out := strings.Builder{}
	out.Grow(len(in))
	for _, r := range in {
		if r == '\n' || r == '\t' {
			out.WriteRune(r)
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		out.WriteRune(r)
	}
	return strings.TrimSpace(truncateRunes(out.String(), operatorMessageMaxRunes))
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(s) <= limit {
		return s
	}
	var b strings.Builder
	count := 0
	for _, r := range s {
		if count >= limit {
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

func operatorMessageRateAllowed(sender string, now time.Time) bool {
	if sender == "" {
		sender = "anonymous"
	}
	operatorMessageRateLimiter.Lock()
	defer operatorMessageRateLimiter.Unlock()
	cutoff := now.Add(-operatorMessageRateWindow)
	bucket := operatorMessageRateLimiter.buckets[sender]
	kept := bucket[:0]
	for _, t := range bucket {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= operatorMessageRatePerSender {
		operatorMessageRateLimiter.buckets[sender] = kept
		return false
	}
	kept = append(kept, now)
	operatorMessageRateLimiter.buckets[sender] = kept
	return true
}

func (s *Server) handleContributeOperatorMessage(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleContributeOperatorMessagesGet(w, r)
	case http.MethodPost:
		s.handleContributeOperatorMessagePost(w, r)
	default:
		jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleContributeOperatorMessagesGet(w http.ResponseWriter, r *http.Request) {
	username := s.resolveContributeCaller(r)
	if username == "" {
		jsonError(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	p := findContributor(username)
	if p == nil {
		jsonError(w, "Contributor profile not found", http.StatusForbidden)
		return
	}
	jsonResponse(w, map[string]any{"messages": pendingOperatorMessages(p)})
}

func (s *Server) handleContributeOperatorMessagePost(w http.ResponseWriter, r *http.Request) {
	if !s.requireContributorWrite(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDecodeBodyBytes)
	var req struct {
		Contributor string `json:"contributor"`
		Text        string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	text := sanitizeOperatorMessageText(req.Text)
	if text == "" {
		jsonError(w, "Message text is required", http.StatusBadRequest)
		return
	}
	if utf8.RuneCountInString(req.Text) > operatorMessageMaxRunes {
		jsonError(w, "Message text is too long", http.StatusBadRequest)
		return
	}
	p := findContributor(strings.TrimSpace(req.Contributor))
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	sender := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	if !operatorMessageRateAllowed(sender, time.Now()) {
		jsonError(w, "Too many messages; try again later", http.StatusTooManyRequests)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	msg := ContributorOperatorMessage{ID: "om-" + randomHex(8), Text: text, Sender: sender, CreatedAt: now}
	p.OperatorMessages = append(p.OperatorMessages, msg)
	if err := saveContributorProfile(p); err != nil {
		jsonError(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	delivered := 0
	if s.contributeHub != nil {
		delivered = s.contributeHub.DeliverContributorOperatorMessage(p.ContributorID, msg)
		if delivered > 0 {
			markOperatorMessageDelivered(p.GitHubUsername, msg.ID)
			msg.DeliveredAt = time.Now().UTC().Format(time.RFC3339)
		}
	}
	s.auditFromRequest(r, "contributor_operator_message", auditDetail("recipient", p.GitHubUsername, "message_id", msg.ID), "")
	jsonResponse(w, map[string]any{"ok": true, "message": msg, "delivered": delivered})
}

func (s *Server) handleContributeOperatorMessageAck(w http.ResponseWriter, r *http.Request) {
	username := s.resolveContributeCaller(r)
	if username == "" {
		jsonError(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDecodeBodyBytes)
	var req struct {
		ID    string `json:"id"`
		Reply string `json:"reply"`
	}
	if r.Body == nil {
		jsonError(w, "Message id is required", http.StatusBadRequest)
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		jsonError(w, "Message id is required", http.StatusBadRequest)
		return
	}
	p := findContributor(username)
	if p == nil {
		jsonError(w, "Contributor profile not found", http.StatusForbidden)
		return
	}
	reply := sanitizeOperatorMessageText(req.Reply)
	now := time.Now().UTC().Format(time.RFC3339)
	matched := false
	for i := range p.OperatorMessages {
		if p.OperatorMessages[i].ID != req.ID {
			continue
		}
		if p.OperatorMessages[i].AcknowledgedAt == "" {
			p.OperatorMessages[i].AcknowledgedAt = now
		}
		if reply != "" {
			p.OperatorMessages[i].Reply = reply
		}
		matched = true
		break
	}
	if !matched {
		jsonError(w, "Message not found", http.StatusNotFound)
		return
	}
	if err := saveContributorProfile(p); err != nil {
		jsonError(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{"ok": true})
}

func pendingOperatorMessages(p *ContributorProfile) []ContributorOperatorMessage {
	if p == nil || len(p.OperatorMessages) == 0 {
		return nil
	}
	out := make([]ContributorOperatorMessage, 0, len(p.OperatorMessages))
	for _, m := range p.OperatorMessages {
		if m.AcknowledgedAt == "" {
			out = append(out, m)
		}
	}
	return out
}

func operatorMessageNotice(m ContributorOperatorMessage) string {
	return "Message from the hive operator: " + m.Text
}

func (h *ContributeWSHub) DeliverContributorOperatorMessage(contributorID string, msg ContributorOperatorMessage) int {
	if h == nil || contributorID == "" || msg.ID == "" {
		return 0
	}
	h.mu.RLock()
	var targets []*ContributorConnection
	for _, c := range h.connections {
		c.mu.Lock()
		matches := c.profile != nil && (c.profile.ContributorID == contributorID || strings.EqualFold(c.profile.GitHubUsername, contributorID))
		c.mu.Unlock()
		if matches {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	delivered := 0
	for _, c := range targets {
		c.mu.Lock()
		if c.profile != nil {
			c.profile.OperatorMessages = append(c.profile.OperatorMessages, msg)
		}
		c.mu.Unlock()
		frameMsg := msg
		if err := c.send(WSMessage{Type: "notice", Seq: h.nextSeq(), Message: operatorMessageNotice(msg), OperatorMessage: &frameMsg}); err != nil {
			h.logger.Warn("[contribute-ws] failed to send operator message", "error", err)
			continue
		}
		delivered++
	}
	return delivered
}

func (s *wsSession) deliverPendingOperatorMessages() {
	if s == nil || s.contributor == nil || s.contributor.profile == nil {
		return
	}
	p := findContributor(s.contributor.profile.GitHubUsername)
	for _, msg := range pendingOperatorMessages(p) {
		frameMsg := msg
		if err := s.contributor.send(WSMessage{Type: "notice", Seq: s.h.nextSeq(), Message: operatorMessageNotice(msg), OperatorMessage: &frameMsg}); err == nil {
			markOperatorMessageDelivered(p.GitHubUsername, msg.ID)
		}
	}
}

func markOperatorMessageDelivered(username, id string) {
	p := findContributor(username)
	if p == nil || id == "" {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	changed := false
	for i := range p.OperatorMessages {
		if p.OperatorMessages[i].ID == id && p.OperatorMessages[i].DeliveredAt == "" {
			p.OperatorMessages[i].DeliveredAt = now
			changed = true
		}
	}
	if changed {
		_ = saveContributorProfile(p)
	}
}
