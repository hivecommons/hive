package review

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/outputschema"
)

const (
	DefaultMaxParallelReviews = 5
	ReviewDispatchStateFile   = "review-dispatch-state.json"
	RecentDispatchTTL         = 24 * time.Hour
	// DefaultDispatchStateDir is the durable data dir. It matches the path the
	// knowledge engine and the dashboard config overlay already persist to,
	// which on a hosted spoke is the PersistentVolumeClaim.
	DefaultDispatchStateDir = "/data"
	DefaultFixerAgent       = "scanner"
	reviewRoleToken         = "review"
)

// ReviewDispatchStatePath is where the review swarm records which
// (repo, number, headSHA, perspective) tuples it has already dispatched. It is
// NOT a cache: it is the reviewer's only memory of what it has already looked
// at. PlanDispatch keeps no cursor — it re-walks the actionable PR list from
// the top every cycle and relies on this file to skip the PRs it already
// dispatched, so the parallel budget lands on the NEXT PRs in the queue.
//
// It therefore lives on the durable data dir, not under AgentReportDir
// (/var/run/hive-metrics), which is scratch space for regenerable per-cycle
// artifacts — actionable.json, tokens.json, github-cache.json — on the
// container's ephemeral writable layer. Storing dispatch state there meant
// every pod restart wiped the reviewer's memory: it re-walked the queue from
// the top, re-reviewed the same first PRs, and never advanced to the rest of
// the queue. Once reviewers publish comments, that also re-posts on those same
// PRs. A var (not const) so tests can point it at a temp dir.
var ReviewDispatchStatePath = filepath.Join(DefaultDispatchStateDir, ReviewDispatchStateFile)

// LegacyReviewDispatchStatePath is the pre-migration location. LoadDispatchState
// falls back to it so a hive upgrading in place keeps the state it already has
// instead of restarting its sweep of the queue from the top.
var LegacyReviewDispatchStatePath = filepath.Join(outputschema.AgentReportDir, ReviewDispatchStateFile)

type AgentCapability struct {
	Name           string
	Enabled        bool
	Paused         bool
	OnDemand       bool
	UsesKick       bool
	Role           string
	LaneKeywords   []string
	DetectKeywords []string
	Aliases        []string
}

type DispatchOptions struct {
	RequireApproval    bool
	FanOut             bool
	MaxParallelReviews int
	// MaxPerspectivesPerPR caps how many perspectives one PR receives for a
	// given head SHA, across cycles rather than within a single one. Each
	// perspective is a separate review comment, so this is the control over
	// how much review traffic a single pull request attracts.
	// Zero means DefaultMaxPerspectivesPerPR.
	MaxPerspectivesPerPR int
	// Perspectives is the set this hive reviews with, including any it
	// defines itself. The zero value means the built-in defaults.
	Perspectives PerspectiveSet
	// CombinedPerspectives dispatches ONE kick covering every perspective a PR
	// still needs, instead of one kick per perspective.
	//
	// It exists because breadth and quiet were in direct conflict. One kick
	// per perspective meant five agent sessions and five review comments on a
	// single PR, and capping that (max_perspectives_per_pr, #7562) bought
	// quiet by never reviewing four of the five perspectives at all. Reviewing
	// all of them in one session costs one comment, and reads the diff and its
	// surrounding tree once rather than five times.
	//
	// The verdicts stay separate: the reviewer emits one per perspective, and
	// any single one of them can still withhold approval.
	CombinedPerspectives bool
	ReviewerAgents       []string
	FixerAgent           string
	ProjectOrg           string
	AIAuthor             string
	// PostComments carries config.ReviewConfig.PostComments into the prompt
	// builder, so reviewers are told to publish their verdict on the PR.
	PostComments bool
	// AllAuthors lifts the agent-authored restriction so every open PR is
	// eligible for review, whoever opened it.
	AllAuthors bool
	// FixHumanPRs lets a changes_requested verdict dispatch a fix kick — an
	// agent checking out the PR branch and PUSHING to it — on a PR this
	// hive's agents did not open. Off, such a PR gets its review published
	// and nothing more: the author decides what to change. AllAuthors never
	// implies this (hivecommons/hive#8421).
	FixHumanPRs bool
	// AcknowledgeNoFindings carries config.ReviewConfig.AcknowledgeNoFindings
	// into the prompt builder, so a clean review still leaves a record.
	AcknowledgeNoFindings bool
	// ReviseRepos allowlists repos whose existing verdicts may be revisited
	// and whose reviews may be corrected in place. See
	// config.ReviewConfig.ReviseRepos.
	ReviseRepos []string
	// ReviseVerdictsBefore re-opens verdicts recorded before this instant for
	// a fresh review even though the PR's head SHA has not moved. Zero
	// disables revisiting entirely.
	ReviseVerdictsBefore time.Time
	Agents               []AgentCapability
	Now                  time.Time
}

