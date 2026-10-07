package dashboard

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/github"
)

// FrontendAgentLastAction is the last issue or pull request an agent itself
// acted on through the hive (#10925, #10933). Current is true only when the
// action belongs to the agent's current round of work (not older than its
// latest kick); an older action is still reported so the card can show it as
// past. The record is in memory only, so nothing from before a hive restart is
// ever reported. At is on the hive's clock, like the payload's timestamp.
type FrontendAgentLastAction struct {
	Repo    string `json:"repo"`
	Number  int    `json:"number"`
	Action  string `json:"action"`
	At      string `json:"at"`
	Current bool   `json:"current"`
}

type agentLastAction struct {
	repo   string
	number int
	action string
	at     time.Time
}

var agentLastActionsMu sync.RWMutex

var agentLastActions = map[string]agentLastAction{}

// agentCausedAuditActions are the write-audit actions an agent itself causes
// through the hive's relays. The hive's own writes (reservation comments and
// labels, automatic labels, hold migrations, advisories) are recorded under
// other actions or never reach the audit sink, so they cannot move an agent's
// item.
var agentCausedAuditActions = map[string]bool{
	github.AuditActionAgentPRCreated:       true,
	github.AuditActionAgentIssueCreated:    true,
	github.AuditActionAgentCommentCreated:  true,
	github.AuditActionIssueClaimed:         true,
	github.AuditActionAgentLabelApplied:    true,
	github.AuditActionAgentReviewRequested: true,
	github.AuditActionIssueClosed:          true,
	github.AuditActionPRClosed:             true,
	github.AuditActionPRReviewed:           true,
	github.AuditActionPRMerged:             true,
}

// RecordAgentLastAction updates the agent's "last item acted on" from one
// hive-mediated write-audit record. Records the governor is attributed with
// (the hive's own approve and merge), records not caused by an agent, and
// writes with no issue or PR number (a branch push) are ignored.
func RecordAgentLastAction(rec github.AuditRecord, at time.Time) {
	agentName := strings.TrimSpace(rec.Agent)
	repo := strings.TrimSpace(rec.Repo)
	if agentName == "" || agentName == github.AttributionAgentGovernor || agentName == "system" ||
		repo == "" || rec.Target <= 0 || !agentCausedAuditActions[rec.Action] {
		return
	}
	agentLastActionsMu.Lock()
	defer agentLastActionsMu.Unlock()
	if prev, ok := agentLastActions[agentName]; ok && prev.at.After(at) {
		return
	}
	agentLastActions[agentName] = agentLastAction{repo: repo, number: rec.Target, action: rec.Action, at: at}
}

func lookupAgentLastAction(agentName string) (agentLastAction, bool) {
	agentLastActionsMu.RLock()
	defer agentLastActionsMu.RUnlock()
	la, ok := agentLastActions[agentName]
	return la, ok
}

// applyAgentLastAction fills the agent's lastAction, doing and summaryUpdated
// (#10927). While the agent is working, doing names the item it last acted on
// this round and summaryUpdated is the time of that action, or the round's
// start when it has not acted yet, so the card's stale warning and age badge
// measure time without progress through the hive.
func applyAgentLastAction(a *FrontendAgent, proc *agent.AgentProcess, busy string) {
	la, ok := lookupAgentLastAction(a.Name)
	current := ok && (proc.LastKick == nil || !la.at.Before(*proc.LastKick))
	if ok {
		a.LastAction = &FrontendAgentLastAction{
			Repo:    la.repo,
			Number:  la.number,
			Action:  la.action,
			At:      la.at.UTC().Format(time.RFC3339),
			Current: current,
		}
	}
	if busy != "working" || proc.Paused {
		return
	}
	switch {
	case current:
		a.Doing = la.repo + "#" + strconv.Itoa(la.number)
		a.SummaryUpdated = la.at.UTC().Format(time.RFC3339)
	case proc.LastKick != nil:
		a.SummaryUpdated = proc.LastKick.UTC().Format(time.RFC3339)
	}
}
