package hub

// Token usage attribution rollups for the hub fleet.
//
// WHAT THIS DOES
//
// Aggregates the per-hive TotalTokens24h figure into by-org, by-owner and
// by-cluster rollups so an operator can answer "who is burning budget?"
// without reading 42+ individual rows.
//
// WHAT THE HUB ACTUALLY KNOWS — and what it does NOT
//
// The hub receives exactly ONE token number per hive, per heartbeat:
// HeartbeatPayload.Tokens24h (pkg/hub/heartbeat.go), stored verbatim on
// RegistryEntry.TotalTokens24h (pkg/hub/server.go). That is the whole of the
// hub's token knowledge. In particular:
//
//   - NO per-model breakdown reaches the hub. The spoke computes one
//     (pkg/tokens/pricing.go + pkg/dashboard/cost.go) and even estimates USD
//     from a real list-price table, but none of that is in the heartbeat
//     payload — only the scalar total is. So this package deliberately does
//     NOT compute currency. Doing so would require inventing a blended
//     per-token price with no model attribution to justify it, which would be
//     a fabricated number wearing a dollar sign. Tokens only. See
//     usageCurrencyNote for the string the UI shows instead.
//
//   - NO per-agent breakdown reaches the hub either. AgentSummary
//     (pkg/hub/heartbeat.go) carries Name/State/Mode and no token counts.
//
// Despite the "24h" in the field name, the value the spoke sends is a
// CUMULATIVE lifetime total (cmd/hive/main.go documents this explicitly:
// "Despite the '24h'-suffixed field name this is a lifetime/window total").
// That is what makes the snapshot history below meaningful: differences
// between consecutive cumulative samples are real consumption deltas.

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	// usageSnapshotInterval is how often the fleet-total token snapshot is
	// sampled. Matches the 15-minute cadence the issue/PR sparklines already
	// use (see sparkSampleInterval in server.go) so the two trends line up on
	// the same time axis.
	usageSnapshotInterval = 15 * time.Minute

	// usageSnapshotMaxPoints bounds snapshot retention. At one sample per
	// usageSnapshotInterval this is 7 days of history (4/hr * 24 * 7 = 672),
	// the same window and the same bound as the issue/PR sparklines.
	usageSnapshotMaxPoints = 672

	// usageUnattributed is the bucket label used when a hive reports no org,
	// no owner, or no cluster. Grouping these under a visible label is
	// deliberate: silently dropping them would make the rollup shares fail to
	// sum to the fleet total, which is worse than an honest "unattributed".
	usageUnattributed = "(unattributed)"

	// usageOrgRepoSep joins the org and repo halves of an org/repo bucket key.
	// GitHub's own "owner/name" convention, so the key reads exactly like the
	// repository slug an operator would paste into a browser.
	usageOrgRepoSep = "/"
)

// usageCurrencyNote is shown wherever a reader might expect a dollar figure.
// The hub cannot derive currency: cost requires per-model token splits, and
// the heartbeat carries only a single blended total. Stated plainly rather
// than papered over with an invented rate.
const usageCurrencyNote = "Tokens only — the hub receives a single blended token total per hive, " +
	"with no per-model split, so no currency estimate is derivable here. " +
	"Per-model cost estimates exist on each hive's own dashboard (/api/cost)."

// UsageBucket is one row of a rollup: a group key plus its consumption.
type UsageBucket struct {
	// Key is the org / owner / cluster name. USER-CONTROLLED — the UI must
	// escape it with esc() before inserting it into the DOM.
	Key string `json:"key"`
	// Label is an OPTIONAL human display label for Key, resolved server-side
	// for owner buckets whose key is an opaque OIDC identity ("ibmid:5500…",
	// "google:1078…") that has a stored display name. Purely cosmetic — the
	// UI shows label||key but keys, filters and links stay on Key. Empty when
	// Key is already the best label (GitHub logins, clusters, org/repo).
	// USER-CONTROLLED (it is a provider-asserted display name) — escape it.
	Label string `json:"label,omitempty"`
	// Tokens is the summed token count for this bucket. int64 throughout:
	// a fleet total summed across 42+ hives, each clamped to 100e9, would
	// overflow a 32-bit int.
	Tokens int64 `json:"tokens"`
	// Hives is how many hives contributed to this bucket.
	Hives int `json:"hives"`
	// SharePct is this bucket's percentage of the fleet total, 0 when the
	// fleet total is zero (avoids a divide-by-zero producing NaN in JSON,
	// which would be invalid and break the UI parse).
	SharePct float64 `json:"sharePct"`
	// HiveIDs are the registry ids of the hives that contributed to this
	// bucket, so the dashboard can link a rollup row back to its hive rows
	// in the fleet table. Sorted for deterministic output. The ids are
	// hub-assigned (not user-controlled), but the UI escapes them anyway.
	HiveIDs []string `json:"hiveIds,omitempty"`
}

