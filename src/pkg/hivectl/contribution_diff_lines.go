package hivectl

import (
	"fmt"
	"strings"
)

// Lines renders the staged diff as the review list every surface shows before
// Apply: counts first, then follow/enable/hub changes, legacy conversions,
// shared branches and every exclusion with its remaining effects. The web page
// and the TUI print the same lines so a reviewer sees one vocabulary.
func (d ContributionDiff) Lines() []string {
	var lines []string
	if d.SelectedCount > 0 {
		lines = append(lines, fmt.Sprintf("+%d repositories selected", d.SelectedCount))
	}
	if d.DeselectedCount > 0 {
		lines = append(lines, fmt.Sprintf("−%d deselected", d.DeselectedCount))
	}
	if d.ExcludedCount > 0 {
		lines = append(lines, fmt.Sprintf("%d excluded", d.ExcludedCount))
	}
	if d.ClearedExclusionCount > 0 {
		lines = append(lines, fmt.Sprintf("%d exclusions cleared", d.ClearedExclusionCount))
	}
	var followOn, followOff int
	for _, f := range d.Follow {
		if f.After {
			followOn++
		} else {
			followOff++
		}
	}
	if followOn > 0 {
		lines = append(lines, fmt.Sprintf("follow on for %d Hive(s)", followOn))
	}
	if followOff > 0 {
		lines = append(lines, fmt.Sprintf("stop following for %d Hive(s)", followOff))
	}
	var enable, disable []string
	for _, e := range d.Enabled {
		if e.After {
			enable = append(enable, e.Target)
		} else {
			disable = append(disable, e.Target)
		}
	}
	if len(enable) > 0 {
		lines = append(lines, "enable "+strings.Join(enable, ", "))
	}
	if len(disable) > 0 {
		lines = append(lines, "disable "+strings.Join(disable, ", ")+" (credentials and selections retained)")
	}
	for _, h := range d.HubFollow {
		lines = append(lines, fmt.Sprintf("hub %s future Hives %s → %s", h.Hub, h.Before, h.After))
	}
	if len(d.LegacyConversions) > 0 {
		lines = append(lines, "legacy conversion: "+strings.Join(d.LegacyConversions, ", "))
	}
	for _, s := range d.SharedBranches {
		lines = append(lines, fmt.Sprintf("shared branch %s changes in all of: %s", s.Subscription, strings.Join(s.Hubs, ", ")))
	}
	for _, x := range d.Exclusions {
		verb := "clear exclusion of "
		if x.Excluded {
			verb = "exclude "
		}
		l := verb + x.Repository + " at " + x.Subscription
		if len(x.ScopedSubscriptions) > 0 {
			l += "; also affects saved membership at " + strings.Join(x.ScopedSubscriptions, ", ")
		}
		if len(x.Sources) > 0 {
			l += "; exclusion still kept by " + strings.Join(x.Sources, ", ")
		}
		if len(x.LegacyUnrestricted) > 0 {
			l += "; legacy unrestricted " + strings.Join(x.LegacyUnrestricted, ", ") + " can still receive it"
		}
		lines = append(lines, l)
	}
	var membershipOnly []string
	for _, m := range d.Membership {
		mentioned := false
		for _, l := range lines {
			if strings.Contains(l, m.Subscription) {
				mentioned = true
				break
			}
		}
		if !mentioned {
			membershipOnly = append(membershipOnly, m.Subscription)
		}
	}
	if len(membershipOnly) > 0 {
		lines = append(lines, "membership rules changed at "+strings.Join(membershipOnly, ", "))
	}
	return lines
}

// Changed names the subscriptions and repositories a staged diff
// touches so a tree can mark them. Repository keys are "subscription|repo".
func (d ContributionDiff) Changed() (subscriptions map[string]bool, repositories map[string]bool) {
	subscriptions, repositories = map[string]bool{}, map[string]bool{}
	for _, m := range d.Membership {
		subscriptions[m.Subscription] = true
	}
	for _, e := range d.Enabled {
		subscriptions[e.Target] = true
	}
	for _, f := range d.Follow {
		subscriptions[f.Target] = true
	}
	for _, t := range append(append([]StageTarget(nil), d.Selected...), d.Deselected...) {
		subscriptions[t.Subscription] = true
		repositories[t.Subscription+"|"+t.Repository] = true
	}
	for _, x := range d.Exclusions {
		subscriptions[x.Subscription] = true
		repositories[x.Subscription+"|"+x.Repository] = true
		for _, s := range x.ScopedSubscriptions {
			subscriptions[s] = true
			repositories[s+"|"+x.Repository] = true
		}
	}
	return subscriptions, repositories
}

// SelectionLabel is the one-line summary of a branch's selection state shown
// next to its checkbox. It never claims "all" when membership is incomplete or
// the branch is a legacy unrestricted profile.
func SelectionLabel(sel TreeSelection, discovery TreeDiscovery) string {
	if sel.Legacy {
		return "Unrestricted — legacy"
	}
	if sel.Repositories == 0 {
		if discovery == TreeComplete {
			return "No repositories"
		}
		return "Off — no saved repositories"
	}
	label := "Off"
	switch sel.State {
	case "all-current":
		label = "All current"
	case "partial":
		label = "Partial"
	}
	label += fmt.Sprintf(" (%d/%d selected", sel.Selected, sel.Repositories)
	if sel.Excluded > 0 {
		label += fmt.Sprintf(", %d excluded", sel.Excluded)
	}
	if sel.Unreviewed > 0 {
		label += fmt.Sprintf(", %d unreviewed", sel.Unreviewed)
	}
	label += ")"
	if sel.MembershipIncomplete {
		label += " — Membership incomplete"
	}
	return label
}
