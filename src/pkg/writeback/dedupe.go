package writeback

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Content-aware suppression. The policy gate decides WHETHER a kind may
// write to an item; this layer decides whether this exact write is worth
// making: the same comment on the same item again (re-post loops such as
// the selfreport on hive#15021), the same comment on several items within
// seconds (the "auto-promoted for contributor-lane merging" notice posted
// on kubestellar/kubestellar#3925 and #3928 6 s apart), or more of one
// kind on one repo in an hour than any human would read. It applies only
// to comment kinds (ContentDedupeKinds) and only when the caller supplied
// the body as preview — labels and closes have no body and stay policy-only.

const (
	// DefaultSameItemTTL is how long an identical body on the same item is
	// remembered (writeback.dedupe.same_item_ttl).
	DefaultSameItemTTL = 24 * time.Hour
	// DefaultBurstItems is the number of distinct items that must receive
	// one body inside DefaultBurstWindow before the rest are held
	// (writeback.dedupe.burst_items); 2 holds the second item.
	DefaultBurstItems = 2
	// DefaultBurstWindow is the cross-item burst window
	// (writeback.dedupe.burst_window).
	DefaultBurstWindow = 60 * time.Second
	// DefaultHourlyCap is the per-repo, per-kind comments-per-hour ceiling
	// (writeback.dedupe.hourly_cap); RateCapExemptKinds are never capped.
	DefaultHourlyCap = 60
	// maxDedupeEntries bounds the persisted same-item table; the oldest
	// entries go first.
	maxDedupeEntries = 5000
	// rateCapWindow is the sliding window of the hourly cap.
	rateCapWindow = time.Hour
	hashLen       = 16
)

// Content reasons.
const (
	// ReasonDuplicate: the same body (modulo marker args, SHAs, timestamps
	// and durations) was already written to this item within same_item_ttl.
	ReasonDuplicate = "duplicate"
	// ReasonBurst: the same body (modulo item numbers, mentions and digits)
	// was just written to another item on this repo within burst_window.
	ReasonBurst = "burst"
	// ReasonRateCap: this repo already received hourly_cap writes of this
	// kind in the last hour.
	ReasonRateCap = "rate-capped"
)

// IsContentReason reports whether reason is one the content layer emits
// (ReasonDuplicate, ReasonBurst, ReasonRateCap) rather than policy. Callers
// that treat a policy refusal as "the operator changed their mind" should
// treat these as "try again later" instead: the policy still allows the
// write, the gate only declined this particular instance of it.
func IsContentReason(reason string) bool {
	switch reason {
	case ReasonDuplicate, ReasonBurst, ReasonRateCap:
		return true
	}
	return false
}

// ContentDedupeKinds are the kinds whose preview is a comment body the
// content layer may dedupe. Kinds that re-run their gate check every tick
// with a constant preview and keep their own idempotence (hold_gate_notice,
// the closes, parent_issue_edit, ci_rerun), the label kinds and issue_file
// (no existing item) are deliberately absent.
var ContentDedupeKinds = map[Kind]bool{
	KindSentinel:             true,
	KindNeedsDecisionComment: true,
	KindSupersessionNotice:   true,
	KindDuplicateFixNotice:   true,
	KindReporterTrustHold:    true,
	KindContributorAdopt:     true,
	KindClaimComment:         true,
	KindGateComment:          true,
	KindWelcomeComment:       true,
}

// RateCapExemptKinds are never held by the hourly cap: everything on the
// merge path (verdict labels, decision replies, adoption, hold-gate upkeep,
// CI re-runs, close-on-merge) plus every label kind, where a missed write
// stalls a merge rather than annoys a reader.
var RateCapExemptKinds = map[Kind]bool{
	KindHoldGateNotice:       true,
	KindStageLabel:           true,
	KindCoveredByPRLabel:     true,
	KindVerdictLabel:         true,
	KindNeedsDecisionComment: true,
	KindContributorAdopt:     true,
	KindCIRerun:              true,
	KindIssueClose:           true,
	KindSupersessionClose:    true,
	KindIssueFile:            true,
}

// ContentDeduped reports whether the content layer applies to kind.
func (k Kind) ContentDeduped() bool { return ContentDedupeKinds[k] }

// RateCapExempt reports whether the hourly cap skips kind.
func (k Kind) RateCapExempt() bool { return RateCapExemptKinds[k] }