// UsageHive identifies a single hive in the zero-consumption list.
type UsageHive struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Org/Owner are USER-CONTROLLED — escape before rendering.
	Org   string `json:"org,omitempty"`
	Owner string `json:"owner,omitempty"`
	// OwnerName mirrors UsageBucket.Label: the resolved display name for an
	// opaque OIDC Owner identity, empty when Owner is already the best label.
	OwnerName string `json:"ownerName,omitempty"`
	// Online distinguishes a hive that is up but idle from one that is simply
	// unreachable; both consume nothing but they mean different things.
	Online bool `json:"online"`
}

// UsageSnapshot is one sampled point of fleet-total token consumption.
// Cumulative, matching the semantics of the underlying counter.
type UsageSnapshot struct {
	T int64 `json:"t"`
	V int64 `json:"v"`
}

// UsageRollup is the full attribution report.
type UsageRollup struct {
	TotalTokens int64 `json:"totalTokens"`
	HiveCount   int   `json:"hiveCount"`
	// ByOrg buckets on "org/repo" (see usageOrgRepoKey), NOT on org alone —
	// two hives in the same org need to be distinguishable. The Go field and
	// the "byOrg" JSON key are deliberately left unrenamed: the wire contract
	// is consumed by the dashboard and potentially by external readers, and
	// churning the name buys nothing that the key values do not already say.
	// The UI label changed instead.
	ByOrg     []UsageBucket `json:"byOrg"`
	ByOwner   []UsageBucket `json:"byOwner"`
	ByCluster []UsageBucket `json:"byCluster"`
	// ZeroConsumption lists claimed hives reporting no tokens. Unassigned
	// placeholders are excluded by the caller — a pool slot with nothing
	// assigned to it is SUPPOSED to consume nothing, so flagging it would be
	// noise. A claimed hive burning nothing is the interesting signal.
	ZeroConsumption []UsageHive `json:"zeroConsumption"`
	// History is the fleet-total trend. Empty until at least one snapshot has
	// been sampled; the UI must not synthesize a line from a single point.
	History []UsageSnapshot `json:"history"`
	// CurrencyNote explains the deliberate absence of dollar figures.
	CurrencyNote string `json:"currencyNote"`
	// Scope is "fleet" for an admin viewing everything, or "own" for a
	// non-admin, whose rollup covers only their own hives.
	Scope string `json:"scope"`
}

// usageGroupKey normalizes a grouping value, folding blank/whitespace-only
// values into the unattributed bucket so they are counted, not dropped.
func usageGroupKey(v string) string {
	if t := strings.TrimSpace(v); t != "" {
		return t
	}
	return usageUnattributed
}