type DispatchState struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Pending     []PendingReview   `json:"pending_reviews,omitempty"`
	Recent      []RecentReview    `json:"recent_reviews,omitempty"`
	Fixes       []PendingFix      `json:"pending_fixes,omitempty"`
	Human       []HumanReviewHold `json:"requires_human,omitempty"`
	// Withheld records the PR heads whose fix kick was refused because the PR
	// is not hive-authored and FixHumanPRs is off. Keyed by head SHA like
	// Fixes, so each refusal is reported (and audited) once per head rather
	// than on every eval cycle, and a new push earns a fresh decision.
	Withheld []WithheldFix `json:"withheld_fixes,omitempty"`
}

// WithheldFix is a fix kick the planner declined to dispatch. It carries what
// the audit trail needs to explain the silence: which PR, who opened it, and
// the setting that would have allowed the push.
type WithheldFix struct {
	Repo     string    `json:"repo"`
	Number   int       `json:"number"`
	HeadSHA  string    `json:"head_sha,omitempty"`
	Author   string    `json:"author,omitempty"`
	Setting  string    `json:"setting"`
	Reason   string    `json:"reason"`
	Withheld time.Time `json:"withheld_at"`
}

// WithheldFixSetting names the config key an operator turns on to let the
// fixer push to PRs the hive did not open. It is what the audit entry cites.
const WithheldFixSetting = "review.fix_human_prs"

type PendingReview struct {
	Repo        string      `json:"repo"`
	Number      int         `json:"number"`
	HeadSHA     string      `json:"head_sha,omitempty"`
	Perspective Perspective `json:"perspective"`
	Agent       string      `json:"agent"`
	AuthorAgent string      `json:"author_agent,omitempty"`
	Dispatched  time.Time   `json:"dispatched_at"`
}

type RecentReview struct {
	Repo        string      `json:"repo"`
	Number      int         `json:"number"`
	HeadSHA     string      `json:"head_sha,omitempty"`
	Perspective Perspective `json:"perspective"`
	Agent       string      `json:"agent"`
	AuthorAgent string      `json:"author_agent,omitempty"`
	Dispatched  time.Time   `json:"dispatched_at,omitempty"`
	Confirmed   time.Time   `json:"confirmed_at"`
}

type PendingFix struct {
	Repo       string    `json:"repo"`
	Number     int       `json:"number"`
	HeadSHA    string    `json:"head_sha,omitempty"`
	Agent      string    `json:"agent"`
	Attempts   int       `json:"attempts"`
	Dispatched time.Time `json:"dispatched_at"`
}

type HumanReviewHold struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	HeadSHA   string    `json:"head_sha,omitempty"`
	Reason    string    `json:"reason"`
	UpdatedAt time.Time `json:"updated_at"`
}

type DispatchKick struct {
	Agent       string
	Message     string
	PRRef       string
	Kind        string
	Repo        string
	Number      int
	HeadSHA     string
	Perspective Perspective
	AuthorAgent string
	// Perspectives is every perspective this kick covers. It is set only by
	// combined dispatch; Perspective stays populated with the first of them so
	// existing consumers (logging, metrics, kick dedup) keep working unchanged
	// rather than silently seeing an empty perspective.
	Perspectives []Perspective
}

type DispatchPlan struct {
	ReviewKicks []DispatchKick
	FixKicks    []DispatchKick
	// WithheldFixes are the refusals NEW this cycle — the ones the caller
	// should log and audit. Refusals already recorded in State.Withheld for
	// the same head are not repeated here.
	WithheldFixes []WithheldFix
	State         DispatchState
}

