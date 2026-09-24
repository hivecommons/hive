package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	jamSuggestionOpen     = "open"
	jamSuggestionAccepted = "accepted"
	jamSuggestionRejected = "rejected"
	jamPollOpen           = "open"
	jamPollDecided        = "decided"
)

type CampaignJamState struct {
	CampaignID     string               `json:"campaign_id"`
	SpecRevisionID string               `json:"spec_revision_id,omitempty"`
	SpecContent    string               `json:"spec_content,omitempty"`
	Threads        []CampaignThread     `json:"threads,omitempty"`
	Suggestions    []CampaignSuggestion `json:"suggestions,omitempty"`
	Polls          []CampaignPoll       `json:"polls,omitempty"`
	Revisions      []CampaignRevision   `json:"revisions,omitempty"`
	ProjectSync    *CampaignProjectSync `json:"project_sync,omitempty"`
	UpdatedAt      string               `json:"updated_at,omitempty"`
}

type CampaignProjectSync struct {
	Enabled        bool                  `json:"enabled"`
	ProjectURL     string                `json:"project_url,omitempty"`
	ProjectID      string                `json:"project_id,omitempty"`
	LastStatus     string                `json:"last_status,omitempty"`
	LastSyncAt     string                `json:"last_sync_at,omitempty"`
	LastError      string                `json:"last_error,omitempty"`
	RetryAdvice    string                `json:"retry_advice,omitempty"`
	PublishedItems []CampaignProjectItem `json:"published_items,omitempty"`
}

type CampaignProjectItem struct {
	Type       string `json:"type"`
	Title      string `json:"title"`
	Body       string `json:"body,omitempty"`
	Status     string `json:"status,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	UpdatedAt  string `json:"updated_at"`
}

type CampaignThread struct {
	ID        string            `json:"id"`
	Section   string            `json:"section"`
	Title     string            `json:"title,omitempty"`
	Comments  []CampaignComment `json:"comments,omitempty"`
	CreatedBy CampaignJamActor  `json:"created_by"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
}

type CampaignComment struct {
	ID        string           `json:"id"`
	Body      string           `json:"body"`
	Author    CampaignJamActor `json:"author"`
	CreatedAt string           `json:"created_at"`
}

type CampaignSuggestion struct {
	ID                string            `json:"id"`
	ThreadID          string            `json:"thread_id,omitempty"`
	Section           string            `json:"section"`
	Body              string            `json:"body,omitempty"`
	ProposedText      string            `json:"proposed_text"`
	Status            string            `json:"status"`
	Author            CampaignJamActor  `json:"author"`
	CreatedAt         string            `json:"created_at"`
	ResolvedBy        *CampaignJamActor `json:"resolved_by,omitempty"`
	ResolvedAt        string            `json:"resolved_at,omitempty"`
	ResolutionComment string            `json:"resolution_comment,omitempty"`
	AppliedRevisionID string            `json:"applied_revision_id,omitempty"`
}

type CampaignPoll struct {
	ID        string               `json:"id"`
	Section   string               `json:"section"`
	Question  string               `json:"question"`
	Options   []CampaignPollOption `json:"options"`
	Votes     []CampaignPollVote   `json:"votes,omitempty"`
	Status    string               `json:"status"`
	CreatedBy CampaignJamActor     `json:"created_by"`
	CreatedAt string               `json:"created_at"`
	Decision  *CampaignDecision    `json:"decision,omitempty"`
}

type CampaignPollOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

type CampaignPollVote struct {
	Voter    CampaignJamActor `json:"voter"`
	OptionID string           `json:"option_id"`
	VotedAt  string           `json:"voted_at"`
}

type CampaignDecision struct {
	Outcome    string           `json:"outcome"`
	Rationale  string           `json:"rationale"`
	DecidedBy  CampaignJamActor `json:"decided_by"`
	DecidedAt  string           `json:"decided_at"`
	RevisionID string           `json:"revision_id"`
}

type CampaignRevision struct {
	ID        string             `json:"id"`
	Content   string             `json:"content,omitempty"`
	Diff      string             `json:"diff,omitempty"`
	Reason    string             `json:"reason,omitempty"`
	Author    CampaignJamActor   `json:"author"`
	CreatedAt string             `json:"created_at"`
	Model     string             `json:"model,omitempty"`
	Agent     string             `json:"agent,omitempty"`
	Decisions []CampaignDecision `json:"decisions,omitempty"`
}

type CampaignJamActor struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Model string `json:"model,omitempty"`
	Agent string `json:"agent,omitempty"`
}

