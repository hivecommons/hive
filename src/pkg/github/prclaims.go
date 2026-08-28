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

	// weakClaimDeferWindow bounds how long a WEAK claim — an external-authored
	// PR (#3768) or a non-closing "Refs #N" reference (#3980) — defers agent
	// dispatch of the issue it covers (kubestellar/hive#4929).
	//
	// Weak claims used to be ignored outright on the agent side, on the
	// reasoning that a PR which never claimed to close the issue must not
	// freeze work on the remainder, and that a stranger's PR must never freeze
	// the hive's own pipeline. Both hold. What neither anticipated is an agent
	// that CANNOT SEE the claiming PR: the scanner's hold-gated policy forbids
	// `gh pr list`/`gh issue list` and makes the kick message its only source
	// of truth, so an issue re-offered under a weak claim is not re-examined,
	// it is re-implemented — and the same policy is what tells agents to write
	// `Refs #N` for multi-phase work, so following the rules is what makes the
	// claim invisible.
	//
	// The window is the bounded middle ground: within it a weak claim
	// suppresses like any other, past it the issue releases even while the PR
	// stays open. Nothing is ever frozen — an adversary must keep opening NEW
	// PRs to extend the delay, which PR creation itself rate-limits — and
	// partially-addressed issues still come back for their remainder. 72h
	// matches claimLedgerTTL so the two bounds on a claim's influence agree.
	weakClaimDeferWindow = 72 * time.Hour

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
	// FirstObservedAt is when this PR was FIRST seen claiming this issue. It
	// anchors the weak-claim deferral window (#4929), and is deliberately not
	// refreshed the way ObservedAt is: the window must age across scans, or a
	// claim re-confirmed every enumeration cycle would restart its own deadline
	// forever and the "bounded" window would be a permanent freeze.
	//
	// Carried forward by Reconcile whenever the SAME PR is re-observed for the
	// same issue; a different PR claiming the issue starts a fresh window,
	// because that is genuinely new work to be deferred behind. Zero on ledgers
	// written before this field existed, which readers normalize to ObservedAt.
	FirstObservedAt time.Time `json:"first_observed_at,omitempty"`
	// ExternalAuthor marks a claim whose PR was opened by an account that is
	// NOT this hive (kubestellar/hive#3768). External claims exist so the
	// contribute queue can stop offering an issue that a human contributor's
	// open PR already fixes. On the agent side, FilterClaimedIssues treats this
	// as a WEAK claim (kubestellar/hive#4929): it defers dispatch for a bounded
	// window rather than either freezing it forever or ignoring it outright, so
	// a stranger's PR still cannot permanently freeze the hive's own pipeline.
	// The field is omitempty-false so ledgers written before #3768 — which only
	// ever recorded hive-authored claims — unmarshal as hive-authored (false),
	// keeping their agent-side suppression intact across the upgrade.
	ExternalAuthor bool `json:"external_author,omitempty"`
	// Reference marks a WEAK claim, recovered from a non-closing reference
	// ("Refs #N", "Part of #N") rather than a closing keyword
	// (kubestellar/hive#3980). Such a PR is demonstrably working the issue but
	// deliberately does not claim to close it — the correct convention when a
	// PR only partially addresses an issue, or when the remainder is gated on a
	// maintainer decision. Before this existed, writing `Refs #N` instead of
	// `Fixes #N` made the work invisible to the guard, and the contribute queue
	// re-offered the issue every cooldown window forever.
	//
	// Like ExternalAuthor, the field GRADES the claim rather than deciding for
	// its consumers: the contribute queue honours reference claims (that is the
	// loop being closed), while FilterClaimedIssues defers agent work behind
	// them for a bounded window (kubestellar/hive#4929): long enough that an
	// agent unable to see the claiming PR (the scanner's hold-gated policy
	// forbids `gh pr list`) is not handed an issue its own open PR already
	// covers, but bounded so a PR that never claimed to finish the issue does
	// not permanently strand its remainder. Also omitempty-false, so pre-#3980
	// ledgers unmarshal as strong claims and keep their existing suppression
	// across the upgrade.
	Reference bool `json:"reference,omitempty"`
}

