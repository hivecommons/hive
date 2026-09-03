package collect

// Per-repo estimated cost attribution — phase 3 of #4836.
//
// WHAT THIS DOES
//
// Hive knows two things separately and has never joined them:
//
//   - WHEN tokens were spent, and by which agent (pkg/tokens — since #4836
//     phase 2 the Claude scanner retains a timestamped per-message usage
//     timeline instead of only a session total)
//   - WHEN an agent produced output against a named repo (the audit log's
//     `repo=` pairs, summarized per repo by the activity collector in phase 1)
//
// The join is an INTERVAL join on time, per agent. Order one agent's audited
// repo events by timestamp. Between two consecutive events, every usage event
// that falls inside is attributed to the repo named by the CLOSING event — the
// reasoning being that work done immediately before filing on a repo was work
// done FOR that repo.
//
// WHAT THIS IS NOT
//
// It is not a bill, and it is not measured. It is an inference from timing, and
// its failure mode is specific and known: an agent that investigates repo A,
// finds nothing worth filing, then files on repo B bills ALL of A's
// investigation to B. Nothing in the data distinguishes those two cases. This is
// documented in the API `limitations` array, not just here, because an operator
// reading a per-repo dollar figure must be able to see it.
//
// TWO BUCKETS ARE MANDATORY AND ARE NEVER NORMALIZED AWAY:
//
//   - `unattributed` — tokens before an agent's first `repo=` event, tokens
//     after its last one, tokens from agents with no repo events at all, and
//     tokens whose timestamp could not be determined. The LEADING interval is
//     never given to the first repo seen: there is no closing event for it, and
//     inventing one is how a per-repo cost number becomes plausibly wrong.
//   - `backend_unsupported` — every non-Claude backend's tokens. Copilot accrues
//     all usage in one lump at session.shutdown and bob parses no per-message
//     timestamps, so neither has an intra-session time distribution to join
//     against. Their spend is REAL and is reported here in full; it is simply
//     not placeable against a repo.
//
// The invariant `Σ by_repo + unattributed + backend_unsupported == hive total`
// holds by construction and is asserted by test. Any token the hive counted
// lands in exactly one bucket.
//
// DOUBLE-COUNTING: this reads the token collector's already-merged
// AggregateSummary, which is the same figure /api/cost reports. It does NOT
// re-read scanner sources, so the copilot scanner's live-capture zero-out
// (which avoids double-counting sessions the MITM sink already recorded) is
// respected rather than bypassed.

import (
	"sort"
	"strconv"
	"time"

	"github.com/hivecommons/hive/pkg/tokens"
)

// CostEstimateDisclaimer travels with every estimated-cost payload (moved here
// from pkg/dashboard's cost.go so both the /api/cost handler and the repo-cost
// join stamp the identical sentence).
const CostEstimateDisclaimer = "Estimated from token counts × published list prices. " +
	"Subscription plans (Claude, Copilot), self-hosted inference (vLLM), and negotiated rates differ from list price. " +
	"Not a bill."

// RepoCostWindow is the lookback for BOTH sides of the join. It matches the
// activity collector's activityWindow so the audit events and the usage events
// cover the same span — joining a 14d event stream against a 30d usage stream
// would silently push 16 days of real spend into `unattributed`.
const RepoCostWindow = activityWindow

// RepoCostBucketUnattributed and RepoCostBucketBackendUnsupported are the two
// mandatory buckets. They are exported in the payload under fixed keys because
// a UI must be able to render them without inferring their meaning, and they
// are always present even when zero.
const (
	RepoCostBucketUnattributed       = "unattributed"
	RepoCostBucketBackendUnsupported = "backend_unsupported"
)

// RepoCostEntry is one repo's attributed estimated cost. USD is a POINTER so a
// repo with no attributable cost renders as "—" and never as "$0.00": those are
// different facts and conflating them is how an operator concludes a repo was
// cheap when it was merely unobserved.
type RepoCostEntry struct {
	Repo string   `json:"repo"`
	USD  *float64 `json:"usd"`
	// Source is "estimated" when every model contributing to this repo had an
	// exact list price, and "estimated_tier" when any contributor fell back to
	// FallbackPrice's coarse tier guess. It is NEVER "actual": nothing here is
	// a billed figure.
	Source      string `json:"source"`
	Input       int64  `json:"input"`
	Output      int64  `json:"output"`
	CacheRead   int64  `json:"cache_read"`
	CacheCreate int64  `json:"cache_create"`
	Tokens      int64  `json:"tokens"`
	// Events is how many DISTINCT audited repo= events actually closed an
	// interval that carried usage for this repo — the evidence behind the
	// figure. It deliberately does NOT count every audited event mentioning
	// the repo: an agent's first event closes nothing (the leading interval is
	// never attributed), and events that closed only empty intervals are not
	// evidence of spend. Counting those would overstate the support for a
	// dollar figure.
	Events int `json:"events"`
	// Agents lists the agents that contributed, sorted, so an operator can see
	// which sandbox drove the cost.
	Agents []string `json:"agents,omitempty"`
}

