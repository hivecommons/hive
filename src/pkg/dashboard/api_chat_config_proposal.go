package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Chat config assist, phase 3a (RFC #19788, issue #19978): an owner's
// config-shaped chat message becomes at most one proposal (one agent, one
// section, one field). The proposal is checked against a fixed allowlist,
// stored server-side, and rendered as a card from the stored copy. The card
// opens the existing agent config dialog with the value filled in, or (phase
// 3b, issue #19985, api_chat_config_apply.go) applies the stored body through
// the same section endpoint on the owner's click. The model never writes, and
// chat text never applies anything.

const (
	// chatConfigProfile names the proposal turn in runner errors.
	chatConfigProfile = "hive-chat-config"
	// chatConfigProposalTTL is how long a proposal card stays usable.
	chatConfigProposalTTL = 10 * time.Minute
	// chatConfigProposalRate is the per-user proposal turns per minute.
	chatConfigProposalRate = 6
	// chatConfigProposalPerUser and chatConfigProposalTotal cap outstanding
	// proposals. Past a cap a new proposal is refused; nothing is evicted.
	chatConfigProposalPerUser = 3
	chatConfigProposalTotal   = 20
	// chatConfigMaxAgents caps the agents listed in the turn input.
	chatConfigMaxAgents = 40
	// chatConfigMaxWhy bounds the model's plain-text reason on the card.
	chatConfigMaxWhy = 280
	// chatConfigMaxReply bounds the model reply the server parses.
	chatConfigMaxReply = 8 * 1024
)

var (
	errChatConfigRate = errors.New("too many config proposals in the last minute; try again shortly")
	errChatConfigCap  = errors.New("too many pending config proposals; dismiss or use one first")
)

// chatConfigProposal is one stored, checked proposal. Every field the card
// shows comes from here, never from model text (Why is shown as plain text).
type chatConfigProposal struct {
	ID      string         `json:"id"`
	User    string         `json:"-"`
	Agent   string         `json:"agent"`
	Section string         `json:"section"`
	Field   string         `json:"field"`
	Before  string         `json:"before"`
	After   string         `json:"after"`
	Effect  string         `json:"effect"`
	Why     string         `json:"why,omitempty"`
	Body    map[string]any `json:"body"`
	Hash    string         `json:"-"`
	Expires time.Time      `json:"expires_at"`
	// Reverses names the applied proposal an Undo proposal reverses (3b).
	Reverses string `json:"reverses,omitempty"`
	// State is "" while pending, then chatConfigInFlight during an Apply
	// and chatConfigApplied after one succeeded (3b).
	State string `json:"-"`
	// AppliedHash is the section hash right after a successful Apply, so an
	// Undo can refuse a section that changed since (3b).
	AppliedHash string `json:"-"`
}

// chatConfigProposalStore is the bounded in-memory store. A restart drops
// every pending proposal. The zero value is ready to use.
type chatConfigProposalStore struct {
	mu     sync.Mutex
	byID   map[string]*chatConfigProposal
	recent map[string][]time.Time
}

func (st *chatConfigProposalStore) sweepLocked(now time.Time) {
	for id, p := range st.byID {
		if !now.Before(p.Expires) {
			delete(st.byID, id)
		}
	}
	for user, times := range st.recent {
		kept := times[:0]
		for _, t := range times {
			if now.Sub(t) < time.Minute {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(st.recent, user)
		} else {
			st.recent[user] = kept
		}
	}
}

// allowTurn records one proposal turn for user, or refuses it past the
// per-user rate or the outstanding caps.
func (st *chatConfigProposalStore) allowTurn(user string, now time.Time) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sweepLocked(now)
	if len(st.recent[user]) >= chatConfigProposalRate {
		return errChatConfigRate
	}
	if err := st.capLocked(user); err != nil {
		return err
	}
	if st.recent == nil {
		st.recent = map[string][]time.Time{}
	}
	st.recent[user] = append(st.recent[user], now)
	return nil
}

func (st *chatConfigProposalStore) capLocked(user string) error {
	total := 0
	for _, p := range st.byID {
		if p.State != chatConfigApplied {
			total++
		}
	}
	if total >= chatConfigProposalTotal {
		return errChatConfigCap
	}
	mine := 0
	for _, p := range st.byID {
		if p.User == user && p.State != chatConfigApplied {
			mine++
		}
	}
	if mine >= chatConfigProposalPerUser {
		return errChatConfigCap
	}
	return nil
}

func (st *chatConfigProposalStore) add(p *chatConfigProposal, now time.Time) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sweepLocked(now)
	if err := st.capLocked(p.User); err != nil {
		return err
	}
	if st.byID == nil {
		st.byID = map[string]*chatConfigProposal{}
	}
	st.byID[p.ID] = p
	return nil
}

