package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/spf13/cobra"
)

// `hivectl evidence backfill` (hivecommons/hive#12371) reconstructs schema v1
// evidence bundles from GitHub pull-request history so the study
// (RFC #12062, pre-registration src/docs/research/preregistration-2026-10.md)
// has a pre-Hive baseline to compare against. It is record-only: it writes
// files under the evidence root and reads nothing that decides a merge.
//
// What history can tell us is written; what it cannot is left ABSENT:
// verdicts, ci, sentinel and shadow are never fabricated, and every bundle
// carries backfilled: true plus a backfill block naming the source, the
// outcome and the exclusion flags that apply.

const (
	evidenceBackfillSource     = "github-api"
	evidenceBackfillAPIDefault = "https://api.github.com"
	evidenceBackfillTokenEnv   = "GITHUB_TOKEN"
	evidenceBackfillPerPage    = 100
	// evidenceBackfillMinMerged is the pre-registration's population floor:
	// repos with fewer merged PRs in the window are flagged, not dropped.
	evidenceBackfillMinMerged = 10
	// evidenceBackfillHiveRepo is Hive developing Hive, which the
	// pre-registration reports separately and never pools.
	evidenceBackfillHiveRepo = "hivecommons/hive"
	// evidenceBackfillUnrecorded is the merge actor/method when the forge
	// did not say. It is a label for "unknown", not a guess.
	evidenceBackfillUnrecorded = "unrecorded"
	// evidenceBackfillMaxWait caps one rate-limit pause; longer resets abort
	// with the time to retry so an operator is never left hanging silently.
	evidenceBackfillMaxWait = 15 * time.Minute
)

// Author kind rules, in the order they are tried. Each bundle records the
// one that fired in backfill.author_kind_rule.
const (
	authorRuleTrailer   = "attribution_trailer" // Hive's "— hive:" trailer in the body
	authorRuleAgentList = "agent_login"         // login named by --agent-login
	authorRuleFooter    = "identity_footer"     // Hive's 🐝 identity footer in the body
	authorRuleBot       = "bot_account"         // GitHub type Bot or "[bot]" suffix
	authorRuleHuman     = "default_human"       // everything else
)

// hiveIdentityFooterMarker is the visible prefix of the footer
// bin/gh-wrapper.sh appends to agent-authored bodies.
const hiveIdentityFooterMarker = "🐝 **Hive Agent**"

type evidenceBackfillOptions struct {
	repo        string
	since       string
	until       string
	root        string
	dryRun      bool
	acmmLevel   int
	agentLogins []string
	flags       []string
	minMerged   int
	apiBase     string
	tokenEnv    string
	signingKey  string
	skipMergers bool
}

