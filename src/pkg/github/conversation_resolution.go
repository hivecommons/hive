package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Conversation resolution at a person's merge.
//
// The merge lanes resolve the hive's own non-blocking review threads only
// after GitHub refused a merge the hive attempted (review_own_threads.go).
// A PR the hive hands to a person at 090-ready-for-human-merge (auto-merge
// off for the repository, a human-merge path, a workflow fence) never gets
// that attempt, so on a base that requires conversation resolution GitHub
// refused the person's merge with "All comments must be resolved" until they
// replied to and resolved the hive's own low-severity notes by hand
// (projectbluefin/actions#779 and #783, 2026-11-25).
//
// ResolveOwnReviewThreadsForHumanMerge closes that gap with the same rules
// (resolveOwnReviewThreads), but only when the base is read as requiring
// conversation resolution. The rule is read beside the required checks
// (RequiredStatusCheckPolicy) from the same two sources: the branch's
// rulesets (GET rules/branches/{branch}, readable without admin rights; a
// pull_request rule's required_review_thread_resolution) and classic branch
// protection (required_conversation_resolution). A rule that cannot be read
// is unknown, and unknown resolves nothing.

// conversationRuleTTL is how long one repository branch's answer is kept,
// the human route's required-checks cache window (humanChecksTTL).
const conversationRuleTTL = 10 * time.Minute

// conversationRuleForbiddenTTL is how long an answer the App was refused
// (403) is kept: the required-checks forbidden cache's window
// (sharedRequiredChecksForbiddenCache), since a missing permission does not
// come back on its own within minutes.
var conversationRuleForbiddenTTL = durationFromEnv(requiredChecksForbiddenTTLEnv, defaultRequiredChecksForbiddenTTL)

// ownThreadsReadyReplyMarker marks the hive's reply in a thread it resolves
// for a person's merge, so a later pass on the same thread (a retry after
// the resolve failed, a note a person re-opened) does not reply twice.
const ownThreadsReadyReplyMarker = "<!-- hive:own-thread-resolved-for-merge -->"

// ownThreadsReadyReply is the reply posted in each thread the hive is about
// to resolve for a person's merge. It is posted before the resolve.
const ownThreadsReadyReply = "The hive is resolving its own non-blocking note so the branch rule that requires resolved conversations does not block a person's merge. The note stays here to read.\n\n" + ownThreadsReadyReplyMarker

// Skipped values ResolveOwnReviewThreadsForHumanMerge adds to the ones
// resolveOwnReviewThreads reports.
const (
	OwnThreadsSkippedRuleUnknown     = "conversation-resolution-unknown"
	OwnThreadsSkippedRuleNotRequired = "conversation-resolution-not-required"
)

type conversationRuleEntry struct {
	until    time.Time
	required bool
	known    bool
}

type conversationRuleCache struct {
	mu      sync.Mutex
	entries map[string]conversationRuleEntry
}

// conversationRuleSlot is the Client field that holds the cache, built on
// first use and shared by every engine over the Client.
type conversationRuleSlot struct {
	once  sync.Once
	cache *conversationRuleCache
}

func (c *Client) conversationRules() *conversationRuleCache {
	if c == nil {
		return nil
	}
	c.conversationRule.once.Do(func() {
		c.conversationRule.cache = &conversationRuleCache{entries: map[string]conversationRuleEntry{}}
	})
	return c.conversationRule.cache
}

// conversationRuleRead is one read of a branch's rule.
type conversationRuleRead struct {
	required bool
	known    bool
	// forbidden: the answer is unknown because GitHub refused a read with
	// 403 (a missing permission, not a rate limit or a server error).
	forbidden bool
}

