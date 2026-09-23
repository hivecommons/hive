package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/dupsweep"
	"github.com/hivecommons/hive/pkg/findingidentity"
	"github.com/hivecommons/hive/pkg/logscrub"
)

// The duplicate sweep is capability (B) of hivecommons/hive#7469: a periodic,
// cross-PR pass that clusters open PRs by changed-file set and SUGGESTS which
// one to keep.
//
// It is deliberately not the review swarm. The reviewer has no GitHub write
// access by design and judges one change at a time without the author's
// rationale; nothing here changes that. The write is performed by the hive
// itself — which already holds the App token — through the same canary-gated,
// scrubbed, audited comment path every other hive-authored comment uses. No
// second write surface is opened and the reviewer gains no permission.
//
// Everything it can do is comment. There is no code path here that closes,
// labels, approves, requests changes on, or merges a PR, because changed-file
// identity is a candidate generator and its known false positives are
// indistinguishable from true positives at this layer.

const (
	// DefaultDuplicateSweepMaxComments caps the comments one sweep pass may
	// write across all repos, mirroring DefaultTaskListSweepMaxCloses. On a
	// 423-PR queue an uncapped first pass would be the loudest thing the hive
	// has ever done; a small cap drains the backlog over several cadences
	// instead, and every pass is idempotent so nothing is lost by going slow.
	DefaultDuplicateSweepMaxComments = 5
	// DefaultDuplicateSweepMaxPRsPerRepo bounds how many open PRs are
	// fingerprinted per repo per pass. Each PR costs at least one ListFiles
	// call on first sight, so this is the sweep's API budget knob.
	DefaultDuplicateSweepMaxPRsPerRepo = 100
	// duplicateSweepPerPage is GitHub's maximum page size for the PR list.
	duplicateSweepPerPage = 100
	// duplicateSweepFilesMaxPages bounds the per-PR file walk. 3 x 100 = 300
	// paths; a PR above that is dropped from clustering rather than partially
	// fingerprinted, because a TRUNCATED file list would fingerprint as
	// equal to some other truncated list and manufacture a false duplicate.
	duplicateSweepFilesMaxPages = 3
	// duplicateSweepFilesCacheMax bounds the head-SHA-keyed file cache.
	duplicateSweepFilesCacheMax = 4096
)

// DuplicateSweepOptions configures one pass.
type DuplicateSweepOptions struct {
	// PostComments authorizes the GitHub write. With it false the sweep still
	// clusters and still reports, but touches nothing — the dry run an
	// operator should look at before letting it speak on contributors' PRs.
	PostComments bool
	// MaxComments caps writes per pass. Zero means
	// DefaultDuplicateSweepMaxComments.
	MaxComments int
	// MaxPRsPerRepo caps fingerprinting per repo. Zero means
	// DefaultDuplicateSweepMaxPRsPerRepo.
	MaxPRsPerRepo int
	// BotAuthors are extra logins to treat as a regenerating bot, beyond the
	// "[bot]" suffix.
	BotAuthors []string
	// Audit receives one event per cluster, posted or not.
	Audit func(DuplicateSweepEvent)
}

// DuplicateSweepEvent describes one suggested cluster.
type DuplicateSweepEvent struct {
	Repo       string
	Survivor   int
	Superseded []int
	// Commented lists the PR numbers a suggestion was actually written to.
	// Empty when PostComments is off or the cap was reached, which is how a
	// dry run is distinguished from a silent failure in the audit log.
	Commented  []int
	Confidence string
	BotSeries  bool
	Files      int
}

// DuplicateSweepResult summarises a pass.
type DuplicateSweepResult struct {
	Clusters []DuplicateSweepEvent
	// Scanned is the number of open PRs successfully fingerprinted.
	Scanned int
	// Skipped counts PRs dropped before clustering (draft, oversized or
	// truncated file list, or a per-PR API failure).
	Skipped int
	// Commented is the total number of comments written.
	Commented int
}

// prFingerprint is one PR's changed-file evidence, cached by head SHA.
type prFingerprint struct {
	files    []string
	diffHash string
}

var (
	dupSweepCacheMu sync.Mutex
	dupSweepCache   = map[string]prFingerprint{}
)

