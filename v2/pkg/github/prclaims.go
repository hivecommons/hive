package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Duplicate-PR guard.
//
// Background: a restart storm (pod relaunching the agent from scratch every
// few minutes) made a quality agent open nine near-identical PRs overnight
// against the same issue. Each fresh start had no memory of the PR it had
// just filed, saw the same open issue, and filed another.
//
// Agents open PRs by running `gh` inside their own shell, so the hive never
// observes the call and cannot intercept it. A prompt instruction ("check
// before opening a PR") is advisory and is exactly what already failed. The
// fix therefore removes the work from the offer: any issue already claimed by
// an open PR authored by this hive is filtered out of the actionable set
// before kick messages are built.

const (
	// claimLedgerTTL bounds how long a cached claim survives without being
	// re-confirmed by a live API call. It is deliberately much longer than a
	// typical kick cadence (hours) so a multi-hour GitHub outage still keeps
	// suppressing duplicates, but short enough that a claim whose PR vanished
	// without our noticing eventually releases the issue back for work.
	claimLedgerTTL = 72 * time.Hour

	// claimSearchPerPage is the page size used when listing open PRs per repo.
	// 100 is the GitHub API maximum and matches the other paginated listers in
	// this package.
	claimSearchPerPage = 100

	// claimSearchMaxPages caps pagination so a repo with a pathological number
	// of open PRs cannot stall an enumeration cycle indefinitely.
	claimSearchMaxPages = 10

	// ClaimLedgerPath is the on-PVC location of the persisted claim ledger.
	// /data is the hive's PersistentVolumeClaim mount, so the ledger survives
	// the pod restarts that caused the incident this guard exists to prevent.
	ClaimLedgerPath = "/data/pr-claims.json"

	// claimLedgerFileMode is the permission mode for the ledger file. It holds
	// no secrets (issue and PR numbers only), so it is world-readable like the
	// other operational state files under /data.
	claimLedgerFileMode = 0o644

	// weakClaimAgentDeferralWindow bounds how long a WEAK claim — one from an
	// external (non-hive) author (#3768) or one parsed from a non-closing
	// reference like `Refs #N` (#3980) — DEFERS the hive's own agent pipeline
	// from taking the issue (see FilterClaimedIssues). Within the window the
	// issue is suppressed exactly like a hive-authored `Fixes` claim, so an
	// agent does not start duplicating work someone visibly has in flight
	// (the dakota#353 shape, kubestellar/hive#3768). Once the claiming PR has
	// been open past the window without merging, the issue is RELEASED even
	// though the claim persists: this is what keeps a stranger's junk PR — or
	// a stalled soft-reference PR — from freezing the hive's pipeline forever,
	// the invariant #3792 protected with an unconditional external-claim
	// bypass. 72h matches claimLedgerTTL: a claim that cannot be reconfirmed
	// for that long dies anyway, so no weak claim ever outlives its deferral
	// authority. Contribute-queue suppression (dashboard selectTask) is NOT
	// window-bounded — it honours every open-PR claim for as long as the PR
	// stays open, because handing a human contributor an issue that anyone's
	// open PR already covers is pure waste (#3980).
	weakClaimAgentDeferralWindow = 72 * time.Hour
)