func LoadDispatchState(path string) (DispatchState, error) {
	explicit := path != ""
	if !explicit {
		path = ReviewDispatchStatePath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// One-time migration: a hive that ran before the state moved to the
		// durable dir still has its dispatch record at the legacy scratch
		// path. Read it so the upgrade does not look like amnesia and re-walk
		// the queue from the top. The next WriteDispatchState lands on the
		// durable path, so this fallback stops firing on its own. Only for the
		// default path — an explicit path means a caller (or a test) asked for
		// exactly that file.
		if explicit || !os.IsNotExist(err) || LegacyReviewDispatchStatePath == path {
			return DispatchState{}, err
		}
		legacy, legacyErr := os.ReadFile(LegacyReviewDispatchStatePath)
		if legacyErr != nil {
			return DispatchState{}, err
		}
		data = legacy
	}
	var state DispatchState
	if err := json.Unmarshal(data, &state); err != nil {
		return DispatchState{}, err
	}
	return state, nil
}

func WriteDispatchState(path string, state DispatchState) error {
	if path == "" {
		path = ReviewDispatchStatePath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func PlanDispatch(prs []PullRequest, artifact Artifact, state DispatchState, opts DispatchOptions) DispatchPlan {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	state.GeneratedAt = now
	state = pruneState(state, prs, opts.ProjectOrg)
	plan := DispatchPlan{State: state}
	if !opts.RequireApproval || !opts.FanOut {
		return plan
	}
	reviewers := reviewCapableAgents(opts)
	maxParallel := opts.MaxParallelReviews
	if maxParallel <= 0 {
		maxParallel = DefaultMaxParallelReviews
	}
	availableSlots := maxParallel
	for _, pr := range prs {
		pr.Repo = fullRepoName(pr.Repo, opts.ProjectOrg)
		if pr.Number <= 0 || pr.Repo == "" {
			continue
		}
		// Review is normally limited to the hive's own output, because that is
		// the work the hive is answerable for. AllAuthors lifts that: on a repo
		// where the queue is the problem, a human's PR waiting on a reviewer is
		// no less stuck than an agent's.
		if !opts.AllAuthors && !isAgentAuthored(pr.Author, opts.AIAuthor) {
			continue
		}
		revisiting := false
		if agg, ok := artifact.AggregateFor(pr.Repo, pr.Number, pr.HeadSHA); ok {
			// A verdict normally settles a PR until its head moves. That is
			// right while the reviewer is sound, and a trap once a
			// reviewer-side defect is found: without this, verdicts produced
			// by a known-broken reviewer stay frozen until someone happens to
			// push a commit.
			if !staleVerdictRevisitable(pr.Repo, agg, opts) {
				plan.State.Pending = removePendingForHead(plan.State.Pending, pr)
				switch agg.Verdict {
				case VerdictChangesRequested:
					if fixPushAllowed(pr, opts) {
						plan.dispatchFix(pr, agg, opts, now)
					} else {
						plan.withholdFix(pr, now)
					}
				case VerdictRequiresHuman:
					plan.holdForHuman(pr, agg, now)
				}
				continue
			}
			// Pending entries are the lifetime record of what this head has
			// already been asked, and a PR with a verdict always has them —
			// so left in place they make every perspective look covered and
			// the revisit silently never dispatches. Clear them so the whole
			// set is asked again. But only once: a revisit already in flight
			// shows as entries dispatched after the cutoff, and clearing
			// those would re-kick the same PR every cycle until its new
			// verdict lands.
			if revisitInFlight(plan.State, pr, opts.ReviseVerdictsBefore) {
				continue
			}
			plan.State.Pending = removePendingForHead(plan.State.Pending, pr)
			revisiting = true
		}
		prReviewers := reviewersForPR(reviewers, pr.AuthorAgent)
		if len(prReviewers) == 0 || availableSlots <= 0 {
			continue
		}
		missing := pendingMissingPerspectives(plan.State, pr, opts.Perspectives.List())
		if len(missing) == 0 {
			continue
		}
		// One kick, every outstanding perspective, one comment. The per-PR cap
		// is deliberately not applied here: it exists to bound how many review
		// COMMENTS one PR collects, and a combined review produces exactly one
		// however many perspectives it covers. Applying it anyway would drop
		// perspectives to buy quiet that has already been bought.
		if opts.CombinedPerspectives {
			agent := prReviewers[0].Name
			msg := BuildCombinedPrompt(pr, missing, PromptOptions{
				PostComments:          opts.PostComments,
				AcknowledgeNoFindings: opts.AcknowledgeNoFindings,
				Revise:                revisiting,
				Perspectives:          opts.Perspectives,
				ProposeFixesOnly:      !fixPushAllowed(pr, opts),
			})
			plan.ReviewKicks = append(plan.ReviewKicks, DispatchKick{Agent: agent, Message: msg, PRRef: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), Kind: "review", Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: missing[0], Perspectives: missing, AuthorAgent: pr.AuthorAgent})
			for _, p := range missing {
				plan.State.Pending = append(plan.State.Pending, PendingReview{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: p, Agent: agent, AuthorAgent: pr.AuthorAgent, Dispatched: now})
			}
			availableSlots--
			continue
		}
		limit := len(missing)
		// Breadth before depth: the parallel budget is spent in PR order, so
		// an uncapped first PR would take every slot for its own perspectives
		// and leave the rest of the queue unreviewed this cycle.
		//
		// The cap is also a lifetime budget per head SHA, not merely a
		// per-cycle one. Perspectives a PR has already been given persist in
		// state.Pending, so without counting them a capped hive still works
		// through every perspective one cycle at a time and posts a separate
		// review comment for each. From a maintainer's side that is the same
		// pile of comments, just spread out. Counting what a head SHA has
		// already received is what makes "max perspectives per PR" mean what
		// it says. A force-push clears the pending entries, so genuinely new
		// code earns a fresh budget.
		if perPR := opts.effectiveMaxPerspectivesPerPR(); perPR > 0 {
			covered := opts.Perspectives.Len() - len(missing)
			remaining := perPR - covered
			if remaining <= 0 {
				continue
			}
			if limit > remaining {
				limit = remaining
			}
		}
		if len(prReviewers) == 1 && limit > 1 {
			limit = 1
		}
		if limit > availableSlots {
			limit = availableSlots
		}
		for i := 0; i < limit; i++ {
			agent := prReviewers[i%len(prReviewers)].Name
			perspective := missing[i]
			msg := BuildPerspectivePromptWith(perspective, pr, PromptOptions{
				PostComments:          opts.PostComments,
				AcknowledgeNoFindings: opts.AcknowledgeNoFindings,
				Revise:                revisiting,
				Perspectives:          opts.Perspectives,
				ProposeFixesOnly:      !fixPushAllowed(pr, opts),
			})
			plan.ReviewKicks = append(plan.ReviewKicks, DispatchKick{Agent: agent, Message: msg, PRRef: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), Kind: "review", Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: perspective, AuthorAgent: pr.AuthorAgent})
			plan.State.Pending = append(plan.State.Pending, PendingReview{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: perspective, Agent: agent, AuthorAgent: pr.AuthorAgent, Dispatched: now})
			availableSlots--
		}
	}
	return plan
}