// claimRank orders claims by evidential strength, highest first. It exists so
// insertLocked can resolve key collisions with a single comparison instead of a
// growing pile of pairwise special cases.
//
// A closing keyword outranks a bare reference (an explicit "this closes it" is
// stronger evidence than "this is about it"), and within each tier a
// hive-authored PR outranks an external one — the #3768 precedence rule, which
// exists so the agent-side filter can never be blinded by an external PR racing
// the map. Equal ranks let the later claim win, matching the pre-#3980 behavior
// of a plain map assignment.
func claimRank(c IssueClaim) int {
	switch {
	case !c.Reference && !c.ExternalAuthor:
		return 3
	case !c.Reference && c.ExternalAuthor:
		return 2
	case c.Reference && !c.ExternalAuthor:
		return 1
	default:
		return 0
	}
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
// Non-closing mentions ("see #12", "related to #12") deliberately do NOT match:
// only a PR that claims to close an issue should suppress work on it.
var claimRefPattern = regexp.MustCompile(
	`(?i)\b(` + strings.Join(closingKeywords, "|") + `)\b\s*:?\s+(?:([\w.-]+/[\w.-]+))?#(\d+)`)

// ParseClaimedIssues extracts every issue this text claims to close. defaultRepo
// supplies the repository for bare `#N` references. Results are de-duplicated
// and returned in first-seen order. A nil/empty text yields no claims.
func ParseClaimedIssues(text, defaultRepo string) []ClaimedRef {
	return parseRefs(claimRefPattern, text, defaultRepo)
}

// parseRefs is the shared body behind ParseClaimedIssues and
// ParseReferencedIssues. Both patterns expose the same three capture groups
// (keyword, optional owner/repo, issue number), so the extraction, repo
// defaulting and de-duplication are identical and live here once.
func parseRefs(pattern *regexp.Regexp, text, defaultRepo string) []ClaimedRef {
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

// referenceKeywords are the idioms a PR uses to say "I am working on this
// issue" WITHOUT claiming to close it (kubestellar/hive#3980). Longest
// alternatives are listed first so the intended word wins the match.
//
// The set is deliberately narrow. "relates to", "related to" and "see" are
// EXCLUDED: they mark a topical cross-reference ("unlike #12", "see #12 for
// background"), not a statement that this PR does work on that issue, and
// treating them as claims would let a passing mention withhold an issue from
// the contribute queue.
var referenceKeywords = []string{
	"references", "referencing", "refs", "ref",
	"addresses", "addressing", "addressed", "address",
	"contributes to", "part of", "towards", "toward",
}

// referenceRefPattern matches a non-closing reference keyword followed by an
// issue reference, allowing a bounded run of prose between the two:
//
//	(?i)                          case-insensitive
//	\b(references|refs|...)\b     a reference keyword as a whole word
//	[^.\n#]{0,40}?                lazy, bounded gap — see below
//	(?:([\w.-]+/[\w.-]+))?#(\d+)  optional owner/repo prefix, then #N
//
// The gap exists because these keywords are written as prose, not as the
// machine-readable trailer a closing keyword conventionally is. The real PR
// that motivated #3980 reads "Addresses a `ci-maintainer` finding from the
// #2364 advisory digest" — keyword and reference separated by six words. A
// strict `keyword\s+#N` would miss it.
//
// The gap is bounded three ways so it cannot swallow a whole paragraph and
// attach a keyword to an unrelated number further down: at most 40 characters,
// lazily matched, and containing no '.', newline, or '#'. Excluding '.' stops
// it crossing a sentence boundary; excluding '#' stops it skipping over a
// nearer reference to reach a further one.
//
// A false positive here costs one issue not being OFFERED to the contribute
// queue while that PR stays open, and is released as soon as the PR closes.
// On the agent side it can defer dispatch for up to weakClaimDeferWindow
// (kubestellar/hive#4929) — never longer, and never while the PR is closed.
var referenceRefPattern = regexp.MustCompile(
	`(?i)\b(` + strings.Join(referenceKeywords, "|") + `)\b[^.\n#]{0,40}?(?:([\w.-]+/[\w.-]+))?#(\d+)`)

// ParseReferencedIssues extracts every issue this text REFERENCES without
// claiming to close (kubestellar/hive#3980). defaultRepo supplies the
// repository for bare `#N` references. Results are de-duplicated and returned
// in first-seen order; nil/empty text yields no refs.
//
// It is the weak-evidence companion to ParseClaimedIssues and is only consulted
// when that (and the branch-name heuristic) found nothing at all.
func ParseReferencedIssues(text, defaultRepo string) []ClaimedRef {
	return parseRefs(referenceRefPattern, text, defaultRepo)
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
// the issue claims parsed from their titles and bodies. Claims from PRs the
// hive did not author are included and marked ExternalAuthor
// (kubestellar/hive#3768): the contribute queue needs them to stop re-offering
// an issue a human contributor's open PR already fixes, while
// FilterClaimedIssues treats them as a WEAK claim that defers (but does not
// permanently suppress) agent work (kubestellar/hive#4929).
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
				text := pr.GetTitle() + "\n" + pr.GetBody()
				refs := ParseClaimedIssues(text, repo)

				// Secondary heuristic for PRs that reference no issue at all
				// (in the real incident, PR #443's body was literally "test").
				// Body parsing cannot see those, but agents conventionally
				// branch as issue-423 / fix-issue-423 / 423-some-slug, so the
				// head ref recovers the link. Only consulted when the body and
				// title yielded nothing, so an explicit `Fixes #N` always wins.
				if len(refs) == 0 {
					if n, ok := issueFromBranchName(headRef(pr)); ok {
						refs = []ClaimedRef{{Repo: repo, Issue: n}}
					}
				}

				// Third and weakest tier (#3980): a PR that references an issue
				// without a closing keyword ("Refs #N", "Part of #N") and whose
				// branch name carries no issue number. Such a PR is working the
				// issue but deliberately not claiming to finish it, and until
				// now produced NO claim at all — so the contribute queue kept
				// re-offering an issue whose work was already open in a PR.
				//
				// Ordered last on purpose. The two tiers above are unchanged and
				// still win, so no claim that exists today changes its target or
				// its strength; this only fills in PRs that previously yielded
				// nothing. Marked Reference so FilterClaimedIssues can keep
				// agent work flowing on a partially-addressed issue.
				reference := false
				if len(refs) == 0 {
					if refRefs := ParseReferencedIssues(text, repo); len(refRefs) > 0 {
						refs = refRefs
						reference = true
					}
				}

				for _, ref := range refs {
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
						Reference:       reference,
					})
				}
			}
			if resp == nil || resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
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
	// weakDefer is how long a weak claim defers agent dispatch (#4929);
	// overridable for tests.
	weakDefer time.Duration
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
		path:      path,
		logger:    logger,
		claims:    make(map[string]IssueClaim),
		ttl:       claimLedgerTTL,
		weakDefer: weakClaimDeferWindow,
		now:       time.Now,
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
		// Ledgers written before FirstObservedAt existed carry a zero anchor.
		// Normalizing to ObservedAt costs such a claim at most one extra
		// window and keeps the field's invariant — always anchored once
		// stored — true for every reader downstream.
		if c.FirstObservedAt.IsZero() {
			c.FirstObservedAt = c.ObservedAt
		}
		l.insertLocked(c)
	}
	return l, nil
}

