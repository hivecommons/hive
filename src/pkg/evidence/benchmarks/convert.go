package benchmarks

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

// Options parameterise one export.
type Options struct {
	HiveID      string
	HiveVersion string
	Cluster     string
	// Since drops PRs whose newest record is not strictly newer than it (the
	// pusher passes its last watermark). Zero exports all.
	Since time.Time
	// Now stamps published_at; zero means time.Now().
	Now time.Time
	// RepoPublic reports whether owner/name is a public repository. nil means
	// unknown, which anonymizes every repo.
	RepoPublic func(repo string) bool
	// RepoConsent is benchmarks.repo_consent: false anonymizes even public repos.
	RepoConsent bool
	// SandboxRuntime resolves the runtime an agent's kicks ran under
	// ("none" when sandboxing is off). nil means "none" for every agent.
	SandboxRuntime func(agent string) string
	// CostLookup attributes kick spend to a PR (author: the PR and the issues
	// it closed; reviewer: the PR). nil leaves cost.basis "unknown".
	CostLookup CostLookup
}

// Roles.
const (
	RoleAuthor   = "author"
	RoleReviewer = "reviewer"
)

// SandboxRuntimeNone is the published runtime when agents run in-process.
const SandboxRuntimeNone = "none"

const (
	kindPRReview     = "pr-review"
	kindUnknown      = "unknown"
	verdictNone      = "none"
	providerUnknown  = "unknown"
	harnessOther     = "other"
	ciSuccess        = "success"
	ciFailure        = "failure"
	ciPending        = "pending"
	ciUnknown        = "unknown"
	reviewSwarmAgent = "review-swarm"
)

var knownHarnesses = map[string]bool{"agy": true, "aider": true, "bob": true, "claude": true, "cline": true, "codex": true,
	"copilot": true, "gemini": true, "goose": true, "kilo": true, "muse": true, "omnigent": true, "omp": true,
	"opencode": true, "openhands": true, "pi": true, "clanker": true}

var verdictRank = map[string]int{"reject": 4, "changes_requested": 3, "requires_human": 2, "comment": 1, "approve": 0}

var kindHints = []struct{ hint, kind string }{
	{"doc", "docs"}, {"sec", "security-fix"}, {"ci", "ci-repair"}, {"release", "release"},
	{"dep", "dependency-update"}, {"renovate", "dependency-update"}, {"triage", "triage"}, {"refactor", "refactor"},
}

var providerPrefixes = []struct{ prefix, provider string }{
	{"claude", "anthropic"}, {"gpt", "openai"}, {"o1", "openai"}, {"o3", "openai"}, {"o4", "openai"},
	{"gemini", "google"}, {"granite", "ibm"}, {"llama", "meta"}, {"mistral", "mistral"},
	{"qwen", "alibaba"}, {"deepseek", "deepseek"}, {"grok", "xai"}, {"kimi", "moonshot"},
}