func reviewersForPR(reviewers []AgentCapability, authorAgent string) []AgentCapability {
	authorAgent = strings.TrimSpace(authorAgent)
	if authorAgent == "" {
		return reviewers
	}
	out := make([]AgentCapability, 0, len(reviewers))
	for _, reviewer := range reviewers {
		if reviewer.Name != authorAgent {
			out = append(out, reviewer)
		}
	}
	return out
}

// holdForHuman records a settled requires_human verdict as a human hold so the
// operator's triage label is applied to the PR, not just written into the
// review body. Before this, only fixer-side exhaustion produced a hold, so the
// most common "a human must decide" outcome — the reviewer saying so — never
// reached the label at all.
//
// An aggregate with no perspectives means no report was ever collected; that
// is an unreviewed PR, not one awaiting a decision, and must not be labelled.
func (p *DispatchPlan) holdForHuman(pr PullRequest, agg Aggregate, now time.Time) {
	if len(agg.Perspectives) == 0 {
		return
	}
	reason := "reviewer verdict requires_human"
	if len(agg.Reasons) > 0 {
		reason = agg.Reasons[0]
	}
	p.State.Human = upsertHuman(p.State.Human, HumanReviewHold{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Reason: reason, UpdatedAt: now})
}

// hiveAuthored reports whether this hive is answerable for the PR: its login
// is an agent's (isAgentAuthored), the audit trail attributes it to one of
// this hive's agents (AuthorAgent), or its body carries the hive attribution
// trailer (HiveAttributed — a PR an agent opened on a person's credentials).
// Anything else was opened by someone the hive does not speak for.
func hiveAuthored(pr PullRequest, opts DispatchOptions) bool {
	return isAgentAuthored(pr.Author, opts.AIAuthor) || strings.TrimSpace(pr.AuthorAgent) != "" || pr.HiveAttributed
}