// IssueClaim records that an open pull request authored by this hive already
// claims to address a specific issue.
type IssueClaim struct {
	// Repo is the issue's repository, in the same form used by config
	// (either "repo" or "owner/repo"), so it can be compared to Issue.Repo.
	Repo string `json:"repo"`
	// Issue is the claimed issue number.
	Issue int `json:"issue"`
	// PRNumber is the pull request making the claim.
	PRNumber int `json:"pr_number"`
	// PRRepo is the repository the claiming PR lives in. It may differ from
	// Repo when the PR uses the cross-repo `owner/repo#N` closing form.
	PRRepo string `json:"pr_repo"`
	// PRURL is the claiming pull request's HTML URL, logged on every
	// suppression so an operator can immediately see what claimed the issue.
	PRURL string `json:"pr_url"`
	// PRAuthor is the login that opened the claiming PR.
	PRAuthor string `json:"pr_author"`
	// ObservedAt is when this claim was last confirmed by a live API call.
	// It is the basis for TTL expiry.
	ObservedAt time.Time `json:"observed_at"`
	// ExternalAuthor marks a claim whose PR was opened by an account that is
	// NOT this hive (kubestellar/hive#3768). External claims exist so the
	// contribute queue can stop offering an issue that a human contributor's
	// open PR already fixes; they deliberately do NOT suppress agent work in
	// FilterClaimedIssues, preserving the original guard's rule that a
	// stranger's PR can never freeze the hive's own pipeline. The field is
	// omitempty-false so ledgers written before #3768 — which only ever
	// recorded hive-authored claims — unmarshal as hive-authored (false),
	// keeping their agent-side suppression intact across the upgrade.
	ExternalAuthor bool `json:"external_author,omitempty"`
	// SoftReference marks a claim parsed from a NON-closing reference keyword
	// (`Refs #N`, `Part of #N`, … — see softRefPattern) rather than a GitHub
	// closing keyword or the branch-name heuristic (kubestellar/hive#3980).
	// A soft reference is exactly the convention a careful PR uses when it
	// advances an issue without finishing it, so it must stop the contribute
	// queue re-offering the issue (the #3980 token-burn loop) — but it is a
	// weaker signal than `Fixes` and never HARD-gates the hive's own agent
	// pipeline; FilterClaimedIssues only defers on it for a bounded window.
	// omitempty-false back-compat mirrors ExternalAuthor: ledgers written
	// before #3980 only ever held closing-keyword/branch claims, which
	// correctly unmarshal as hard (false).
	SoftReference bool `json:"soft_reference,omitempty"`
	// FirstObservedAt is when this PR was FIRST seen making this claim, kept
	// stable across re-observations of the same PR (insertLocked carries it
	// forward), unlike ObservedAt which refreshes every scan. It anchors the
	// weakClaimAgentDeferralWindow: "how long has this weak claim been
	// deferring the agent pipeline" must age even while the claim keeps being
	// re-confirmed. Zero on entries written before #3980; firstObserved()
	// falls back to ObservedAt for those.
	FirstObservedAt time.Time `json:"first_observed_at"`
}

// firstObserved returns when the claiming PR was first seen making this claim,
// falling back to ObservedAt for ledger entries written before #3980 (whose
// FirstObservedAt is zero). The fallback errs toward MORE deferral only by the
// age the old entry had already accrued, never resets it.
func (c IssueClaim) firstObserved() time.Time {
	if c.FirstObservedAt.IsZero() {
		return c.ObservedAt
	}
	return c.FirstObservedAt
}

// strength ranks a claim for ledger-slot precedence when several open PRs
// claim the same issue. Higher wins. A hard (closing-keyword or branch-name)
// claim outranks a soft reference, and within each, hive-authored outranks
// external: the agent-side filter honours only hive-hard claims unbounded, so
// letting any weaker claim overwrite one would blind FilterClaimedIssues and
// re-open the duplicate-PR hole (#3768's precedence rule, generalized for
// #3980). Every claim, whatever its rank, suppresses the contribute queue.
func (c IssueClaim) strength() int {
	s := 0
	if !c.SoftReference {
		s += 2
	}
	if !c.ExternalAuthor {
		s++
	}
	return s
}

// Key identifies the issue a claim covers.
func (c IssueClaim) Key() string {
	return claimKey(c.Repo, c.Issue)
}

func claimKey(repo string, issue int) string {
	return fmt.Sprintf("%s#%d", repo, issue)
}

// closingKeywords is GitHub's full set of issue-closing keywords. Matching is
// case-insensitive. Source: GitHub docs, "Linking a pull request to an issue".
var closingKeywords = []string{
	"close", "closes", "closed",
	"fix", "fixes", "fixed",
	"resolve", "resolves", "resolved",
}

