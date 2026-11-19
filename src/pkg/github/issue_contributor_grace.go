package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// ClaimBlockReasonOwnerGracePrefix is the IssueDispatchBlockReason prefix for
// a contributor claim refused inside the owner grace window
// (hivecommons/hive#18898): the issue was filed moments ago by an owner, a
// maintainer or this hive's own App, who (or whose dispatcher) normally
// claims it within minutes. #18866 and #18880 each grew a duplicate
// contributor PR because the lane claimed the issue seconds after filing.
const ClaimBlockReasonOwnerGracePrefix = "owner grace: "

// Author associations GitHub reports for an owner or maintainer of the
// repository. Anything else (CONTRIBUTOR, NONE, FIRST_TIMER, ...) is an
// outside reporter and never waits for a grace.
var ownerGraceAssociations = map[string]bool{
	"OWNER":        true,
	"MEMBER":       true,
	"COLLABORATOR": true,
}

// botLoginSuffix is the suffix the REST API appends to a GitHub App's login.
const botLoginSuffix = "[bot]"

// SetContributorGrace installs governor.claims.contributor_grace_s as a
// duration: how long an owner/maintainer/hive-filed issue is withheld from
// the contributor lane. d <= 0 turns the grace off.
func (c *Client) SetContributorGrace(d time.Duration) {
	if c == nil {
		return
	}
	if d < 0 {
		d = 0
	}
	c.contributorGraceNanos.Store(int64(d))
}

func (c *Client) contributorGrace() time.Duration {
	if c == nil {
		return 0
	}
	return time.Duration(c.contributorGraceNanos.Load())
}

// ContributorGraceGuard is the contribute queue's cheap pre-check
// (hivecommons/hive#18898): createdAt comes from the enumerated issue, and an
// issue older than the grace, or any issue with the grace off, answers ""
// without a forge read. Only an issue still inside the window reads the
// issue (and, for the `help wanted` waiver, its timeline).
func (c *Client) ContributorGraceGuard(ctx context.Context, repo string, number int, createdAt time.Time) string {
	grace := c.contributorGrace()
	if grace <= 0 {
		return ""
	}
	now := time.Now()
	if !createdAt.IsZero() && now.Sub(createdAt) >= grace {
		return ""
	}
	gate, err := c.issueClaimGate(ctx, repo, number)
	if err != nil {
		// Fail open: a lookup failure must not stall the queue; the ledger's
		// own admission check reads the issue again at claim time.
		return ""
	}
	return c.ownerGraceReason(ctx, repo, number, gate, now)
}

// ownerGraceReason returns the refusal for a contributor claim on an issue
// inside the owner grace window, or "".
func (c *Client) ownerGraceReason(ctx context.Context, repo string, number int, gate issueClaimGateResult, now time.Time) string {
	grace := c.contributorGrace()
	if grace <= 0 || gate.createdAt.IsZero() || gate.closed {
		return ""
	}
	age := now.Sub(gate.createdAt)
	if age < 0 {
		age = 0
	}
	if age >= grace {
		return ""
	}
	if !c.ownerOrHiveFiled(gate) {
		return ""
	}
	if graceHasLabel(gate.labels, helpWantedLabel) && c.helpWantedAddedByPerson(ctx, repo, number) {
		return ""
	}
	return fmt.Sprintf("%sfiled by %s %s ago; contributor claims wait %s unless a person adds `%s`",
		ClaimBlockReasonOwnerGracePrefix, gate.author, graceAgeText(age), graceAgeText(grace), helpWantedLabel)
}

// ownerOrHiveFiled reports whether the issue's author is an owner or
// maintainer (author association) or one of this hive's own bot logins.
func (c *Client) ownerOrHiveFiled(gate issueClaimGateResult) bool {
	if ownerGraceAssociations[strings.ToUpper(gate.association)] {
		return true
	}
	return c.isHiveBotAccount(gate.author)
}

// isHiveBotAccount reports whether login is this hive's own App or one of the
// configured additional bot logins, with or without the "[bot]" suffix.
func (c *Client) isHiveBotAccount(login string) bool {
	login = strings.TrimSuffix(strings.TrimSpace(login), botLoginSuffix)
	if login == "" || c == nil {
		return false
	}
	for _, bot := range c.hiveBotLogins() {
		if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(bot), botLoginSuffix), login) {
			return true
		}
	}
	return false
}

// helpWantedAddedByPerson reports whether a non-bot actor applied the
// `help wanted` label to the issue: a person opening the issue to outside
// help waives the grace. A timeline failure keeps the grace (fail closed:
// waiving needs positive evidence).
func (c *Client) helpWantedAddedByPerson(ctx context.Context, repo string, number int) bool {
	if c == nil || c.client == nil {
		return false
	}
	owner, name := c.splitRepo(repo)
	opts := &gh.ListOptions{PerPage: 100}
	for {
		events, resp, err := c.client.Issues.ListIssueTimeline(ctx, owner, name, number, opts)
		if err != nil {
			if c.logger != nil {
				c.logger.Debug("owner grace: timeline unreadable, keeping the grace", "repo", repo, "number", number, "error", err)
			}
			return false
		}
		for _, event := range events {
			if event == nil || !strings.EqualFold(event.GetEvent(), "labeled") ||
				!strings.EqualFold(strings.TrimSpace(event.GetLabel().GetName()), helpWantedLabel) {
				continue
			}
			actor := event.GetActor()
			if actor == nil || actor.GetLogin() == "" || strings.EqualFold(actor.GetType(), "Bot") ||
				strings.HasSuffix(actor.GetLogin(), botLoginSuffix) || c.isHiveBotAccount(actor.GetLogin()) {
				continue
			}
			return true
		}
		if resp == nil || resp.NextPage == 0 {
			return false
		}
		opts.Page = resp.NextPage
	}
}

func graceHasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), want) {
			return true
		}
	}
	return false
}

// graceAgeText renders a duration in whole minutes ("3m"), or "under 1m".
func graceAgeText(d time.Duration) string {
	if m := int(d / time.Minute); m > 0 {
		return fmt.Sprintf("%dm", m)
	}
	return "under 1m"
}
