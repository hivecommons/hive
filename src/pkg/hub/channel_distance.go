package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
)

// ============================================================================
// CHANNEL DISTANCE — "how much is queued to be promoted into this channel?"
// ============================================================================
//
// The channel rows say WHICH build each track points at, but not how far apart
// the tracks have drifted. "stable -> 55bd2bc" and "candidate -> 9eefa3a" are
// two opaque SHAs; whether stable is one commit behind or two hundred is the
// question an operator actually has, and answering it meant leaving the
// dashboard for a GitHub compare view.
//
// WHAT EACH ROW IS MEASURED AGAINST
//
// Each channel is compared to the stage IMMEDIATELY upstream of it in the
// promotion order (releaseChannels is most-stable-first, so upstream is the
// next element): stable is measured against candidate, candidate against edge.
// Edge has no upstream — it is where builds enter — so it carries no distance.
//
// Measuring stable against EDGE instead was the obvious alternative and is
// worse on both counts: the number would be roughly the sum of the two hops,
// so it restates what candidate's own row already says, and it hides WHICH
// hop the backlog is stuck at — a large stable→edge gap reads identically
// whether soak is starved or promotion is stalled. One hop per row keeps every
// number actionable and makes the column comparable down the list.
//
// WHY BOTH DIRECTIONS
//
// A single signed number would be a lie whenever the tracks have diverged, and
// they routinely do: channels follow different branches (v4 and v5 today), and
// a branch that is synced from another carries commits the other has not got
// while also missing commits merged since the last sync. GitHub's compare API
// reports that case as "diverged" with BOTH counts non-zero, and the UI shows
// both rather than collapsing them into a direction that does not exist.
//
// WHY THE CACHE IS PERMANENT
//
// Distance between two FIXED commits is immutable — the same property
// commitOrderCache relies on. Channels move, but a moved channel is a new SHA
// pair and therefore a new key, so entries are never stale; they only become
// unreferenced. That makes a bounded permanent cache correct here, with no TTL
// to tune and no window in which the dashboard shows a distance that has since
// changed.

// channelDistanceCacheMax bounds the resolved-distance cache. Each entry is a
// pair of short SHAs and two ints, so the ceiling exists to stop unbounded
// growth across many channel moves, not because entries are expensive. Dropped
// wholesale on overflow, matching commitOrderCacheMax next door: the entries
// are cheap to re-derive and an LRU would add moving parts for no gain.
const channelDistanceCacheMax = 512

// channelDistanceKey identifies one immutable distance question: how does head
// stand relative to base. Short SHAs, so the same commit written long and
// short shares one entry.
type channelDistanceKey struct {
	base string
	head string
}

// channelDistance is the answer for one key.
type channelDistance struct {
	// Status is GitHub's compare status: "identical", "ahead", "behind" or
	// "diverged". Kept verbatim rather than derived from the counts, so an
	// "identical" row is distinguishable from a pair that simply resolved to
	// 0/0 because the compare failed.
	Status string
	// Ahead is how many commits head has that base does not.
	Ahead int
	// Behind is how many commits base has that head does not.
	Behind int
}

var (
	channelDistanceMu    sync.RWMutex
	channelDistanceCache = map[channelDistanceKey]channelDistance{}
)