// claimRefPattern matches a closing keyword followed by an issue reference in
// either the same-repo (`#123`) or cross-repo (`owner/repo#123`) form.
//
//	(?i)                        case-insensitive
//	\b(close|closes|...)\b      a closing keyword as a whole word
//	\s*:?\s+                    optional colon, then whitespace
//	(?:([\w.-]+/[\w.-]+))?#(\d+) optional owner/repo prefix, then #N
//
// Recognized non-closing mentions (`Refs #12`, `Part of #12`, …) match the
// separate softRefPattern below (#3980); everything else ("see #12", a bare
// changelog-style "#12") matches neither pattern and claims nothing.
var claimRefPattern = regexp.MustCompile(
	`(?i)\b(` + strings.Join(closingKeywords, "|") + `)\b\s*:?\s+(?:([\w.-]+/[\w.-]+))?#(\d+)`)

// softReferenceKeywords are the recognized NON-closing reference forms — the
// convention a careful PR uses to link an issue it advances without claiming
// to close it (kubestellar/hive#3980, e.g. PR #3898's deliberate "Refs #3498").
// Regex fragments, not literals, because two forms are multi-word. The list is
// deliberately closed: a bare `#N` mention or free prose ("see #12") must
// never lock an issue, so only these keyword forms register.
var softReferenceKeywords = []string{
	`refs?`,            // Refs #N / Ref #N
	`part\s+of`,        // Part of #N
	`relate[sd]?\s+to`, // Relates to #N / Related to #N
	`addresses`,        // Addresses #N
}

// softRefPattern matches a soft-reference keyword followed by an issue
// reference, with the same case-insensitivity, optional colon, and cross-repo
// (`owner/repo#N`) forms claimRefPattern supports.
var softRefPattern = regexp.MustCompile(
	`(?i)\b(` + strings.Join(softReferenceKeywords, "|") + `)\b\s*:?\s+(?:([\w.-]+/[\w.-]+))?#(\d+)`)

// ParseClaimedIssues extracts every issue this text claims to close. defaultRepo
// supplies the repository for bare `#N` references. Results are de-duplicated
// and returned in first-seen order. A nil/empty text yields no claims.
func ParseClaimedIssues(text, defaultRepo string) []ClaimedRef {
	return parseIssueRefs(claimRefPattern, text, defaultRepo)
}

// ParseSoftReferencedIssues extracts every issue this text references with a
// recognized non-closing keyword (softReferenceKeywords). Same de-duplication
// and cross-repo semantics as ParseClaimedIssues; the two sets may overlap
// when a text says both `Fixes #N` and `refs #N` — FetchClaims resolves that
// overlap in favour of the closing (hard) claim.
func ParseSoftReferencedIssues(text, defaultRepo string) []ClaimedRef {
	return parseIssueRefs(softRefPattern, text, defaultRepo)
}

// parseIssueRefs is the shared extractor behind both keyword families. The
// pattern must expose the optional owner/repo prefix as group 2 and the issue
// number as group 3, as claimRefPattern and softRefPattern both do.
func parseIssueRefs(pattern *regexp.Regexp, text, defaultRepo string) []ClaimedRef {
	if text == "" {
		return nil
	}
	matches := pattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	var refs []ClaimedRef
	for _, m := range matches {
		// m[2] is the optional owner/repo prefix, m[3] the issue number.
		if len(m) < 4 {
			continue
		}
		num, err := strconv.Atoi(m[3])
		if err != nil || num <= 0 {
			continue
		}
		repo := defaultRepo
		if m[2] != "" {
			repo = m[2]
		}
		key := claimKey(repo, num)
		if seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, ClaimedRef{Repo: repo, Issue: num})
	}
	return refs
}

// ClaimedRef is a single parsed issue reference.
type ClaimedRef struct {
	Repo  string
	Issue int
}

// branchIssuePattern matches the branch-naming conventions agents use to encode
// the issue they are working on:
//
//	issue-423, issue/423, fix-issue-423, fix/423, 423-add-retry
//
// The number must be delimited (start of string or a -/_// separator, and end
// of string or a -/_// separator) so a version string like "bump-v2-1" or a
// SHA-ish suffix cannot be mistaken for an issue number.
var branchIssuePattern = regexp.MustCompile(`(?i)(?:^|[/_-])(?:issue[/_-]?|fix[/_-]|gh[/_-])?(\d+)(?:[/_-]|$)`)