// lookup returns a copy of the unexpired pending proposal id when it belongs
// to user. A proposal that is being applied or was applied is not pending.
func (st *chatConfigProposalStore) lookup(id, user string, now time.Time) (chatConfigProposal, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sweepLocked(now)
	p, ok := st.byID[id]
	if !ok || p.User != user || p.State != "" {
		return chatConfigProposal{}, false
	}
	return *p, true
}

// remove deletes a pending proposal. A proposal in flight or applied is left
// to the Apply path (api_chat_config_apply.go), which settles it.
func (st *chatConfigProposalStore) remove(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if p, ok := st.byID[id]; ok && p.State == "" {
		delete(st.byID, id)
	}
}

// chatRequestIsVerifiedOwner is the owner check chat applies before any
// config assist behaviour: the owner role plus the server-side verification
// marker requireOwnerRole also needs.
func chatRequestIsVerifiedOwner(r *http.Request) bool {
	return isOwnerRole(r.Header.Get("X-Hive-Role")) && r.Header.Get(ownerRoleVerifiedHeader) == "true"
}

// chatConfigAgentRow is one agent's allowlisted values in the turn input,
// live overrides included. Nothing else about the agent is sent.
type chatConfigAgentRow struct {
	Name            string            `json:"name"`
	Enabled         bool              `json:"enabled"`
	Backend         string            `json:"effective_backend"`
	Model           string            `json:"effective_model"`
	ReasoningEffort string            `json:"reasoning_effort"`
	Models          []string          `json:"allowed_models"`
	Efforts         []string          `json:"allowed_reasoning_efforts"`
	Cadences        map[string]string `json:"cadences"`
}

// chatConfigInput is the whole isolated turn input. It is built from config
// and the agent manager only: no chatBuildSnapshot, no chat history, no issue
// or PR text, no prompts, hooks or connection auth.
type chatConfigInput struct {
	Modes  []string             `json:"governor_modes"`
	Agents []chatConfigAgentRow `json:"agents"`
}

// chatConfigAgentValues is an agent's current allowlisted values, live
// overrides included, without the allowed-value lists. The section hash is
// computed from it.
func (s *Server) chatConfigAgentValues(name string, a config.AgentConfig) chatConfigAgentRow {
	row := chatConfigAgentRow{Name: name, Enabled: a.Enabled, Backend: a.Backend, Model: a.Model, ReasoningEffort: a.ReasoningEffort, Cadences: map[string]string{}}
	if s.deps.AgentMgr != nil {
		if proc, err := s.deps.AgentMgr.GetStatusFast(name); err == nil && proc != nil {
			if proc.ModelOverride != "" {
				row.Model = proc.ModelOverride
			}
			if proc.BackendOverride != "" {
				row.Backend = proc.BackendOverride
			}
		}
	}
	for mode, mc := range s.deps.Config.Governor.Modes {
		if c, ok := mc.Cadences[name]; ok {
			row.Cadences[mode] = string(c)
		}
	}
	return row
}

func (s *Server) chatConfigAgentRow(name string, a config.AgentConfig) chatConfigAgentRow {
	row := s.chatConfigAgentValues(name, a)
	row.Models = s.modelIDsForBackend(row.Backend)
	if len(row.Models) == 0 {
		row.Models = append([]string(nil), cliStaticFallback(row.Backend)...)
	}
	if row.Backend == ompBackendID {
		row.Efforts = s.ompReasoningEffortsForModel(row.Model)
	} else {
		row.Efforts = append([]string(nil), config.ReasoningEffortsByBackend[row.Backend]...)
	}
	return row
}