var (
	reMarkerArgs = regexp.MustCompile(`<!--\s*(hive:[A-Za-z0-9_.-]+)[^>]*-->`)
	reSHA        = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	reRFC3339    = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:?\d{2})?\b`)
	reDuration   = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h|d)(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h|d))*\b`)
	reSpace      = regexp.MustCompile(`\s+`)
	reItemRef    = regexp.MustCompile(`(?:\b[\w.-]+/[\w.-]+)?#\d+`)
	reMention    = regexp.MustCompile(`@[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?\b`)
	reDigits     = regexp.MustCompile(`\d+`)
)

// normalizeSameItem strips the parts of a body that legitimately change
// between two posts of the same message: HTML-comment marker arguments
// (keeping the marker name), SHAs, timestamps, durations and whitespace.
func normalizeSameItem(body string) string {
	if key, ok := DecisionKeyFromBody(body); ok && key != "" {
		// A decision question is the same question however a model
		// reworded it (hivecommons/hive#15499): its identity is the PR it
		// names as redundant, or its normalised words.
		return "decision:" + key
	}
	s := reMarkerArgs.ReplaceAllString(body, "<!-- $1 -->")
	s = reRFC3339.ReplaceAllString(s, "<ts>")
	s = reSHA.ReplaceAllString(s, "<sha>")
	s = reDuration.ReplaceAllString(s, "<dur>")
	s = reSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// normalizeBurst additionally strips item references, mentions and digits
// so the same notice addressed to different items hashes alike.
func normalizeBurst(body string) string {
	s := normalizeSameItem(body)
	s = reItemRef.ReplaceAllString(s, "<item>")
	s = reMention.ReplaceAllString(s, "<user>")
	s = reDigits.ReplaceAllString(s, "<n>")
	s = reSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func contentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:hashLen]
}

// SameItemHash is the same-item dedupe key component for body.
func SameItemHash(body string) string { return contentHash(normalizeSameItem(body)) }

// BurstHash is the cross-item burst key component for body.
func BurstHash(body string) string { return contentHash(normalizeBurst(body)) }

// burstHit is one item that received a body.
type burstHit struct {
	Number int       `json:"number"`
	At     time.Time `json:"at"`
}

// dedupeState is the content layer's table, persisted beside the repo
// records so a restart does not re-fire a notice the previous process
// already sent.
type dedupeState struct {
	// SameItem maps repo|number|kind|hash to the last time it was written.
	SameItem map[string]time.Time `json:"same_item,omitempty"`
	// Burst maps repo|kind|hash to the items that received it.
	Burst map[string][]burstHit `json:"burst,omitempty"`
	// Hourly maps repo|kind to the times of allowed writes.
	Hourly map[string][]time.Time `json:"hourly,omitempty"`
	// warned remembers bursts already logged (one WARN per burst).
	warned map[string]bool
}

func newDedupeState() *dedupeState {
	return &dedupeState{SameItem: map[string]time.Time{}, Burst: map[string][]burstHit{}, Hourly: map[string][]time.Time{}, warned: map[string]bool{}}
}

func (s *dedupeState) init() {
	if s.SameItem == nil {
		s.SameItem = map[string]time.Time{}
	}
	if s.Burst == nil {
		s.Burst = map[string][]burstHit{}
	}
	if s.Hourly == nil {
		s.Hourly = map[string][]time.Time{}
	}
	if s.warned == nil {
		s.warned = map[string]bool{}
	}
}

func (s *dedupeState) empty() bool {
	return len(s.SameItem) == 0 && len(s.Burst) == 0 && len(s.Hourly) == 0
}

func sameItemKey(repo string, number int, kind Kind, hash string) string {
	return fmt.Sprintf("%s|%d|%s|%s", repoKey(repo), number, kind, hash)
}

func burstKey(repo string, kind Kind, hash string) string {
	return fmt.Sprintf("%s|%s|%s", repoKey(repo), kind, hash)
}

func hourlyKey(repo string, kind Kind) string {
	return repoKey(repo) + "|" + string(kind)
}