// headRef returns the PR's head branch name, guarding the nested pointers.
func headRef(pr *gh.PullRequest) string {
	if pr == nil || pr.GetHead() == nil {
		return ""
	}
	return pr.GetHead().GetRef()
}

// issueFromBranchName recovers an issue number from a branch name. It is a
// heuristic, not a guarantee: it is only consulted when a PR carries no
// closing-keyword reference at all, and a false positive costs at most one
// suppressed kick for one cycle.
func issueFromBranchName(branch string) (int, bool) {
	if branch == "" {
		return 0, false
	}
	m := branchIssuePattern.FindStringSubmatch(branch)
	if len(m) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// HiveIdentity describes which PR authors count as "this hive". A PR opened by
// a human contributor must never suppress agent work, so the guard only honours
// claims from the hive's own accounts.
type HiveIdentity struct {
	// AIAuthor is the GitHub account agents open PRs as (project.ai_author).
	AIAuthor string
	// AppLogin is the GitHub App bot login, e.g. "kubestellar-hive[bot]".
	// It is set when the hive authenticates as an installation rather than a
	// personal token, in which case PRs are authored by the bot account.
	AppLogin string
}

// Matches reports whether the given PR author login belongs to this hive.
// Comparison is case-insensitive because GitHub logins are.
func (h HiveIdentity) Matches(author string) bool {
	if author == "" {
		return false
	}
	if h.AIAuthor != "" && strings.EqualFold(author, h.AIAuthor) {
		return true
	}
	if h.AppLogin != "" && strings.EqualFold(author, h.AppLogin) {
		return true
	}
	return false
}

// IsZero reports whether no identity is configured at all. With no identity we
// cannot tell our own PRs from anyone else's, so the guard declines to attribute
// live claims (and falls back to the ledger) rather than suppressing work based
// on a stranger's PR.
func (h HiveIdentity) IsZero() bool {
	return h.AIAuthor == "" && h.AppLogin == ""
}

// FetchClaims lists open PRs across the client's configured repos and returns
// the issue claims parsed from their titles and bodies — both closing-keyword
// (hard) claims and non-closing soft references, the latter marked
// SoftReference (#3980). Claims from PRs the
// hive did not author are included and marked ExternalAuthor
// (kubestellar/hive#3768): the contribute queue needs them to stop re-offering
// an issue a human contributor's open PR already fixes, while
// FilterClaimedIssues continues to honour only hive-authored claims for agent
// work.
//
// A per-repo API failure is reported via err but the successfully-scanned repos
// are still returned, so the caller can merge partial results into the ledger
// instead of discarding everything.
func (c *Client) FetchClaims(ctx context.Context, identity HiveIdentity) ([]IssueClaim, error) {
	if c == nil || c.client == nil {
		return nil, fmt.Errorf("nil github client")
	}
	if identity.IsZero() {
		return nil, fmt.Errorf("no hive identity configured (project.ai_author unset and no GitHub App login)")
	}

	now := time.Now()
	var claims []IssueClaim
	var firstErr error

	for _, repo := range c.getRepos() {
		owner, repoName := c.splitRepo(repo)
		opts := &gh.PullRequestListOptions{
			State:       "open",
			ListOptions: gh.ListOptions{PerPage: claimSearchPerPage},
		}
		for page := 0; page < claimSearchMaxPages; page++ {
			prs, resp, err := c.client.PullRequests.List(ctx, owner, repoName, opts)
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("listing open PRs for %s/%s: %w", owner, repoName, err)
				}
				if c.logger != nil {
					c.logger.Warn("pr-claim scan failed for repo", "repo", repo, "error", err)
				}
				break
			}
			for _, pr := range prs {
				if pr == nil {
					continue
				}
				author := safeGetLogin(pr.GetUser())
				// #3768: claims are no longer restricted to hive-authored PRs.
				// A human contributor's open PR claims its issue too — marked
				// ExternalAuthor below so the agent-side filter can keep
				// ignoring it while the contribute queue honours it.
				external := !identity.Matches(author)
				// Title and body are both scanned: `Fixes #N` conventionally
				// lives in the body, but many agents put it in the title.
				// Note: draft PRs are included — PullRequests.List(state=open)
				// returns them and nothing here filters on GetDraft(). That is
				// deliberate: a draft is still "someone is on it" for offer
				// suppression, and it is independent of fetchPRs' draft
				// handling for the actionable list (#3963/#3970), which this
				// scan does not share.
				text := pr.GetTitle() + "\n" + pr.GetBody()
				refs := ParseClaimedIssues(text, repo)

				// Secondary heuristic for PRs that reference no issue at all
				// (in the real incident, PR #443's body was literally "test").
				// Body parsing cannot see those, but agents conventionally
				// branch as issue-423 / fix-issue-423 / 423-some-slug, so the
				// head ref recovers the link. Only consulted when the body and
				// title yielded no CLOSING reference, so an explicit `Fixes #N`
				// always wins — and a numeric branch still hard-claims exactly
				// as it did before #3980 even when soft references are present.
				if len(refs) == 0 {
					if n, ok := issueFromBranchName(headRef(pr)); ok {
						refs = []ClaimedRef{{Repo: repo, Issue: n}}
					}
				}

				appendClaim := func(ref ClaimedRef, soft bool) {
					claims = append(claims, IssueClaim{
						Repo:            ref.Repo,
						Issue:           ref.Issue,
						PRNumber:        pr.GetNumber(),
						PRRepo:          repo,
						PRURL:           pr.GetHTMLURL(),
						PRAuthor:        author,
						ObservedAt:      now,
						FirstObservedAt: now,
						ExternalAuthor:  external,
						SoftReference:   soft,
					})
				}

				hardClaimed := make(map[string]bool, len(refs))
				for _, ref := range refs {
					hardClaimed[claimKey(ref.Repo, ref.Issue)] = true
					appendClaim(ref, false)
				}

				// #3980: non-closing references (`Refs #N`, `Part of #N`, …)
				// also claim — as SOFT claims. This is what makes PR #3898's
				// deliberate "Refs #3498 — no Fixes keyword" visible to the
				// contribute queue instead of the issue being re-offered every
				// cooldown window. An issue this same PR already hard-claims
				// is skipped: the closing claim is the stronger record.
				for _, ref := range ParseSoftReferencedIssues(text, repo) {
					if hardClaimed[claimKey(ref.Repo, ref.Issue)] {
						continue
					}
					appendClaim(ref, true)
				}
			}
			if resp == nil || resp.NextPage == 0 {
				break
			}
			opts.ListOptions.Page = resp.NextPage
		}
	}

	return claims, firstErr
}

