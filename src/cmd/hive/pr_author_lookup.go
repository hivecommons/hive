package main

import (
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/scheduler"
)

// prAuthorLookupTTL bounds how stale the kick's "PRs you opened" view may be.
// The audit read walks rotated backups, so it is not repeated on every kick;
// a PR opened inside the TTL shows as yours on a kick after it.
const prAuthorLookupTTL = 5 * time.Minute

// cachedPRAuthorLookup is the scheduler's PR-author lookup
// (hivecommons/hive#19325): the audit trail's "repo#n" -> opening agent map
// over auditPRAttributionWindow, the same attribution the review-feedback
// sweep routes by, cached for prAuthorLookupTTL. read is auditPRAgents in
// production. The returned map is shared; callers must not modify it.
func cachedPRAuthorLookup(org string, read func(org string, since time.Time, auditPath string) map[string]string, now func() time.Time) scheduler.PRAuthorLookup {
	var (
		mu     sync.Mutex
		cached map[string]string
		at     time.Time
	)
	return func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		t := now()
		if cached == nil || t.Sub(at) >= prAuthorLookupTTL {
			cached = read(org, t.Add(-auditPRAttributionWindow), "")
			if cached == nil {
				cached = map[string]string{}
			}
			at = t
		}
		return cached
	}
}
