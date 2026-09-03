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
	DefaultFixerAgent         = "scanner"
	reviewRoleToken           = "review"
)

var ReviewDispatchStatePath = filepath.Join(outputschema.AgentReportDir, ReviewDispatchStateFile)

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
	ReviewerAgents     []string
	FixerAgent         string
	ProjectOrg         string
	AIAuthor           string
	Agents             []AgentCapability
	Now                time.Time
}

type DispatchState struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Pending     []PendingReview   `json:"pending_reviews,omitempty"`
	Fixes       []PendingFix      `json:"pending_fixes,omitempty"`
	Human       []HumanReviewHold `json:"requires_human,omitempty"`
}

type PendingReview struct {
	Repo        string      `json:"repo"`
	Number      int         `json:"number"`
	HeadSHA     string      `json:"head_sha,omitempty"`
	Perspective Perspective `json:"perspective"`
	Agent       string      `json:"agent"`
	Dispatched  time.Time   `json:"dispatched_at"`
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
}

type DispatchPlan struct {
	ReviewKicks []DispatchKick
	FixKicks    []DispatchKick
	State       DispatchState
}

func LoadDispatchState(path string) (DispatchState, error) {
	if path == "" {
		path = ReviewDispatchStatePath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return DispatchState{}, err
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
		if pr.Number <= 0 || pr.Repo == "" || !isAgentAuthored(pr.Author, opts.AIAuthor) {
			continue
		}
		if agg, ok := artifact.AggregateFor(pr.Repo, pr.Number, pr.HeadSHA); ok {
			plan.State.Pending = removePendingForHead(plan.State.Pending, pr)
			if agg.Verdict == VerdictChangesRequested {
				plan.dispatchFix(pr, agg, opts, now)
			}
			continue
		}
		if len(reviewers) == 0 || availableSlots <= 0 {
			continue
		}
		missing := pendingMissingPerspectives(plan.State, pr)
		if len(missing) == 0 {
			continue
		}
		limit := len(missing)
		if len(reviewers) == 1 && limit > 1 {
			limit = 1
		}
		if limit > availableSlots {
			limit = availableSlots
		}
		for i := 0; i < limit; i++ {
			agent := reviewers[i%len(reviewers)].Name
			perspective := missing[i]
			msg := BuildPerspectivePrompt(perspective, pr)
			plan.ReviewKicks = append(plan.ReviewKicks, DispatchKick{Agent: agent, Message: msg, PRRef: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), Kind: "review", Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: perspective})
			plan.State.Pending = append(plan.State.Pending, PendingReview{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: perspective, Agent: agent, Dispatched: now})
			availableSlots--
		}
	}
	return plan
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
	deliveredSet := map[string]bool{}
	for _, k := range delivered {
		deliveredSet[dispatchKickKey(k)] = true
	}
	undelivered := map[string]bool{}
	for _, k := range planned {
		if !deliveredSet[dispatchKickKey(k)] {
			undelivered[dispatchKickKey(k)] = true
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
	return state
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

func pendingMissingPerspectives(state DispatchState, pr PullRequest) []Perspective {
	pending := map[Perspective]bool{}
	for _, p := range state.Pending {
		if samePRHead(p.Repo, p.Number, p.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			pending[p.Perspective] = true
		}
	}
	var missing []Perspective
	for _, p := range DefaultPerspectives {
		if !pending[p] {
			missing = append(missing, p)
		}
	}
	return missing
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
	openPRs := map[string]bool{}
	for _, pr := range prs {
		repo := fullRepoName(pr.Repo, org)
		openHeads[reviewKey(repo, pr.Number, pr.HeadSHA)] = true
		openPRs[fmt.Sprintf("%s#%d", repo, pr.Number)] = true
	}
	var pending []PendingReview
	for _, p := range state.Pending {
		if openHeads[reviewKey(p.Repo, p.Number, p.HeadSHA)] {
			pending = append(pending, p)
		}
	}
	state.Pending = pending
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