// fetchCommitCompareCounts asks GitHub how head stands relative to base and
// returns the status plus both commit counts.
//
// Deliberately separate from fetchCommitCompareStatus rather than a widening
// of it: that hook is stubbed by the upgrade-completion tests, and changing
// its signature would rewrite unrelated tests to serve a dashboard feature.
// Both read the same endpoint; only the decoded fields differ.
//
// A var so tests can supply distances without a network round-trip.
var fetchCommitCompareCounts = func(base, head string, logger *slog.Logger) (channelDistance, error) {
	client := &http.Client{Timeout: commitCompareTimeout}
	compareURL := fmt.Sprintf("%s/repos/hivecommons/hive/compare/%s...%s",
		githubAPIBase, url.PathEscape(base), url.PathEscape(head))
	req, err := http.NewRequest(http.MethodGet, compareURL, nil)
	if err != nil {
		return channelDistance{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return channelDistance{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return channelDistance{}, fmt.Errorf("compare %s...%s: HTTP %d", base, head, resp.StatusCode)
	}
	var result struct {
		Status   string `json:"status"`
		AheadBy  int    `json:"ahead_by"`
		BehindBy int    `json:"behind_by"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return channelDistance{}, err
	}
	if result.Status == "" {
		return channelDistance{}, fmt.Errorf("compare %s...%s: empty status", base, head)
	}
	return channelDistance{Status: result.Status, Ahead: result.AheadBy, Behind: result.BehindBy}, nil
}

// channelUpstream returns the channel one stage ahead of ch in the promotion
// order — the track whose builds flow INTO ch — or "" for the newest channel,
// which has no upstream.
//
// Derived from releaseChannels rather than hardcoded, so adding a track to
// that list wires its distance row automatically and cannot leave a stale
// mapping behind.
func channelUpstream(ch string) string {
	for i, c := range releaseChannels {
		if c == ch && i+1 < len(releaseChannels) {
			return releaseChannels[i+1]
		}
	}
	return ""
}

// resolveChannelDistance returns how head stands relative to base, serving the
// permanent cache when it can and otherwise fetching once.
//
// Unlike commitAtOrAheadOfTarget this resolves INLINE. That is safe here and
// not there: this runs only on the channel-target refresh, which already makes
// several synchronous registry round-trips behind the same 5-minute TTL and
// holds no server lock, whereas commitAtOrAheadOfTarget is called under s.mu
// on the heartbeat path.
//
// A failure returns the zero value, which renders as no distance at all — the
// row keeps its SHA and simply says nothing about drift. Showing "0 behind"
// for an unresolved compare would assert the tracks are level, which is the
// one answer that must never be guessed.
func resolveChannelDistance(base, head string, logger *slog.Logger) channelDistance {
	if base == "" || head == "" {
		return channelDistance{}
	}
	if sameCommit(base, head) {
		return channelDistance{Status: "identical"}
	}
	key := channelDistanceKey{base: shortSHA(base), head: shortSHA(head)}

	channelDistanceMu.RLock()
	if d, ok := channelDistanceCache[key]; ok {
		channelDistanceMu.RUnlock()
		return d
	}
	channelDistanceMu.RUnlock()

	d, err := fetchCommitCompareCounts(base, head, logger)
	if err != nil {
		if logger != nil {
			logger.Warn("channel distance: compare failed — the row will render without a distance",
				"base", base, "head", head, "error", err)
		}
		return channelDistance{}
	}

	channelDistanceMu.Lock()
	if len(channelDistanceCache) >= channelDistanceCacheMax {
		channelDistanceCache = map[channelDistanceKey]channelDistance{}
	}
	channelDistanceCache[key] = d
	channelDistanceMu.Unlock()
	return d
}

// annotateChannelDistances fills each target's distance to its upstream
// channel, in place.
//
// Targets with no SHA are skipped: a channel that did not resolve has nothing
// to compare, and a distance against "" would be a fabricated answer. So is a
// pair whose upstream did not resolve — the row renders its SHA alone rather
// than claiming the tracks are level.
func annotateChannelDistances(targets []ChannelTarget, logger *slog.Logger) {
	shaByChannel := make(map[string]string, len(targets))
	for _, t := range targets {
		if t.SHA != "" {
			shaByChannel[t.Channel] = t.SHA
		}
	}
	for i := range targets {
		up := channelUpstream(targets[i].Channel)
		if up == "" {
			continue
		}
		upSHA, ok := shaByChannel[up]
		if !ok || targets[i].SHA == "" {
			continue
		}
		// base = upstream, head = this channel, so Ahead/Behind read from THIS
		// row's point of view: "behind" is what is waiting to be promoted in.
		d := resolveChannelDistance(upSHA, targets[i].SHA, logger)
		if d.Status == "" {
			continue
		}
		targets[i].CompareTo = up
		targets[i].CompareStatus = d.Status
		targets[i].Ahead = d.Ahead
		targets[i].Behind = d.Behind
	}
}