// SweepDuplicatePRs clusters open PRs across the client's repos by
// changed-file set and, when opts.PostComments is set, posts one idempotent
// suggestion per cluster target.
//
// A per-repo API failure is recorded and the remaining repos are still swept:
// a partial suggestion set is strictly better than none, and nothing here is
// destructive enough that a partial view could cause harm.
func (c *Client) SweepDuplicatePRs(ctx context.Context, opts DuplicateSweepOptions) (*DuplicateSweepResult, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	maxComments := opts.MaxComments
	if maxComments <= 0 {
		maxComments = DefaultDuplicateSweepMaxComments
	}
	maxPRs := opts.MaxPRsPerRepo
	if maxPRs <= 0 {
		maxPRs = DefaultDuplicateSweepMaxPRsPerRepo
	}

	result := &DuplicateSweepResult{}
	var firstErr error
	var all []dupsweep.PR

	for _, repo := range c.getRepos() {
		prs, skipped, err := c.fingerprintOpenPRs(ctx, repo, maxPRs)
		result.Skipped += skipped
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if c.logger != nil {
				c.logger.Warn("duplicate sweep: repo scan failed", slog.String("repo", repo), slog.String("error", err.Error()))
			}
			continue
		}
		result.Scanned += len(prs)
		all = append(all, prs...)
	}

	clusters := dupsweep.Find(all, dupsweep.Options{
		BotAuthors: opts.BotAuthors,
		FindingKey: func(pr dupsweep.PR) string {
			return pr.FindingKey
		},
	})
	for _, cluster := range clusters {
		event := DuplicateSweepEvent{
			Repo:       cluster.Repo,
			Survivor:   cluster.Survivor.Number,
			Confidence: string(cluster.Confidence),
			BotSeries:  cluster.BotSeries,
			Files:      len(cluster.Files),
		}
		for _, m := range cluster.Superseded {
			event.Superseded = append(event.Superseded, m.Number)
		}
		if opts.PostComments {
			for _, target := range cluster.Targets() {
				if result.Commented >= maxComments {
					break
				}
				body := dupsweep.Render(cluster, target)
				if err := c.ensureDuplicateSweepComment(ctx, cluster, target, body); err != nil {
					if firstErr == nil {
						firstErr = err
					}
					if c.logger != nil {
						c.logger.Warn("duplicate sweep: comment failed",
							slog.String("repo", target.Repo), slog.Int("number", target.Number),
							slog.String("error", err.Error()))
					}
					continue
				}
				result.Commented++
				event.Commented = append(event.Commented, target.Number)
			}
		}
		result.Clusters = append(result.Clusters, event)
		if opts.Audit != nil {
			opts.Audit(event)
		}
	}
	return result, firstErr
}

