package dashboard

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

// Which hive agent fills each profile a bound model hands items to, and the
// owner's pin (hivecommons/hive#18690).
//
// A profile nothing fills stops every hand-off of its states while the items
// stay withheld from the generic lanes, so the gap has to be visible where
// the owner can close it: GET /api/workflows lists each bound repository's
// profiles with the resolved agent or the reason there is none (the
// workflow row on Settings → Repos warns and offers an agent picker), the
// stage tracker carries the same reason onto the item's row, and PUT
// /api/workflows/dispatch/profiles saves the pick as
// workflow.dispatch.profiles.<profile>.agent.

// WorkflowDispatchProfileStatus is one profile of one bound repository.
type WorkflowDispatchProfileStatus struct {
	// Repo is the binding's repository, as the binding spells it.
	Repo string `json:"repo"`
	config.WorkflowDispatchResolution
	// States are the model states handed to the profile.
	States []string `json:"states,omitempty"`
	// Candidates are the agents the profile could be pinned to, each with
	// whether it may do what the profile needs here and why not
	// (hivecommons/hive#18749): the picker marks the ones that do not.
	Candidates []config.WorkflowDispatchAgentChoice `json:"candidates"`
	// NotStarted are the hand-offs of this profile an agent declined or
	// did not act on, whose item still waits.
	NotStarted []WorkflowDispatchHandoff `json:"not_started,omitempty"`
}

// workflowDispatchNeed is what a dispatch target asks of its agent.
func workflowDispatchNeed(repo string, t workflowmodel.DispatchTarget) config.WorkflowDispatchNeed {
	return config.WorkflowDispatchNeed{Repo: repo, Reviews: !t.ProducesChange()}
}

// workflowDispatchProblemFix is the remedy every "nothing fills the profile"
// message ends with.
const workflowDispatchProblemFix = "Pick an agent that may do the profile's work on the repository's workflow row in Settings → Repos, or raise an agent's mode"

// WorkflowDispatchNoAgentError is the error of a hand-off whose profile
// nothing fills: ErrWorkflowDispatchNoAgent with the resolver's reason and
// where to fix it.
func WorkflowDispatchNoAgentError(res config.WorkflowDispatchResolution) error {
	problem := res.Problem
	if problem == "" {
		problem = "no agent fills " + res.Profile
	}
	return fmt.Errorf("%w: %s. %s (workflow.dispatch.profiles.%s.agent)", ErrWorkflowDispatchNoAgent, problem, workflowDispatchProblemFix, res.Profile)
}

// workflowDispatchProfiles resolves every profile of every bound model,
// ordered by repository then profile. The resolution is the owner's "Send
// to <profile>" one, so a profile turned off for automatic dispatch still
// shows the agent a manual send would use.
func workflowDispatchProfiles(cfg *config.Config, lookup workflowProfileLookup) []WorkflowDispatchProfileStatus {
	out := []WorkflowDispatchProfileStatus{}
	if cfg == nil || lookup == nil {
		return out
	}
	for _, b := range cfg.Project.WorkflowBindings {
		p, err := lookup(b.Name, b.Version)
		if err != nil || p == nil || p.Name == hivedefault.Name {
			continue
		}
		byProfile := map[string]*WorkflowDispatchProfileStatus{}
		var order []string
		for sid, t := range p.Dispatch {
			st := byProfile[t.Profile]
			if st == nil {
				need := workflowDispatchNeed(b.Repo, t)
				st = &WorkflowDispatchProfileStatus{Repo: b.Repo, WorkflowDispatchResolution: cfg.ResolveWorkflowDispatch(t.Profile, need, true), Candidates: cfg.WorkflowDispatchCandidates(need)}
				st.Disabled = cfg.Workflow.Dispatch.Profile(t.Profile).Disabled
				byProfile[t.Profile] = st
				order = append(order, t.Profile)
			}
			st.States = append(st.States, sid)
		}
		sort.Strings(order)
		for _, name := range order {
			sort.Strings(byProfile[name].States)
			out = append(out, *byProfile[name])
		}
	}
	return out
}

// annotateWorkflowDispatch fills the agent, or the reason there is none, on
// each dispatched state of a bound repository's tracker block.
func annotateWorkflowDispatch(cfg *config.Config, repo string, p *workflowmodel.Profile, block *StageTrackerWorkflow) {
	if cfg == nil || p == nil || block == nil {
		return
	}
	for key, d := range block.Dispatch {
		t, ok := p.Dispatch[d.State]
		if !ok {
			continue
		}
		res := cfg.ResolveWorkflowDispatch(d.Profile, workflowDispatchNeed(repo, t), true)
		d.Agent, d.Problem = res.Agent, res.Problem
		block.Dispatch[key] = d
	}
}

// handleWorkflowDispatchProfilePut pins (agent set) or unpins (agent
// empty) the hive agent a model profile is handed to. Owner only, audited
// as workflow_dispatch_agent; the next scheduler pass reads the live
// config, so no restart is needed.
func (s *Server) handleWorkflowDispatchProfilePut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Profile string `json:"profile"`
		Agent   string `json:"agent"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	body.Profile, body.Agent = strings.TrimSpace(body.Profile), strings.TrimSpace(body.Agent)
	if body.Profile == "" {
		jsonError(w, "body must be {\"profile\": \"name\", \"agent\": \"name or empty\"}", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	previous := strings.TrimSpace(cfg.Workflow.Dispatch.Profile(body.Profile).Agent)
	changed, err := cfg.SetWorkflowDispatchAgent(body.Profile, body.Agent)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if changed {
		if err := s.saveConfig(); err != nil {
			_, _ = cfg.SetWorkflowDispatchAgent(body.Profile, previous)
			jsonError(w, "save config: "+err.Error(), http.StatusInternalServerError)
			return
		}
		detail := body.Profile + " -> " + body.Agent
		if body.Agent == "" {
			detail = body.Profile + " -> (default)"
		}
		s.auditFromRequest(r, "workflow_dispatch_agent", detail, body.Agent)
	}
	jsonResponse(w, map[string]interface{}{
		"changed":  changed,
		"profile":  body.Profile,
		"agent":    body.Agent,
		"dispatch": s.workflowDispatchStatuses(),
		"agents":   workflowDispatchAgentChoices(cfg),
	})
}

// workflowDispatchAgentChoices is the picker's agent list, never null.
func workflowDispatchAgentChoices(cfg *config.Config) []string {
	if names := cfg.WorkflowDispatchAgentChoices(); names != nil {
		return names
	}
	return []string{}
}
