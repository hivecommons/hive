package hub

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Server-side filter / sort / pagination for /api/saas/my-hives.
//
// At fleet scale the My Hives payload is the hub's hottest endpoint and used
// to ship EVERY visible row on every poll. The set-wide computations (drift
// norm, fleet alerts, cluster-outage suppression) are properties of the whole
// visible set and still run over it — this layer scopes only what goes on the
// WIRE afterwards. With no query parameters the response is byte-compatible
// with the old behavior (all rows), so existing dashboards keep working.

// maxMyHivesPerPage caps per_page so a caller cannot ask for a pathological
// page size; 0/absent per_page means "no pagination" (full set, back-compat).
const maxMyHivesPerPage = 500

type myHivesQuery struct {
	q       string // case-insensitive substring across id/name/org/project/owner/repo/cluster
	status  string // "online", "offline", advisory issue bucket, or exact provStatus (available, assigned, provisioning, error, ...)
	cluster string // ClusterID or ClusterName, case-insensitive
	sortKey string // id | name | org | owner | cluster | status | last_seen | advisory_activity
	desc    bool
	page    int // 1-based; 0 = no pagination
	perPage int
}

// active reports whether the request asked for any server-side scoping at all;
// when false the handler returns the full set exactly as before.
func (q myHivesQuery) active() bool {
	return q.q != "" || q.status != "" || q.cluster != "" || q.sortKey != "" || q.perPage > 0
}

func parseMyHivesQuery(v url.Values) myHivesQuery {
	q := myHivesQuery{
		q:       strings.TrimSpace(v.Get("q")),
		status:  strings.ToLower(strings.TrimSpace(v.Get("status"))),
		cluster: strings.TrimSpace(v.Get("cluster")),
		sortKey: strings.ToLower(strings.TrimSpace(v.Get("sort"))),
		desc:    strings.EqualFold(strings.TrimSpace(v.Get("order")), "desc"),
	}
	if n, err := strconv.Atoi(v.Get("per_page")); err == nil && n > 0 {
		if n > maxMyHivesPerPage {
			n = maxMyHivesPerPage
		}
		q.perPage = n
		q.page = 1
	}
	if n, err := strconv.Atoi(v.Get("page")); err == nil && n > 1 && q.perPage > 0 {
		q.page = n
	}
	return q
}

func entryMatches(e *MyHiveEntry, q myHivesQuery) bool {
	if q.q != "" {
		needle := strings.ToLower(q.q)
		hay := strings.ToLower(strings.Join([]string{
			e.ID, e.Name, e.Org, e.ProjectName, e.Owner, e.PrimaryRepo,
			strings.Join(e.Repos, " "), e.ClusterID, e.ClusterName,
		}, "\x00"))
		if !strings.Contains(hay, needle) {
			return false
		}
	}
	switch q.status {
	case "":
	case "online":
		if !e.Online {
			return false
		}
	case "offline":
		if e.Online {
			return false
		}
	case "advisory-fresh", "advisory-aging", "advisory-stale", "advisory-unknown":
		if e.AdvisoryIssueActivity.Bucket != strings.TrimPrefix(q.status, "advisory-") {
			return false
		}
	default:
		if !strings.EqualFold(e.ProvStatus, q.status) {
			return false
		}
	}
	if q.cluster != "" &&
		!strings.EqualFold(e.ClusterID, q.cluster) &&
		!strings.EqualFold(e.ClusterName, q.cluster) {
		return false
	}
	return true
}

// applyMyHivesQuery filters, sorts and pages the assembled entry set. It
// returns the wire view plus the matched count (pre-pagination) so the client
// can render "showing X of Y". The input slice is not mutated; sorting
// operates on the filtered copy.
func applyMyHivesQuery(entries []MyHiveEntry, q myHivesQuery) (view []MyHiveEntry, matched int) {
	filtered := make([]MyHiveEntry, 0, len(entries))
	for i := range entries {
		if entryMatches(&entries[i], q) {
			filtered = append(filtered, entries[i])
		}
	}
	matched = len(filtered)

	if q.sortKey != "" {
		key := func(e *MyHiveEntry) string {
			switch q.sortKey {
			case "id":
				return e.ID
			case "name":
				if e.Name != "" {
					return e.Name
				}
				return e.ID
			case "org":
				return e.Org
			case "owner":
				return e.Owner
			case "cluster":
				return e.ClusterName + "\x00" + e.ClusterID
			case "status":
				return e.ProvStatus
			case "last_seen":
				// RFC3339 sorts lexically; empty (never beat) sorts first asc.
				return e.LastHeartbeat
			case "advisory_activity":
				return e.AdvisoryIssueActivity.LastActivityAt
			default:
				return ""
			}
		}
		sort.SliceStable(filtered, func(i, j int) bool {
			a, b := strings.ToLower(key(&filtered[i])), strings.ToLower(key(&filtered[j]))
			if q.desc {
				return a > b
			}
			return a < b
		})
	}

	if q.perPage > 0 {
		start := (q.page - 1) * q.perPage
		if start >= len(filtered) {
			return []MyHiveEntry{}, matched
		}
		end := start + q.perPage
		if end > len(filtered) {
			end = len(filtered)
		}
		return filtered[start:end], matched
	}
	return filtered, matched
}

// myHivesSummary aggregates fleet-level counts over the caller's FULL visible
// set (never the filtered page) so summary tiles stay truthful regardless of
// the active filter. Rides every response — it is a handful of ints.
func myHivesSummary(entries []MyHiveEntry) map[string]int {
	s := map[string]int{"total": len(entries)}
	for i := range entries {
		e := &entries[i]
		if e.Online {
			s["online"]++
		} else {
			s["offline"]++
		}
		switch e.ProvStatus {
		case statusAvailable:
			s["pool_available"]++
		case "provisioning":
			s["provisioning"]++
		case "error":
			s["errors"]++
		}
		if e.AssignedUnclaimed {
			s["assigned_unclaimed"]++
		}
		if e.Upgrading {
			s["upgrading"]++
		}
		if e.UpgradeFailed {
			s["upgrade_failed"]++
		}
		switch e.AdvisoryIssueActivity.Bucket {
		case advisoryIssueBucketFresh:
			s["advisory_issue_fresh"]++
		case advisoryIssueBucketAging:
			s["advisory_issue_aging"]++
		case advisoryIssueBucketStale:
			s["advisory_issue_stale"]++
		default:
			s["advisory_issue_unknown"]++
		}
	}
	return s
}