// fingerprintOpenPRs lists a repo's open PRs and resolves each one's
// changed-file evidence. Returns the fingerprinted PRs and the count dropped.
func (c *Client) fingerprintOpenPRs(ctx context.Context, repo string, maxPRs int) ([]dupsweep.PR, int, error) {
	owner, repoName := c.splitRepo(repo)
	opts := &gh.PullRequestListOptions{
		State:       "open",
		ListOptions: gh.ListOptions{PerPage: duplicateSweepPerPage},
	}
	var out []dupsweep.PR
	skipped := 0
	for len(out)+skipped < maxPRs {
		page, resp, err := c.client.PullRequests.List(ctx, owner, repoName, opts)
		if err != nil {
			return out, skipped, fmt.Errorf("listing open PRs for %s: %w", repo, err)
		}
		for _, pr := range page {
			if pr == nil {
				continue
			}
			if len(out)+skipped >= maxPRs {
				break
			}
			if pr.GetDraft() {
				skipped++
				continue
			}
			fp, ok := c.prChangedFiles(ctx, owner, repoName, pr)
			if !ok {
				skipped++
				continue
			}
			out = append(out, dupsweep.PR{
				Repo:       repo,
				Number:     pr.GetNumber(),
				Title:      pr.GetTitle(),
				Author:     safeGetLogin(pr.GetUser()),
				URL:        pr.GetHTMLURL(),
				CreatedAt:  pr.GetCreatedAt().Time,
				Files:      fp.files,
				DiffHash:   fp.diffHash,
				FindingKey: findingKeyFromText(pr.GetBody()),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, skipped, nil
}

func findingKeyFromText(text string) string {
	fields := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "-*• \t")
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		key = strings.NewReplacer(" ", "_", "-", "_").Replace(key)
		switch key {
		case findingidentity.MetaKey, "finding_identity", "finding_identity_key",
			findingidentity.MetaSubjectDigest, "finding_subject_digest", "audit_subject_digest",
			findingidentity.MetaPredicate, "finding_predicate", "audit_predicate",
			findingidentity.MetaLocation, "finding_location", "normalized_location", "path", "file":
			fields[key] = strings.TrimSpace(value)
		}
	}
	return findingidentity.KeyFromFields(fields)
}

// prChangedFiles resolves a PR's changed-file set and diff hash, cached by
// head SHA. The cache is what makes a cadenced sweep affordable: a PR that has
// not been pushed to since the last pass costs zero extra calls.
//
// Returns ok=false — meaning "exclude this PR from clustering" — whenever the
// evidence is incomplete. That is the fail-closed direction: an incomplete
// file list fingerprints as a DIFFERENT, shorter set that could coincide with
// another truncated list, and a duplicate suggestion invented from truncation
// would send a human to close a PR that is not a duplicate at all.
func (c *Client) prChangedFiles(ctx context.Context, owner, repo string, pr *gh.PullRequest) (prFingerprint, bool) {
	headSHA := ""
	if pr.GetHead() != nil {
		headSHA = pr.GetHead().GetSHA()
	}
	if headSHA == "" {
		return prFingerprint{}, false
	}
	key := fmt.Sprintf("%s/%s#%d@%s", owner, repo, pr.GetNumber(), headSHA)

	dupSweepCacheMu.Lock()
	if cached, ok := dupSweepCache[key]; ok {
		dupSweepCacheMu.Unlock()
		return cached, true
	}
	dupSweepCacheMu.Unlock()

	patches := map[string]string{}
	var files []string
	opts := &gh.ListOptions{PerPage: duplicateSweepPerPage}
	complete := false
	for page := 0; page < duplicateSweepFilesMaxPages; page++ {
		list, resp, err := c.client.PullRequests.ListFiles(ctx, owner, repo, pr.GetNumber(), opts)
		if err != nil {
			return prFingerprint{}, false
		}
		for _, f := range list {
			if f == nil {
				continue
			}
			files = append(files, f.GetFilename())
			patches[f.GetFilename()] = f.GetPatch()
		}
		if resp == nil || resp.NextPage == 0 {
			complete = true
			break
		}
		opts.Page = resp.NextPage
	}
	if !complete {
		return prFingerprint{}, false
	}
	// GitHub caps the files endpoint on very large PRs, so a short list is
	// possible even when paging terminated cleanly. Same guard as
	// ListMergedPRFiles (pr_reach.go): refuse a partial fingerprint.
	if reported := pr.GetChangedFiles(); reported > len(files) {
		return prFingerprint{}, false
	}
	if len(files) == 0 {
		return prFingerprint{}, false
	}

	fp := prFingerprint{files: files, diffHash: dupsweep.DiffFingerprint(patches)}
	dupSweepCacheMu.Lock()
	if len(dupSweepCache) >= duplicateSweepFilesCacheMax {
		dupSweepCache = map[string]prFingerprint{}
	}
	dupSweepCache[key] = fp
	dupSweepCacheMu.Unlock()
	return fp, true
}

// ensureDuplicateSweepComment posts body on target, or edits the sweep's prior
// comment for the same cluster in place. Never posts twice for one cluster.
//
// The body passes through the same three filters every hive-authored write
// does, and the order matters:
//
//  1. NeutralizeMentions — PR titles are author-controlled prose quoted
//     verbatim, so one containing "@someone" would re-notify that person on
//     every edit. Idempotent, and Render already applied it, so this is the
//     belt-and-suspenders pass that holds for any future caller.
//  2. Canary scan — agent-sourced text reaching GitHub is an exfiltration
//     channel and honours the same fail-closed contract as CreateIssue/
//     CreatePR/CreateIssueComment (kubestellar/hive#4960).
//  3. logscrub — secrets. It runs AFTER the mention pass so nothing it
//     redacts can re-introduce a mention.
//
// Then the shared line-boundary, UTF-8-safe truncation, so a huge cluster
// cannot exceed GitHub's comment limit.
func (c *Client) ensureDuplicateSweepComment(ctx context.Context, cluster dupsweep.Cluster, target dupsweep.PR, body string) error {
	body = advisory.NeutralizeMentions(body)
	if leak, ok := c.scanCanaryText(body, "hive-duplicate-sweep:"+target.Repo); ok {
		if c.canaryFailClosed {
			return fmt.Errorf("ioscan canary leak detected: agent=%s source=%s", leak.Agent, leak.Source)
		}
	}
	body = truncateDigest(logscrub.ScrubString(body))

	owner, repoName := c.splitRepo(target.Repo)
	marker := dupsweep.MarkerFor(cluster)
	comments, err := c.listIssueComments(ctx, owner, repoName, target.Number)
	if err != nil {
		return err
	}
	for _, cm := range comments {
		if cm == nil || !strings.Contains(cm.GetBody(), marker) {
			continue
		}
		if cm.GetBody() == body {
			return nil
		}
		if _, _, err := c.client.Issues.EditComment(ctx, owner, repoName, cm.GetID(), &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			return fmt.Errorf("editing duplicate-sweep comment on %s#%d: %w", target.Repo, target.Number, err)
		}
		return nil
	}
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repoName, target.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		return fmt.Errorf("creating duplicate-sweep comment on %s#%d: %w", target.Repo, target.Number, err)
	}
	return nil
}
