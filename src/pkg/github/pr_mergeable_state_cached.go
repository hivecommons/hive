package github

import "strings"

// CachedPRMergeableState answers GitHub's mergeable_state for exactly head
// of repo#number from the PR detail cache, without a request. The cache is
// what this hive already read: the enumeration's CI enrichment fetches every
// actionable and held PR's detail each cycle (EnrichCIStatus), and the
// GraphQL PR batch stores the same. ok is false when nothing current is
// cached, when the cached detail is for another head, or when GitHub had not
// computed the state yet (`unknown`, or no state at all): none of those says
// anything about this head.
//
// repo may be "owner/name" or a bare name in the client's org; the cache is
// keyed by whichever spelling the reader that stored the detail used, so
// both are tried.
func (c *Client) CachedPRMergeableState(repo string, number int, head string) (state string, ok bool) {
	head = strings.TrimSpace(head)
	if c == nil || number <= 0 || head == "" {
		return "", false
	}
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	if strings.TrimSpace(name) == "" {
		return "", false
	}
	var keys []string
	if owner != "" {
		keys = append(keys, owner+"/"+name)
	}
	if owner == "" || strings.EqualFold(owner, c.org) {
		keys = append(keys, name)
	}
	for _, key := range keys {
		pr, hit := c.cachedPRDetailAny(key, number)
		if !hit || strings.TrimSpace(pr.GetHead().GetSHA()) != head {
			continue
		}
		s := strings.ToLower(strings.TrimSpace(pr.GetMergeableState()))
		if s == "" || s == "unknown" {
			continue
		}
		return s, true
	}
	return "", false
}
