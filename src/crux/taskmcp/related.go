package taskmcp

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const DuplicateSweepReason = "duplicate_sweep"

func FilterRelatedWork(scope Scope, current RelatedItem, candidates []RelatedItem, recencyWindow time.Duration, now time.Time, page PageRequest) (RelatedWorkData, PageInfo) {
	if now.IsZero() {
		now = time.Now()
	}
	var out []RelatedItem
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.Repo) == "" || !strings.EqualFold(candidate.Repo, scope.Repo) {
			continue
		}
		if candidate.Number == scope.Number && strings.EqualFold(candidate.Kind, current.Kind) {
			continue
		}
		reasons := relatedReasons(scope, current, candidate, recencyWindow, now)
		if len(reasons) == 0 {
			continue
		}
		candidate.Reasons = mergeReasons(candidate.Reasons, reasons)
		out = append(out, candidate)
	}
	sort.SliceStable(out, func(i, j int) bool {
		it, jt := relatedTime(out[i]), relatedTime(out[j])
		if !it.Equal(jt) {
			return it.After(jt)
		}
		return out[i].Number < out[j].Number
	})
	return PaginateRelated(out, page)
}

func FilterHistory(scope Scope, current RelatedItem, candidates []RelatedItem, recencyWindow time.Duration, now time.Time, limit int) []RelatedItem {
	if now.IsZero() {
		now = time.Now()
	}
	if limit <= 0 {
		limit = DefaultTaskMCPHistoryLimit
	}
	var out []RelatedItem
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.Repo) == "" || !strings.EqualFold(candidate.Repo, scope.Repo) {
			continue
		}
		if candidate.Number == scope.Number && strings.EqualFold(candidate.Kind, current.Kind) {
			continue
		}
		relevant := overlaps(current.Files, candidate.Files) || citesIssue(candidate.Data.Title, scope) || citesIssue(candidate.Data.Body, scope)
		if !isHistorical(candidate, recencyWindow, now) || !relevant {
			continue
		}
		candidate.Reasons = mergeReasons(candidate.Reasons, historyReasons(scope, current, candidate, recencyWindow, now))
		out = append(out, candidate)
	}
	sort.SliceStable(out, func(i, j int) bool {
		it, jt := relatedTime(out[i]), relatedTime(out[j])
		if !it.Equal(jt) {
			return it.After(jt)
		}
		return out[i].Number < out[j].Number
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func historyReasons(scope Scope, current, candidate RelatedItem, recencyWindow time.Duration, now time.Time) []string {
	var reasons []string
	if overlaps(current.Files, candidate.Files) {
		reasons = append(reasons, "file_overlap")
	}
	if citesIssue(candidate.Data.Title, scope) || citesIssue(candidate.Data.Body, scope) {
		reasons = append(reasons, "citation")
	}
	if isHistorical(candidate, recencyWindow, now) {
		reasons = append(reasons, "recent_history")
	}
	sort.Strings(reasons)
	return reasons
}

func relatedReasons(scope Scope, current, candidate RelatedItem, recencyWindow time.Duration, now time.Time) []string {
	seen := map[string]bool{}
	add := func(reason string) {
		if reason != "" {
			seen[reason] = true
		}
	}
	for _, reason := range candidate.Reasons {
		if reason == DuplicateSweepReason {
			add(reason)
		}
	}
	if overlaps(current.Files, candidate.Files) {
		add("file_overlap")
	}
	if citesIssue(candidate.Data.Title, scope) || citesIssue(candidate.Data.Body, scope) {
		add("citation")
	}
	if recentlyMerged(candidate, recencyWindow, now) {
		add("recently_merged")
	}
	out := make([]string, 0, len(seen))
	for reason := range seen {
		out = append(out, reason)
	}
	sort.Strings(out)
	return out
}

// citesIssue reports whether text refers to the scoped issue or pull request:
// as "#N", as "owner/repo#N", or as a link to it in the scoped repository.
//
// What stands before the reference matters as much as what follows it.
// "kubernetes/kubernetes#42" is issue 42 of another repository, and "abc#42"
// is not a reference at all, so a bare "#N" counts only at the start of the
// text or after a character that cannot end a repository name or a word.
func citesIssue(text string, scope Scope) bool {
	if scope.Number <= 0 || text == "" {
		return false
	}
	const (
		before = `(?:^|[^0-9a-z_./-])`
		after  = `(?:[^0-9a-z_-]|$)`
	)
	number := strconv.Itoa(scope.Number)
	patterns := []string{before + `#` + number + after}
	if repo := strings.ToLower(strings.TrimSpace(scope.Repo)); repo != "" {
		quoted := regexp.QuoteMeta(repo)
		patterns = append(patterns, before+quoted+`#`+number+after)
		if strings.Count(repo, "/") == 1 {
			// https://host/owner/repo/issues/N, .../pull/N, and GitLab's
			// .../-/issues/N. The repository must follow the host directly.
			patterns = append(patterns, `://[^/\s]+/`+quoted+`(?:/-)?/(?:issues|pull|pulls)/`+number+`(?:[^0-9]|$)`)
		}
	}
	lower := strings.ToLower(text)
	for _, pattern := range patterns {
		if regexp.MustCompile(pattern).MatchString(lower) {
			return true
		}
	}
	return false
}

func overlaps(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, file := range a {
		set[strings.ToLower(strings.TrimSpace(file))] = true
	}
	for _, file := range b {
		if set[strings.ToLower(strings.TrimSpace(file))] {
			return true
		}
	}
	return false
}

func isHistorical(item RelatedItem, window time.Duration, now time.Time) bool {
	if strings.EqualFold(item.Kind, "pull_request") {
		return item.MergedAt != nil && withinWindow(*item.MergedAt, window, now)
	}
	if strings.EqualFold(item.State, "closed") {
		return withinWindow(relatedTime(item), window, now)
	}
	return false
}

func recentlyMerged(item RelatedItem, window time.Duration, now time.Time) bool {
	if item.MergedAt == nil || window <= 0 {
		return false
	}
	return withinWindow(*item.MergedAt, window, now)
}

func withinWindow(t time.Time, window time.Duration, now time.Time) bool {
	if t.IsZero() || window <= 0 {
		return false
	}
	age := now.Sub(t)
	return age >= 0 && age <= window
}

func relatedTime(item RelatedItem) time.Time {
	if item.UpdatedAt != nil {
		return *item.UpdatedAt
	}
	if item.MergedAt != nil {
		return *item.MergedAt
	}
	return time.Time{}
}

func mergeReasons(existing, extra []string) []string {
	seen := map[string]bool{}
	for _, reason := range existing {
		if reason != "" {
			seen[reason] = true
		}
	}
	for _, reason := range extra {
		if reason != "" {
			seen[reason] = true
		}
	}
	out := make([]string, 0, len(seen))
	for reason := range seen {
		out = append(out, reason)
	}
	sort.Strings(out)
	return out
}