func (s *Server) chatConfigInput() chatConfigInput {
	cfg := s.deps.Config
	in := chatConfigInput{Modes: slices.Sorted(maps.Keys(cfg.Governor.Modes))}
	names := make([]string, 0, len(cfg.Agents))
	for n := range cfg.Agents {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > chatConfigMaxAgents {
		names = names[:chatConfigMaxAgents]
	}
	for _, n := range names {
		in.Agents = append(in.Agents, s.chatConfigAgentRow(n, cfg.Agents[n]))
	}
	return in
}

// chatConfigRowHash is the hash of an agent's allowlisted section values,
// live overrides included, stored with the proposal (the phase 3b binding
// check compares it).
func chatConfigRowHash(row chatConfigAgentRow) string {
	raw, _ := json.Marshal(struct {
		Enabled         bool              `json:"enabled"`
		Backend         string            `json:"backend"`
		Model           string            `json:"model"`
		ReasoningEffort string            `json:"reasoning_effort"`
		Cadences        map[string]string `json:"cadences"`
	}{row.Enabled, row.Backend, row.Model, row.ReasoningEffort, row.Cadences})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func chatConfigSystemPrompt() string {
	return strings.TrimSpace(`
You turn one config request from a hive owner into at most ONE proposed change to one agent.
You are given the only settings you may change and their allowed values. You cannot apply anything; the owner reviews your proposal and saves it in the dashboard.

Allowed changes (exactly one field):
- section "models", field "model": one value from that agent's allowed_models.
- section "models", field "reasoning_effort": one value from that agent's allowed_reasoning_efforts.
- section "cadences", field = one governor mode from governor_modes, value = seconds (number) or an interval string such as "30m"; 0 or "pause" pauses.
- section "general", field "enabled": false only (disable the agent).
Everything else (backend, enabling an agent, prompts, hooks, tools, connections, repos, review or merge gates, roles, chat settings) is out of scope.

Reply with ONLY one JSON object and no other text:
{"agent": "<name>", "section": "<models|cadences|general>", "set": {"<field>": <value>}, "why": "<one short sentence>"}
or, when no allowed change fits:
{"none": "<one short sentence saying why>"}
If the request needs more than one change, propose the first and name the rest in "why".`)
}

func chatConfigUserPrompt(question string, input chatConfigInput) string {
	raw, _ := json.Marshal(input)
	return "Current allowlisted settings (JSON):\n" + string(raw) + "\n\nOwner request:\n" + question
}

// chatConfigReply is the model's reply shape. Free text is never acted on.
type chatConfigReply struct {
	Agent   string                     `json:"agent"`
	Section string                     `json:"section"`
	Set     map[string]json.RawMessage `json:"set"`
	Why     string                     `json:"why"`
	None    *string                    `json:"none"`
}

func parseChatConfigReply(reply string) (chatConfigReply, error) {
	reply = strings.TrimSpace(reply)
	if len(reply) > chatConfigMaxReply {
		return chatConfigReply{}, errors.New("reply too long")
	}
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end < start {
		return chatConfigReply{}, errors.New("reply is not a JSON object")
	}
	var out chatConfigReply
	if err := json.Unmarshal([]byte(reply[start:end+1]), &out); err != nil {
		return chatConfigReply{}, errors.New("reply is not valid JSON")
	}
	return out, nil
}

// chatConfigEffect is the side-effect line the card shows per field.
func chatConfigEffect(section, field string, activeMode string) string {
	switch section {
	case "models":
		return "Saving restarts the agent."
	case "general":
		return "Saving stops the agent."
	case "cadences":
		if field == activeMode {
			return "Changes the cadence of the active governor mode (" + field + ")."
		}
		return "Changes the cadence of governor mode " + field + " (not the active mode)."
	}
	return ""
}

// checkChatConfigReply applies the RFC §3 step 4 checks to a parsed reply and
// returns the proposal to store (without id, user or expiry), or the reason
// it is refused. The allowlist and value-list checks are stricter than the
// endpoints; the endpoint validation helpers then run on the same body.
func (s *Server) checkChatConfigReply(rep chatConfigReply, input chatConfigInput) (*chatConfigProposal, error) {
	var row *chatConfigAgentRow
	for i := range input.Agents {
		if input.Agents[i].Name == rep.Agent {
			row = &input.Agents[i]
			break
		}
	}
	if row == nil {
		return nil, fmt.Errorf("unknown agent %q", chatTruncate(rep.Agent, 64))
	}
	agentCfg, ok := s.deps.Config.Agents[row.Name]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", row.Name)
	}
	if len(rep.Set) != 1 {
		return nil, errors.New("a proposal must set exactly one field")
	}
	var field string
	var raw json.RawMessage
	for k, v := range rep.Set {
		field, raw = k, v
	}
	p := &chatConfigProposal{Agent: row.Name, Section: rep.Section, Field: field}
	switch rep.Section {
	case "models":
		var value string
		if err := json.Unmarshal(raw, &value); err != nil || value == "" {
			return nil, errors.New("models value must be a non-empty string")
		}
		body := agentModelsBody{}
		switch field {
		case "model":
			if !slices.Contains(row.Models, value) {
				return nil, fmt.Errorf("model %q is not offered for backend %q", chatTruncate(value, 64), row.Backend)
			}
			p.Before = row.Model
			body.Model = value
		case "reasoning_effort":
			if !slices.Contains(row.Efforts, value) {
				return nil, fmt.Errorf("reasoning effort %q is not offered for backend %q", chatTruncate(value, 32), row.Backend)
			}
			p.Before = row.ReasoningEffort
			body.ReasoningEffort = &value
		default:
			return nil, fmt.Errorf("field %q is not allowed in section models", chatTruncate(field, 64))
		}
		if _, err := s.validateModelsBody(row.Name, agentCfg, body); err != nil {
			return nil, err
		}
		p.After = value
		p.Body = map[string]any{field: value}
	case "cadences":
		gov := &s.deps.Config.Governor
		if _, ok := gov.Modes[field]; !ok {
			return nil, fmt.Errorf("unknown governor mode %q", chatTruncate(field, 64))
		}
		cadences, err := validateCadencesBody(gov, map[string]json.RawMessage{field: raw})
		if err != nil {
			return nil, err
		}
		c := cadences[field]
		p.Before = row.Cadences[field]
		p.After = string(c)
		p.Body = map[string]any{field: chatConfigCadenceBodyValue(c)}
	case "general":
		if field != "enabled" {
			return nil, fmt.Errorf("field %q is not allowed in section general", chatTruncate(field, 64))
		}
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil || value {
			return nil, errors.New("general.enabled may only be set to false")
		}
		body := map[string]interface{}{"enabled": false}
		if err := validateAgentGeneralInput(body); err != nil {
			return nil, err
		}
		p.Before, p.After = strconv.FormatBool(row.Enabled), "false"
		p.Body = body
	default:
		return nil, fmt.Errorf("section %q is not allowed", chatTruncate(rep.Section, 64))
	}
	if p.Before == p.After {
		return nil, errors.New("the proposed value equals the current value")
	}
	activeMode := ""
	if st := s.chatStatusSnapshot(); st != nil {
		activeMode = st.Governor.Mode
	}
	p.Effect = chatConfigEffect(p.Section, p.Field, activeMode)
	p.Why = chatTruncate(strings.Join(strings.Fields(rep.Why), " "), chatConfigMaxWhy)
	p.Hash = chatConfigRowHash(*row)
	return p, nil
}

// chatConfigCadenceBodyValue is a cadence as the dialog's cadence field holds
// it: seconds for an interval (0 for pause) and the cadence itself otherwise,
// as GET /api/config/agent serves it.
func chatConfigCadenceBodyValue(c config.Cadence) any {
	if c.Mode() == config.CadenceModeInterval {
		if c.IsPaused() {
			return int64(0)
		}
		return int64(parseCadenceDuration(c.Interval()).Seconds())
	}
	return string(c)
}

func newChatConfigProposalID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// chatConfigProposalReply handles a chat message on the config route. It
// returns ok=false when the message is not for config assist (flag off, not a
// verified owner, or not config-shaped), so the caller answers it as before.
func (s *Server) chatConfigProposalReply(r *http.Request, route chatRoute, query string) (map[string]any, bool) {
	if s == nil || s.deps == nil || s.deps.Config == nil || !s.deps.Config.Chat.ConfigAssistEnabled() {
		return nil, false
	}
	if route.Kind == chatRouteHelp || route.Kind == chatRoutePresence || route.Kind == chatRouteBare {
		return nil, false
	}
	if !chatRequestIsVerifiedOwner(r) || !chatConfigShapedRE.MatchString(query) {
		return nil, false
	}
	runner := s.chatGroundedRunner()
	if runner == nil {
		return map[string]any{
			"answer": "Config assist is on, but no inference backend is wired for chat, so I cannot propose a change. Configure the governor reviewer endpoint, or edit the agent in its config dialog.",
			"status": "config_assist_unavailable",
		}, true
	}
	user := requestUser(r)
	question := chatTruncate(strings.TrimSpace(query), chatGroundedMaxQuestion)
	reject := func(agent, section, reason string) (map[string]any, bool) {
		s.auditFromRequest(r, "chat_config_rejected", auditDetail("agent", agent, "section", section, "reason", reason), agent)
		return map[string]any{
			"answer": "I could not turn that into a config proposal: " + reason + ".",
			"status": "config_proposal_rejected",
		}, true
	}
	now := time.Now()
	if err := s.chatConfigProposals.allowTurn(user, now); err != nil {
		return reject("", "", err.Error())
	}
	slots := s.chatGroundedSlots()
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		return reject("", "", errChatGroundingBusy.Error())
	}
	input := s.chatConfigInput()
	ctx, cancel := context.WithTimeout(r.Context(), chatGroundedTimeout)
	defer cancel()
	reply, err := runner.RunDeciderTurn(ctx, chatConfigProfile, chatConfigSystemPrompt(), chatConfigUserPrompt(question, input))
	if err != nil {
		return reject("", "", chatGroundingReason(err))
	}
	rep, err := parseChatConfigReply(reply)
	if err != nil {
		return reject("", "", err.Error())
	}
	if rep.None != nil {
		return map[string]any{
			"answer": "No config change proposed: " + chatTruncate(strings.Join(strings.Fields(*rep.None), " "), chatConfigMaxWhy),
			"status": "ok",
		}, true
	}
	p, err := s.checkChatConfigReply(rep, input)
	if err != nil {
		return reject(chatTruncate(rep.Agent, 64), chatTruncate(rep.Section, 32), err.Error())
	}
	p.ID, p.User, p.Expires = newChatConfigProposalID(), user, now.Add(chatConfigProposalTTL)
	if err := s.chatConfigProposals.add(p, now); err != nil {
		return reject(p.Agent, p.Section, err.Error())
	}
	s.auditFromRequest(r, "chat_config_proposed", auditDetail(
		"proposal", p.ID, "agent", p.Agent, "section", p.Section, "field", p.Field,
		p.Field, auditChange(p.Before, p.After), "question", question), p.Agent)
	return map[string]any{
		"answer":          "Proposed change for " + p.Agent + ". Click Apply on the card to save it, or open it in the agent config dialog to review it first.",
		"status":          "ok",
		"config_proposal": p,
	}, true
}