// conversationResolutionRequired reports whether owner/repo's branch requires
// every review conversation resolved before a merge (required) and whether
// that could be read at all (known). The answer is cached per repository
// and branch, an unknown one included: for conversationRuleTTL, or
// conversationRuleForbiddenTTL when a read was refused for permission. An
// unknown answer is logged once, when the cache is filled.
func (c *Client) conversationResolutionRequired(ctx context.Context, owner, repo, branch string) (required, known bool) {
	branch = strings.TrimSpace(branch)
	if c == nil || c.client == nil || branch == "" {
		return false, false
	}
	key := strings.ToLower(strings.TrimSpace(owner)+"/"+strings.TrimSpace(repo)) + "@" + branch
	now := time.Now()
	cache := c.conversationRules()
	if cache != nil {
		cache.mu.Lock()
		e, ok := cache.entries[key]
		cache.mu.Unlock()
		if ok && now.Before(e.until) {
			return e.required, e.known
		}
	}
	read := readConversationResolutionRule(ctx, c.client, owner, repo, branch)
	ttl := conversationRuleTTL
	if !read.known && read.forbidden {
		ttl = conversationRuleForbiddenTTL
	}
	if cache != nil {
		cache.mu.Lock()
		cache.entries[key] = conversationRuleEntry{until: now.Add(ttl), required: read.required, known: read.known}
		cache.mu.Unlock()
	}
	if !read.known && c.logger != nil {
		c.logger.Debug("review threads: could not read whether the base requires conversation resolution; the hive's own notes stay open",
			slog.String("repo", owner+"/"+repo), slog.String("branch", branch),
			slog.Bool("forbidden", read.forbidden), slog.Duration("cached_for", ttl))
	}
	return read.required, read.known
}

// readConversationResolutionRule reads the rulesets first: one call that
// needs no admin rights and answers for a rulesets-only repository. Only
// when no ruleset requires it is classic protection read. Not required is
// known only when both sources answered.
func readConversationResolutionRule(ctx context.Context, client *gh.Client, owner, repo, branch string) conversationRuleRead {
	rules := rulesetRequiresThreadResolution(ctx, client, owner, repo, branch)
	if rules.known && rules.required {
		return rules
	}
	classic := classicRequiresConversationResolution(ctx, client, owner, repo, branch)
	if classic.known && classic.required {
		return classic
	}
	known := rules.known && classic.known
	return conversationRuleRead{known: known, forbidden: !known && (rules.forbidden || classic.forbidden)}
}

// forbiddenRead reports whether err is GitHub refusing a read with 403. A
// rate limit is its own error type (gh.RateLimitError,
// gh.AbuseRateLimitError) and does not count.
func forbiddenRead(err error) bool {
	var er *gh.ErrorResponse
	return errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusForbidden
}