// fixPushAllowed is the authorship check in front of every fix kick. A fix
// kick tells an agent to push a commit onto the PR branch, so it is only ever
// dispatched for a PR the hive itself opened — unless the operator turned on
// review.fix_human_prs, which grants exactly that for everyone else's PRs.
// AllAuthors is deliberately not consulted: it widens what is REVIEWED, and
// reviewing a contributor's PR is not a reason to rewrite it
// (hivecommons/hive#8421).
func fixPushAllowed(pr PullRequest, opts DispatchOptions) bool {
	return opts.FixHumanPRs || hiveAuthored(pr, opts)
}

// withholdFix records that a changes_requested PR got no fix kick because
// fixPushAllowed refused it. Once per head: the state entry is what stops
// the next cycle re-reporting the same refusal, and the plan entry is what
// the caller audits this cycle.
func (p *DispatchPlan) withholdFix(pr PullRequest, now time.Time) {
	for _, w := range p.State.Withheld {
		if samePRHead(w.Repo, w.Number, w.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			return
		}
	}
	w := WithheldFix{
		Repo:     pr.Repo,
		Number:   pr.Number,
		HeadSHA:  pr.HeadSHA,
		Author:   strings.TrimSpace(pr.Author),
		Setting:  WithheldFixSetting,
		Reason:   "PR was not opened by a hive agent; review published, fix not pushed",
		Withheld: now,
	}
	p.State.Withheld = append(p.State.Withheld, w)
	p.WithheldFixes = append(p.WithheldFixes, w)
}

func (p *DispatchPlan) dispatchFix(pr PullRequest, agg Aggregate, opts DispatchOptions, now time.Time) {
	idx := fixIndex(p.State.Fixes, pr.Repo, pr.Number, pr.HeadSHA)
	if idx >= 0 {
		return
	}
	attempts := maxFixAttemptsForPR(p.State.Fixes, pr.Repo, pr.Number)
	if attempts >= escalation.MaxReEngagements {
		p.State.Human = upsertHuman(p.State.Human, HumanReviewHold{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Reason: fmt.Sprintf("review fix cap reached (%d/%d)", attempts, escalation.MaxReEngagements), UpdatedAt: now})
		return
	}
	agent := selectFixerAgent(pr, opts)
	if agent == "" {
		p.State.Human = upsertHuman(p.State.Human, HumanReviewHold{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Reason: "no enabled kick-capable fixer agent is available", UpdatedAt: now})
		return
	}
	attempts++
	msg := BuildFixPrompt(pr, agg, attempts, escalation.MaxReEngagements)
	p.FixKicks = append(p.FixKicks, DispatchKick{Agent: agent, Message: msg, PRRef: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), Kind: "fix", Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA})
	p.State.Fixes = append(p.State.Fixes, PendingFix{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Agent: agent, Attempts: attempts, Dispatched: now})
}

func selectFixerAgent(pr PullRequest, opts DispatchOptions) string {
	candidates := []string{strings.TrimSpace(opts.FixerAgent), strings.TrimSpace(pr.Lane), DefaultFixerAgent}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		for _, a := range opts.Agents {
			if a.Name == candidate && a.Enabled && !a.Paused && !a.OnDemand && a.UsesKick {
				return candidate
			}
		}
	}
	return ""
}

