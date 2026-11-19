package dashboard

import (
	"strconv"
	"strings"
	"time"
)

// Hand-offs an agent declined or did not act on (hivecommons/hive#18749).
//
// A delivered kick is not a started hand-off. The dispatch pass reviews each
// one and records it as not started when the agent declined, ended its turn
// without the deliverable, or may not do what the profile needs. Such an
// item is still withheld from every other lane, so it is shown where a gap
// in who fills a profile is: on the profile's line of the workflow row on
// Settings → Repos (GET /api/workflows `dispatch[].not_started`) and on the
// item's Activity Monitor row (`workflows.repos[repo].handoffs`).

// WorkflowDispatchHandoff is one hand-off that was not started and whose
// item still waits in the state it was handed off in.
type WorkflowDispatchHandoff struct {
	// Ref is the item ("owner/repo#N"); Repo and Number are its parts, Repo
	// org-qualified.
	Ref     string `json:"ref"`
	Repo    string `json:"repo"`
	Number  int    `json:"number"`
	Profile string `json:"profile"`
	State   string `json:"state"`
	// Agent is the agent the item was handed to; Reason why it did not
	// start.
	Agent  string    `json:"agent"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	// Attempts counts the hand-offs to an agent that could do the work.
	Attempts int `json:"attempts"`
	// Retrying is true while the hive will hand the item to another
	// qualifying agent on its own; false once it waits for the owner.
	Retrying bool `json:"retrying"`
	// Next says, in a few words, what happens now.
	Next string `json:"next"`
	// ReasonAt, ReasonAge and ReasonSession say when and in which hand-off
	// session Reason was decided (session is ids only); Stale is true once a
	// newer hand-off of the item exists, so Reason is history rather than
	// the item's state now (hivecommons/hive#18916).
	ReasonAt      *time.Time `json:"reason_at,omitempty"`
	ReasonAge     string     `json:"reason_age,omitempty"`
	ReasonSession string     `json:"reason_session,omitempty"`
	Stale         bool       `json:"stale"`
}

// StageTrackerWorkflowHandoff is a not-started hand-off on a tracker row.
type StageTrackerWorkflowHandoff struct {
	Profile  string `json:"profile"`
	State    string `json:"state"`
	Agent    string `json:"agent"`
	Reason   string `json:"reason"`
	Retrying bool   `json:"retrying"`
	Next     string `json:"next,omitempty"`
	// ReasonAt, ReasonAge, ReasonSession and Stale: see WorkflowDispatchHandoff.
	ReasonAt      *time.Time `json:"reason_at,omitempty"`
	ReasonAge     string     `json:"reason_age,omitempty"`
	ReasonSession string     `json:"reason_session,omitempty"`
	Stale         bool       `json:"stale"`
}

// workflowDispatchHandoffs is the live not-started list, never nil.
func (s *Server) workflowDispatchHandoffs() []WorkflowDispatchHandoff {
	if s == nil || s.deps == nil || s.deps.WorkflowDispatchHandoffs == nil {
		return []WorkflowDispatchHandoff{}
	}
	if list := s.deps.WorkflowDispatchHandoffs(); list != nil {
		return list
	}
	return []WorkflowDispatchHandoff{}
}

// workflowDispatchStatuses is workflowDispatchProfiles with each profile's
// not-started hand-offs attached.
func (s *Server) workflowDispatchStatuses() []WorkflowDispatchProfileStatus {
	if s == nil || s.deps == nil {
		return []WorkflowDispatchProfileStatus{}
	}
	out := workflowDispatchProfiles(s.deps.Config, s.workflowProfileLookup())
	attachWorkflowHandoffsToProfiles(out, s.workflowDispatchHandoffs())
	return out
}

// attachWorkflowHandoffsToProfiles files each hand-off under the profile of
// the bound repository it belongs to.
func attachWorkflowHandoffsToProfiles(profiles []WorkflowDispatchProfileStatus, handoffs []WorkflowDispatchHandoff) {
	for i := range profiles {
		for _, h := range handoffs {
			if h.Profile == profiles[i].Profile && workflowRepoMatches(h.Repo, profiles[i].Repo) {
				profiles[i].NotStarted = append(profiles[i].NotStarted, h)
			}
		}
	}
}

// attachWorkflowHandoffsToTracker files each hand-off under its repository's
// tracker block, keyed by the item number (the JSON key the SPA indexes by
// `item.number`).
func attachWorkflowHandoffsToTracker(w *StageTrackerWorkflows, handoffs []WorkflowDispatchHandoff) {
	if w == nil {
		return
	}
	for _, h := range handoffs {
		for repo, block := range w.Repos {
			if !workflowRepoMatches(h.Repo, repo) {
				continue
			}
			if block.Handoffs == nil {
				block.Handoffs = map[string]StageTrackerWorkflowHandoff{}
			}
			block.Handoffs[strconv.Itoa(h.Number)] = StageTrackerWorkflowHandoff{Profile: h.Profile, State: h.State, Agent: h.Agent, Reason: h.Reason, Retrying: h.Retrying, Next: h.Next,
				ReasonAt: h.ReasonAt, ReasonAge: h.ReasonAge, ReasonSession: h.ReasonSession, Stale: h.Stale}
			w.Repos[repo] = block
		}
	}
}

// workflowRepoMatches compares two spellings of a repository, one of which
// may lack the owner ("techdocs" and "cncf/techdocs").
func workflowRepoMatches(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	bare := func(r string) string { return r[strings.LastIndex(r, "/")+1:] }
	if strings.Contains(a, "/") && strings.Contains(b, "/") {
		return false
	}
	return bare(a) == bare(b)
}
