package dashboard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Chat config assist, phase 3b (RFC #19788 §4, issue #19985): the proposal
// card's Apply sends the stored body to the existing agent config section
// endpoint with the owner's session and an X-Hive-Chat-Proposal header. The
// header is only a binding check; a request without it is a normal dialog
// save, and chat has no server-side code path that calls the endpoint itself.
//
// The check runs under agentConfigMu, which every section handler holds from
// its first read through saveConfig, so two Applies, or an Apply racing a
// dialog save, cannot both pass the hash compare. The proposal is marked in
// flight there (a second click gets 409), consumed only on a 2xx, and returned
// to pending on any other status.
//
// Undo builds a reverse proposal from the applied proposal's before value (or
// from the chat_config_proposed audit line after a restart) and goes through
// the same check, the same owner click and the same endpoint.

// chatConfigProposalHeader carries the proposal id on an Apply.
const chatConfigProposalHeader = "X-Hive-Chat-Proposal"

const (
	chatConfigInFlight = "in_flight"
	chatConfigApplied  = "applied"
)

var (
	errChatConfigNoProposal = errors.New("proposal not found or expired; ask again")
	errChatConfigInFlight   = errors.New("proposal is already being applied")
)

// claim marks the pending proposal id of user in flight and returns a copy.
func (st *chatConfigProposalStore) claim(id, user string, now time.Time) (chatConfigProposal, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sweepLocked(now)
	p, ok := st.byID[id]
	if !ok || p.User != user || p.State == chatConfigApplied {
		return chatConfigProposal{}, errChatConfigNoProposal
	}
	if p.State == chatConfigInFlight {
		return chatConfigProposal{}, errChatConfigInFlight
	}
	p.State = chatConfigInFlight
	return *p, nil
}

// release returns an in-flight proposal to pending, until its expiry.
func (st *chatConfigProposalStore) release(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if p, ok := st.byID[id]; ok && p.State == chatConfigInFlight {
		p.State = ""
	}
}

// markApplied consumes an in-flight proposal. It stays readable for Undo, with
// the section hash after the save, for one more TTL.
func (st *chatConfigProposalStore) markApplied(id, hash string, now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if p, ok := st.byID[id]; ok && p.State == chatConfigInFlight {
		p.State, p.AppliedHash, p.Expires = chatConfigApplied, hash, now.Add(chatConfigProposalTTL)
	}
}

// applied returns a copy of user's applied proposal id.
func (st *chatConfigProposalStore) applied(id, user string, now time.Time) (chatConfigProposal, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sweepLocked(now)
	p, ok := st.byID[id]
	if !ok || p.User != user || p.State != chatConfigApplied {
		return chatConfigProposal{}, false
	}
	return *p, true
}

// drop deletes id in any state.
func (st *chatConfigProposalStore) drop(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.byID, id)
}

// chatConfigCanonicalJSON renders a body so the stored proposal body and a
// request body compare equal when they carry the same JSON values.
func chatConfigCanonicalJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return ""
	}
	out, _ := json.Marshal(generic)
	return string(out)
}

// chatConfigModelsBodyJSON is the set fields of a models body, as the card
// sends them.
func chatConfigModelsBodyJSON(b agentModelsBody) map[string]any {
	m := map[string]any{}
	if b.Backend != "" {
		m["backend"] = b.Backend
	}
	if b.Model != "" {
		m["model"] = b.Model
	}
	if b.ReasoningEffort != nil {
		m["reasoning_effort"] = *b.ReasoningEffort
	}
	return m
}

// chatConfigCurrentHash is the section hash of agent as it is now. Callers
// hold agentConfigMu.
func (s *Server) chatConfigCurrentHash(agent string) (string, bool) {
	a, ok := s.deps.Config.Agents[agent]
	if !ok {
		return "", false
	}
	return chatConfigRowHash(s.chatConfigAgentValues(agent, a)), true
}

// chatConfigApplyWriter records the status and the error text of the section
// handler's response, so the Apply can be settled after it.
type chatConfigApplyWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *chatConfigApplyWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *chatConfigApplyWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= 300 && w.body.Len() < 1024 {
		w.body.Write(b[:min(len(b), 1024-w.body.Len())])
	}
	return w.ResponseWriter.Write(b)
}

// chatConfigApply is one Apply in progress inside a section handler.
type chatConfigApply struct {
	s *Server
	r *http.Request
	w *chatConfigApplyWriter
	p chatConfigProposal
}