type campaignJamPostRequest struct {
	SpecContent string `json:"spec_content"`
	Reason      string `json:"reason"`
	Model       string `json:"model"`
	Agent       string `json:"agent"`
}

type campaignJamThreadRequest struct {
	ThreadID string `json:"thread_id"`
	Section  string `json:"section"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	Model    string `json:"model"`
	Agent    string `json:"agent"`
}

type campaignJamSuggestionRequest struct {
	Action            string `json:"action"`
	SuggestionID      string `json:"suggestion_id"`
	ThreadID          string `json:"thread_id"`
	Section           string `json:"section"`
	Body              string `json:"body"`
	ProposedText      string `json:"proposed_text"`
	ResolutionComment string `json:"resolution_comment"`
	SpecContent       string `json:"spec_content"`
	Model             string `json:"model"`
	Agent             string `json:"agent"`
}

type campaignJamPollRequest struct {
	Action    string   `json:"action"`
	PollID    string   `json:"poll_id"`
	Section   string   `json:"section"`
	Question  string   `json:"question"`
	Options   []string `json:"options"`
	OptionID  string   `json:"option_id"`
	Outcome   string   `json:"outcome"`
	Rationale string   `json:"rationale"`
	Model     string   `json:"model"`
	Agent     string   `json:"agent"`
}

type campaignJamDisk struct {
	Campaigns map[string]*CampaignJamState `json:"campaigns"`
}

var campaignJamStoreMu sync.Mutex
var campaignJamIDSeq uint64

func (s *Server) handleCampaignJamGet(w http.ResponseWriter, r *http.Request) {
	state, err := s.loadCampaignJam(campaignIDFromRequest(r))
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "jam": state})
}

func (s *Server) handleCampaignJamPost(w http.ResponseWriter, r *http.Request) {
	if !requireJamWriteRole(w, r) {
		return
	}
	id := campaignIDFromRequest(r)
	var req campaignJamPostRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.SpecContent = strings.TrimSpace(req.SpecContent)
	if req.SpecContent == "" {
		jsonError(w, "spec_content required", http.StatusBadRequest)
		return
	}
	state, err := s.mutateCampaignJam(id, func(state *CampaignJamState) error {
		recordJamRevision(state, req.SpecContent, strings.TrimSpace(req.Reason), jamActorFromRequest(r, req.Agent, req.Model), nil)
		return nil
	})
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.auditFromRequest(r, "campaign_jam_revision", auditDetail("campaign", id), strings.TrimSpace(req.Agent))
	jsonResponse(w, map[string]any{"ok": true, "jam": state})
}

func (s *Server) handleCampaignJamThreadsGet(w http.ResponseWriter, r *http.Request) {
	state, err := s.loadCampaignJam(campaignIDFromRequest(r))
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "threads": state.Threads})
}

func (s *Server) handleCampaignJamThreadsPost(w http.ResponseWriter, r *http.Request) {
	if !requireJamWriteRole(w, r) {
		return
	}
	id := campaignIDFromRequest(r)
	var req campaignJamThreadRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	state, err := s.mutateCampaignJam(id, func(state *CampaignJamState) error {
		now := jamNow()
		actor := jamActorFromRequest(r, req.Agent, req.Model)
		if strings.TrimSpace(req.ThreadID) != "" {
			for i := range state.Threads {
				if state.Threads[i].ID == strings.TrimSpace(req.ThreadID) {
					body := strings.TrimSpace(req.Body)
					if body == "" {
						return errors.New("body required")
					}
					state.Threads[i].Comments = append(state.Threads[i].Comments, CampaignComment{ID: jamID("comment"), Body: body, Author: actor, CreatedAt: now})
					state.Threads[i].UpdatedAt = now
					return nil
				}
			}
			return errors.New("thread not found")
		}
		section := strings.TrimSpace(req.Section)
		body := strings.TrimSpace(req.Body)
		if section == "" || body == "" {
			return errors.New("section and body required")
		}
		thread := CampaignThread{ID: jamID("thread"), Section: section, Title: strings.TrimSpace(req.Title), CreatedBy: actor, CreatedAt: now, UpdatedAt: now}
		thread.Comments = append(thread.Comments, CampaignComment{ID: jamID("comment"), Body: body, Author: actor, CreatedAt: now})
		state.Threads = append(state.Threads, thread)
		return nil
	})
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "jam": state})
}

func (s *Server) handleCampaignJamSuggestionsGet(w http.ResponseWriter, r *http.Request) {
	state, err := s.loadCampaignJam(campaignIDFromRequest(r))
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "suggestions": state.Suggestions})
}

func (s *Server) handleCampaignJamSuggestionsPost(w http.ResponseWriter, r *http.Request) {
	if !requireJamWriteRole(w, r) {
		return
	}
	id := campaignIDFromRequest(r)
	var req campaignJamSuggestionRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	action := firstRunNonEmpty(strings.TrimSpace(req.Action), "create")
	state, err := s.mutateCampaignJam(id, func(state *CampaignJamState) error {
		actor := jamActorFromRequest(r, req.Agent, req.Model)
		switch action {
		case "create":
			section := strings.TrimSpace(req.Section)
			proposed := strings.TrimSpace(req.ProposedText)
			if section == "" || proposed == "" {
				return errors.New("section and proposed_text required")
			}
			state.Suggestions = append(state.Suggestions, CampaignSuggestion{
				ID: jamID("suggestion"), ThreadID: strings.TrimSpace(req.ThreadID), Section: section, Body: strings.TrimSpace(req.Body),
				ProposedText: proposed, Status: jamSuggestionOpen, Author: actor, CreatedAt: jamNow(),
			})
		case "accept", "reject":
			if action == "accept" && !requireJamMaintainerRole(w, r) {
				return errJamResponseWritten
			}
			return resolveJamSuggestion(state, strings.TrimSpace(req.SuggestionID), action, strings.TrimSpace(req.SpecContent), strings.TrimSpace(req.ResolutionComment), actor)
		default:
			return errors.New("unsupported suggestion action")
		}
		return nil
	})
	if errors.Is(err, errJamResponseWritten) {
		return
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "jam": state})
}

func (s *Server) handleCampaignJamPollsGet(w http.ResponseWriter, r *http.Request) {
	state, err := s.loadCampaignJam(campaignIDFromRequest(r))
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "polls": state.Polls})
}

func (s *Server) handleCampaignJamPollsPost(w http.ResponseWriter, r *http.Request) {
	if !requireJamWriteRole(w, r) {
		return
	}
	id := campaignIDFromRequest(r)
	var req campaignJamPollRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	action := firstRunNonEmpty(strings.TrimSpace(req.Action), "create")
	state, err := s.mutateCampaignJam(id, func(state *CampaignJamState) error {
		actor := jamActorFromRequest(r, req.Agent, req.Model)
		switch action {
		case "create":
			return createJamPoll(state, req, actor)
		case "vote":
			return voteJamPoll(state, strings.TrimSpace(req.PollID), strings.TrimSpace(req.OptionID), actor)
		case "decide":
			if !requireJamMaintainerRole(w, r) {
				return errJamResponseWritten
			}
			return decideJamPoll(state, strings.TrimSpace(req.PollID), strings.TrimSpace(req.Outcome), strings.TrimSpace(req.Rationale), actor)
		default:
			return errors.New("unsupported poll action")
		}
	})
	if errors.Is(err, errJamResponseWritten) {
		return
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "jam": state})
}

var errJamResponseWritten = errors.New("jam response written")

func requireJamWriteRole(w http.ResponseWriter, r *http.Request) bool {
	if !config.RoleAtLeast(r.Header.Get("X-Hive-Role"), config.RoleReadWrite) {
		jsonError(w, "read-write access required", http.StatusForbidden)
		return false
	}
	return true
}

func requireJamMaintainerRole(w http.ResponseWriter, r *http.Request) bool {
	return requireMergerOrOwnerRole(w, r)
}

func (s *Server) loadCampaignJam(id string) (*CampaignJamState, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("campaign id required")
	}
	campaignJamStoreMu.Lock()
	defer campaignJamStoreMu.Unlock()
	disk, err := s.readCampaignJamDisk()
	if err != nil {
		return nil, err
	}
	return ensureCampaignJamState(disk, id).clone(), nil
}

func (s *Server) mutateCampaignJam(id string, fn func(*CampaignJamState) error) (*CampaignJamState, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("campaign id required")
	}
	campaignJamStoreMu.Lock()
	defer campaignJamStoreMu.Unlock()
	disk, err := s.readCampaignJamDisk()
	if err != nil {
		return nil, err
	}
	state := ensureCampaignJamState(disk, id)
	if err := fn(state); err != nil {
		return nil, err
	}
	state.UpdatedAt = jamNow()
	sortJamState(state)
	if err := s.writeCampaignJamDisk(disk); err != nil {
		return nil, err
	}
	return state.clone(), nil
}

func ensureCampaignJamState(disk campaignJamDisk, id string) *CampaignJamState {
	id = strings.TrimSpace(id)
	state := disk.Campaigns[id]
	if state == nil {
		state = &CampaignJamState{CampaignID: id}
		disk.Campaigns[id] = state
	}
	return state
}

func (s *Server) readCampaignJamDisk() (campaignJamDisk, error) {
	disk := campaignJamDisk{Campaigns: map[string]*CampaignJamState{}}
	path := s.campaignJamPath()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return disk, nil
	}
	if err != nil {
		return disk, err
	}
	if len(raw) == 0 {
		return disk, nil
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		return campaignJamDisk{}, err
	}
	if disk.Campaigns == nil {
		disk.Campaigns = map[string]*CampaignJamState{}
	}
	return disk, nil
}

func (s *Server) writeCampaignJamDisk(disk campaignJamDisk) error {
	path := s.campaignJamPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func (s *Server) campaignJamPath() string {
	base := ""
	if s != nil && s.deps != nil && s.deps.Config != nil {
		base = strings.TrimSpace(s.deps.Config.Data.MetricsDir)
		if base == "" {
			base = strings.TrimSpace(s.deps.Config.Data.LogsDir)
		}
		if base == "" {
			base = strings.TrimSpace(s.deps.Config.Data.AgentsDir)
		}
	}
	if base == "" {
		base = "."
	}
	return filepath.Join(base, "campaign-jam.json")
}

func recordJamRevision(state *CampaignJamState, content, reason string, actor CampaignJamActor, decisions []CampaignDecision) CampaignRevision {
	now := jamNow()
	revision := CampaignRevision{
		ID: jamID("rev"), Content: content, Diff: jamDiff(state.SpecContent, content), Reason: reason,
		Author: actor, CreatedAt: now, Model: actor.Model, Agent: actor.Agent, Decisions: decisions,
	}
	state.Revisions = append(state.Revisions, revision)
	state.SpecContent = content
	state.SpecRevisionID = revision.ID
	return revision
}

func resolveJamSuggestion(state *CampaignJamState, id, action, content, comment string, actor CampaignJamActor) error {
	if id == "" {
		return errors.New("suggestion_id required")
	}
	for i := range state.Suggestions {
		if state.Suggestions[i].ID != id {
			continue
		}
		if state.Suggestions[i].Status != jamSuggestionOpen {
			return errors.New("suggestion already resolved")
		}
		now := jamNow()
		state.Suggestions[i].ResolvedBy = &actor
		state.Suggestions[i].ResolvedAt = now
		state.Suggestions[i].ResolutionComment = comment
		if action == "reject" {
			state.Suggestions[i].Status = jamSuggestionRejected
			return nil
		}
		if content == "" {
			content = applySectionSuggestion(state.SpecContent, state.Suggestions[i].Section, state.Suggestions[i].ProposedText)
		}
		rev := recordJamRevision(state, content, "accepted suggestion "+id, actor, nil)
		state.Suggestions[i].Status = jamSuggestionAccepted
		state.Suggestions[i].AppliedRevisionID = rev.ID
		return nil
	}
	return errors.New("suggestion not found")
}

func createJamPoll(state *CampaignJamState, req campaignJamPollRequest, actor CampaignJamActor) error {
	section := strings.TrimSpace(req.Section)
	question := strings.TrimSpace(req.Question)
	if section == "" || question == "" || len(req.Options) < 2 {
		return errors.New("section, question, and at least two options required")
	}
	options := make([]CampaignPollOption, 0, len(req.Options))
	for _, option := range req.Options {
		label := strings.TrimSpace(option)
		if label == "" {
			continue
		}
		options = append(options, CampaignPollOption{ID: jamID("option"), Label: label})
	}
	if len(options) < 2 {
		return errors.New("at least two non-empty options required")
	}
	state.Polls = append(state.Polls, CampaignPoll{ID: jamID("poll"), Section: section, Question: question, Options: options, Status: jamPollOpen, CreatedBy: actor, CreatedAt: jamNow()})
	return nil
}

func voteJamPoll(state *CampaignJamState, pollID, optionID string, actor CampaignJamActor) error {
	if pollID == "" || optionID == "" {
		return errors.New("poll_id and option_id required")
	}
	for i := range state.Polls {
		poll := &state.Polls[i]
		if poll.ID != pollID {
			continue
		}
		if poll.Status != jamPollOpen {
			return errors.New("poll is closed")
		}
		if !pollHasOption(poll, optionID) {
			return errors.New("poll option not found")
		}
		for j := range poll.Votes {
			if poll.Votes[j].Voter.Name == actor.Name {
				decrementPollOption(poll, poll.Votes[j].OptionID)
				poll.Votes[j].OptionID = optionID
				poll.Votes[j].VotedAt = jamNow()
				incrementPollOption(poll, optionID)
				return nil
			}
		}
		poll.Votes = append(poll.Votes, CampaignPollVote{Voter: actor, OptionID: optionID, VotedAt: jamNow()})
		incrementPollOption(poll, optionID)
		return nil
	}
	return errors.New("poll not found")
}

func decideJamPoll(state *CampaignJamState, pollID, outcome, rationale string, actor CampaignJamActor) error {
	if pollID == "" || outcome == "" || rationale == "" {
		return errors.New("poll_id, outcome, and rationale required")
	}
	for i := range state.Polls {
		poll := &state.Polls[i]
		if poll.ID != pollID {
			continue
		}
		decision := CampaignDecision{Outcome: outcome, Rationale: rationale, DecidedBy: actor, DecidedAt: jamNow()}
		content := state.SpecContent
		if content != "" {
			content += "\n\n"
		}
		content += "Decision (" + poll.Section + "): " + outcome + "\nRationale: " + rationale
		rev := recordJamRevision(state, content, "poll decision "+pollID, actor, []CampaignDecision{decision})
		decision.RevisionID = rev.ID
		state.Polls[i].Status = jamPollDecided
		state.Polls[i].Decision = &decision
		state.Revisions[len(state.Revisions)-1].Decisions = []CampaignDecision{decision}
		return nil
	}
	return errors.New("poll not found")
}

func pollHasOption(poll *CampaignPoll, optionID string) bool {
	for _, option := range poll.Options {
		if option.ID == optionID {
			return true
		}
	}
	return false
}

func incrementPollOption(poll *CampaignPoll, optionID string) {
	for i := range poll.Options {
		if poll.Options[i].ID == optionID {
			poll.Options[i].Count++
			return
		}
	}
}

func decrementPollOption(poll *CampaignPoll, optionID string) {
	for i := range poll.Options {
		if poll.Options[i].ID == optionID && poll.Options[i].Count > 0 {
			poll.Options[i].Count--
			return
		}
	}
}

func applySectionSuggestion(current, section, proposed string) string {
	if strings.TrimSpace(current) == "" {
		return strings.TrimSpace(proposed)
	}
	marker := "## " + strings.TrimSpace(section)
	lines := strings.Split(current, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == marker {
			start = i
			break
		}
	}
	if start < 0 {
		return strings.TrimRight(current, "\n") + "\n\n" + marker + "\n" + strings.TrimSpace(proposed)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			end = i
			break
		}
	}
	out := append([]string{}, lines[:start+1]...)
	out = append(out, strings.Split(strings.TrimSpace(proposed), "\n")...)
	out = append(out, lines[end:]...)
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

func jamDiff(before, after string) string {
	before = strings.TrimSpace(before)
	after = strings.TrimSpace(after)
	if before == after {
		return ""
	}
	if before == "" {
		return "+ " + after
	}
	if after == "" {
		return "- " + before
	}
	return "- " + before + "\n+ " + after
}

func jamActorFromRequest(r *http.Request, agent, model string) CampaignJamActor {
	agent = strings.TrimSpace(agent)
	model = strings.TrimSpace(model)
	if agent != "" {
		return CampaignJamActor{Type: "agent", Name: agent, Agent: agent, Model: model}
	}
	return CampaignJamActor{Type: "human", Name: requestUser(r), Model: model}
}

func jamNow() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func jamID(prefix string) string {
	seq := atomic.AddUint64(&campaignJamIDSeq, 1)
	return prefix + "-" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "") + "-" + strconv.FormatUint(seq, 10)
}

func sortJamState(state *CampaignJamState) {
	sort.SliceStable(state.Threads, func(i, j int) bool { return state.Threads[i].UpdatedAt > state.Threads[j].UpdatedAt })
	sort.SliceStable(state.Suggestions, func(i, j int) bool { return state.Suggestions[i].CreatedAt > state.Suggestions[j].CreatedAt })
	sort.SliceStable(state.Polls, func(i, j int) bool { return state.Polls[i].CreatedAt > state.Polls[j].CreatedAt })
}

func (state *CampaignJamState) clone() *CampaignJamState {
	if state == nil {
		return nil
	}
	raw, _ := json.Marshal(state)
	var out CampaignJamState
	_ = json.Unmarshal(raw, &out)
	return &out
}
