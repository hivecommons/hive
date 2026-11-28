package scheduler

import (
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// SetSurgeDuration attaches the governor's read-only dwell snapshot. No flow
// signal adds kicks or overrides cadence, budget, or permission gates.
func (s *Scheduler) SetSurgeDuration(read func() (time.Duration, bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.surgeDuration = read
}

func (s *Scheduler) addGuideFlow(agent string, actionable *github.ActionableResult, message string) string {
	if message == "" || s.cfg.BaseAgentName(agent) != "guide" {
		return message
	}
	s.mu.RLock()
	read := s.surgeDuration
	s.mu.RUnlock()
	dwell, known := time.Duration(0), false
	if read != nil {
		dwell, known = read()
	}
	active, _ := s.activeReposForAgent(agent)
	allowed := make(map[string]bool, len(active))
	for _, repo := range active {
		allowed[config.QualifyRepo(s.cfg.Project.Org, repo)] = true
	}
	return message + "\n" + guideFlowLine(actionable, allowed, dwell, known)
}

// guideFlowLine uses the existing bounded enumeration, not a new GitHub scan.
// The mean is explicitly a sample of attributed merges; scan truncation and
// cached fallbacks mean it must never be presented as an exhaustive fleet mean.
func guideFlowLine(a *github.ActionableResult, allowed map[string]bool, dwell time.Duration, dwellKnown bool) string {
	const day = 24 * time.Hour
	surge, mean, oldest := "unknown", "unknown", "unknown"
	snapshot := "unknown"
	// cloggedBy names each threshold that tripped, so a single breach (a
	// long SURGE, one long-lived item) is not read as slow merges when the
	// merge sample is empty (hivecommons/hive#20273).
	var cloggedBy []string
	if dwellKnown && dwell >= 0 {
		surge = fmt.Sprintf("%dm", int64(dwell/time.Minute))
		if dwell >= 3*day {
			cloggedBy = append(cloggedBy, "surge")
		}
	}
	n := 0
	if a != nil && !a.GeneratedAt.IsZero() {
		snapshot = a.GeneratedAt.UTC().Format(time.RFC3339)
		maxAge := 0
		for _, issue := range a.Issues.Items {
			if allowed[issue.Repo] && issue.AgeMinutes > maxAge {
				maxAge = issue.AgeMinutes
			}
		}
		for _, pr := range a.PRs.Items {
			if !allowed[pr.Repo] || pr.CreatedAt.IsZero() || !guideFlowAwaitsFlow(pr) {
				continue
			}
			age := int(a.GeneratedAt.Sub(pr.CreatedAt) / time.Minute)
			if age > maxAge {
				maxAge = age
			}
		}
		oldest = fmt.Sprintf("%dm", maxAge)
		if maxAge >= 14*24*60 {
			cloggedBy = append(cloggedBy, "oldest")
		}
		var totalMinutes float64
		seen := make(map[string]bool)
		for _, pr := range a.PRs.Attributed {
			key := fmt.Sprintf("%s#%d", pr.Repo, pr.Number)
			if !allowed[pr.Repo] || seen[key] || pr.MergedAt.IsZero() || pr.CreatedAt.IsZero() ||
				pr.MergedAt.Before(a.GeneratedAt.Add(-14*day)) || pr.MergedAt.After(a.GeneratedAt) || pr.CreatedAt.After(pr.MergedAt) {
				continue
			}
			seen[key] = true
			totalMinutes += pr.MergedAt.Sub(pr.CreatedAt).Minutes()
			n++
		}
		if n > 0 {
			avg := totalMinutes / float64(n)
			mean = fmt.Sprintf("%dm", int64(avg))
			if avg > (7 * day).Minutes() {
				cloggedBy = append(cloggedBy, "mttm")
			}
		}
	}
	status, reasons := "normal", "none"
	if len(cloggedBy) > 0 {
		status, reasons = "clogged", strings.Join(cloggedBy, ",")
	} else if surge == "unknown" || mean == "unknown" || oldest == "unknown" {
		status = "unknown"
	}
	return fmt.Sprintf("HIVE_FLOW: %s clogged_by=%s surge=%s mttm_sample=%s merged_sample=%d window=14d oldest_actionable=%s scope=authorized-repos surge_scope=hive source=cached-attributed-prs snapshot=%s\n", status, reasons, surge, mean, n, oldest, snapshot)
}

// guideFlowAwaitsFlow reports whether an open PR's age says anything about
// review/merge flow. A draft is not ready, and a PR whose review decision is
// CHANGES_REQUESTED waits on its author; either can stay open for months by
// design, and counting it made one such PR hold the oldest-actionable
// threshold true for as long as it lived (hivecommons/hive#20273).
func guideFlowAwaitsFlow(pr github.PullRequest) bool {
	if pr.Draft {
		return false
	}
	return pr.Protection == nil || pr.Protection.ReviewDecision != github.ReviewDecisionChangesRequested
}