// ClaimLedger is the persisted issue→PR claim mapping. It is the fail-closed
// half of the guard: when GitHub is unreachable (the real incident logged
// `401 Bad credentials`), a live-only check would see zero claims and happily
// duplicate. The ledger keeps the last known claim set instead.
type ClaimLedger struct {
	path   string
	logger *slog.Logger
	// mu guards claims. The ledger was single-goroutine (the eval cycle) until
	// #3768 gave the contribute WS hub a read path into it, so reads now happen
	// concurrently with the eval cycle's Reconcile/Save writes.
	mu sync.RWMutex
	// claims is keyed by "repo#issue".
	claims map[string]IssueClaim
	// ttl bounds entry lifetime; overridable for tests.
	ttl time.Duration
	// now is the clock, overridable for tests.
	now func() time.Time
}

// ledgerFile is the on-disk shape of the ledger.
type ledgerFile struct {
	SavedAt time.Time    `json:"saved_at"`
	Claims  []IssueClaim `json:"claims"`
}

// NewClaimLedger creates an empty ledger backed by path. Use LoadClaimLedger to
// read existing state from disk.
func NewClaimLedger(path string, logger *slog.Logger) *ClaimLedger {
	if path == "" {
		path = ClaimLedgerPath
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &ClaimLedger{
		path:   path,
		logger: logger,
		claims: make(map[string]IssueClaim),
		ttl:    claimLedgerTTL,
		now:    time.Now,
	}
}

// LoadClaimLedger reads the ledger from disk, dropping entries past the TTL.
// A missing file is not an error — it yields an empty ledger, which is the
// correct state on a hive's very first boot.
func LoadClaimLedger(path string, logger *slog.Logger) (*ClaimLedger, error) {
	l := NewClaimLedger(path, logger)
	data, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return l, fmt.Errorf("reading claim ledger %s: %w", l.path, err)
	}
	var file ledgerFile
	if err := json.Unmarshal(data, &file); err != nil {
		// A corrupt ledger must not wedge startup. Report it and continue with
		// an empty one; the next successful fetch repopulates it.
		return l, fmt.Errorf("parsing claim ledger %s: %w", l.path, err)
	}
	cutoff := l.now().Add(-l.ttl)
	for _, c := range file.Claims {
		if c.Issue <= 0 || c.Repo == "" {
			continue
		}
		if c.ObservedAt.Before(cutoff) {
			continue
		}
		l.insertLocked(c)
	}
	return l, nil
}

