package github

import (
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// PRCIActivity is display evidence, not a merge authorization. Required is
// resolved by the existing protection collector; an unresolved set stays unknown.
type PRCIActivity struct {
	Head               string            `json:"head"`
	Source             string            `json:"source"`
	ObservedAt         *time.Time        `json:"observed_at,omitempty"`
	RequiredKnown      bool              `json:"required_known"`
	Required           []string          `json:"required,omitempty"`
	RequiredChecks     []PRRequiredCheck `json:"required_checks,omitempty"`
	RequiredSource     string            `json:"required_source,omitempty"`
	Checks             []PRActivityCheck `json:"checks,omitempty"`
	StatusesKnown      bool              `json:"statuses_known"`
	StatusesObservedAt *time.Time        `json:"statuses_observed_at,omitempty"`
}

type PRActivityCheck struct {
	Name       string     `json:"name"`
	State      string     `json:"state"`
	Conclusion string     `json:"conclusion,omitempty"`
	URL        string     `json:"url,omitempty"`
	Since      *time.Time `json:"since,omitempty"`
	ID         int64      `json:"id,omitempty"`
	NodeID     string     `json:"node_id,omitempty"`
	AppID      *int64     `json:"app_id,omitempty"`
	Head       string     `json:"head,omitempty"`
	Kind       string     `json:"kind,omitempty"`
}

// With AppKnown, nil or -1 is a forge-observed any-App constraint.
type PRRequiredCheck struct {
	Name     string `json:"name"`
	AppID    *int64 `json:"app_id"`
	AppKnown bool   `json:"app_known"`
}

func prCIActivity(head, source string, checks []*gh.CheckRun, observed *time.Time) *PRCIActivity {
	out := &PRCIActivity{Head: head, Source: source, ObservedAt: observed}
	for _, cr := range checks {
		if cr == nil || (cr.GetHeadSHA() != "" && !strings.EqualFold(cr.GetHeadSHA(), head)) {
			continue
		}
		check := PRActivityCheck{Name: cr.GetName(), State: cr.GetStatus(), Conclusion: cr.GetConclusion(), URL: cr.GetDetailsURL()}
		check.ID, check.NodeID, check.Head = cr.GetID(), cr.GetNodeID(), cr.GetHeadSHA()
		if cr.App != nil && cr.App.GetID() > 0 {
			check.AppID = clonePtr(cr.App.ID)
		}
		if at := cr.GetStartedAt(); !at.IsZero() {
			t := at.Time
			check.Since = &t
		}

		if at := cr.GetCompletedAt(); cr.GetStatus() == "completed" && !at.IsZero() {
			t := at.Time
			check.Since = &t
		}
		out.Checks = append(out.Checks, check)
	}
	return out
}

// ChecksForRequired excludes other producers when forge policy pins an App.
// Missing producer detail is unknown, not permission to use an optional App.
func (a *PRCIActivity) ChecksForRequired(name string) []PRActivityCheck {
	var constraints []PRRequiredCheck
	for _, required := range a.RequiredChecks {
		if required.Name == name {
			constraints = append(constraints, required)
		}
	}
	var out []PRActivityCheck
	for _, check := range a.Checks {
		if check.Name != name || (check.Head != "" && !strings.EqualFold(check.Head, a.Head)) {
			continue
		}
		if len(constraints) == 0 {
			out = append(out, check)
			continue
		}
		unknown := false
		for _, required := range constraints {
			if !required.AppKnown {
				unknown = true
				continue
			}
			if required.AppID == nil || *required.AppID == -1 || (check.AppID != nil && *check.AppID == *required.AppID) {
				out = append(out, check)
				unknown = false
				break
			}
			unknown = unknown || check.AppID == nil
		}
		if unknown {
			check.State, check.Conclusion = "unknown", ""
			out = append(out, check)
		}
	}
	return out
}

func (a *PRCIActivity) setRequired(set map[string]bool, known bool) {
	a.RequiredKnown = known
	a.Required = nil
	for name, required := range set {
		if required {
			a.Required = append(a.Required, name)
		}
	}
	sort.Strings(a.Required)
}

func (a *PRCIActivity) addCommitStatuses(statuses []*gh.RepoStatus, observedAt time.Time) {
	a.StatusesKnown, a.StatusesObservedAt = true, &observedAt
	for _, status := range statuses {
		if status == nil {
			continue
		}
		check := PRActivityCheck{
			Name: status.GetContext(), State: "completed", URL: status.GetTargetURL(),
			ID: status.GetID(), NodeID: status.GetNodeID(), Head: a.Head, Kind: "commit_status",
		}
		switch status.GetState() {
		case "success":
			check.Conclusion = "success"
		case "failure", "error":
			check.Conclusion = "failure"
		case "pending":
			check.State = "pending"
		default:
			check.State = "unknown"
		}
		if at := status.GetCreatedAt(); !at.IsZero() {
			t := at.Time
			check.Since = &t
		}
		duplicate := false
		for i, existing := range a.Checks {
			if check.NodeID != "" && existing.NodeID == check.NodeID {
				a.Checks[i] = check
				duplicate = true
				break
			}
		}

		if !duplicate {
			a.Checks = append(a.Checks, check)
		}
	}
}

// NeedsCommitStatusEvidence identifies REST snapshots whose any-App contexts
// also need the legacy status side before they can be called passing.
func (a *PRCIActivity) NeedsCommitStatusEvidence() bool {
	if a.Source != "github-check-runs" {
		return false
	}
	for _, binding := range a.RequiredChecks {
		if binding.AppKnown && (binding.AppID == nil || *binding.AppID == -1) {
			return true
		}
	}
	return false
}