func newEvidenceBackfillCommand(env *commandEnv) *cobra.Command {
	opts := &evidenceBackfillOptions{}
	cmd := &cobra.Command{
		Use:   "backfill --repo owner/repo --since DATE",
		Short: "Build schema v1 evidence bundles from GitHub pull-request history (record-only)",
		Long: "Reconstruct evidence bundles for pull requests created in [--since, --until] from the\n" +
			"GitHub API and write them under the evidence root, one per PR head. Only what history\n" +
			"records is written: author (with a documented kind heuristic), outcome and timestamps,\n" +
			"merge actor, and revert / follow-up links under the same rules as the live recorder.\n" +
			"Verdicts, CI, sentinel and shadow observations are left absent, never fabricated, and\n" +
			"every bundle is marked backfilled: true. Existing backfilled bundles and live bundles that already\n" +
			"record a merge are skipped; a live bundle without a merge gets a sibling .backfill.json\n" +
			"(reported as supplemented), so re-running is safe. Nothing here reads into or alters merge eligibility.\n\n" +
			"Authentication: the token in $GITHUB_TOKEN (or --github-token-env), else `gh auth token`.",
		Args: argsNone(),
		Example: "  hivectl evidence backfill --repo ublue-os/bluefin --since 2025-01-01 --dry-run\n" +
			"  hivectl evidence backfill --repo kubestellar/console --since 2026-03-01 --until 2026-05-01 --acmm-level 3\n" +
			"  hivectl evidence backfill --repo hivecommons/hive --since 2026-06-01 --flag hive_on_hive --root ./evidence",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEvidenceBackfill(cmd, env, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.repo, "repo", "", "repository as owner/repo (required)")
	f.StringVar(&opts.since, "since", "", "include PRs created at or after this date (YYYY-MM-DD or RFC 3339; required)")
	f.StringVar(&opts.until, "until", "", "include PRs created before this date (default: now)")
	f.StringVar(&opts.root, "root", "", "evidence root directory (default: the hive's /data/evidence)")
	f.BoolVar(&opts.dryRun, "dry-run", false, "fetch and classify but write nothing; print counts only")
	f.IntVar(&opts.acmmLevel, "acmm-level", 0, "ACMM level known to be in force for this window (0 = not recorded)")
	f.StringArrayVar(&opts.agentLogins, "agent-login", nil, "login to classify as an agent author (repeatable)")
	f.StringArrayVar(&opts.flags, "flag", nil, "exclusion flag to stamp on every bundle: "+strings.Join(evidence.BackfillFlags, ", ")+" (repeatable)")
	f.IntVar(&opts.minMerged, "min-merged", evidenceBackfillMinMerged, "flag the repo under_min_merged when fewer PRs merged in the window")
	f.StringVar(&opts.apiBase, "github-api", evidenceBackfillAPIDefault, "GitHub REST API base URL")
	f.StringVar(&opts.tokenEnv, "github-token-env", evidenceBackfillTokenEnv, "environment variable holding the GitHub token")
	f.StringVar(&opts.signingKey, "signing-key-file", "", "Ed25519 key to sign written bundles (default: unsigned)")
	f.BoolVar(&opts.skipMergers, "skip-mergers", false, "do not fetch each merged PR for its merger (one request per merged PR)")
	return cmd
}

// backfillPR is the subset of the GitHub pull-request object the backfill
// reads. Field names follow the REST API.
type backfillPR struct {
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	State          string     `json:"state"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ClosedAt       *time.Time `json:"closed_at"`
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	User           struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
	} `json:"base"`
	MergedBy *struct {
		Login string `json:"login"`
	} `json:"merged_by"`
}

// backfillFetcher talks to the GitHub REST API with the smallest surface the
// backfill needs. sleep is a hook so tests can observe rate-limit pauses
// without waiting.
type backfillFetcher struct {
	base   string
	token  string
	client *http.Client
	sleep  func(context.Context, time.Duration) error
	calls  int
}

func newBackfillFetcher(base, token string) *backfillFetcher {
	return &backfillFetcher{
		base:   strings.TrimRight(base, "/"),
		token:  token,
		client: &http.Client{Timeout: 60 * time.Second},
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
}

// get performs one GET and decodes JSON into out, pausing on a primary or
// secondary rate limit and retrying once the window resets.
func (f *backfillFetcher) get(ctx context.Context, path string, query url.Values, out any) error {
	u := f.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "hivectl-evidence-backfill")
		if f.token != "" {
			req.Header.Set("Authorization", "Bearer "+f.token)
		}
		f.calls++
		resp, err := f.client.Do(req)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if wait, limited := backfillRateLimitWait(resp, time.Now()); limited {
			if wait > evidenceBackfillMaxWait {
				return fmt.Errorf("github rate limit exhausted; retry after %s", wait.Round(time.Second))
			}
			if err := f.sleep(ctx, wait); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			msg := strings.TrimSpace(string(body))
			if len(msg) > 200 {
				msg = msg[:200] + "…"
			}
			return fmt.Errorf("github %s: HTTP %d: %s", path, resp.StatusCode, msg)
		}
		return json.Unmarshal(body, out)
	}
}

// backfillRateLimitWait reports whether resp is a rate-limit response and
// how long to pause before retrying: Retry-After when present, else until
// X-RateLimit-Reset, else one minute.
func backfillRateLimitWait(resp *http.Response, now time.Time) (time.Duration, bool) {
	limited := resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0")
	if !limited {
		return 0, false
	}
	if ra, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && ra > 0 {
		return time.Duration(ra) * time.Second, true
	}
	if reset, err := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")), 10, 64); err == nil && reset > 0 {
		if d := time.Unix(reset, 0).Sub(now) + time.Second; d > 0 {
			return d, true
		}
	}
	return time.Minute, true
}

// listPRs pages through the repo's pull requests newest-first and returns
// those created in [since, until), oldest first. Paging stops at the first
// page that is entirely older than since.
func (f *backfillFetcher) listPRs(ctx context.Context, repo string, since, until time.Time) ([]backfillPR, error) {
	var out []backfillPR
	for page := 1; ; page++ {
		q := url.Values{
			"state": {"all"}, "sort": {"created"}, "direction": {"desc"},
			"per_page": {strconv.Itoa(evidenceBackfillPerPage)}, "page": {strconv.Itoa(page)},
		}
		var batch []backfillPR
		if err := f.get(ctx, "/repos/"+repo+"/pulls", q, &batch); err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		olderThanSince := true
		for _, pr := range batch {
			if pr.CreatedAt.Before(since) {
				continue
			}
			olderThanSince = false
			if !until.IsZero() && !pr.CreatedAt.Before(until) {
				continue
			}
			out = append(out, pr)
		}
		if olderThanSince || len(batch) < evidenceBackfillPerPage {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// mergedBy fills pr.MergedBy from the single-PR endpoint, which is the only
// place GitHub reports it.
func (f *backfillFetcher) mergedBy(ctx context.Context, repo string, pr *backfillPR) error {
	var full backfillPR
	if err := f.get(ctx, "/repos/"+repo+"/pulls/"+strconv.Itoa(pr.Number), nil, &full); err != nil {
		return err
	}
	pr.MergedBy = full.MergedBy
	if pr.MergeCommitSHA == "" {
		pr.MergeCommitSHA = full.MergeCommitSHA
	}
	return nil
}

// backfillAuthor applies the documented heuristics in order and returns the
// author plus the rule that decided it.
func backfillAuthor(pr backfillPR, agentLogins []string) (evidence.Author, string) {
	login := strings.TrimSpace(pr.User.Login)
	if meta, ok := ghpkg.ParseAttributionTrailer(pr.Body); ok {
		return evidence.Author{Login: login, Kind: evidence.AuthorAgent, Agent: meta.Agent, Backend: meta.Backend, Model: meta.Model}, authorRuleTrailer
	}
	for _, l := range agentLogins {
		if strings.EqualFold(strings.TrimSpace(l), login) {
			return evidence.Author{Login: login, Kind: evidence.AuthorAgent}, authorRuleAgentList
		}
	}
	if strings.Contains(pr.Body, hiveIdentityFooterMarker) {
		return evidence.Author{Login: login, Kind: evidence.AuthorAgent}, authorRuleFooter
	}
	if strings.EqualFold(pr.User.Type, "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return evidence.Author{Login: login, Kind: evidence.AuthorBot}, authorRuleBot
	}
	return evidence.Author{Login: login, Kind: evidence.AuthorHuman}, authorRuleHuman
}

// backfillBundle builds the bundle for one PR. Links are added by the
// caller once every PR in the window is known.
func backfillBundle(repo string, pr backfillPR, now time.Time, opts *evidenceBackfillOptions, flags []string) *evidence.Bundle {
	author, rule := backfillAuthor(pr, opts.agentLogins)
	b := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion,
		ID:            evidence.BundleID(repo, pr.Number, pr.Head.SHA),
		Repo:          repo,
		Number:        pr.Number,
		Author:        author,
		BaseSHA:       pr.Base.SHA,
		HeadSHA:       pr.Head.SHA,
		Policy:        evidence.Policy{ACMMLevel: opts.acmmLevel},
		CreatedAt:     pr.CreatedAt.UTC(),
		UpdatedAt:     now,
		Backfilled:    true,
		Backfill: &evidence.Backfill{
			Source:         evidenceBackfillSource,
			At:             now,
			State:          evidence.BackfillStateOpen,
			AuthorKindRule: rule,
			Flags:          slices.Clone(flags),
		},
	}
	switch {
	case pr.MergedAt != nil:
		actor := evidenceBackfillUnrecorded
		if pr.MergedBy != nil && strings.TrimSpace(pr.MergedBy.Login) != "" {
			actor = strings.TrimSpace(pr.MergedBy.Login)
		}
		sha := pr.MergeCommitSHA
		if sha == "" {
			sha = pr.Head.SHA
		}
		b.Merge = &evidence.MergeEvent{Actor: actor, Method: evidenceBackfillUnrecorded, At: pr.MergedAt.UTC(), SHA: sha}
		if !pr.CreatedAt.IsZero() && !pr.CreatedAt.After(*pr.MergedAt) {
			b.Merge.OpenedAt = pr.CreatedAt.UTC()
		}
		b.Backfill.State = evidence.BackfillStateMerged
	case pr.ClosedAt != nil:
		b.Backfill.State = evidence.BackfillStateClosed
		b.Backfill.ClosedAt = pr.ClosedAt.UTC()
	}
	return b
}

// backfillSupplementPath is where a backfilled record is written beside a live
// bundle at path; the evidence loader reads every *.json under the PR dir.
func backfillSupplementPath(path string) string {
	return strings.TrimSuffix(path, ".json") + ".backfill.json"
}

// backfillTarget decides where the backfilled record for the bundle path goes.
// A free path is used as is. A live bundle that has no merge event and is not
// itself backfilled gets a sibling .backfill.json so the PR gains a GitHub
// outcome; anything else already on disk is skipped.
func backfillTarget(path string) (target string, skip bool) {
	if _, err := os.Stat(path); err != nil {
		return path, false
	}
	existing, err := ghpkg.LoadReviewEvidence(path)
	if err != nil || existing.Backfilled || existing.Backfill != nil || existing.Merge != nil {
		return "", true
	}
	sup := backfillSupplementPath(path)
	if _, err := os.Stat(sup); err == nil {
		return "", true
	}
	return sup, false
}

// backfillStore is the on-disk view the run links against: bundles written
// by this run plus any already present under root. latest returns the
// newest bundle for a PR, loading from disk when it was not built here.
type backfillStore struct {
	root  string
	built map[int]*evidence.Bundle
	disk  map[int]*evidence.Bundle
	repo  string
}

func (s *backfillStore) latest(number int) *evidence.Bundle {
	if b, ok := s.built[number]; ok {
		return b
	}
	if b, ok := s.disk[number]; ok {
		return b
	}
	entries, err := ghpkg.ListReviewEvidence(s.root, s.repo, number)
	if err != nil || len(entries) == 0 {
		s.disk[number] = nil
		return nil
	}
	s.disk[number] = entries[len(entries)-1].Bundle
	return s.disk[number]
}

// linkBackfill sets revert_of / followup_of on b under the live recorder's
// rules (same reference parser, same 14-day window, same lookup cap) and
// returns the ids of the earlier bundles that need the reverse link.
func linkBackfill(b *evidence.Bundle, text string, store *backfillStore) (reverts, followups []*evidence.Bundle) {
	if b.Merge == nil || strings.TrimSpace(text) == "" {
		return nil, nil
	}
	revertNums, followupNums := evidence.ChangeRefs(b.Repo, b.Number, text)
	looked := 0
	link := func(have *[]string, numbers []int) []*evidence.Bundle {
		var targets []*evidence.Bundle
		for _, n := range numbers {
			if looked >= evidence.MaxLinkRefs {
				break
			}
			looked++
			t := store.latest(n)
			if t == nil || t.Merge == nil || !evidence.WithinLinkWindow(t.Merge.At, b.Merge.At) {
				continue
			}
			if !slices.Contains(*have, t.ID) {
				*have = append(*have, t.ID)
			}
			targets = append(targets, t)
		}
		return targets
	}
	reverts = link(&b.RevertOf, revertNums)
	followups = link(&b.FollowupOf, followupNums)
	return reverts, followups
}

// addBacklink records from on target's reverse list; true when it changed.
func addBacklink(target *evidence.Bundle, from string, revert bool) bool {
	list := &target.FollowedUpBy
	if revert {
		list = &target.RevertedBy
	}
	if slices.Contains(*list, from) {
		return false
	}
	*list = append(*list, from)
	return true
}

// writeBackfillBundle seals and writes b atomically.
func writeBackfillBundle(path string, b *evidence.Bundle, key []byte) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if err := evidence.Seal(b, key); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// backfillToken resolves the GitHub token: the named env var, then GH_TOKEN,
// then `gh auth token`. An empty result still runs (public repos, low limit).
var backfillToken = func(tokenEnv string) string {
	for _, name := range []string{tokenEnv, "GH_TOKEN"} {
		if name == "" {
			continue
		}
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func parseBackfillDate(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", value); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("date %q must be YYYY-MM-DD or RFC 3339", value)
}

func validateBackfillOptions(opts *evidenceBackfillOptions) (since, until time.Time, err error) {
	owner, name, ok := strings.Cut(strings.TrimSpace(opts.repo), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return since, until, &usageError{message: "--repo must be owner/repo"}
	}
	opts.repo = owner + "/" + name
	if strings.TrimSpace(opts.since) == "" {
		return since, until, &usageError{message: "--since is required"}
	}
	if since, err = parseBackfillDate(opts.since); err != nil {
		return since, until, &usageError{message: "--since: " + err.Error()}
	}
	if strings.TrimSpace(opts.until) != "" {
		if until, err = parseBackfillDate(opts.until); err != nil {
			return since, until, &usageError{message: "--until: " + err.Error()}
		}
		if !until.After(since) {
			return since, until, &usageError{message: "--until must be after --since"}
		}
	}
	for _, f := range opts.flags {
		if !slices.Contains(evidence.BackfillFlags, f) {
			return since, until, &usageError{message: fmt.Sprintf("--flag %q: expected one of %s", f, strings.Join(evidence.BackfillFlags, ", "))}
		}
	}
	if opts.acmmLevel < 0 {
		return since, until, &usageError{message: "--acmm-level must be 0 or positive"}
	}
	return since, until, nil
}

func runEvidenceBackfill(cmd *cobra.Command, env *commandEnv, opts *evidenceBackfillOptions) error {
	since, until, err := validateBackfillOptions(opts)
	if err != nil {
		return err
	}
	if !validOutput(env.options.output) {
		return &usageError{message: fmt.Sprintf("invalid --output %q; expected table, json, yaml, or jsonl", env.options.output)}
	}
	var key []byte
	if opts.signingKey != "" {
		k, err := evidence.LoadSigningKey(opts.signingKey)
		if err != nil {
			return fmt.Errorf("--signing-key-file: %w", err)
		}
		key = k
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	errOut := cmd.ErrOrStderr()
	fetcher := newBackfillFetcher(opts.apiBase, backfillToken(opts.tokenEnv))
	prs, err := fetcher.listPRs(ctx, opts.repo, since, until)
	if err != nil {
		return fmt.Errorf("listing pull requests: %w", err)
	}

	flags := slices.Clone(opts.flags)
	if strings.EqualFold(opts.repo, evidenceBackfillHiveRepo) && !slices.Contains(flags, evidence.BackfillFlagHiveOnHive) {
		flags = append(flags, evidence.BackfillFlagHiveOnHive)
	}
	merged := 0
	for _, pr := range prs {
		if pr.MergedAt != nil {
			merged++
		}
	}
	if merged < opts.minMerged && !slices.Contains(flags, evidence.BackfillFlagUnderMinMerged) {
		flags = append(flags, evidence.BackfillFlagUnderMinMerged)
	}
	slices.Sort(flags)

	now := time.Now().UTC().Truncate(time.Second)
	store := &backfillStore{root: opts.root, repo: opts.repo, built: map[int]*evidence.Bundle{}, disk: map[int]*evidence.Bundle{}}
	counts := map[string]int{"open": 0, "closed": 0, "merged": 0, "human": 0, "agent": 0, "bot": 0}
	var toWrite []*evidence.Bundle
	skipped, supplemented := 0, 0
	paths := map[int]string{}
	fetchErrors := 0
	for i := range prs {
		pr := &prs[i]
		path := ghpkg.ReviewEvidencePath(opts.root, opts.repo, pr.Number, pr.Head.SHA)
		target, skip := backfillTarget(path)
		if skip {
			skipped++
			continue
		}
		if target != path {
			supplemented++
		}
		paths[pr.Number] = target
		if pr.MergedAt != nil && !opts.skipMergers && !opts.dryRun {
			if err := fetcher.mergedBy(ctx, opts.repo, pr); err != nil {
				if ctx.Err() != nil {
					return err
				}
				fetchErrors++
				_, _ = fmt.Fprintf(errOut, "warning: %s#%d: merger not fetched: %v\n", opts.repo, pr.Number, err)
			}
		}
		b := backfillBundle(opts.repo, *pr, now, opts, flags)
		counts[b.Backfill.State]++
		counts[b.Author.Kind]++
		store.built[pr.Number] = b
		toWrite = append(toWrite, b)
	}

	// Links need every bundle in the window built first; PRs are in number
	// order so an earlier merge is always known before the PR that reverts it.
	type backlink struct {
		target *evidence.Bundle
		from   string
		revert bool
	}
	var backlinks []backlink
	links := 0
	for i := range prs {
		b, ok := store.built[prs[i].Number]
		if !ok {
			continue
		}
		reverts, followups := linkBackfill(b, prs[i].Title+"\n"+prs[i].Body, store)
		links += len(b.RevertOf) + len(b.FollowupOf)
		for _, t := range reverts {
			backlinks = append(backlinks, backlink{t, b.ID, true})
		}
		for _, t := range followups {
			backlinks = append(backlinks, backlink{t, b.ID, false})
		}
	}

	summary := map[string]any{
		"repo":          opts.repo,
		"since":         since.Format(time.RFC3339),
		"until":         "now",
		"fetched":       len(prs),
		"to_write":      len(toWrite),
		"skipped":       skipped,
		"supplemented":  supplemented,
		"merged":        counts["merged"],
		"closed":        counts["closed"],
		"open":          counts["open"],
		"human":         counts["human"],
		"agent":         counts["agent"],
		"bot":           counts["bot"],
		"links":         links,
		"flags":         strings.Join(flags, ","),
		"api_calls":     fetcher.calls,
		"merger_errors": fetchErrors,
		"dry_run":       opts.dryRun,
	}
	if !until.IsZero() {
		summary["until"] = until.Format(time.RFC3339)
	}
	if opts.dryRun {
		return env.print(cmd, summary)
	}

	// Reverse links go on in memory first so bundles built in this run are
	// written once, complete. Bundles already on disk are rewritten in place,
	// as the live recorder does.
	diskTouched := map[string]*evidence.Bundle{}
	for _, bl := range backlinks {
		if !addBacklink(bl.target, bl.from, bl.revert) {
			continue
		}
		if _, builtHere := store.built[bl.target.Number]; !builtHere {
			bl.target.UpdatedAt = now
			diskTouched[bl.target.ID] = bl.target
		}
	}
	written, relinked := 0, 0
	var writeErrs []error
	for _, b := range toWrite {
		if err := writeBackfillBundle(paths[b.Number], b, key); err != nil {
			writeErrs = append(writeErrs, fmt.Errorf("%s: %w", b.ID, err))
			continue
		}
		written++
	}
	for _, b := range diskTouched {
		path := ghpkg.ReviewEvidencePath(opts.root, b.Repo, b.Number, b.HeadSHA)
		if err := writeBackfillBundle(path, b, key); err != nil {
			writeErrs = append(writeErrs, fmt.Errorf("backlink %s: %w", b.ID, err))
			continue
		}
		relinked++
	}
	summary["written"] = written
	summary["relinked"] = relinked
	summary["write_errors"] = len(writeErrs)
	for _, e := range writeErrs {
		_, _ = fmt.Fprintf(errOut, "warning: %v\n", e)
	}
	if err := env.print(cmd, summary); err != nil {
		return err
	}
	if len(writeErrs) > 0 {
		return errors.New("some bundles were not written; see warnings")
	}
	return nil
}