// insertLocked adds a claim to the map, resolving key collisions by claim
// strength: hard beats soft, and within each, hive-authored beats external
// (#3768's hive-precedence rule, generalized for #3980's soft references).
// A weaker claim can never overwrite a stronger one, so the agent-side filter
// — which hard-suppresses only on hive-hard claims — can never be blinded by
// a soft or external PR racing the stronger claim to the map. Equal-strength
// claims replace (refreshing ObservedAt); when the SAME PR is re-observed,
// its original FirstObservedAt is carried forward so the weak-claim deferral
// window ages instead of resetting every scan. Callers must hold l.mu (or own
// the ledger exclusively, as LoadClaimLedger does before publishing it).
func (l *ClaimLedger) insertLocked(c IssueClaim) {
	key := c.Key()
	if existing, ok := l.claims[key]; ok {
		if c.strength() < existing.strength() {
			return
		}
		c = carryFirstObserved(existing, c)
	}
	l.claims[key] = c
}

// carryFirstObserved preserves the earlier FirstObservedAt when the SAME PR is
// re-observed making the same claim, so the weak-claim deferral window ages
// across scans instead of resetting on every refresh (#3980). A DIFFERENT PR
// keeps its own (fresh) FirstObservedAt: a new claiming PR earns a new window.
func carryFirstObserved(existing, c IssueClaim) IssueClaim {
	if existing.PRRepo == c.PRRepo && existing.PRNumber == c.PRNumber {
		if first := existing.firstObserved(); c.FirstObservedAt.IsZero() || first.Before(c.FirstObservedAt) {
			c.FirstObservedAt = first
		}
	}
	return c
}

// SetTTL overrides the entry lifetime. Intended for tests.
func (l *ClaimLedger) SetTTL(d time.Duration) {
	if l == nil || d <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ttl = d
}

// SetClock overrides the ledger's time source. Intended for tests.
func (l *ClaimLedger) SetClock(fn func() time.Time) {
	if l == nil || fn == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = fn
}

// Claims returns the current claim set, sorted for stable output.
func (l *ClaimLedger) Claims() []IssueClaim {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	out := make([]IssueClaim, 0, len(l.claims))
	for _, c := range l.claims {
		out = append(out, c)
	}
	l.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Issue < out[j].Issue
	})
	return out
}

// Len reports the number of claims currently held.
func (l *ClaimLedger) Len() int {
	if l == nil {
		return 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.claims)
}