func ConfirmDelivered(state DispatchState, planned, delivered []DispatchKick) DispatchState {
	now := time.Now().UTC()
	deliveredSet := map[string]bool{}
	for _, k := range delivered {
		deliveredSet[dispatchKickKey(k)] = true
		if k.Kind == "review" {
			// One recent entry per perspective the kick covered, not one for
			// the kick. The verdict relay binds a late verdict to a recent
			// entry by perspective, so a combined kick recorded under its
			// first perspective alone would leave every other verdict in the
			// array with nothing to bind to once its pending entry is gone.
			for _, p := range kickPerspectives(k) {
				state.Recent = upsertRecentReview(state.Recent, RecentReview{Repo: k.Repo, Number: k.Number, HeadSHA: k.HeadSHA, Perspective: p, Agent: k.Agent, AuthorAgent: k.AuthorAgent, Confirmed: now})
			}
		}
	}
	// Undelivered is keyed per PERSPECTIVE, because pending entries are. A
	// combined kick wrote one pending entry for each perspective it covered;
	// if it never reached the agent, every one of those must be released or
	// the ones after the first stay "pending" for a review that never
	// happened — never re-dispatched, never judged, and holding the PR's
	// unanimity check open indefinitely.
	undelivered := map[string]bool{}
	for _, k := range planned {
		if deliveredSet[dispatchKickKey(k)] {
			continue
		}
		if k.Kind != "review" {
			undelivered[dispatchKickKey(k)] = true
			continue
		}
		for _, p := range kickPerspectives(k) {
			one := k
			one.Perspective = p
			undelivered[dispatchKickKey(one)] = true
		}
	}
	var pending []PendingReview
	for _, p := range state.Pending {
		k := DispatchKick{Kind: "review", Agent: p.Agent, Repo: p.Repo, Number: p.Number, HeadSHA: p.HeadSHA, Perspective: p.Perspective}
		if !undelivered[dispatchKickKey(k)] {
			pending = append(pending, p)
		}
	}
	state.Pending = pending
	var fixes []PendingFix
	for _, f := range state.Fixes {
		k := DispatchKick{Kind: "fix", Agent: f.Agent, Repo: f.Repo, Number: f.Number, HeadSHA: f.HeadSHA}
		if !undelivered[dispatchKickKey(k)] {
			fixes = append(fixes, f)
		}
	}
	state.Fixes = fixes
	state.Recent = pruneRecentReviews(state.Recent, now)
	return state
}

// kickPerspectives is every perspective a kick covers: the combined list when
// set, else the single Perspective. Never empty for a review kick.
func kickPerspectives(k DispatchKick) []Perspective {
	if len(k.Perspectives) > 0 {
		return k.Perspectives
	}
	return []Perspective{k.Perspective}
}

func upsertRecentReview(items []RecentReview, item RecentReview) []RecentReview {
	for i := range items {
		if recentReviewSameDispatch(items[i], item) {
			items[i] = item
			return items
		}
	}
	return append(items, item)
}

func pruneRecentReviews(items []RecentReview, now time.Time) []RecentReview {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := now.Add(-RecentDispatchTTL)
	out := items[:0]
	for _, item := range items {
		if item.Confirmed.IsZero() || !item.Confirmed.Before(cutoff) {
			out = append(out, item)
		}
	}
	return out
}

func recentReviewSameDispatch(a, b RecentReview) bool {
	return strings.EqualFold(strings.TrimSpace(a.Repo), strings.TrimSpace(b.Repo)) &&
		a.Number == b.Number &&
		strings.TrimSpace(a.HeadSHA) == strings.TrimSpace(b.HeadSHA) &&
		a.Perspective == b.Perspective &&
		a.Agent == b.Agent
}