// anchorFirstObservedLocked stamps c.FirstObservedAt, carrying the anchor
// forward from prev when the SAME pull request was already claiming the SAME
// issue. That carry-forward is the whole mechanism: Reconcile's authoritative
// path REPLACES the claim map every enumeration cycle, so a claim that took its
// anchor from the fresh scan would reset its deferral window on every pass and
// the bound would never be reached.
//
// A different PR claiming the issue, or a first sighting, anchors at this
// observation. Callers must hold l.mu.
func (l *ClaimLedger) anchorFirstObservedLocked(prev map[string]IssueClaim, c IssueClaim) IssueClaim {
	if existing, ok := prev[c.Key()]; ok &&
		existing.PRNumber == c.PRNumber && existing.PRRepo == c.PRRepo &&
		!existing.FirstObservedAt.IsZero() {
		c.FirstObservedAt = existing.FirstObservedAt
		return c
	}
	if c.FirstObservedAt.IsZero() {
		c.FirstObservedAt = c.ObservedAt
	}
	if c.FirstObservedAt.IsZero() {
		c.FirstObservedAt = l.now()
	}
	return c
}

// weakDeferExpired reports whether a weak claim's deferral window has elapsed,
// i.e. whether the issue should be released back for agent work despite the
// claiming PR still being open. A claim with no usable anchor is treated as
// NOT expired: an unanchored claim is one we just started tracking, and
// defaulting to "expired" would make the very first cycle after an upgrade
// re-offer everything the window exists to hold back.
func (l *ClaimLedger) weakDeferExpired(c IssueClaim) bool {
	if l == nil {
		return true
	}
	l.mu.RLock()
	window, now := l.weakDefer, l.now()
	l.mu.RUnlock()
	if window <= 0 {
		return true
	}
	anchor := c.FirstObservedAt
	if anchor.IsZero() {
		anchor = c.ObservedAt
	}
	if anchor.IsZero() {
		return false
	}
	return now.Sub(anchor) >= window
}