// chatConfigApplyBegin runs the binding check for a section write that
// carries X-Hive-Chat-Proposal. Callers hold agentConfigMu and have decoded
// the body. Without the header it returns (nil, true) and the write is a
// normal dialog save. With it, a failed check has already answered 403 or 409
// and returns ok=false; a passed check returns the in-flight Apply, whose
// writer the handler must answer through and whose finish it must defer.
func (s *Server) chatConfigApplyBegin(w http.ResponseWriter, r *http.Request, agent, section string, body any) (*chatConfigApply, bool) {
	id := strings.TrimSpace(r.Header.Get(chatConfigProposalHeader))
	if id == "" {
		return nil, true
	}
	fail := func(status int, reason string) (*chatConfigApply, bool) {
		s.auditFromRequest(r, "chat_config_apply_failed", auditDetail(
			"proposal", chatTruncate(id, 64), "section", section, "status", strconv.Itoa(status), "reason", reason), agent)
		jsonError(w, reason, status)
		return nil, false
	}
	if !s.deps.Config.Chat.ConfigAssistEnabled() {
		return fail(http.StatusForbidden, "chat config assist is off")
	}
	if !chatRequestIsVerifiedOwner(r) {
		return fail(http.StatusForbidden, "only the hive owner can apply a chat config proposal")
	}
	p, err := s.chatConfigProposals.claim(id, requestUser(r), time.Now())
	if err != nil {
		return fail(http.StatusConflict, err.Error())
	}
	// Defense in depth: the proposal was allowlisted when it was made, but
	// the Apply re-checks it so a seed-owned or secret key can never be
	// written through this path, whatever ended up in the store.
	if err := s.chatConfigApplyFieldAllowed(p.Section, p.Field, p.Body); err != nil {
		s.chatConfigProposals.drop(p.ID)
		return fail(http.StatusForbidden, err.Error())
	}
	reason := ""
	hash, _ := s.chatConfigCurrentHash(agent)
	switch {
	case p.Agent != agent || p.Section != section:
		reason = "proposal is for a different agent or section"
	case chatConfigCanonicalJSON(body) != chatConfigCanonicalJSON(p.Body):
		reason = "request body does not match the proposal"
	case hash != p.Hash:
		reason = "the agent's settings changed since the proposal; ask again"
	}
	if reason != "" {
		s.chatConfigProposals.release(p.ID)
		return fail(http.StatusConflict, reason)
	}
	return &chatConfigApply{s: s, r: r, w: &chatConfigApplyWriter{ResponseWriter: w}, p: p}, true
}

// finish settles the Apply once the section handler has answered, still
// under agentConfigMu: a 2xx consumes the proposal (and, for an Undo, the
// proposal it reverses and writes chat_config_undo); anything else returns it
// to pending and writes chat_config_apply_failed.
func (a *chatConfigApply) finish() {
	s, p := a.s, a.p
	status := a.w.status
	if status == 0 {
		status = http.StatusOK
	}
	if status >= 200 && status < 300 {
		if p.Reverses != "" {
			s.chatConfigProposals.drop(p.ID)
			s.chatConfigProposals.drop(p.Reverses)
			s.auditFromRequest(a.r, "chat_config_undo", auditDetail(
				"proposal", p.ID, "reverses", p.Reverses, "section", p.Section, "field", p.Field,
				"before_fp", chatConfigValueFingerprint(p.Before), "after_fp", chatConfigValueFingerprint(p.After)), p.Agent)
			return
		}
		hash, _ := s.chatConfigCurrentHash(p.Agent)
		s.chatConfigProposals.markApplied(p.ID, hash, time.Now())
		s.auditFromRequest(a.r, "chat_config_applied", auditDetail(
			"proposal", p.ID, "section", p.Section, "field", p.Field,
			"before_fp", chatConfigValueFingerprint(p.Before), "after_fp", chatConfigValueFingerprint(p.After)), p.Agent)
		return
	}
	s.chatConfigProposals.release(p.ID)
	reason := http.StatusText(status)
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(a.w.body.Bytes(), &e) == nil && e.Error != "" {
		reason = e.Error
	}
	s.auditFromRequest(a.r, "chat_config_apply_failed", auditDetail(
		"proposal", p.ID, "section", p.Section, "status", strconv.Itoa(status), "reason", chatTruncate(reason, 200)), p.Agent)
}

// chatConfigFingerprintLen is the hex length of a value fingerprint in the
// Apply and Undo audit lines.
const chatConfigFingerprintLen = 12