func BuildFixPrompt(pr PullRequest, agg Aggregate, attempt, maxAttempts int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[review-fix:%s#%d]\n", pr.Repo, pr.Number)
	fmt.Fprintf(&b, "Fix review findings for PR %s#%d", pr.Repo, pr.Number)
	if pr.Title != "" {
		fmt.Fprintf(&b, " — %s", pr.Title)
	}
	b.WriteString(".\n")
	if pr.URL != "" {
		fmt.Fprintf(&b, "URL: %s\n", pr.URL)
	}
	if pr.HeadSHA != "" {
		fmt.Fprintf(&b, "Head SHA: %s\n", pr.HeadSHA)
	}
	fmt.Fprintf(&b, "Auto-fix attempt %d of %d. Stay narrowly scoped to the review findings below; push a new commit to the PR branch.\n\n", attempt, maxAttempts)
	b.WriteString("Aggregated review findings:\n")
	if len(agg.Findings) == 0 && len(agg.Reasons) == 0 {
		b.WriteString("- Review requested changes but did not include structured findings; inspect the PR conversation and latest diff.\n")
	}
	for _, reason := range agg.Reasons {
		fmt.Fprintf(&b, "- %s\n", reason)
	}
	for _, f := range agg.Findings {
		fmt.Fprintf(&b, "- [%s/%s] %s: %s\n", f.Perspective, f.Finding.Severity, f.Finding.Title, f.Finding.Summary)
	}
	b.WriteString("\nReturn the standard fix AgentReport when done. If the findings are unsafe or ambiguous, request human review instead of guessing.\n")
	return b.String()
}

func (a Artifact) AggregateFor(repo string, number int, headSHA string) (Aggregate, bool) {
	wantRepo := strings.TrimSpace(repo)
	wantSHA := strings.TrimSpace(headSHA)
	for _, item := range a.Items {
		if item.Number == number && strings.TrimSpace(item.Repo) == wantRepo && strings.TrimSpace(item.HeadSHA) == wantSHA {
			return item, true
		}
	}
	return Aggregate{}, false
}

// effectiveMaxPerspectivesPerPR returns the per-PR perspective cap, or 0 for
// "no cap". Unlimited is the default deliberately: fanning every perspective
// out at once is the review swarm's designed behavior, and a hive that wants
// depth on each PR should keep getting it. The cap is for the opposite
// situation — a queue too deep to review in depth — and is opt-in so no
// existing hive silently changes shape.
func (o DispatchOptions) effectiveMaxPerspectivesPerPR() int {
	if o.MaxPerspectivesPerPR <= 0 {
		return 0
	}
	return o.MaxPerspectivesPerPR
}

func reviewCapableAgents(opts DispatchOptions) []AgentCapability {
	allowed := map[string]bool{}
	for _, name := range opts.ReviewerAgents {
		if n := strings.TrimSpace(name); n != "" {
			allowed[n] = true
		}
	}
	var out []AgentCapability
	for _, a := range opts.Agents {
		if a.Name == "" || !a.Enabled || a.Paused || a.OnDemand || !a.UsesKick {
			continue
		}
		if len(allowed) > 0 {
			if allowed[a.Name] {
				out = append(out, a)
			}
			continue
		}
		if hasReviewCapability(a) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func hasReviewCapability(a AgentCapability) bool {
	fields := append([]string{a.Name, a.Role}, a.Aliases...)
	fields = append(fields, a.LaneKeywords...)
	fields = append(fields, a.DetectKeywords...)
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), reviewRoleToken) {
			return true
		}
	}
	return false
}

func pendingMissingPerspectives(state DispatchState, pr PullRequest, perspectives []Perspective) []Perspective {
	pending := map[Perspective]bool{}
	for _, p := range state.Pending {
		if samePRHead(p.Repo, p.Number, p.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			pending[p.Perspective] = true
		}
	}
	if len(perspectives) == 0 {
		perspectives = DefaultPerspectives
	}
	var missing []Perspective
	for _, p := range perspectives {
		if !pending[p] {
			missing = append(missing, p)
		}
	}
	return missing
}

// revisitInFlight reports whether this head already has a review dispatched
// after the revise cutoff — i.e. the revisit has been kicked and its verdict
// has not yet replaced the stale one.
func revisitInFlight(state DispatchState, pr PullRequest, cutoff time.Time) bool {
	for _, p := range state.Pending {
		if samePRHead(p.Repo, p.Number, p.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) && p.Dispatched.After(cutoff) {
			return true
		}
	}
	return false
}

func removePendingForHead(pending []PendingReview, pr PullRequest) []PendingReview {
	out := pending[:0]
	for _, p := range pending {
		if !samePRHead(p.Repo, p.Number, p.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			out = append(out, p)
		}
	}
	return out
}

func pruneState(state DispatchState, prs []PullRequest, org string) DispatchState {
	openHeads := map[string]bool{}
	authorAgents := map[string]string{}
	openPRs := map[string]bool{}
	for _, pr := range prs {
		repo := fullRepoName(pr.Repo, org)
		key := reviewKey(repo, pr.Number, pr.HeadSHA)
		openHeads[key] = true
		if strings.TrimSpace(pr.AuthorAgent) != "" {
			authorAgents[key] = pr.AuthorAgent
		}
		openPRs[fmt.Sprintf("%s#%d", repo, pr.Number)] = true
	}
	var pending []PendingReview
	for _, p := range state.Pending {
		key := reviewKey(p.Repo, p.Number, p.HeadSHA)
		if openHeads[key] {
			if strings.TrimSpace(p.AuthorAgent) == "" {
				p.AuthorAgent = authorAgents[key]
			}
			if strings.TrimSpace(p.AuthorAgent) != "" && p.Agent == p.AuthorAgent {
				continue
			}
			pending = append(pending, p)
		}
	}
	state.Pending = pending
	state.Recent = pruneRecentReviews(state.Recent, time.Now().UTC())
	var fixes []PendingFix
	for _, f := range state.Fixes {
		if openPRs[fmt.Sprintf("%s#%d", f.Repo, f.Number)] {
			fixes = append(fixes, f)
		}
	}
	state.Fixes = fixes
	var human []HumanReviewHold
	for _, h := range state.Human {
		if openHeads[reviewKey(h.Repo, h.Number, h.HeadSHA)] {
			human = append(human, h)
		}
	}
	state.Human = human
	var withheld []WithheldFix
	for _, w := range state.Withheld {
		if openHeads[reviewKey(w.Repo, w.Number, w.HeadSHA)] {
			withheld = append(withheld, w)
		}
	}
	state.Withheld = withheld
	return state
}

