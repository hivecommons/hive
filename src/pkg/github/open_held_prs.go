package github

import (
	"fmt"
	"strings"
)

// RepoAtCapacityLine is the exact line PR-opening agents see in their kick,
// and hive-open-pr refuses with, once a repo holds as many open, still-held
// hive PRs as its repo_policies[].max_open_held_prs cap
// (hivecommons/hive#13645).
const RepoAtCapacityLine = "repo at capacity: review or close existing PRs first"

// OpenHeldPRs is one repo's open-held-PR cap as StampOpenHeldPRs last saw
// it: Count open PRs the hive App authored that still carry a hold, against
// a cap of Max.
type OpenHeldPRs struct {
	Count int `json:"count"`
	Max   int `json:"max"`
}

// AtCapacity reports whether the repo may open no further PRs.
func (o OpenHeldPRs) AtCapacity() bool {
	return o.Max > 0 && o.Count >= o.Max
}

// openHeldPRState is the client's per-repo cap state. Guarded by
// Client.openHeldMu.
type openHeldPRState struct {
	limit func(repo string) int
	// counts are the open held hive PRs per lower-cased owner/repo.
	counts map[string]int
}

// SetMaxOpenHeldPRsFunc installs the per-repo cap resolver
// (config.RepoMaxOpenHeldPRs). nil, or a resolver returning 0 for a repo,
// means that repo has no cap.
func (c *Client) SetMaxOpenHeldPRsFunc(fn func(repo string) int) {
	if c == nil {
		return
	}
	c.openHeldMu.Lock()
	defer c.openHeldMu.Unlock()
	c.openHeld.limit = fn
}

func (c *Client) openHeldKey(repo string) string {
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	if owner == "" {
		return strings.ToLower(name)
	}
	return strings.ToLower(owner + "/" + name)
}

// countOpenHeldHivePRs counts, per repo key, the held PRs in items that the
// hive App authored. Human and other bots' PRs never count; a PR whose hold
// was removed (a person, or the #13606 test/docs auto-release) has left the
// held list and so no longer counts.
func (c *Client) countOpenHeldHivePRs(items []HoldItem) map[string]int {
	counts := map[string]int{}
	if c.appBotLogin == "" {
		return counts
	}
	for _, it := range items {
		if it.Type != "pr" || !strings.EqualFold(strings.TrimSpace(it.Author), c.appBotLogin) {
			continue
		}
		counts[c.openHeldKey(it.Repo)]++
	}
	return counts
}

// StampOpenHeldPRs recounts the open held hive PRs per repo from
// actionable's hold list, keeps the counts for the hive-open-pr refusal, and
// records each capped repo's count on actionable.OpenHeldPRs for the kick.
// Repos without a cap are left out, so an unconfigured hive stamps nothing.
func (c *Client) StampOpenHeldPRs(actionable *ActionableResult) {
	if c == nil || actionable == nil {
		return
	}
	counts := c.countOpenHeldHivePRs(actionable.Hold.Items)
	c.openHeldMu.Lock()
	c.openHeld.counts = counts
	limit := c.openHeld.limit
	c.openHeldMu.Unlock()
	if limit == nil {
		actionable.OpenHeldPRs = nil
		return
	}
	stamped := map[string]OpenHeldPRs{}
	for key, n := range counts {
		if limitN := limit(key); limitN > 0 {
			stamped[key] = OpenHeldPRs{Count: n, Max: limitN}
		}
	}
	if len(stamped) == 0 {
		actionable.OpenHeldPRs = nil
		return
	}
	actionable.OpenHeldPRs = stamped
}

// OpenHeldPRsFor returns repo's entry (owner/repo or bare name) from a
// stamped OpenHeldPRs map.
func OpenHeldPRsFor(stamped map[string]OpenHeldPRs, repo string) (OpenHeldPRs, bool) {
	if len(stamped) == 0 {
		return OpenHeldPRs{}, false
	}
	if o, ok := stamped[strings.ToLower(repo)]; ok {
		return o, true
	}
	for key, o := range stamped {
		if repoRefsMatch(key, repo) {
			return o, true
		}
	}
	return OpenHeldPRs{}, false
}

// openHeldPRCapRefusal returns the reason a PR-open request for repo is
// refused because the repo is at its max_open_held_prs cap, or "" when the
// repo has no cap or is below it. Issue filing is a different relay and is
// never affected.
func (c *Client) openHeldPRCapRefusal(repo string) string {
	c.openHeldMu.Lock()
	defer c.openHeldMu.Unlock()
	if c.openHeld.limit == nil {
		return ""
	}
	key := c.openHeldKey(repo)
	limitN := c.openHeld.limit(key)
	if limitN <= 0 {
		return ""
	}
	n := c.openHeld.counts[key]
	if n < limitN {
		return ""
	}
	return fmt.Sprintf("%s (%s has %d open held hive PRs; max_open_held_prs is %d)", RepoAtCapacityLine, key, n, limitN)
}

// noteOpenHeldPROpened counts a held PR the relay just opened in repo, so
// requests arriving before the next stamp cannot overshoot the cap.
func (c *Client) noteOpenHeldPROpened(repo string) {
	c.openHeldMu.Lock()
	defer c.openHeldMu.Unlock()
	if c.openHeld.counts == nil {
		c.openHeld.counts = map[string]int{}
	}
	c.openHeld.counts[c.openHeldKey(repo)]++
}
