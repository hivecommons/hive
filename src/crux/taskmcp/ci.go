package taskmcp

import (
	"sort"
	"strings"
	"time"
)

const DefaultCIHealthStaleAfter = 15 * time.Minute

type CICacheInput struct {
	RequiredChecks []string
	DefaultBranch  string
	States         map[string]string
	FailingPRs     map[string]int
	LastGreenSHA   map[string]string
	CachedAt       time.Time
	PollInterval   time.Duration
}

func BuildCIHealth(input CICacheInput, now time.Time, page PageRequest) (CIHealthData, PageInfo) {
	if now.IsZero() {
		now = time.Now()
	}
	seen := map[string]bool{}
	for _, name := range input.RequiredChecks {
		name = strings.TrimSpace(name)
		if name != "" {
			seen[name] = true
		}
	}
	for name := range input.States {
		if strings.TrimSpace(name) != "" {
			seen[name] = true
		}
	}
	for name := range input.FailingPRs {
		if strings.TrimSpace(name) != "" {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	checks := make([]CheckHealth, 0, len(names))
	for _, name := range names {
		state := strings.TrimSpace(input.States[name])
		if state == "" {
			state = "unknown"
		}
		pendingApproval := isPendingApprovalCheck(name, state)
		if pendingApproval {
			state = "pending"
		}
		checks = append(checks, CheckHealth{Name: name, State: state, DefaultBranch: input.DefaultBranch, FailingPRs: input.FailingPRs[name], LastGreenSHA: input.LastGreenSHA[name], PendingApproval: pendingApproval})
	}
	data, info := PaginateChecks(checks, page)
	staleAfter := input.PollInterval
	if staleAfter <= 0 {
		staleAfter = DefaultCIHealthStaleAfter
	}
	if !input.CachedAt.IsZero() {
		data.CacheAgeSecs = int64(now.Sub(input.CachedAt).Seconds())
		data.CacheStale = now.Sub(input.CachedAt) > staleAfter
	}
	return data, info
}

func isPendingApprovalCheck(name, state string) bool {
	lower := strings.ToLower(name + " " + state)
	return strings.Contains(lower, "lgtm") || strings.Contains(lower, "approve")
}