// buildUsageBuckets groups entries by the supplied key function and returns
// buckets sorted by consumption descending. Ties break on Key ascending so
// the ordering is deterministic — without that, two equal-token buckets would
// swap positions between refreshes and the table would appear to flicker.
func buildUsageBuckets(hives []RegistryEntry, keyOf func(RegistryEntry) string, total int64) []UsageBucket {
	if keyOf == nil {
		return []UsageBucket{}
	}
	agg := map[string]*UsageBucket{}
	for _, h := range hives {
		k := usageGroupKey(keyOf(h))
		b, ok := agg[k]
		if !ok {
			b = &UsageBucket{Key: k}
			agg[k] = b
		}
		// Clamp negatives to zero. TotalTokens24h is clamped on ingest, but a
		// registry entry restored from an older on-disk file has not been
		// through that path, and one negative value would corrupt every share.
		if h.TotalTokens24h > 0 {
			b.Tokens += h.TotalTokens24h
		}
		b.Hives++
		if id := strings.TrimSpace(h.ID); id != "" {
			b.HiveIDs = append(b.HiveIDs, id)
		}
	}
	out := make([]UsageBucket, 0, len(agg))
	for _, b := range agg {
		if total > 0 {
			// float64 conversion is safe here: token counts are far below
			// 2^53, so no precision is lost in the ratio.
			b.SharePct = float64(b.Tokens) / float64(total) * 100
		}
		sort.Strings(b.HiveIDs)
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tokens != out[j].Tokens {
			return out[i].Tokens > out[j].Tokens
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// usageOrgRepoKey builds the org/repo grouping key for a hive.
//
// WHY org/repo AND NOT org ALONE: two hives in the same org are indistinguishable
// in an org-only rollup, so a heavy repo hides behind a quiet sibling. Keying on
// the slug separates them while still sorting neighbours together.
//
// WHY PrimaryRepo AND NOT the whole Repos slice: a hive may watch several repos,
// but it has exactly one primary. Joining the slice would produce a different key
// for every combination and mint one-hive buckets that never aggregate; the
// primary is the single stable identity the hive already declares.
//
// Edge cases, all deliberate:
//   - org + primary repo → "org/repo".
//   - org, no primary repo → "org" (never "org/", which reads as a truncated
//     slug and looks like a rendering bug).
//   - no org, primary repo  → "repo". Attributing it to a fabricated org would be
//     worse than a bare repo name, and dropping the hive would break the sum.
//   - neither → "" so usageGroupKey folds it into usageUnattributed, preserving
//     the existing placeholder handling (see isUnassignedPlaceholder) and keeping
//     the shares summing to the fleet total.
func usageOrgRepoKey(h RegistryEntry) string {
	org := strings.TrimSpace(h.Org)
	repo := strings.TrimSpace(h.PrimaryRepo)
	switch {
	case org != "" && repo != "":
		return org + usageOrgRepoSep + repo
	case org != "":
		return org
	default:
		// repo alone, or "" which usageGroupKey turns into usageUnattributed.
		return repo
	}
}

// isUnassignedPlaceholder mirrors the dashboard's isPlaceholderHive() helper
// (saas.go) server-side: provStatus "available" is authoritative, with an
// "available-" org prefix as the fallback for placeholders that have not yet
// reported provStatus. Kept in lockstep with the JS so the two can never
// disagree about what counts as a pool slot.
func isUnassignedPlaceholder(org, provStatus string) bool {
	if strings.EqualFold(strings.TrimSpace(provStatus), "available") {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(org), "available-")
}

// buildUsageRollup computes the full attribution report over the supplied
// hives. placeholderOf reports whether a hive is an unassigned pool slot; a
// nil func treats every hive as claimed.
//
// Callers must pass an already-authorization-scoped slice: this function does
// no filtering of its own beyond placeholders.
func buildUsageRollup(hives []RegistryEntry, placeholderOf func(RegistryEntry) bool, history []UsageSnapshot, scope string) UsageRollup {
	out := UsageRollup{
		ByOrg:           []UsageBucket{},
		ByOwner:         []UsageBucket{},
		ByCluster:       []UsageBucket{},
		ZeroConsumption: []UsageHive{},
		History:         []UsageSnapshot{},
		CurrencyNote:    usageCurrencyNote,
		Scope:           scope,
	}
	if history != nil {
		out.History = history
	}

	// Placeholders are excluded from every rollup, not just the
	// zero-consumption list: counting empty pool slots as hives would dilute
	// every share and inflate the hive counts with slots nobody owns.
	claimed := make([]RegistryEntry, 0, len(hives))
	for _, h := range hives {
		if placeholderOf != nil && placeholderOf(h) {
			continue
		}
		claimed = append(claimed, h)
	}

	for _, h := range claimed {
		if h.TotalTokens24h > 0 {
			out.TotalTokens += h.TotalTokens24h
		}
	}
	out.HiveCount = len(claimed)

	out.ByOrg = buildUsageBuckets(claimed, usageOrgRepoKey, out.TotalTokens)
	out.ByOwner = buildUsageBuckets(claimed, func(h RegistryEntry) string { return h.Owner }, out.TotalTokens)
	out.ByCluster = buildUsageBuckets(claimed, func(h RegistryEntry) string {
		// Prefer the human-readable cluster name, fall back to the ID.
		if n := strings.TrimSpace(h.ClusterName); n != "" {
			return n
		}
		return h.ClusterID
	}, out.TotalTokens)

	for _, h := range claimed {
		if h.TotalTokens24h > 0 {
			continue
		}
		out.ZeroConsumption = append(out.ZeroConsumption, UsageHive{
			ID:     h.ID,
			Name:   h.Name,
			Org:    h.Org,
			Owner:  h.Owner,
			Online: h.Online,
		})
	}
	// Deterministic order for the zero list too.
	sort.Slice(out.ZeroConsumption, func(i, j int) bool {
		return out.ZeroConsumption[i].Name < out.ZeroConsumption[j].Name
	})

	return out
}

// appendUsageSnapshot appends a fleet-total sample if usageSnapshotInterval has
// elapsed since the last one, and trims to usageSnapshotMaxPoints. Returns the
// (possibly unchanged) history.
//
// Rate-limiting on append rather than on a timer means the history is driven by
// heartbeat traffic, exactly like the issue/PR sparklines — no extra goroutine,
// and nothing to leak.
func appendUsageSnapshot(history []UsageSnapshot, total int64, now time.Time) []UsageSnapshot {
	ts := now.Unix()
	if n := len(history); n > 0 {
		if ts-history[n-1].T < int64(usageSnapshotInterval/time.Second) {
			return history
		}
	}
	history = append(history, UsageSnapshot{T: ts, V: total})
	if len(history) > usageSnapshotMaxPoints {
		history = history[len(history)-usageSnapshotMaxPoints:]
	}
	return history
}

// sampleUsageHistory records a fleet-total snapshot if the sampling interval
// has elapsed.
//
// LOCK ORDER: takes s.mu.RLock (to read the registry) and then s.usageMu.
// Callers must NOT already hold s.mu — it is a non-reentrant sync.RWMutex and
// re-locking it deadlocks. The heartbeat path calls this only after unlocking.
func (s *HubServer) sampleUsageHistory(now time.Time) {
	s.mu.RLock()
	var total int64
	for _, h := range s.registry.Hives {
		if isUnassignedPlaceholder(h.Org, "") {
			continue
		}
		if h.TotalTokens24h > 0 {
			total += h.TotalTokens24h
		}
	}
	s.mu.RUnlock()

	s.usageMu.Lock()
	s.usageHistory = appendUsageSnapshot(s.usageHistory, total, now)
	s.usageMu.Unlock()
}

// usageHistorySnapshot returns a copy of the sampled history. A copy, not the
// slice itself, so a caller ranging over it cannot race a concurrent append.
func (s *HubServer) usageHistorySnapshot() []UsageSnapshot {
	s.usageMu.RLock()
	defer s.usageMu.RUnlock()
	out := make([]UsageSnapshot, len(s.usageHistory))
	copy(out, s.usageHistory)
	return out
}

// handleUsage serves GET /api/saas/usage — the token attribution rollup.
//
// AUTHORIZATION: fleet-wide usage is admin-only. A non-admin sees a rollup
// computed over ONLY the hives they own, and gets scope "own" so the UI can
// label it honestly. This reuses the existing getAuthUser + hubAdminUsername
// rule rather than inventing a new one. Registered behind requireAuth, so an
// unauthenticated caller never reaches this handler at all.
func (s *HubServer) handleUsage(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	isAdmin := isHubAdmin(username)

	s.mu.RLock()
	scoped := make([]RegistryEntry, 0, len(s.registry.Hives))
	for _, h := range s.registry.Hives {
		if isAdmin || canonicalEqual(h.Owner, username) {
			scoped = append(scoped, h)
		}
	}
	s.mu.RUnlock()

	scope := "own"
	// History is a FLEET total, so it is only meaningful — and only
	// authorized — for an admin. A non-admin gets no history rather than a
	// fleet-wide line they are not entitled to see.
	var history []UsageSnapshot
	if isAdmin {
		scope = "fleet"
		history = s.usageHistorySnapshot()
	}

	rollup := buildUsageRollup(scoped, func(h RegistryEntry) bool {
		return isUnassignedPlaceholder(h.Org, "")
	}, history, scope)

	// Cosmetic decoration: resolve opaque OIDC owner identities
	// ("ibmid:5500…") to their stored display names so the By-owner table and
	// zero-consumption chips read like people, not subjects. Keys stay raw —
	// they are what links/filters match on.
	label := s.identityLabeler()
	for i := range rollup.ByOwner {
		if l := label(rollup.ByOwner[i].Key); l != rollup.ByOwner[i].Key {
			rollup.ByOwner[i].Label = l
		}
	}
	for i := range rollup.ZeroConsumption {
		if l := label(rollup.ZeroConsumption[i].Owner); l != rollup.ZeroConsumption[i].Owner {
			rollup.ZeroConsumption[i].OwnerName = l
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rollup)
}
