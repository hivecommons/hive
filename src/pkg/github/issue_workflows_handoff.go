package github

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	itemblocker "github.com/hivecommons/hive/pkg/blocker"
	"github.com/hivecommons/hive/pkg/writeback"
)

// WorkflowsLaneLabel routes an issue to the lane that can push
// .github/workflows/** (config.WorkflowsCapableLane). The relay adds it when a
// lane whose token cannot push workflow files parks a workflow-only change,
// and the scheduler sends the issue to that lane instead of back to the one
// that could not land it.
const WorkflowsLaneLabel = "hive/workflows-lane"

// SetWorkflowsLaneFunc installs the fleet lookup the relay asks
// (config.WorkflowsLaneFor): given the requesting agent and the repository,
// the lane that can push workflow files there for it, or "" when the agent
// can push them itself or no lane serving the repository can. Nil, or never
// set, keeps every workflow-only park with the owner.
func (c *Client) SetWorkflowsLaneFunc(fn func(agent, repo string) string) {
	if c == nil {
		return
	}
	c.workflowsLaneMu.Lock()
	defer c.workflowsLaneMu.Unlock()
	c.workflowsLane = fn
}

// SetWorkflowsSelfPushFunc installs the fleet lookup (config.AgentPushesWorkflows)
// that says whether an agent's own scoped token asks for the Workflows
// permission. Nil, or never set, treats every lane as unable to push them.
func (c *Client) SetWorkflowsSelfPushFunc(fn func(agent string) bool) {
	if c == nil {
		return
	}
	c.workflowsLaneMu.Lock()
	defer c.workflowsLaneMu.Unlock()
	c.workflowsSelfPush = fn
}

func (c *Client) workflowsSelfPushFor(agent string) bool {
	if c == nil || strings.TrimSpace(agent) == "" {
		return false
	}
	c.workflowsLaneMu.RLock()
	fn := c.workflowsSelfPush
	c.workflowsLaneMu.RUnlock()
	return fn != nil && fn(strings.TrimSpace(agent))
}

func (c *Client) workflowsLaneFor(agent, repo string) string {
	if c == nil {
		return ""
	}
	c.workflowsLaneMu.RLock()
	fn := c.workflowsLane
	c.workflowsLaneMu.RUnlock()
	if fn == nil {
		return ""
	}
	return strings.TrimSpace(fn(agent, repo))
}

// workflowsPermissionRE is the phrase lane parks use when blocker.Infer's
// narrower rules miss it: Danathar/arch-bootc#634 said "This agent's GitHub
// App token does not carry the Workflows permission".
var workflowsPermissionRE = regexp.MustCompile("(?i)\\bworkflows?`?\\s+permission")

// parkIsWorkflowsOnly reports whether a park's text says the remaining change
// is one this lane cannot push because it is under .github/workflows/: the
// structured workflows blocker marker, prose blocker.Infer classifies as one,
// or the text naming .github/workflows next to "workflows permission". A park
// that lists lettered options is a choice for the owner, never handed off.
func parkIsWorkflowsOnly(text string) bool {
	if len(unparkOptionLabels(text)) > 0 {
		return false
	}
	if k, ok := itemblocker.ParseMarker(text); ok {
		return k == itemblocker.CapabilityWorkflowsPermission
	}
	if k, ok := itemblocker.Infer(text); ok && k == itemblocker.CapabilityWorkflowsPermission {
		return true
	}
	return strings.Contains(strings.ToLower(text), ".github/workflows") && workflowsPermissionRE.MatchString(text)
}