// Convert folds bundles per PR and emits an author record for each PR a hive
// agent opened and a reviewer record for each PR the swarm scored. Output is
// sorted by id so exports are byte-stable.
func Convert(bundles []*evidence.Bundle, opts Options) []Record {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	hash := HiveHash(opts.HiveID)
	version := strings.TrimSpace(opts.HiveVersion)
	if version == "" {
		version = kindUnknown
	}
	byPR := map[string][]*evidence.Bundle{}
	for _, b := range bundles {
		if b == nil || b.Repo == "" || b.Number == 0 {
			continue
		}
		k := evidence.PRKey(b.Repo, b.Number)
		byPR[k] = append(byPR[k], b)
	}
	folded := evidence.FoldPRs(bundles, "", "")
	var out []Record
	for key, group := range byPR {
		pr := folded[key]
		if pr == nil {
			continue
		}
		latest := pr.Bundle
		if !opts.Since.IsZero() && !latest.UpdatedAt.After(opts.Since) {
			continue
		}
		public := opts.RepoConsent && opts.RepoPublic != nil && opts.RepoPublic(latest.Repo)
		base := Record{
			SchemaVersion: SchemaVersion,
			PublishedAt:   now.Format(time.RFC3339),
			Source:        Source{HiveID: hash, HiveVersion: version, Cluster: opts.Cluster, Exporter: Exporter},
			Task:          Task{RepoConsent: public},
			Outcome:       outcomeOf(pr, group),
			Quality:       qualityOf(latest, pr),
		}
		if public {
			base.Task.Repo = latest.Repo
			base.Task.RepoVisibility = "public"
			if owner, _, ok := strings.Cut(latest.Repo, "/"); ok {
				base.Source.Org = owner
			}
		}
		if lvl := latest.Policy.ACMMLevel; lvl >= 0 && lvl <= maxACMM {
			l := lvl
			base.Task.ACMMLevel = &l
		}
		a := latest.Author
		if a.Kind == evidence.AuthorAgent && strings.TrimSpace(a.Model) != "" {
			rec := base
			rec.ID = recordID(hash, latest.Repo, latest.Number, RoleAuthor)
			name := strings.TrimSpace(a.Agent)
			if name == "" {
				name = "agent"
			}
			rec.Agent = Agent{Name: name, Role: RoleAuthor, Harness: harnessOf(a.Backend)}
			rec.Model = Model{Provider: providerOf(a.Model), Name: strings.TrimSpace(a.Model)}
			rec.SandboxRuntime = runtimeOf(opts, a, name)
			rec.Task.Kind = kindOf(a.TaskKind, name)
			rec.Cost = costOf(opts.CostLookup, authorCostRefs(latest))
			out = append(out, rec)
		}
		if backend, model, ok := dominantReviewer(latest.Verdicts); ok {
			rec := base
			rec.ID = recordID(hash, latest.Repo, latest.Number, RoleReviewer)
			rec.Agent = Agent{Name: reviewSwarmAgent, Role: RoleReviewer, Harness: harnessOf(backend)}
			rec.Model = Model{Provider: providerOf(model), Name: model}
			rec.SandboxRuntime = SandboxRuntimeNone
			rec.Task.Kind = kindPRReview
			rec.Cost = costOf(opts.CostLookup, prCostRefs(latest.Repo, latest.Number, costKindPR))
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

const (
	costKindIssue = "issue"
	costKindPR    = "pr"
)

// prCostRefs spells one ref both ways the kick path records it.
func prCostRefs(repo string, number int, kind string) []CostRef {
	full := repo + "#" + strconv.Itoa(number)
	refs := []CostRef{{Ref: full, Kind: kind}}
	if _, short, ok := strings.Cut(repo, "/"); ok && short != "" {
		refs = append(refs, CostRef{Ref: short + "#" + strconv.Itoa(number), Kind: kind})
	}
	return refs
}

// authorCostRefs lists the refs an authoring kick may have been launched
// against: the PR itself and every same-repo issue it closed.
func authorCostRefs(b *evidence.Bundle) []CostRef {
	refs := prCostRefs(b.Repo, b.Number, costKindIssue)
	for _, c := range b.IssueClosures {
		repo := c.Repo
		if repo == "" {
			repo = b.Repo
		}
		if c.Issue > 0 {
			refs = append(refs, prCostRefs(repo, c.Issue, costKindIssue)...)
		}
	}
	return refs
}

func costOf(lookup CostLookup, refs []CostRef) Cost {
	if lookup != nil {
		if c, ok := lookup(refs); ok && c.USDEst != nil && *c.USDEst > 0 {
			if c.Basis == "" {
				c.Basis = CostBasisTokensXPricing
			}
			return c
		}
	}
	return Cost{Basis: CostBasisUnknown}
}

// maxACMM is the highest ACMM level the schema accepts.
const maxACMM = 6

func runtimeOf(opts Options, a evidence.Author, agent string) string {
	if rt := strings.TrimSpace(a.Runtime); rt != "" {
		return rt
	}
	if opts.SandboxRuntime != nil {
		if rt := strings.TrimSpace(opts.SandboxRuntime(agent)); rt != "" {
			return rt
		}
	}
	return SandboxRuntimeNone
}

func outcomeOf(pr *evidence.PR, group []*evidence.Bundle) Outcome {
	latest := pr.Bundle
	first := group[0]
	heads := map[string]bool{}
	for _, b := range group {
		if b.CreatedAt.Before(first.CreatedAt) {
			first = b
		}
		if b.HeadSHA != "" {
			heads[b.HeadSHA] = true
		}
	}
	merged := pr.Merge != nil || (pr.Outcome != nil && pr.Outcome.State == evidence.BackfillStateMerged) ||
		(latest.Backfill != nil && latest.Backfill.State == evidence.BackfillStateMerged)
	closed := !merged && pr.Closed
	o := Outcome{
		PROpened:           true,
		PRMerged:           merged,
		PRClosedUnmerged:   closed,
		CIStatusAtMerge:    ciStatus(latest.CI.Status),
		ReviewVerdict:      worstVerdict(latest.Verdicts),
		HumanInterventions: len(latest.HumanActions),
		Retries:            max(len(heads)-1, 0),
	}
	switch first.CI.Status {
	case ciSuccess:
		t := true
		o.CIGreenFirstTry = &t
	case ciFailure:
		f := false
		o.CIGreenFirstTry = &f
	}
	opened := pr.OpenedAt
	var end time.Time
	if merged {
		if pr.Merge != nil {
			end = pr.Merge.At
			switch pr.Merge.Method {
			case "merge", "squash", "rebase":
				o.MergeMethod = pr.Merge.Method
			default:
				o.MergeMethod = evidence.MergeUnrecorded
			}
		} else {
			o.MergeMethod = evidence.MergeUnrecorded
			if pr.Outcome != nil {
				end = pr.Outcome.At
			}
		}
		if !end.IsZero() {
			o.MergedAt = end.UTC().Format(time.RFC3339)
		}
	} else if closed && pr.Outcome != nil {
		end = pr.Outcome.At
	}
	if !opened.IsZero() {
		o.OpenedAt = opened.UTC().Format(time.RFC3339)
		if !end.IsZero() && !end.Before(opened) {
			secs := int64(end.Sub(opened).Seconds())
			o.WallClockS = &secs
		}
	}
	if len(latest.Verdicts) > 0 {
		minConf, seen := 1.0, false
		models := map[string]bool{}
		for _, v := range latest.Verdicts {
			if v.Confidence < minConf {
				minConf = v.Confidence
			}
			seen = true
			if m := strings.TrimSpace(v.Model); m != "" {
				models[m] = true
			}
		}
		if seen {
			c := min(max(minConf, 0), 1)
			o.ReviewConfidence = &c
		}
		for m := range models {
			o.ReviewerModels = append(o.ReviewerModels, m)
		}
		sort.Strings(o.ReviewerModels)
	}
	return o
}

func qualityOf(latest *evidence.Bundle, pr *evidence.PR) *Quality {
	q := &Quality{}
	any := false
	if pr.Merge != nil || (pr.Outcome != nil && pr.Outcome.State == evidence.BackfillStateMerged) {
		rv, fu, ro := len(pr.RevertedBy) > 0, len(pr.FollowedUpBy) > 0, len(pr.IssueReopens) > 0
		q.RevertedWithin14d, q.FollowedUpWithin14d, q.IssueReopened = &rv, &fu, &ro
		any = true
	}
	if lvl := latest.Policy.ACMMLevel; lvl >= 0 && lvl <= maxACMM {
		l := lvl
		q.ACMMLevel = &l
		any = true
	}
	if n := len(latest.Sentinel); n > 0 {
		q.SentinelFindings = &n
		any = true
	}
	if !any {
		return nil
	}
	return q
}

func ciStatus(s string) string {
	switch s {
	case ciSuccess, ciFailure, ciPending:
		return s
	}
	return ciUnknown
}

func worstVerdict(vs []evidence.Verdict) string {
	best, bestRank := verdictNone, -1
	for _, v := range vs {
		if r, ok := verdictRank[v.Verdict]; ok && r > bestRank {
			best, bestRank = v.Verdict, r
		}
	}
	return best
}

// dominantReviewer is the (backend, model) pair that cast most verdicts.
func dominantReviewer(vs []evidence.Verdict) (backend, model string, ok bool) {
	type pair struct{ b, m string }
	counts := map[pair]int{}
	for _, v := range vs {
		m := strings.TrimSpace(v.Model)
		if m == "" {
			continue
		}
		counts[pair{strings.TrimSpace(v.Backend), m}]++
	}
	best := pair{}
	for p, n := range counts {
		if n > counts[best] || (n == counts[best] && (best.m == "" || p.m < best.m)) {
			best = p
		}
	}
	return best.b, best.m, best.m != ""
}

func harnessOf(backend string) string {
	b := strings.ToLower(strings.TrimSpace(backend))
	if knownHarnesses[b] {
		return b
	}
	return harnessOther
}

func providerOf(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if p, _, ok := strings.Cut(m, "/"); ok && p != "" {
		return p
	}
	for _, pp := range providerPrefixes {
		if strings.HasPrefix(m, pp.prefix) {
			return pp.provider
		}
	}
	return providerUnknown
}

// kindOf prefers the task kind stamped on the bundle and otherwise guesses
// from the agent's name.
func kindOf(stamped, agent string) string {
	if k := strings.TrimSpace(stamped); k != "" {
		return k
	}
	return TaskKindFor(agent)
}

// TaskKindFor guesses the benchmark task kind from an agent's name
// ("docs-writer" → docs); "unknown" when nothing matches. It is what gets
// stamped on new bundles until agents declare a kind.
func TaskKindFor(agent string) string {
	n := strings.ToLower(agent)
	for _, h := range kindHints {
		if strings.Contains(n, h.hint) {
			return h.kind
		}
	}
	return kindUnknown
}

func itoa(n int) string { return strconv.Itoa(n) }
