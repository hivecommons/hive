package collect

// Launch-cost join (#12128): places the token collector's per-message usage
// timeline against the kick path's LaunchRecords, so spend can be read per
// issue, per PR, and per head SHA — the attribution Bob's token review asked
// for (RFC #12134). It reuses the repo-cost machinery wholesale: the same
// summary, the same window, the same pricing (tokens.EstimateCostUSD), the
// same nil-vs-zero USD discipline, and the same two mandatory buckets so the
// invariant Σ(launches) + unattributed + backend_unsupported == total holds.
//
// The join differs from repo-cost in one deliberate way: a launch is an OPENING
// event (we know what the agent was asked to do before it starts spending), so
// usage is owned by the most recent launch for that agent, from launch time
// until the next launch or LaunchOwnershipMax — whichever comes first. Usage
// before an agent's first retained launch, or past the ownership horizon, is
// unattributed rather than guessed.
//
// Nothing here gates: this is a read-only view over the existing collector.

import (
	"sort"
	"strconv"
	"time"

	"github.com/hivecommons/hive/pkg/tokens"
)

// LaunchOwnershipMax caps how long a single launch keeps owning an agent's
// usage when no later launch arrives. Agent turns are minutes to a couple of
// hours; a multi-day gap means the agent was restarted or kicked by a path
// that does not record launches (manual send-kick, startup), and billing that
// to a stale ref would be exactly the guesswork #4836 forbids.
const LaunchOwnershipMax = 6 * time.Hour

// LaunchCostEntry is the spend attributed to one delivered kick.
type LaunchCostEntry struct {
	Agent      string             `json:"agent"`
	Trigger    string             `json:"trigger"`
	Repo       string             `json:"repo,omitempty"`
	LaunchedAt string             `json:"launched_at"`
	Refs       []tokens.LaunchRef `json:"refs"`
	// USD is nil when no usage landed on the launch (nil ≠ 0).
	USD         *float64 `json:"usd"`
	Source      string   `json:"source"`
	Input       int64    `json:"input"`
	Output      int64    `json:"output"`
	CacheRead   int64    `json:"cache_read"`
	CacheCreate int64    `json:"cache_create"`
	Tokens      int64    `json:"tokens"`
	// Events is how many usage events landed on this launch.
	Events int `json:"events"`
}