// workflowsHandoff checks a lane's workflow-only park against what the hive
// can actually push. A lane whose own token asks for the Workflows permission,
// on an installation that grants it, is refused the park and keeps the issue
// (projectbluefin/testsuite#1059: scanner, at the merger tier, parked "the
// hive app cannot push .github/workflows/" the same day the App pushed
// e2e.yml in #1089). Any other lane hands it to the lane that can push it:
// the issue is not parked, it carries WorkflowsLaneLabel, and the posted text
// says where it went. Inactive (the park stands) when the text is not a
// workflow-only park, when no lane can push it, when the installation is
// known to lack `workflows: write`, or when a lane that pushes workflow files
// quotes GitHub's rejection of its push.
func (c *Client) workflowsHandoff(ctx context.Context, agent, repo, title, body string) issueDecisionDowngrade {
	if !parkIsWorkflowsOnly(title + "\n" + body) {
		return issueDecisionDowngrade{}
	}
	if c.workflowsSelfPushFor(agent) {
		// A park quoting GitHub's rejection is what the note below asks
		// for when a push still bounces: evidence, so it stands.
		if itemblocker.QuotesWorkflowsRejection(title + "\n" + body) {
			return issueDecisionDowngrade{}
		}
		if granted, known := c.installationGrantsWorkflows(ctx); known && granted {
			return issueDecisionDowngrade{Active: true, SelfPush: true, Note: workflowsSelfPushNote(agent)}
		}
		return issueDecisionDowngrade{}
	}
	lane := c.workflowsLaneFor(agent, repo)
	if lane == "" {
		return issueDecisionDowngrade{}
	}
	if granted, known := c.installationGrantsWorkflows(ctx); known && !granted {
		return issueDecisionDowngrade{}
	}
	return issueDecisionDowngrade{Active: true, Lane: lane, Note: workflowsHandoffNote(agent, lane)}
}

func workflowsSelfPushNote(agent string) string {
	who := "this lane"
	if a := strings.TrimSpace(agent); a != "" {
		who = "`" + a + "`"
	}
	return "Not parked: " + who + "'s token can push `.github/workflows/`. Its tier asks for the Workflows permission and the App installation grants `workflows: write`, so " +
		who + " pushes the change. If a push is still rejected, park again quoting GitHub's error. Owner may override with `/hive hold` or `needs-human`."
}

func workflowsHandoffNote(agent, lane string) string {
	from := "this lane"
	if a := strings.TrimSpace(agent); a != "" {
		from = "`" + a + "`"
	}
	return "Handed to `" + lane + "`: Hive did not park this for the owner. The change is under `.github/workflows/`, which " + from +
		"'s token cannot push and `" + lane + "`'s can, so `" + lane + "` lands it (label `" + WorkflowsLaneLabel +
		"`). Owner may override with `/hive hold` or `needs-human`."
}

func appendWorkflowsHandoff(body string, d issueDecisionDowngrade) string {
	body = strings.TrimRight(body, "\n")
	seen := d.Note
	if d.Lane != "" {
		seen = "Handed to `" + d.Lane + "`:"
	}
	if strings.Contains(body, seen) {
		return body
	}
	if body == "" {
		return d.Note
	}
	return body + "\n\n" + d.Note
}

// workflowsGrantTTL is how long the installation's `workflows` grant is
// trusted before it is read again: a grant or revocation shows within a few
// minutes, and a burst of parks costs one GitHub call.
const workflowsGrantTTL = 5 * time.Minute

type workflowsGrantCache struct {
	mu      sync.Mutex
	at      time.Time
	granted bool
}

// installationProbe reads the App installation's granted permissions; tests
// swap it.
var installationProbe = func(ctx context.Context, auth *AppAuth) (*InstallationInfo, error) {
	return auth.VerifyInstallation(ctx)
}