// Lookup returns the claim covering the given issue, if any.
func (l *ClaimLedger) Lookup(repo string, issue int) (IssueClaim, bool) {
	if l == nil {
		return IssueClaim{}, false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	c, ok := l.claims[claimKey(repo, issue)]
	return c, ok
}

// Reconcile merges a freshly-fetched claim set into the ledger.
//
// When live is authoritative (a complete, successful scan), claims that were
// present before but absent now are dropped: their PR was closed, merged, or
// its body edited, so the issue is released back for work. That is what makes
// a closed-without-merge PR re-open its issue.
//
// When live is NOT authoritative (the API call failed, wholly or partly), the
// previous claims are retained and only refreshed with whatever did come back.
// This is the fail-closed path: suppressing one legitimate kick for a cycle is
// cheap; nine duplicate PRs are not.
func (l *ClaimLedger) Reconcile(live []IssueClaim, authoritative bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if authoritative {
		// The whole map is rebuilt, but a claim's FirstObservedAt must survive
		// the rebuild when the same PR is re-observed — otherwise every scan
		// would restart the weak-claim deferral window and it could never
		// expire (#3980).
		prev := l.claims
		l.claims = make(map[string]IssueClaim, len(live))
		for _, c := range live {
			if c.Issue <= 0 || c.Repo == "" {
				continue
			}
			if existing, ok := prev[c.Key()]; ok {
				c = carryFirstObserved(existing, c)
			}
			l.insertLocked(c)
		}
		return
	}
	for _, c := range live {
		if c.Issue <= 0 || c.Repo == "" {
			continue
		}
		l.insertLocked(c)
	}
	l.pruneLocked()
}

// pruneLocked drops entries older than the TTL. It runs on the
// non-authoritative path so a permanently-failing API cannot pin a stale claim
// forever. Callers must hold l.mu.
func (l *ClaimLedger) pruneLocked() {
	cutoff := l.now().Add(-l.ttl)
	for k, c := range l.claims {
		if c.ObservedAt.Before(cutoff) {
			delete(l.claims, k)
		}
	}
}

// Save writes the ledger to disk atomically (write temp, then rename), matching
// the convention used by the other /data state writers in this repo.
func (l *ClaimLedger) Save() error {
	if l == nil {
		return nil
	}
	data, err := json.MarshalIndent(ledgerFile{
		SavedAt: l.now(),
		Claims:  l.Claims(),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling claim ledger: %w", err)
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, data, claimLedgerFileMode); err != nil {
		return fmt.Errorf("writing temp claim ledger: %w", err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return fmt.Errorf("renaming claim ledger: %w", err)
	}
	return nil
}

// RedStaleFunc reports whether the claiming PR (prRepo, prNumber) is red on a
// required check AND stale (its red head SHA unchanged past the staleness
// threshold). Fix #3 uses it to DECIDE NOT to suppress a claimed issue: a
// healthy claiming PR (green/pending, or a freshly-changed head) still
// suppresses its issue, but a red+stale one releases the issue so the work can
// be re-taken. It is supplied by the caller so this package stays free of the
// escalation-store dependency; a nil func means "never release" (old behavior).
// GENERIC: the predicate keys only off check state + commit staleness.
type RedStaleFunc func(prRepo string, prNumber int) bool

// FilterClaimedIssues removes from result every issue an open PR already
// claims, logging each suppression with the claiming PR's URL. A hive-authored
// closing claim suppresses unconditionally (the original #3792 guard); a WEAK
// claim — external authorship or a soft `Refs`-style reference (#3980) —
// suppresses only within weakClaimAgentDeferralWindow of its first
// observation, then releases the issue so no stranger's or stalled PR can
// freeze the pipeline indefinitely.
//
// redStale (may be nil) is Fix #3's release valve: when it reports the claiming
// PR is red+stale, the issue is NOT suppressed — it is kept actionable so a
// fresh fix can happen instead of the issue being frozen behind a dead PR.
//
// It mutates result in place (Issues.Items, Issues.Count, Issues.SLAViolations)
// and returns the number of issues suppressed. A nil result or nil ledger is a
// no-op, so callers need no extra guards.
func FilterClaimedIssues(result *ActionableResult, ledger *ClaimLedger, redStale RedStaleFunc, logger *slog.Logger) int {
	if result == nil || ledger == nil || ledger.Len() == 0 {
		return 0
	}
	kept := make([]Issue, 0, len(result.Issues.Items))
	suppressed := 0
	for _, issue := range result.Issues.Items {
		claim, ok := ledger.Lookup(issue.Repo, issue.Number)
		if !ok {
			kept = append(kept, issue)
			continue
		}
		// Fix #3: release (do NOT suppress) when the claiming PR is red on a
		// required check AND stale. The claiming PR lives in claim.PRRepo /
		// claim.PRNumber (which may differ from the issue's repo for cross-repo
		// closes). A healthy PR — green, pending, or a recently-moved head —
		// fails this predicate and is still suppressed, exactly as before.
		// Checked before the weak-claim deferral: a dead PR should not defer
		// anything either.
		if redStale != nil && redStale(claim.PRRepo, claim.PRNumber) {
			kept = append(kept, issue)
			if logger != nil {
				logger.Info("releasing issue: claiming PR red+stale",
					"repo", issue.Repo,
					"issue", issue.Number,
					"issue_title", issue.Title,
					"claimed_by_pr", claim.PRNumber,
					"pr_repo", claim.PRRepo,
					"pr_url", claim.PRURL,
				)
			}
			continue
		}
		// WEAK claims — external authorship (#3768) or a non-closing soft
		// reference (#3980) — DEFER agent work for a bounded window instead of
		// the old all-or-nothing: #3768/#3792 let external claims through
		// unconditionally so a stranger's junk PR could never freeze the
		// pipeline, but that left the original dakota#353 duplicate shape
		// alive across the agent/contributor seam (an agent could start an
		// issue a contributor's open PR was already fixing). Within
		// weakClaimAgentDeferralWindow of the claim's first observation the
		// issue is suppressed like any other claim; past it, the issue is
		// released even though the claim persists, so the junk-PR freeze is
		// bounded rather than possible. Only a hive-authored closing claim
		// (the hive's own live PR) suppresses without a window.
		if claim.ExternalAuthor || claim.SoftReference {
			if time.Since(claim.firstObserved()) >= weakClaimAgentDeferralWindow {
				kept = append(kept, issue)
				if logger != nil {
					logger.Info("weak claim past deferral window: keeping issue actionable",
						"repo", issue.Repo,
						"issue", issue.Number,
						"claimed_by_pr", claim.PRNumber,
						"pr_repo", claim.PRRepo,
						"pr_url", claim.PRURL,
						"external_author", claim.ExternalAuthor,
						"soft_reference", claim.SoftReference,
					)
				}
				continue
			}
		}
		suppressed++
		if logger != nil {
			logger.Info("suppressing issue already claimed by an open hive PR",
				"repo", issue.Repo,
				"issue", issue.Number,
				"issue_title", issue.Title,
				"claimed_by_pr", claim.PRNumber,
				"pr_url", claim.PRURL,
				"pr_author", claim.PRAuthor,
			)
		}
	}
	if suppressed == 0 {
		return 0
	}

	slaViolations := 0
	for _, issue := range kept {
		if issue.AgeMinutes > slaThresholdMinutes {
			slaViolations++
		}
	}
	result.Issues.Items = kept
	result.Issues.Count = len(kept)
	result.Issues.SLAViolations = slaViolations
	return suppressed
}

// ApplyDuplicatePRGuard is the single entry point wired into the enumeration
// cycle. It fetches live claims, reconciles them into the persisted ledger,
// saves the ledger, and filters the actionable set.
//
// On a fetch failure it does NOT clear the ledger — it keeps the last known
// claim set and filters with that (fail closed). It returns the number of
// issues suppressed.
func ApplyDuplicatePRGuard(
	ctx context.Context,
	client *Client,
	ledger *ClaimLedger,
	identity HiveIdentity,
	result *ActionableResult,
	redStale RedStaleFunc,
	logger *slog.Logger,
) int {
	if ledger == nil || result == nil {
		return 0
	}

	live, err := client.FetchClaims(ctx, identity)
	authoritative := err == nil
	if err != nil && logger != nil {
		logger.Warn("duplicate-PR guard: claim fetch failed, falling back to persisted ledger (fail closed)",
			"error", err,
			"cached_claims", ledger.Len(),
		)
	}
	ledger.Reconcile(live, authoritative)

	if saveErr := ledger.Save(); saveErr != nil && logger != nil {
		logger.Warn("duplicate-PR guard: failed to persist claim ledger", "error", saveErr)
	}

	suppressed := FilterClaimedIssues(result, ledger, redStale, logger)
	if logger != nil {
		logger.Info("duplicate-PR guard applied",
			"claims", ledger.Len(),
			"suppressed", suppressed,
			"authoritative", authoritative,
		)
	}
	return suppressed
}