// handleChatConfigProposalDecline serves POST
// /api/chat/config-proposals/{id}/decline: the card's Dismiss. Owner-only,
// and only the owner who requested the proposal can dismiss it.
func (s *Server) handleChatConfigProposalDecline(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	id := r.PathValue("id")
	p, ok := s.chatConfigProposals.lookup(id, requestUser(r), time.Now())
	if !ok {
		jsonError(w, "proposal not found or expired", http.StatusNotFound)
		return
	}
	s.chatConfigProposals.remove(p.ID)
	s.auditFromRequest(r, "chat_config_declined", auditDetail("proposal", p.ID, "section", p.Section, "field", p.Field), p.Agent)
	jsonResponse(w, map[string]any{"ok": true, "status": "declined"})
}

// chatConfigVia reads the dialog's `via=chat&proposal=<id>` attribution on an
// agent config section save. The attribution is trusted only when the id
// names a stored, unexpired proposal of the same user, agent and section
// whose field the body sets; the returned proposal is then non-nil. A claim
// that fails the check is returned as advisory (claimed=true, nil proposal)
// and recorded as via_unverified, never as via=chat.
func (s *Server) chatConfigVia(r *http.Request, agent, section string, sets func(field string) bool) (p *chatConfigProposal, claimed bool) {
	q := r.URL.Query()
	if q.Get("via") != "chat" {
		return nil, false
	}
	got, ok := s.chatConfigProposals.lookup(q.Get("proposal"), requestUser(r), time.Now())
	if !ok || got.Agent != agent || got.Section != section || !sets(got.Field) {
		return nil, true
	}
	return &got, true
}

// chatConfigViaDetail appends the attribution to a section audit detail.
func chatConfigViaDetail(detail string, p *chatConfigProposal, claimed bool, before, after string) string {
	switch {
	case p != nil:
		extra := auditDetail("via", "chat", "proposal", p.ID)
		if before != "" || after != "" {
			extra += ", " + auditDetail(p.Field, auditChange(before, after))
		}
		return detail + ", " + extra
	case claimed:
		return detail + ", " + auditDetail("via_unverified", "chat")
	}
	return detail
}