// chatConfigValueFingerprint identifies a value in an audit line without
// writing the value itself: a truncated SHA-256, or auditUnset when unset.
func chatConfigValueFingerprint(v string) string {
	if v == "" {
		return auditUnset
	}
	sum := sha256.Sum256([]byte(v))
	return "sha256:" + hex.EncodeToString(sum[:])[:chatConfigFingerprintLen]
}

// chatConfigSecretFieldRE matches field names that carry credentials. No
// chat proposal may write one, allowlisted or not.
var chatConfigSecretFieldRE = regexp.MustCompile(`(?i)(secret|token|password|passwd|credential|api_?key|private_?key)`)

// chatConfigApplyFieldAllowed is the Apply's own allowlist: one field of the
// models, cadences or general section, set by a body with exactly that one
// key, and never a seed-owned config key or a secret.
func (s *Server) chatConfigApplyFieldAllowed(section, field string, body map[string]any) error {
	for k := range body {
		if config.IsSeedOwnedKey(k) {
			return fmt.Errorf("%q is owned by the config file or the hub and cannot be changed from chat", chatTruncate(k, 64))
		}
		if chatConfigSecretFieldRE.MatchString(k) {
			return fmt.Errorf("%q is a secret and cannot be changed from chat", chatTruncate(k, 64))
		}
	}
	if config.IsSeedOwnedKey(field) || config.IsSeedOwnedKey(section) {
		return fmt.Errorf("%q is owned by the config file or the hub and cannot be changed from chat", chatTruncate(field, 64))
	}
	if chatConfigSecretFieldRE.MatchString(field) {
		return fmt.Errorf("%q is a secret and cannot be changed from chat", chatTruncate(field, 64))
	}
	if _, ok := body[field]; !ok || len(body) != 1 {
		return errors.New("a chat proposal must set exactly its one field")
	}
	allowed := false
	switch section {
	case "models":
		allowed = field == "model" || field == "reasoning_effort"
	case "cadences":
		_, allowed = s.deps.Config.Governor.Modes[field]
	case "general":
		allowed = field == "enabled"
	}
	if !allowed {
		return fmt.Errorf("field %q of section %q cannot be changed from chat", chatTruncate(field, 64), chatTruncate(section, 32))
	}
	return nil
}

// chatConfigFieldValue is the current value of one allowlisted field, in the
// form the proposal's before and after values use.
func chatConfigFieldValue(row chatConfigAgentRow, section, field string) string {
	switch section {
	case "models":
		if field == "reasoning_effort" {
			return row.ReasoningEffort
		}
		return row.Model
	case "cadences":
		return row.Cadences[field]
	case "general":
		return strconv.FormatBool(row.Enabled)
	}
	return ""
}

// chatConfigUndoBody is the section body that sets field back to before.
func (s *Server) chatConfigUndoBody(section, field, before string) (map[string]any, error) {
	switch section {
	case "models":
		if field == "model" {
			if before == "" {
				return nil, errors.New("the model was unset before; change it in the agent config dialog")
			}
			return map[string]any{"model": before}, nil
		}
		if field == "reasoning_effort" {
			return map[string]any{"reasoning_effort": before}, nil
		}
	case "cadences":
		if before == "" {
			return nil, errors.New("the cadence was unset before; change it in the agent config dialog")
		}
		raw, _ := json.Marshal(before)
		cadences, err := validateCadencesBody(&s.deps.Config.Governor, map[string]json.RawMessage{field: raw})
		if err != nil {
			return nil, err
		}
		return map[string]any{field: chatConfigCadenceBodyValue(cadences[field])}, nil
	case "general":
		if field == "enabled" {
			b, err := strconv.ParseBool(before)
			if err != nil {
				return nil, errors.New("the previous value is not recorded")
			}
			return map[string]any{"enabled": b}, nil
		}
	}
	return nil, errors.New("this change cannot be undone from chat")
}

// chatConfigAuditFields parses an audit detail written by auditDetail. The
// first occurrence of a key wins; the free-text question is always last.
func chatConfigAuditFields(detail string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(detail, ", ") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		if _, seen := out[k]; !seen {
			out[k] = v
		}
	}
	return out
}