// RefCostEntry aggregates every launch that named one work ref.
type RefCostEntry struct {
	Ref    string `json:"ref"`
	Kind   string `json:"kind"`
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`
	// Launches is the repeat-spend counter: how many kicks named this ref.
	Launches int      `json:"launches"`
	Agents   []string `json:"agents,omitempty"`
	// HeadSHAs lists the distinct PR heads launched against (PR refs only).
	HeadSHAs        []string `json:"head_shas,omitempty"`
	FirstLaunchedAt string   `json:"first_launched_at"`
	LastLaunchedAt  string   `json:"last_launched_at"`
	// Share* split each launch's spend evenly across the refs it named. A
	// review kick names one PR so its share is exact; a governor kick naming
	// twenty issues gives each a twentieth. LaunchTokens/LaunchUSD are the
	// undivided totals of every launch that named the ref — the upper bound.
	ShareTokens  int64    `json:"share_tokens"`
	ShareUSD     *float64 `json:"share_usd"`
	LaunchTokens int64    `json:"launch_tokens"`
	LaunchUSD    *float64 `json:"launch_usd"`
	Source       string   `json:"source"`
}

// LaunchCostResponse is the /api/launch-cost payload.
type LaunchCostResponse struct {
	Ready bool   `json:"ready"`
	Phase string `json:"phase"`

	ByLaunch           []LaunchCostEntry `json:"by_launch"`
	ByRef              []RefCostEntry    `json:"by_ref"`
	Unattributed       RepoCostEntry     `json:"unattributed"`
	BackendUnsupported RepoCostEntry     `json:"backend_unsupported"`

	TotalUSD         float64 `json:"total_usd"`
	AttributedTokens int64   `json:"attributed_tokens"`
	TotalTokens      int64   `json:"total_tokens"`

	// LaunchesRetained is how many launch records were available to the join;
	// 0 with Ready=true means the kick path has not recorded anything yet.
	LaunchesRetained int `json:"launches_retained"`
	WindowHours      int `json:"window_hours"`
	OwnershipHours   int `json:"ownership_hours"`

	PriceTableDate string    `json:"price_table_date"`
	Disclaimer     string    `json:"disclaimer"`
	Limitations    []string  `json:"limitations"`
	CollectedAt    time.Time `json:"collected_at,omitempty"`
}

// LaunchCostLimitations is the honesty block that travels with the payload.
func LaunchCostLimitations() []string {
	return []string{
		"Attribution only: nothing here gates or budgets kicks. The governor window budget remains the single enforced budget.",
		"Usage is owned by the most recent recorded launch for the same agent until the next launch or the ownership horizon; usage before the first retained launch or past the horizon is unattributed.",
		"Only governor and review-swarm kicks record launches; manual send-kick and startup prompts do not, so their spend is unattributed.",
		"share_* splits a launch's spend evenly across every ref it named; launch_* is the undivided total of every launch naming the ref.",
		"Head SHAs are known only for review-swarm PR launches.",
		"Backends without a per-message usage timeline contribute whole-session totals to backend_unsupported.",
	}
}

type launchCostAccumulator struct {
	repoCostAccumulator
	events int
}

func (a *launchCostAccumulator) addPoint(p repoUsagePoint) {
	a.add(p)
	a.events++
}

type launchKey struct {
	agent string
	atMs  int64
}

func (k launchKey) String() string { return k.agent + "@" + strconv.FormatInt(k.atMs, 10) }

type refAccumulator struct {
	ref              tokens.LaunchRef
	launches         int
	agents           map[string]bool
	heads            map[string]bool
	firstMs, lastMs  int64
	shareTokens      int64
	shareUSD         float64
	launchTokens     int64
	launchUSD        float64
	touched, anyTier bool
}

// ComputeLaunchCost is the pure join. summary nil -> Ready=false.
func ComputeLaunchCost(summary *tokens.AggregateSummary, launches []tokens.LaunchRecord, now time.Time) LaunchCostResponse {
	resp := LaunchCostResponse{
		Phase:              "launch_attribution",
		ByLaunch:           []LaunchCostEntry{},
		ByRef:              []RefCostEntry{},
		WindowHours:        int(RepoCostWindow / time.Hour),
		OwnershipHours:     int(LaunchOwnershipMax / time.Hour),
		PriceTableDate:     tokens.PriceTableDate(),
		Disclaimer:         CostEstimateDisclaimer,
		Limitations:        LaunchCostLimitations(),
		Unattributed:       RepoCostEntry{Repo: RepoCostBucketUnattributed, Source: "estimated"},
		BackendUnsupported: RepoCostEntry{Repo: RepoCostBucketBackendUnsupported, Source: "estimated"},
		CollectedAt:        now,
		LaunchesRetained:   len(launches),
	}
	if summary == nil {
		return resp
	}
	resp.Ready = true

	byAgent := map[string][]tokens.LaunchRecord{}
	for _, l := range launches {
		if l.Agent == "" || l.LaunchedAtMs == 0 {
			continue
		}
		byAgent[l.Agent] = append(byAgent[l.Agent], l)
	}
	for agent, ls := range byAgent {
		sort.SliceStable(ls, func(i, j int) bool { return ls[i].LaunchedAtMs < ls[j].LaunchedAtMs })
		byAgent[agent] = ls
	}

	windowStartMs := now.Add(-RepoCostWindow).UnixMilli()
	ownershipMs := LaunchOwnershipMax.Milliseconds()
	byLaunch := map[launchKey]*launchCostAccumulator{}
	launchByKey := map[launchKey]tokens.LaunchRecord{}
	unattributed := &repoCostAccumulator{}
	backendUnsupported := &repoCostAccumulator{}

	for i := range summary.Sessions {
		sess := &summary.Sessions[i]
		if !repoCostBackendSupportsTimeline(sess.Backend) || len(sess.Usage) == 0 {
			backendUnsupported.add(repoUsagePoint{
				agent: sess.Agent,
				model: sess.Model,
				usage: tokens.UsageEvent{
					Input:       sess.InputTokens,
					Output:      sess.OutputTokens,
					CacheRead:   sess.CacheRead,
					CacheCreate: sess.CacheCreate,
				},
			})
			continue
		}
		agentLaunches := byAgent[sess.Agent]
		for _, u := range sess.Usage {
			model := u.Model
			if model == "" {
				model = sess.Model
			}
			p := repoUsagePoint{tsMs: u.TimestampMs, agent: sess.Agent, model: model, usage: u}
			owner, ok := attributeLaunch(agentLaunches, u.TimestampMs, windowStartMs, ownershipMs)
			if !ok {
				unattributed.add(p)
				continue
			}
			key := launchKey{agent: owner.Agent, atMs: owner.LaunchedAtMs}
			acc := byLaunch[key]
			if acc == nil {
				acc = &launchCostAccumulator{}
				byLaunch[key] = acc
				launchByKey[key] = owner
			}
			acc.addPoint(p)
		}
	}

	refs := map[string]*refAccumulator{}
	for key, acc := range byLaunch {
		rec := launchByKey[key]
		entry := launchEntry(rec, acc)
		resp.ByLaunch = append(resp.ByLaunch, entry)
		resp.AttributedTokens += entry.Tokens
		resp.TotalUSD += acc.usd
		if len(rec.Refs) == 0 {
			continue
		}
		shareTokens := entry.Tokens / int64(len(rec.Refs))
		shareUSD := acc.usd / float64(len(rec.Refs))
		for _, r := range rec.Refs {
			if r.Ref == "" {
				continue
			}
			ra := refs[r.Ref]
			if ra == nil {
				ra = &refAccumulator{ref: r, agents: map[string]bool{}, heads: map[string]bool{}}
				refs[r.Ref] = ra
			}
			ra.note(rec, r, shareTokens, shareUSD, entry.Tokens, acc.usd, acc.anyTier)
		}
	}
	// Launches that named refs but received no usage still count toward the
	// repeat-spend counter: a kick that was delivered is a launch even when the
	// agent did nothing with it.
	for _, rec := range launches {
		key := launchKey{agent: rec.Agent, atMs: rec.LaunchedAtMs}
		if _, counted := byLaunch[key]; counted || rec.LaunchedAtMs < windowStartMs {
			continue
		}
		for _, r := range rec.Refs {
			if r.Ref == "" {
				continue
			}
			ra := refs[r.Ref]
			if ra == nil {
				ra = &refAccumulator{ref: r, agents: map[string]bool{}, heads: map[string]bool{}}
				refs[r.Ref] = ra
			}
			ra.note(rec, r, 0, 0, 0, 0, false)
		}
	}
	for _, ra := range refs {
		resp.ByRef = append(resp.ByRef, ra.entry())
	}

	sort.Slice(resp.ByLaunch, func(i, j int) bool {
		a, b := resp.ByLaunch[i], resp.ByLaunch[j]
		if a.LaunchedAt != b.LaunchedAt {
			return a.LaunchedAt > b.LaunchedAt
		}
		return a.Agent < b.Agent
	})
	sort.Slice(resp.ByRef, func(i, j int) bool {
		a, b := resp.ByRef[i], resp.ByRef[j]
		if a.LaunchTokens != b.LaunchTokens {
			return a.LaunchTokens > b.LaunchTokens
		}
		if a.Launches != b.Launches {
			return a.Launches > b.Launches
		}
		return a.Ref < b.Ref
	})

	resp.Unattributed = unattributed.entry(RepoCostBucketUnattributed)
	resp.BackendUnsupported = backendUnsupported.entry(RepoCostBucketBackendUnsupported)
	resp.TotalUSD += unattributed.usd + backendUnsupported.usd
	resp.TotalTokens = resp.AttributedTokens + resp.Unattributed.Tokens + resp.BackendUnsupported.Tokens
	return resp
}

func launchEntry(rec tokens.LaunchRecord, acc *launchCostAccumulator) LaunchCostEntry {
	base := acc.entry("")
	refs := rec.Refs
	if refs == nil {
		refs = []tokens.LaunchRef{}
	}
	return LaunchCostEntry{
		Agent:       rec.Agent,
		Trigger:     rec.Trigger,
		Repo:        rec.Repo,
		LaunchedAt:  msToRFC3339(rec.LaunchedAtMs),
		Refs:        refs,
		USD:         base.USD,
		Source:      base.Source,
		Input:       base.Input,
		Output:      base.Output,
		CacheRead:   base.CacheRead,
		CacheCreate: base.CacheCreate,
		Tokens:      base.Tokens,
		Events:      acc.events,
	}
}

func (ra *refAccumulator) note(rec tokens.LaunchRecord, r tokens.LaunchRef, shareTokens int64, shareUSD float64, launchTokens int64, launchUSD float64, tier bool) {
	ra.launches++
	ra.agents[rec.Agent] = true
	if r.HeadSHA != "" {
		ra.heads[r.HeadSHA] = true
	}
	if r.Kind == tokens.LaunchRefKindPR {
		// A PR launch is the more specific identity for the same "repo#N" key.
		ra.ref.Kind = tokens.LaunchRefKindPR
	}
	if ra.firstMs == 0 || rec.LaunchedAtMs < ra.firstMs {
		ra.firstMs = rec.LaunchedAtMs
	}
	if rec.LaunchedAtMs > ra.lastMs {
		ra.lastMs = rec.LaunchedAtMs
	}
	if launchTokens > 0 || launchUSD > 0 {
		ra.touched = true
	}
	ra.shareTokens += shareTokens
	ra.shareUSD += shareUSD
	ra.launchTokens += launchTokens
	ra.launchUSD += launchUSD
	if tier {
		ra.anyTier = true
	}
}

func (ra *refAccumulator) entry() RefCostEntry {
	e := RefCostEntry{
		Ref:             ra.ref.Ref,
		Kind:            ra.ref.Kind,
		Repo:            ra.ref.Repo,
		Number:          ra.ref.Number,
		Launches:        ra.launches,
		FirstLaunchedAt: msToRFC3339(ra.firstMs),
		LastLaunchedAt:  msToRFC3339(ra.lastMs),
		ShareTokens:     ra.shareTokens,
		LaunchTokens:    ra.launchTokens,
		Source:          "estimated",
	}
	if ra.anyTier {
		e.Source = "estimated_tier"
	}
	if ra.touched {
		share, launch := ra.shareUSD, ra.launchUSD
		e.ShareUSD = &share
		e.LaunchUSD = &launch
	}
	for a := range ra.agents {
		e.Agents = append(e.Agents, a)
	}
	sort.Strings(e.Agents)
	for h := range ra.heads {
		e.HeadSHAs = append(e.HeadSHAs, h)
	}
	sort.Strings(e.HeadSHAs)
	return e
}

// attributeLaunch returns the launch that owns usage at tsMs: the latest
// launch for the agent at or before tsMs, provided tsMs is inside the window
// and within LaunchOwnershipMax of that launch.
func attributeLaunch(launches []tokens.LaunchRecord, tsMs, windowStartMs, ownershipMs int64) (tokens.LaunchRecord, bool) {
	if tsMs == 0 || len(launches) == 0 || tsMs < windowStartMs {
		return tokens.LaunchRecord{}, false
	}
	// First launch strictly after tsMs; the one before it owns tsMs.
	i := sort.Search(len(launches), func(i int) bool { return launches[i].LaunchedAtMs > tsMs })
	if i == 0 {
		return tokens.LaunchRecord{}, false
	}
	owner := launches[i-1]
	if tsMs-owner.LaunchedAtMs > ownershipMs {
		return tokens.LaunchRecord{}, false
	}
	return owner, true
}

func msToRFC3339(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