// insertLocked adds a claim to the map, resolving key collisions with
// hive-precedence: when the same issue is claimed by both a hive-authored PR
// and an external one (#3768), the hive-authored claim wins, so the agent-side
// filter — which honours only hive claims — can never be blinded by an
// external PR racing it to the map. Callers must hold l.mu (or own the ledger
// exclusively, as LoadClaimLedger does before publishing it).
func (l *ClaimLedger) insertLocked(c IssueClaim) {
	key := c.Key()
	if existing, ok := l.claims[key]; ok && claimRank(c) < claimRank(existing) {
		return
	}
	l.claims[key] = c
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

// SetWeakDeferWindow overrides how long a weak claim defers agent dispatch.
// Intended for tests. A non-positive duration is ignored rather than treated as
// "defer nothing", so a mis-set value cannot silently reopen #4929.
func (l *ClaimLedger) SetWeakDeferWindow(d time.Duration) {
	if l == nil || d <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.weakDefer = d
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
		// prev is read for deferral anchors only; the map itself is replaced,
		// which is what drops claims whose PR closed.
		prev := l.claims
		l.claims = make(map[string]IssueClaim, len(live))
		for _, c := range live {
			if c.Issue <= 0 || c.Repo == "" {
				continue
			}
			l.insertLocked(l.anchorFirstObservedLocked(prev, c))
		}
		return
	}
	for _, c := range live {
		if c.Issue <= 0 || c.Repo == "" {
			continue
		}
		l.insertLocked(l.anchorFirstObservedLocked(l.claims, c))
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

// FilterClaimedIssues removes from result every issue an open PR already claims,
// logging each suppression with the claiming PR's URL.
//
// A claim that asserts it CLOSES the issue, from a PR this hive authored,
// suppresses for as long as it stands. A WEAK claim — external authorship
// (#3768) or a non-closing "Refs #N" reference (#3980) — suppresses only for
// weakClaimDeferWindow from when it was first observed, then releases even
// while the PR stays open (#4929). Weak claims used to be ignored here
// entirely; see the branch below for why that stopped being safe.
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
		// A WEAK claim is one that does not assert it closes the issue: an
		// EXTERNAL PR the hive did not author (#3768), or a non-closing
		// "Refs #N" reference (#3980). Both used to be waved straight through
		// here, on two rules that still hold — a stranger's junk PR must never
		// freeze the hive's own pipeline, and a PR that never claimed to finish
		// an issue must not strand the remainder behind it.
		//
		// What they missed is the agent that cannot look (#4929). The scanner's
		// hold-gated policy forbids `gh pr list`/`gh issue list`, so an issue
		// re-offered under a weak claim is not re-examined against the open PR,
		// it is re-implemented from scratch — twice on one issue, two days
		// apart, in the report that prompted this. Waving the issue through
		// only avoids stranding when someone downstream can SEE the claim.
		//
		// So weak claims now DEFER rather than either freeze or vanish: they
		// suppress for weakClaimDeferWindow measured from when the PR was first
		// seen claiming the issue, then release even while it stays open. Both
		// original rules survive in bounded form.
		if claim.ExternalAuthor || claim.Reference {
			// The red+stale valve applies BEFORE the window: a dead PR defers
			// nothing, which keeps an abandoned weak claim from costing the
			// issue three days.
			if redStale != nil && redStale(claim.PRRepo, claim.PRNumber) {
				kept = append(kept, issue)
				continue
			}
			if ledger.weakDeferExpired(claim) {
				kept = append(kept, issue)
				if logger != nil {
					logger.Info("releasing issue: weak claim past its deferral window",
						"repo", issue.Repo,
						"issue", issue.Number,
						"claimed_by_pr", claim.PRNumber,
						"pr_repo", claim.PRRepo,
						"pr_url", claim.PRURL,
						"external", claim.ExternalAuthor,
						"reference", claim.Reference,
						"first_observed", claim.FirstObservedAt,
					)
				}
				continue
			}
			suppressed++
			if logger != nil {
				logger.Info("deferring issue: open PR weakly claims it",
					"repo", issue.Repo,
					"issue", issue.Number,
					"issue_title", issue.Title,
					"claimed_by_pr", claim.PRNumber,
					"pr_repo", claim.PRRepo,
					"pr_url", claim.PRURL,
					"pr_author", claim.PRAuthor,
					"external", claim.ExternalAuthor,
					"reference", claim.Reference,
					"first_observed", claim.FirstObservedAt,
				)
			}
			continue
		}
		// Fix #3: release (do NOT suppress) when the claiming PR is red on a
		// required check AND stale. The claiming PR lives in claim.PRRepo /
		// claim.PRNumber (which may differ from the issue's repo for cross-repo
		// closes). A healthy PR — green, pending, or a recently-moved head —
		// fails this predicate and is still suppressed, exactly as before.
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