// installationGrantsWorkflows reports whether the App installation grants
// `workflows: write`. known is false for a token-authenticated client or a
// failed read, so callers keep their previous behaviour.
func (c *Client) installationGrantsWorkflows(ctx context.Context) (granted, known bool) {
	if c == nil || c.appAuth == nil {
		return false, false
	}
	g := &c.workflowsGrant
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.at.IsZero() && time.Since(g.at) < workflowsGrantTTL {
		return g.granted, true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	info, err := installationProbe(ctx, c.appAuth)
	if err != nil {
		return false, false
	}
	g.at, g.granted = time.Now(), info.Granted(itemblocker.CapabilityWorkflowsPermission.Permission(), "write")
	return g.granted, true
}

// WorkflowsPushable reports whether the hive can push .github/workflows/**
// in repo: a lane whose token asks for the Workflows permission serves it
// (an empty agent asks the lane lookup for any capable lane) and the
// installation grants `workflows: write`. Unknown is false.
func (c *Client) WorkflowsPushable(ctx context.Context, repo string) bool {
	if c.workflowsLaneFor("", repo) == "" {
		return false
	}
	granted, known := c.installationGrantsWorkflows(ctx)
	return known && granted
}

// CapabilityAvailable is the blocker ledger's check (blocker.Ledger
// SetCapabilityCheck): a workflows-permission blocker on repo#number is
// refuted when WorkflowsPushable holds and the item is not a fork PR. A fork
// branch takes no workflow-file push from the App whatever the installation
// grants (projectbluefin/testsuite#1037: the fixer's pushes were refused
// three times), so that blocker stands. Other kinds are never refuted here.
// The ledger keys webhook reports by the configured repo name, which may be
// short, so the lookups get owner/repo.
func (c *Client) CapabilityAvailable(repo string, number int, k itemblocker.Kind) bool {
	if c == nil || k != itemblocker.CapabilityWorkflowsPermission {
		return false
	}
	owner, name := c.splitRepo(repo)
	ctx := context.Background()
	if !c.WorkflowsPushable(ctx, owner+"/"+name) {
		return false
	}
	fork, known := c.forkPR(ctx, owner, name, number)
	return known && !fork
}

// WorkflowsRejectionProves is the blocker ledger's proof check
// (blocker.Ledger SetProofCheck): a quoted GitHub rejection of a workflow
// push proves the App lacks `workflows` only when the reporting lane's own
// token asks for it. The reporter is the text's `— hive: agent=` trailer; a
// report without one (a person pasting the error) is trusted.
func (c *Client) WorkflowsRejectionProves(text string) bool {
	agent := trailerAgent(text)
	return agent == "" || c.workflowsSelfPushFor(agent)
}

// forkPR reports whether owner/name#number is a pull request whose head is
// in another repository (or a deleted one). known is false when GitHub
// cannot be read.
func (c *Client) forkPR(ctx context.Context, owner, name string, number int) (fork, known bool) {
	issue, _, err := c.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return false, false
	}
	if !issue.IsPullRequest() {
		return false, true
	}
	pr, _, err := c.client.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		return false, false
	}
	head := pr.GetHead().GetRepo().GetFullName()
	return head == "" || !strings.EqualFold(head, owner+"/"+name), true
}

// SweepWorkflowsBlockers re-checks the blocker ledger's capability blockers
// against what the hive can push (CapabilityAvailable) and lifts the ones it
// refutes. Each released workflows blocker gets WorkflowsLaneLabel, so once
// its park is answered the issue goes to the lane that can push the change,
// not back to the lane that parked it. The park label itself stays: a person
// still answers it, and "Let hive decide" is no longer disabled by a blocker.
// It returns the released entries.
func (c *Client) SweepWorkflowsBlockers(ctx context.Context) ([]itemblocker.Entry, error) {
	if c == nil {
		return nil, nil
	}
	released := itemblocker.Shared().RecheckCapabilities(c.CapabilityAvailable)
	var firstErr error
	for _, e := range released {
		if e.Blocker.Kind != itemblocker.CapabilityWorkflowsPermission {
			continue
		}
		// An unsolicited routing label, gated like the stage engine's own
		// labels; a refusal leaves the blocker released and the label off.
		owner, name := c.splitRepo(e.Repo)
		if d := c.WritebackAllowed(owner+"/"+name, writeback.Item{Number: e.Number}, writeback.KindStageLabel, WorkflowsLaneLabel); !d.Allowed {
			continue
		}
		if err := c.AddLabels(ctx, e.Repo, e.Number, []string{WorkflowsLaneLabel}); err != nil && firstErr == nil { // writeback:gated
			firstErr = err
		}
	}
	return released, firstErr
}