// RepoCostResponse is the GET /api/repo-cost payload.
type RepoCostResponse struct {
	Ready bool   `json:"ready"`
	Phase string `json:"phase"`
	// ByRepo excludes the two mandatory buckets, which are reported separately
	// so no consumer can accidentally sum them into a "per-repo" total or
	// normalize them away as rounding.
	ByRepo             []RepoCostEntry `json:"by_repo"`
	Unattributed       RepoCostEntry   `json:"unattributed"`
	BackendUnsupported RepoCostEntry   `json:"backend_unsupported"`

	// TotalUSD is the sum of every bucket below — the hive-wide estimated total
	// this response partitions. Pricing is linear in token counts, so it tracks
	// /api/cost's figure; it can differ marginally because /api/cost merges
	// model ids by normalized spelling before pricing while this prices each
	// usage event at its own recorded model id. TOKEN totals are exact either
	// way, which is why the partition invariant is asserted on tokens.
	TotalUSD float64 `json:"total_usd"`
	// AttributedTokens / TotalTokens give the attribution RATE directly, so the
	// UI need not derive it and cannot derive it differently.
	AttributedTokens int64 `json:"attributed_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	// WindowHours is the lookback the join actually used. It travels WITH the
	// numbers because audit retention is size-triggered (5MB x 3 backups), so
	// the effective lookback varies per hive and is SHORTEST on the busiest
	// ones — exactly where per-repo cost matters most. A per-repo figure whose
	// window is unknown is not comparable across the fleet.
	WindowHours int `json:"window_hours"`
	// OldestEventAt is the earliest audited repo= event the join could see,
	// which is the real lower bound on the window when rotation has already
	// discarded older entries. Empty when there were no events.
	OldestEventAt string `json:"oldest_event_at,omitempty"`

	PriceTableDate string   `json:"price_table_date"`
	Disclaimer     string   `json:"disclaimer"`
	Limitations    []string `json:"limitations"`

	// CollectedAt is when this snapshot was actually computed by
	// RepoCostCollector (repo_cost_collector.go), NOT when this HTTP response
	// was served — those now differ by up to repoCostCollectInterval because
	// the endpoint serves a cached snapshot rather than recomputing per
	// request (#4943). Zero when no collection has ever completed (Ready is
	// then also false). A consumer diffing against time.Now() can detect a
	// stalled collector — a stuck ticker goroutine — the same way a stale
	// heartbeat is detected elsewhere in the dashboard.
	CollectedAt time.Time `json:"collected_at,omitempty"`
}

// RepoCostLimitations is surfaced in the payload so the UI renders the method's
// caveats alongside the numbers rather than burying them in code comments.
func RepoCostLimitations() []string {
	return []string{
		"Claude sessions only. Copilot records all token usage in a single lump at session shutdown and bob parses no per-message timestamps, so neither can be placed in time. Their spend is real and is reported in full under backend_unsupported — it is not missing, it is not attributable.",
		"Attribution is inferred from TIMING, not measured. Tokens spent between two audited repo= events are billed to the repo named by the later event.",
		"KNOWN BIAS: an agent that investigates repo A, finds nothing to file, then files on repo B bills ALL of A's investigation to B. This is the method's failure mode, not a rounding error — treat a repo's figure as an upper bound when neighbouring repos show activity but little cost.",
		"Tokens before an agent's first repo= event, after its last, or from agents with no repo events at all are reported as unattributed and are NEVER spread across repos.",
		"A repo with no attributable cost reports null, not zero. Null means unobserved; it does not mean free.",
		"Bounded by audit-log retention, which is size-triggered (5MB x 3 backups) rather than age-based. The effective lookback therefore varies per hive and is shortest on the busiest ones. Compare oldest_event_at against window_hours before comparing figures across hives.",
		"Estimated from token counts x published list prices, never a billed figure. Models absent from the price table are priced from a coarse tier guess and marked source=estimated_tier.",
	}
}

// repoUsagePoint is one attributable slice of spend: tokens at a time, owned by
// an agent, priced by a model.
type repoUsagePoint struct {
	tsMs  int64
	agent string
	model string
	usage tokens.UsageEvent
}

// repoCostAccumulator sums tokens and dollars into one bucket.
type repoCostAccumulator struct {
	usd                                   float64
	input, output, cacheRead, cacheCreate int64
	agents                                map[string]bool
	// anyTier records that at least one contributing model was priced from the
	// coarse tier fallback rather than an exact list price.
	anyTier bool
	// closers holds each distinct audited event that closed a non-empty
	// interval for this repo, keyed agent+timestamp so two agents filing at
	// the same instant are two pieces of evidence, not one. Events counts real
	// evidence rather than every event that merely names the repo.
	closers map[string]bool
	// touched distinguishes "no cost attributed" from "zero cost attributed",
	// which is what lets USD stay nil and the UI render an honest "—".
	touched bool
}

// noteCloser records that a specific audited event closed a non-empty interval
// for this bucket. Called only from the attributed path — the two mandatory
// buckets have no closing events by definition.
func (a *repoCostAccumulator) noteCloser(agent string, tsMs int64) {
	if a.closers == nil {
		a.closers = map[string]bool{}
	}
	a.closers[agent+"@"+strconv.FormatInt(tsMs, 10)] = true
}

func (a *repoCostAccumulator) add(p repoUsagePoint) {
	usd, exact := tokens.EstimateCostUSD(p.model, p.usage.Input, p.usage.Output, p.usage.CacheRead, p.usage.CacheCreate)
	a.usd += usd
	if !exact {
		a.anyTier = true
	}
	a.input += p.usage.Input
	a.output += p.usage.Output
	a.cacheRead += p.usage.CacheRead
	a.cacheCreate += p.usage.CacheCreate
	if p.agent != "" {
		if a.agents == nil {
			a.agents = map[string]bool{}
		}
		a.agents[p.agent] = true
	}
	a.touched = true
}

func (a *repoCostAccumulator) entry(name string) RepoCostEntry {
	e := RepoCostEntry{
		Repo:        name,
		Input:       a.input,
		Output:      a.output,
		CacheRead:   a.cacheRead,
		CacheCreate: a.cacheCreate,
		Tokens:      a.input + a.output + a.cacheRead + a.cacheCreate,
		Events:      len(a.closers),
		Source:      "estimated",
	}
	if a.anyTier {
		e.Source = "estimated_tier"
	}
	// nil != zero: only a bucket that actually received usage gets a dollar
	// figure. An untouched bucket renders "—".
	if a.touched {
		usd := a.usd
		e.USD = &usd
	}
	for agent := range a.agents {
		e.Agents = append(e.Agents, agent)
	}
	sort.Strings(e.Agents)
	return e
}

// repoAuditEvent is one audited repo= output event, already time-resolved.
type repoAuditEvent struct {
	tsMs int64
	repo string
}

// ComputeRepoCost performs the interval join. It is a pure function of its
// inputs — no clock, no I/O — so the invariant test can drive it directly.
//
// summary is the hive's merged token summary (the same one /api/cost prices).
// entries are audited output actions carrying repo= over the window.
func ComputeRepoCost(summary *tokens.AggregateSummary, entries []AuditEntry, now time.Time) RepoCostResponse {
	resp := RepoCostResponse{
		Phase:              "phase_3_interval_join",
		ByRepo:             []RepoCostEntry{},
		WindowHours:        int(RepoCostWindow / time.Hour),
		PriceTableDate:     tokens.PriceTableDate(),
		Disclaimer:         CostEstimateDisclaimer,
		Limitations:        RepoCostLimitations(),
		Unattributed:       RepoCostEntry{Repo: RepoCostBucketUnattributed, Source: "estimated"},
		BackendUnsupported: RepoCostEntry{Repo: RepoCostBucketBackendUnsupported, Source: "estimated"},
		CollectedAt:        now,
	}
	if summary == nil {
		return resp
	}
	resp.Ready = true

	// --- Side A: audited repo events, grouped per agent and time-ordered ---
	byAgentEvents := map[string][]repoAuditEvent{}
	var oldest string
	for _, e := range entries {
		m := repoRe.FindStringSubmatch(e.Detail)
		if m == nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil {
			continue
		}
		agent := activityAgent(e)
		byAgentEvents[agent] = append(byAgentEvents[agent], repoAuditEvent{tsMs: t.UnixMilli(), repo: m[1]})
		if oldest == "" || e.Timestamp < oldest {
			oldest = e.Timestamp
		}
	}
	for agent := range byAgentEvents {
		ev := byAgentEvents[agent]
		sort.Slice(ev, func(i, j int) bool { return ev[i].tsMs < ev[j].tsMs })
		byAgentEvents[agent] = ev
	}
	resp.OldestEventAt = oldest

	// --- Side B: usage points, split by whether they are placeable in time ---
	windowStartMs := now.Add(-RepoCostWindow).UnixMilli()
	byRepo := map[string]*repoCostAccumulator{}
	unattributed := &repoCostAccumulator{}
	backendUnsupported := &repoCostAccumulator{}

	for i := range summary.Sessions {
		sess := &summary.Sessions[i]

		// Any backend that cannot produce a per-message timeline contributes
		// its FULL session total to backend_unsupported. This is the branch
		// that keeps the invariant honest: those tokens are counted, just not
		// placed. A Claude session that somehow carries no timeline (an older
		// persisted snapshot from before phase 2, say) lands here too — the
		// tokens are real and must not vanish.
		if sess.Backend != tokens.BackendClaude || len(sess.Usage) == 0 {
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

		// A Claude session's retained timeline sums to its session total
		// exactly (pkg/tokens asserts this), so iterating the timeline
		// accounts for the whole session and never double-counts it against
		// the summed fields.
		events := byAgentEvents[sess.Agent]
		for _, u := range sess.Usage {
			model := u.Model
			if model == "" {
				model = sess.Model
			}
			p := repoUsagePoint{tsMs: u.TimestampMs, agent: sess.Agent, model: model, usage: u}

			closer, ok := attributeRepo(events, u.TimestampMs, windowStartMs)
			if !ok {
				unattributed.add(p)
				continue
			}
			acc := byRepo[closer.repo]
			if acc == nil {
				acc = &repoCostAccumulator{}
				byRepo[closer.repo] = acc
			}
			acc.add(p)
			// Only an event that closed an interval carrying real usage counts
			// as evidence behind this repo's figure.
			acc.noteCloser(sess.Agent, closer.tsMs)
		}
	}

	for repo, acc := range byRepo {
		entry := acc.entry(repo)
		resp.ByRepo = append(resp.ByRepo, entry)
		resp.AttributedTokens += entry.Tokens
		resp.TotalUSD += acc.usd
	}
	sort.Slice(resp.ByRepo, func(i, j int) bool {
		a, b := resp.ByRepo[i], resp.ByRepo[j]
		if a.Tokens != b.Tokens {
			return a.Tokens > b.Tokens
		}
		return a.Repo < b.Repo
	})

	resp.Unattributed = unattributed.entry(RepoCostBucketUnattributed)
	resp.BackendUnsupported = backendUnsupported.entry(RepoCostBucketBackendUnsupported)
	resp.TotalUSD += unattributed.usd + backendUnsupported.usd
	resp.TotalTokens = resp.AttributedTokens + resp.Unattributed.Tokens + resp.BackendUnsupported.Tokens

	return resp
}

// attributeRepo finds the repo that owns the usage at tsMs: the first audited
// event at or after tsMs, whose repo is the CLOSING event of the interval the
// usage falls in.
//
// It returns ok=false — sending the tokens to `unattributed` — in every case
// where honest attribution is impossible:
//
//   - tsMs == 0: the usage carries no parseable timestamp, so it cannot be
//     placed in any interval.
//   - tsMs is before the window: the audit events that would have closed its
//     interval have already rotated out of the log. Attributing it to the
//     oldest surviving event would silently bill an unknown span to whichever
//     repo happened to survive rotation.
//   - no event at or after tsMs: the interval has no closing event (the agent
//     spent tokens and has not yet filed anything). Assigning it to the
//     PRECEDING event would be attributing work to a repo the agent had
//     already finished with.
//
// The leading interval is handled by the same rule rather than by a special
// case: usage before an agent's first event DOES find a closing event, so it
// would be billed to the first repo. That is exactly the behaviour #4836
// forbids, so it is excluded explicitly below.
func attributeRepo(events []repoAuditEvent, tsMs, windowStartMs int64) (repoAuditEvent, bool) {
	if tsMs == 0 || len(events) == 0 {
		return repoAuditEvent{}, false
	}
	if tsMs < windowStartMs {
		return repoAuditEvent{}, false
	}
	// Never attribute the leading interval: usage that precedes an agent's
	// FIRST audited event has no opening event, so the span it belongs to is
	// unbounded and may predate the log entirely.
	if tsMs <= events[0].tsMs {
		return repoAuditEvent{}, false
	}
	// First event at or after tsMs closes the interval.
	i := sort.Search(len(events), func(i int) bool { return events[i].tsMs >= tsMs })
	if i >= len(events) {
		// Trailing usage with nothing filed after it.
		return repoAuditEvent{}, false
	}
	return events[i], true
}
