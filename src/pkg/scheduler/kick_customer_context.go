package scheduler

import (
	"strings"

	"github.com/hivecommons/hive/pkg/github"
)

// CustomerContextLookup returns the customer-context block for the people
// behind some GitHub logins, already capped, scanned and fenced
// (customers.Service.KickContext), or "" when none is a customer. The
// scheduler does not import the customers package: it hands over logins and
// appends what comes back.
type CustomerContextLookup func(logins []string) string

// SetCustomerContextLookup installs (or with nil, removes) the lookup the
// kick seam reads.
func (s *Scheduler) SetCustomerContextLookup(fn CustomerContextLookup) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.customerContext = fn
}

// customerContextKickHeader opens the block; the seam is skipped when the
// kick already carries it.
const customerContextKickHeader = "# Customer context"

// customerContextMaxLogins bounds how many logins one kick asks about.
const customerContextMaxLogins = 20

// addCustomerContext appends who the kicked issues are for: the authors,
// assignees and claimers that are customers. The block is bounded and
// fenced by the lookup. Nothing is appended with no lookup installed or no
// customer among the issues.
func (s *Scheduler) addCustomerContext(message string, issues []github.Issue) string {
	if message == "" || strings.Contains(message, customerContextKickHeader) {
		return message
	}
	s.mu.RLock()
	fn := s.customerContext
	s.mu.RUnlock()
	if fn == nil {
		return message
	}
	var logins []string
	add := func(l string) {
		if l = strings.TrimSpace(l); l != "" && len(logins) < customerContextMaxLogins {
			logins = append(logins, l)
		}
	}
	for _, is := range issues {
		if is.Number <= 0 {
			continue // a Linear or Jira item has no GitHub login
		}
		if is.AuthorIsHuman {
			add(is.Author)
		}
		for _, a := range is.Assignees {
			add(a)
		}
		add(is.ClaimedBy)
	}
	if len(logins) == 0 {
		return message
	}
	block := strings.TrimSpace(fn(logins))
	if block == "" || !strings.HasPrefix(block, customerContextKickHeader) {
		return message
	}
	return appendKickSection(message, block)
}