// contentCheckLocked is the content layer; it runs only after the policy
// gate allowed the write, and only for comment kinds with a body. It
// returns the (possibly refusing) decision and records an allowed write.
func (g *Gate) contentCheckLocked(repo string, item Item, kind Kind, preview string, policy Decision) Decision {
	if preview == "" || !kind.ContentDeduped() {
		return policy
	}
	cfg := g.cfgLocked().Dedupe
	if !cfg.EnabledOrDefault() {
		return policy
	}
	now := g.deps.Now().UTC()
	st := g.dedupe
	st.init()
	g.pruneDedupeLocked(now)

	sameKey := sameItemKey(repo, item.Number, kind, SameItemHash(preview))
	if at, ok := st.SameItem[sameKey]; ok && now.Sub(at) < cfg.SameItemTTLOrDefault() {
		return Decision{Reason: ReasonDuplicate, Detail: fmt.Sprintf("identical %s already written to #%d at %s", kind, item.Number, at.Format(time.RFC3339))}
	}

	bKey := burstKey(repo, kind, BurstHash(preview))
	window := cfg.BurstWindowOrDefault()
	var others []burstHit
	for _, h := range st.Burst[bKey] {
		if h.Number != item.Number && now.Sub(h.At) < window {
			others = append(others, h)
		}
	}
	if need := cfg.BurstItemsOrDefault(); need > 0 && len(others) >= need-1 {
		first := others[0]
		for _, h := range others[1:] {
			if h.At.Before(first.At) {
				first = h
			}
		}
		if !st.warned[bKey] {
			st.warned[bKey] = true
			g.deps.Logger.Warn("writeback: notification burst held", "repo", repo, "kind", string(kind), "count", len(others)+1, "first", first.Number, "window", window.String())
		}
		return Decision{Reason: ReasonBurst, Detail: fmt.Sprintf("same %s already sent to #%d within %s; held to avoid a notification burst", kind, first.Number, window)}
	}

	hKey := hourlyKey(repo, kind)
	if limit := cfg.HourlyCapOrDefault(); limit > 0 && !kind.RateCapExempt() {
		n := 0
		for _, t := range st.Hourly[hKey] {
			if now.Sub(t) < rateCapWindow {
				n++
			}
		}
		if n >= limit {
			return Decision{Reason: ReasonRateCap, Detail: fmt.Sprintf("%d %s writes on %s in the last hour (writeback.dedupe.hourly_cap=%d)", n, kind, repo, limit)}
		}
	}

	st.SameItem[sameKey] = now
	st.Burst[bKey] = append(st.Burst[bKey], burstHit{Number: item.Number, At: now})
	st.Hourly[hKey] = append(st.Hourly[hKey], now)
	g.saveLocked()
	return policy
}

// pruneDedupeLocked drops expired entries and bounds the same-item table.
func (g *Gate) pruneDedupeLocked(now time.Time) {
	st := g.dedupe
	if st == nil {
		return
	}
	st.init()
	cfg := g.cfgLocked().Dedupe
	ttl := cfg.SameItemTTLOrDefault()
	for k, at := range st.SameItem {
		if now.Sub(at) >= ttl {
			delete(st.SameItem, k)
		}
	}
	if len(st.SameItem) > maxDedupeEntries {
		type kv struct {
			k  string
			at time.Time
		}
		all := make([]kv, 0, len(st.SameItem))
		for k, at := range st.SameItem {
			all = append(all, kv{k, at})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
		for _, e := range all[:len(all)-maxDedupeEntries] {
			delete(st.SameItem, e.k)
		}
	}
	window := cfg.BurstWindowOrDefault()
	for k, hits := range st.Burst {
		kept := hits[:0]
		for _, h := range hits {
			if now.Sub(h.At) < window {
				kept = append(kept, h)
			}
		}
		if len(kept) == 0 {
			delete(st.Burst, k)
			delete(st.warned, k)
			continue
		}
		st.Burst[k] = kept
	}
	for k, ts := range st.Hourly {
		kept := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < rateCapWindow {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(st.Hourly, k)
			continue
		}
		st.Hourly[k] = kept
	}
}

// DedupeCounters is the content-suppression block of Status (last hour).
type DedupeCounters struct {
	Enabled      bool `json:"enabled"`
	SameItem     int  `json:"dedupe_same_item"`
	Burst        int  `json:"dedupe_burst"`
	RateCapped   int  `json:"rate_capped"`
	HourlyCap    int  `json:"hourly_cap"`
	BurstItems   int  `json:"burst_items"`
	BurstWindowS int  `json:"burst_window_s"`
	SameItemTTLS int  `json:"same_item_ttl_s"`
}

func (g *Gate) dedupeCountersLocked(now time.Time) DedupeCounters {
	cfg := g.cfgLocked().Dedupe
	c := DedupeCounters{
		Enabled:      cfg.EnabledOrDefault(),
		HourlyCap:    cfg.HourlyCapOrDefault(),
		BurstItems:   cfg.BurstItemsOrDefault(),
		BurstWindowS: int(cfg.BurstWindowOrDefault() / time.Second),
		SameItemTTLS: int(cfg.SameItemTTLOrDefault() / time.Second),
	}
	cut := now.Add(-rateCapWindow)
	for _, s := range g.recent {
		if s.At.Before(cut) {
			continue
		}
		switch s.Reason {
		case ReasonDuplicate:
			c.SameItem++
		case ReasonBurst:
			c.Burst++
		case ReasonRateCap:
			c.RateCapped++
		}
	}
	return c
}
