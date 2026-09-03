package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// Self-authorization gate (kubestellar/hive#5117).
//
// Every fix-capable policy teaches the same loop: find a problem, file an
// issue, open a PR citing it. Nothing in that loop distinguishes "an issue a
// human filed" from "an issue I filed sixty seconds ago", so the
// tracked-rationale precondition is satisfiable by the agent alone. In
// tuna-os/tunaos-packages, issue #581 proposed a decomposition and PR #583
// implemented phase 1 of it, introducing a module-sourcing pattern with no
// precedent in the repository. Both were hive-filed; no human agreed to the
// direction in between. The code was fine — that is what makes it a governance
// bug rather than a code bug.
//
// This gate does not block the work. It applies the "hold" label the merge gate
// already keys on, so the PR is filed and visible but cannot merge until a
// person looks at it. Blocking creation instead would throw away a finished,
// often-correct change to make a point about process.
//
// WHAT COUNTS AS EVIDENCE
//
// The gate fires on POSITIVE evidence only: it must actually read an issue,
// find a non-human author, and find no human acknowledgement. An issue it
// cannot read at all decides nothing — a network error must not manufacture a
// hold on an unrelated PR, because a label people learn to strip on sight stops
// gating anything. The one asymmetry is deliberate: once the author is known to
// be non-human, a FAILED acknowledgement lookup holds, because at that point
// self-authorship is established and only the excuse is missing.

// HumanAckLabel is the label a human applies to an agent-filed issue to say
// "yes, take this direction". It is one of several acknowledgements the gate
// accepts (see issueHasHumanAcknowledgement) and exists for the case where a
// maintainer wants to approve without writing a comment.
const HumanAckLabel = "approved-direction"

// selfAuthCommentPageSize bounds the acknowledgement scan. The first page is
// enough: the gate needs to know whether ANY human has engaged, and a human who
// engaged only after 100 bot comments is not the acknowledgement this is
// looking for.
const selfAuthCommentPageSize = 100

// SetHiveIdentity records which accounts count as "this hive", so the
// self-authorization gate can recognise an issue the hive filed under a plain
// user account (project.ai_author) as well as under the App bot. It is
// optional: with no identity configured the gate still recognises bot-authored
// issues by login suffix and account type, which is what the App-authored
// incident case looks like.
func (c *Client) SetHiveIdentity(id HiveIdentity) {
	if c == nil {
		return
	}
	c.hiveIdentityMu.Lock()
	defer c.hiveIdentityMu.Unlock()
	c.hiveIdentity = id
}

func (c *Client) getHiveIdentity() HiveIdentity {
	c.hiveIdentityMu.RLock()
	defer c.hiveIdentityMu.RUnlock()
	return c.hiveIdentity
}

// isHumanAuthor reports whether a GitHub account is a person other than this
// hive. It is the single definition the whole gate turns on, so the three ways
// an account can fail to be one live here rather than being re-derived per call
// site: it is empty, it is a bot, or it is one of our own accounts.
//
// The "[bot]" login suffix is checked as well as the account type because the
// type is absent from some abbreviated API payloads, and a login ending in
// "[bot]" is never a person — GitHub reserves the suffix.
func (c *Client) isHumanAuthor(user *gh.User) bool {
	login := strings.TrimSpace(user.GetLogin())
	if login == "" {
		return false
	}
	if strings.EqualFold(user.GetType(), "Bot") || strings.HasSuffix(login, "[bot]") {
		return false
	}
	if c != nil && strings.EqualFold(login, c.appBotLogin) && c.appBotLogin != "" {
		return false
	}
	if c != nil && c.getHiveIdentity().Matches(login) {
		return false
	}
	return true
}

// isKnownNonHumanAuthor is isHumanAuthor's positive counterpart, and the two are
// deliberately NOT complements: both are false when the account is unknown.
//
// An abbreviated or partial API payload can carry no login at all, and "we could
// not tell" must not read as "a bot filed it" — that is how a gate starts firing
// on issues real people opened. The tri-state is the whole point, so the gate
// asks this question rather than negating the other one.
func (c *Client) isKnownNonHumanAuthor(user *gh.User) bool {
	if strings.TrimSpace(user.GetLogin()) == "" {
		return false
	}
	return !c.isHumanAuthor(user)
}

// SelfAuthorization is the gate's finding for one PR request.
type SelfAuthorization struct {
	// Held is true when the PR must carry "hold" regardless of ACMM level.
	Held bool
	// Issue is the agent-filed issue that decided it, for the log and the
	// explanatory comment. Zero when Held is false.
	Issue int
	// Repo is that issue's repository ("owner/repo").
	Repo string
	// Reason is a short human-readable explanation, used verbatim in the audit
	// entry and the log.
	Reason string
}

// rationaleIssues collects the issues a PR request offers as its rationale:
// everything the title and body claim to close or reference, plus the request's
// own IssueN list. Both parsers are reused as-is rather than re-implemented, so
// the gate reads exactly the references the claim ledger already understands.
func rationaleIssues(repo, title, body string, declared []int) []ClaimedRef {
	text := title + "\n" + body
	refs := ParseClaimedIssues(text, repo)
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		seen[fmt.Sprintf("%s#%d", ref.Repo, ref.Issue)] = true
	}
	add := func(candidates []ClaimedRef) {
		for _, ref := range candidates {
			key := fmt.Sprintf("%s#%d", ref.Repo, ref.Issue)
			if seen[key] {
				continue
			}
			seen[key] = true
			refs = append(refs, ref)
		}
	}
	add(ParseReferencedIssues(text, repo))
	for _, n := range declared {
		if n > 0 {
			add([]ClaimedRef{{Repo: repo, Issue: n}})
		}
	}
	return refs
}