// rulesetRequiresThreadResolution reads the rules that apply to branch and
// reports whether a pull_request rule sets required_review_thread_resolution.
func rulesetRequiresThreadResolution(ctx context.Context, client *gh.Client, owner, repo, branch string) conversationRuleRead {
	path := fmt.Sprintf("repos/%s/%s/rules/branches/%s?per_page=100", owner, repo, url.PathEscape(branch))
	req, err := client.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return conversationRuleRead{}
	}
	var rules []struct {
		Type       string          `json:"type"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if _, err := client.Do(ctx, req, &rules); err != nil {
		return conversationRuleRead{forbidden: forbiddenRead(err)}
	}
	for _, r := range rules {
		if r.Type != "pull_request" || len(r.Parameters) == 0 {
			continue
		}
		var p struct {
			RequiredReviewThreadResolution bool `json:"required_review_thread_resolution"`
		}
		if err := json.Unmarshal(r.Parameters, &p); err != nil {
			return conversationRuleRead{}
		}
		if p.RequiredReviewThreadResolution {
			return conversationRuleRead{required: true, known: true}
		}
	}
	return conversationRuleRead{known: true}
}

// classicRequiresConversationResolution reads classic branch protection's
// required_conversation_resolution. It asks the required-status-checks
// sub-resource first, the read the required-checks policy already makes
// (requiredStatusChecksFromProtectionEndpoint): its "Branch not protected"
// answers an unprotected branch in one call and a refusal there (403)
// means the protection cannot be read. Only a protected branch is read in
// full.
func classicRequiresConversationResolution(ctx context.Context, client *gh.Client, owner, repo, branch string) conversationRuleRead {
	_, _, err := requiredStatusChecksFromProtectionEndpoint(ctx, client, owner, repo, branch)
	if errors.Is(err, gh.ErrBranchNotProtected) {
		return conversationRuleRead{known: true}
	}
	if err != nil {
		// A 404 that is not "Branch not protected" can be a protected
		// branch with no required checks; the full read tells. Anything
		// else (403, a rate limit, a server error) cannot be read.
		var er *gh.ErrorResponse
		if !errors.As(err, &er) || er.Response == nil || er.Response.StatusCode != http.StatusNotFound {
			return conversationRuleRead{forbidden: forbiddenRead(err)}
		}
	}
	p, _, err := client.Repositories.GetBranchProtection(ctx, owner, repo, branch)
	if errors.Is(err, gh.ErrBranchNotProtected) {
		return conversationRuleRead{known: true}
	}
	if err != nil || p == nil {
		return conversationRuleRead{forbidden: forbiddenRead(err)}
	}
	return conversationRuleRead{required: p.GetRequiredConversationResolution() != nil && p.GetRequiredConversationResolution().Enabled, known: true}
}

// ownThreadsReadyReplied reports whether t already carries the hive's reply
// for a person's merge.
func (c *Client) ownThreadsReadyReplied(t rawReviewThread) bool {
	for _, cmt := range t.Comments.Nodes {
		if c.isOwnThreadAuthor(cmt) && strings.Contains(cmt.Body, ownThreadsReadyReplyMarker) {
			return true
		}
	}
	return false
}

// ResolveOwnReviewThreadsForHumanMerge is the stage pass's answer to a head
// handed to a person at 090-ready-for-human-merge on base: when the base
// requires conversation resolution and every unresolved thread on the PR is
// the hive's own and marked non-blocking, it resolves them all, with the
// rules and the audit of ResolveOwnReviewThreadsForMerge
// (trigger=ready-for-human-merge). With reply set it first replies once in
// each of those threads to say why (a thread that already carries that
// reply gets no second one); a reply that fails does not stop the resolve.
// A paused repository, a base that does not require conversation
// resolution, or one whose rule cannot be read resolves nothing (Skipped
// names which).
func (c *Client) ResolveOwnReviewThreadsForHumanMerge(ctx context.Context, repo string, number int, base string, reply bool) (OwnReviewThreadsOutcome, error) {
	var out OwnReviewThreadsOutcome
	if c == nil {
		return out, ErrNoGitHubClient
	}
	if c.RepoIsPaused(repo) {
		out.Skipped = "paused"
		return out, nil
	}
	owner, name := c.splitRepo(repo)
	required, known := c.conversationResolutionRequired(ctx, owner, name, base)
	switch {
	case !known:
		out.Skipped = OwnThreadsSkippedRuleUnknown
		return out, nil
	case !required:
		out.Skipped = OwnThreadsSkippedRuleNotRequired
		return out, nil
	}
	var beforeResolve func(context.Context, rawReviewThread)
	if reply {
		meta := c.attributionMeta(ownThreadsAuditAgent)
		beforeResolve = func(ctx context.Context, t rawReviewThread) {
			if c.ownThreadsReadyReplied(t) {
				return
			}
			if err := c.replyToReviewThread(ctx, t.ID, ownThreadsReadyReply); err != nil {
				c.logger.Warn("review threads: could not reply in the hive's own thread before resolving it",
					slog.String("repo", repo), slog.Int("number", number), slog.String("thread", t.ID),
					slog.String("error", err.Error()))
				return
			}
			c.recordWriteAudit(AuditActionPRReviewed, meta,
				WriteTarget{Repo: repo, Number: number},
				"state", reviewStateThreadReplied,
				"thread", t.ID,
				"trigger", ownThreadsReadyTrigger)
		}
	}
	return c.resolveOwnReviewThreads(ctx, repo, number, ownThreadsReadyTrigger, beforeResolve)
}