// chatConfigAppliedFromAudit rebuilds an applied proposal of user from the
// audit log after a restart (or the store's TTL) dropped it: the
// chat_config_proposed line gives the agent, section, field and before/after
// values, and a section line attributed via=chat to the same proposal proves
// it was applied. A proposal already undone is not returned. stale reports
// that a later write to the same agent section (by anyone) is logged after
// the Apply, so an Undo would overwrite a newer change even when the field
// reads the same again.
func (s *Server) chatConfigAppliedFromAudit(id, user string) (p chatConfigProposal, stale, ok bool) {
	if s.audit == nil || id == "" {
		return chatConfigProposal{}, false, false
	}
	entries := s.audit.Recent(0) // newest first
	var proposed map[string]string
	appliedAt, undone := -1, false
	for i, e := range entries {
		if e.User != user {
			continue
		}
		switch {
		case e.Action == "chat_config_proposed" && proposed == nil:
			if f := chatConfigAuditFields(e.Detail); f["proposal"] == id {
				proposed = f
			}
		case e.Action == "chat_config_undo":
			if chatConfigAuditFields(e.Detail)["reverses"] == id {
				undone = true
			}
		case strings.HasPrefix(e.Action, "config_agent_") && appliedAt < 0:
			if strings.Contains(e.Detail+",", ", via=chat, proposal="+id+",") {
				appliedAt = i
			}
		}
	}
	if proposed == nil || appliedAt < 0 || undone {
		return chatConfigProposal{}, false, false
	}
	field := proposed["field"]
	before, after, cut := strings.Cut(proposed[field], " -> ")
	if !cut {
		return chatConfigProposal{}, false, false
	}
	agent, section := proposed["agent"], proposed["section"]
	for _, e := range entries[:appliedAt] {
		if e.Agent == agent && e.Action == "config_agent_"+section {
			stale = true
			break
		}
	}
	unset := func(v string) string {
		if v == auditUnset {
			return ""
		}
		return v
	}
	return chatConfigProposal{
		ID: id, User: user, Agent: agent, Section: section, Field: field,
		Before: unset(before), After: unset(after), State: chatConfigApplied,
	}, stale, true
}

// handleChatConfigProposalUndo serves POST
// /api/chat/config-proposals/{id}/undo: the applied card's Undo. It returns
// a reverse proposal bound to the section as it is now; the card then applies
// it through the section endpoint like any other proposal. 409 when the
// section changed since the Apply.
func (s *Server) handleChatConfigProposalUndo(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if !s.deps.Config.Chat.ConfigAssistEnabled() {
		jsonError(w, "chat config assist is off", http.StatusForbidden)
		return
	}
	s.agentConfigMu.Lock()
	defer s.agentConfigMu.Unlock()

	id, user, now := r.PathValue("id"), requestUser(r), time.Now()
	staleMsg := "the agent's settings changed since the change was applied; undo it in the agent config dialog"
	orig, ok := s.chatConfigProposals.applied(id, user, now)
	if !ok {
		var stale bool
		if orig, stale, ok = s.chatConfigAppliedFromAudit(id, user); !ok {
			jsonError(w, "no applied proposal with that id to undo", http.StatusNotFound)
			return
		}
		if stale {
			jsonError(w, staleMsg, http.StatusConflict)
			return
		}
	}
	agentCfg, ok := s.deps.Config.Agents[orig.Agent]
	if !ok {
		jsonError(w, "agent not found", http.StatusConflict)
		return
	}
	row := s.chatConfigAgentValues(orig.Agent, agentCfg)
	hash := chatConfigRowHash(row)
	// With the store's post-save hash, any change to the section blocks the
	// Undo. From the audit log only the field itself can be compared.
	if (orig.AppliedHash != "" && hash != orig.AppliedHash) ||
		(orig.AppliedHash == "" && chatConfigFieldValue(row, orig.Section, orig.Field) != orig.After) {
		jsonError(w, staleMsg, http.StatusConflict)
		return
	}
	body, err := s.chatConfigUndoBody(orig.Section, orig.Field, orig.Before)
	if err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	if err := s.chatConfigApplyFieldAllowed(orig.Section, orig.Field, body); err != nil {
		jsonError(w, err.Error(), http.StatusForbidden)
		return
	}
	activeMode := ""
	if st := s.chatStatusSnapshot(); st != nil {
		activeMode = st.Governor.Mode
	}
	rev := &chatConfigProposal{
		ID: newChatConfigProposalID(), User: user, Agent: orig.Agent, Section: orig.Section, Field: orig.Field,
		Before: orig.After, After: orig.Before, Effect: chatConfigEffect(orig.Section, orig.Field, activeMode),
		Body: body, Hash: hash, Expires: now.Add(chatConfigProposalTTL), Reverses: orig.ID,
	}
	if err := s.chatConfigProposals.add(rev, now); err != nil {
		jsonError(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "config_proposal": rev})
}