func fullRepoName(repo, org string) string {
	if repo == "" || strings.Contains(repo, "/") || org == "" {
		return repo
	}
	return org + "/" + repo
}

func isAgentAuthored(author, aiAuthor string) bool {
	author = strings.TrimSpace(author)
	return author != "" && (strings.EqualFold(author, strings.TrimSpace(aiAuthor)) || strings.HasSuffix(author, "[bot]"))
}

func samePRHead(repo string, number int, sha string, wantRepo string, wantNumber int, wantSHA string) bool {
	return number == wantNumber && strings.TrimSpace(repo) == strings.TrimSpace(wantRepo) && strings.TrimSpace(sha) == strings.TrimSpace(wantSHA)
}

func fixIndex(fixes []PendingFix, repo string, number int, headSHA string) int {
	for i, f := range fixes {
		if samePRHead(f.Repo, f.Number, f.HeadSHA, repo, number, headSHA) {
			return i
		}
	}
	return -1
}

func maxFixAttemptsForPR(fixes []PendingFix, repo string, number int) int {
	max := 0
	for _, f := range fixes {
		if f.Repo == repo && f.Number == number && f.Attempts > max {
			max = f.Attempts
		}
	}
	return max
}

func upsertHuman(items []HumanReviewHold, item HumanReviewHold) []HumanReviewHold {
	for i := range items {
		if samePRHead(items[i].Repo, items[i].Number, items[i].HeadSHA, item.Repo, item.Number, item.HeadSHA) {
			items[i] = item
			return items
		}
	}
	return append(items, item)
}

func dispatchKickKey(k DispatchKick) string {
	return fmt.Sprintf("%s|%s|%s#%d@%s|%s", k.Kind, k.Agent, strings.TrimSpace(k.Repo), k.Number, strings.TrimSpace(k.HeadSHA), k.Perspective)
}

// staleVerdictRevisitable reports whether an existing verdict should be set
// aside so the PR is reviewed again, despite its head SHA being unchanged.
//
// Both gates must pass. The repo must be allowlisted for revision, so a
// revisit can only happen where the hive also holds the quieter in-place
// correction path and cannot stack a second review on someone's PR. And the
// verdict must predate the configured cutoff, which is what scopes the
// correction to verdicts produced by the reviewer that was wrong.
//
// The cutoff is self-limiting: re-reviewing records a fresh timestamp that is
// necessarily after it, so a PR is revisited at most once per bump rather than
// entering a loop.
func staleVerdictRevisitable(repo string, agg Aggregate, opts DispatchOptions) bool {
	if opts.ReviseVerdictsBefore.IsZero() || len(opts.ReviseRepos) == 0 {
		return false
	}
	if !reviseRepoAllowed(repo, opts.ReviseRepos) {
		return false
	}
	if agg.RecordedAt.IsZero() {
		// An undated verdict cannot be shown to predate the cutoff. Leave it
		// alone rather than guess: re-reviewing on a guess is how a narrow
		// correction turns into a sweep.
		return false
	}
	return agg.RecordedAt.Before(opts.ReviseVerdictsBefore)
}

// reviseRepoAllowed reports whether a repo is in the revision allowlist.
func reviseRepoAllowed(repo string, allowlist []string) bool {
	want := strings.ToLower(strings.TrimSpace(repo))
	if want == "" {
		return false
	}
	for _, entry := range allowlist {
		if strings.EqualFold(strings.TrimSpace(entry), want) {
			return true
		}
	}
	return false
}
