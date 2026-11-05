package github

import (
	"regexp"
	"strings"

	itemblocker "github.com/hivecommons/hive/pkg/blocker"
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

// workflowsHandoff turns a lane's workflow-only park into a hand-off to the
// lane that can push it: the issue is not parked, it carries
// WorkflowsLaneLabel, and the posted text says where it went. Inactive when
// the requester can push workflows itself, when no lane can (the owner still
// applies it), or when the text is not a workflow-only park.
func (c *Client) workflowsHandoff(agent, repo, title, body string) issueDecisionDowngrade {
	if !parkIsWorkflowsOnly(title + "\n" + body) {
		return issueDecisionDowngrade{}
	}
	lane := c.workflowsLaneFor(agent, repo)
	if lane == "" {
		return issueDecisionDowngrade{}
	}
	return issueDecisionDowngrade{Active: true, Lane: lane, Note: workflowsHandoffNote(agent, lane)}
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
	if strings.Contains(body, "Handed to `"+d.Lane+"`:") {
		return body
	}
	if body == "" {
		return d.Note
	}
	return body + "\n\n" + d.Note
}