// EvaluateSelfAuthorization decides whether a PR request's rationale rests
// entirely on issues the hive filed and no human has acknowledged.
//
// A PR that cites NOTHING is not this bug and is left alone: "no tracked
// rationale at all" is a different problem with a different answer, and
// widening the gate to cover it would hold most of the fleet's routine work.
func (c *Client) EvaluateSelfAuthorization(ctx context.Context, repo, title, body string, declared []int) SelfAuthorization {
	if c == nil || c.client == nil {
		return SelfAuthorization{}
	}
	refs := rationaleIssues(repo, title, body, declared)
	if len(refs) == 0 {
		return SelfAuthorization{}
	}

	var firstSelfFiled *SelfAuthorization
	for _, ref := range refs {
		owner, name := splitRepoRef(ref.Repo, c.org)
		if owner == "" || name == "" {
			continue
		}
		issue, _, err := c.client.Issues.Get(ctx, owner, name, ref.Issue)
		if err != nil {
			// No evidence either way. Deliberately not a hold: see the file
			// comment — a hold nobody can explain is a hold people learn to
			// strip.
			c.logger.Warn("self-authorization gate: could not read a cited issue, it decides nothing",
				"repo", ref.Repo, "issue", ref.Issue, "error", err.Error())
			continue
		}
		if c.isHumanAuthor(issue.GetUser()) {
			// A person proposed this. That is the precondition working.
			return SelfAuthorization{}
		}
		if !c.isKnownNonHumanAuthor(issue.GetUser()) {
			// Neither provably a person nor provably one of ours. No evidence,
			// so no hold — same rule as an issue we could not read at all.
			c.logger.Warn("self-authorization gate: cited issue has no identifiable author, it decides nothing",
				"repo", ref.Repo, "issue", ref.Issue)
			continue
		}
		acknowledged, ackErr := c.issueHasHumanAcknowledgement(ctx, owner, name, issue)
		if acknowledged {
			return SelfAuthorization{}
		}
		if firstSelfFiled != nil {
			continue
		}
		reason := fmt.Sprintf("issue %s#%d was filed by %s and no human has acknowledged it",
			ref.Repo, ref.Issue, issue.GetUser().GetLogin())
		if ackErr != nil {
			// Authorship is established; only the acknowledgement lookup
			// failed. Holding is the safe half of the asymmetry.
			reason = fmt.Sprintf("issue %s#%d was filed by %s and its acknowledgements could not be read (%v)",
				ref.Repo, ref.Issue, issue.GetUser().GetLogin(), ackErr)
		}
		firstSelfFiled = &SelfAuthorization{Held: true, Issue: ref.Issue, Repo: ref.Repo, Reason: reason}
	}
	if firstSelfFiled == nil {
		return SelfAuthorization{}
	}
	return *firstSelfFiled
}

// issueHasHumanAcknowledgement reports whether any person has signalled assent
// on an agent-filed issue. Four signals count, in ascending cost:
//
//  1. the approval label, for a maintainer who wants to approve without prose;
//  2. a human assignee, which is how a maintainer says "yes, and I own it";
//  3. a human comment;
//  4. nothing — which is the incident.
//
// The error is returned rather than swallowed because the caller treats "no
// acknowledgement found" and "could not look" differently.
func (c *Client) issueHasHumanAcknowledgement(ctx context.Context, owner, repo string, issue *gh.Issue) (bool, error) {
	for _, label := range issue.Labels {
		if strings.EqualFold(label.GetName(), HumanAckLabel) {
			return true, nil
		}
	}
	for _, assignee := range issue.Assignees {
		if c.isHumanAuthor(assignee) {
			return true, nil
		}
	}
	if issue.GetComments() == 0 {
		// The issue payload already told us there are none; skip the call.
		return false, nil
	}
	comments, _, err := c.client.Issues.ListComments(ctx, owner, repo, issue.GetNumber(), &gh.IssueListCommentsOptions{
		ListOptions: gh.ListOptions{PerPage: selfAuthCommentPageSize},
	})
	if err != nil {
		return false, err
	}
	for _, comment := range comments {
		if c.isHumanAuthor(comment.GetUser()) {
			return true, nil
		}
	}
	return false, nil
}

// splitRepoRef normalises "owner/repo" or a bare repo name into its two halves,
// defaulting the owner to the hive's org the way CreatePR does.
func splitRepoRef(repo, defaultOwner string) (string, string) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "", ""
	}
	if owner, name, ok := strings.Cut(repo, "/"); ok {
		return strings.TrimSpace(owner), strings.TrimSpace(name)
	}
	return strings.TrimSpace(defaultOwner), repo
}

// selfAuthorizationNotice is the comment left on a held PR. A "hold" label with
// no explanation reads as a malfunction, and the person who has to clear it
// needs to know what would clear it.
func selfAuthorizationNotice(finding SelfAuthorization) string {
	return fmt.Sprintf(`> [!IMPORTANT]
> **Held for human sign-off on the direction, not on the code.**
>
> This PR's only tracked rationale is %s#%d, which the hive filed itself — %s. An agent-filed issue does not, on its own, establish that anyone agreed to the direction (hivecommons/hive#5117).
>
> The change may well be right; nothing here is a review of it. To release the hold, acknowledge the direction on that issue — comment on it, assign yourself, or add the `+"`%s`"+` label — and remove the `+"`hold`"+` label here.`,
		finding.Repo, finding.Issue, finding.Reason, HumanAckLabel)
}
