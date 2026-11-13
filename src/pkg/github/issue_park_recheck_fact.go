package github

import (
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/writeback"
)

// The park re-check tells the write-back gate when a park is the hive's own
// (writeback.Item.HiveAppliedPark), and the gate then posts the note without
// the review opt-in label. Two things keep that allowance to what it is for.
//
// What "the hive applied the park" means. The hive's bot writes
// stage/045-needs-decision on any issue that carries a hard-suppress label,
// whoever applied that label: a maintainer adds `needs-spec`, the stage
// engine derives 045, and the 045 label event names the bot. `needs-spec`
// is not a parking label for the un-park commands (issue_park_labels.go), so
// the park is dated from the bot's event although a person parked the issue.
// The fact is therefore true only when every label that parks the issue on a
// person was put there by the hive (parkHiveApplied).
//
// What the note may list. The re-check counts a merged pull request in any
// repository that refers to the issue. On an issue with the opt-in label, or
// one the hive filed, the note lists each of them, as it always has. A note
// the gate allows only because of the fact lists the changes the park text
// names and the pull requests merged in the project's own repositories, and
// nothing else (parkProjectChanges). Two things such a note says about a
// listed change are held to the same line: "the change this issue asks for
// has landed" is said only for an item in a project repository
// (parkProjectNamed), and a closing comment is quoted only when GitHub named
// who closed the pull request, so the words are that account's.

// parkAppliedByHive reports whether actor, the login that applied a label,
// is one of this hive's own identities: its App bot (with or without the
// "[bot]" suffix), an additional hive bot login, or the configured
// ai_author. It is the test the blocked sweep applies to the actor of a
// `blocked` label (BlockedLabelAppliedByHive): a contributor relay applies
// both labels with a hub-minted installation token, so the event names the
// App bot. An empty actor, any other account, and a client with no hive
// identity configured report false.
//
// On a hive that runs on a personal access token, project.ai_author is a
// hive identity. When that account is the operator's own, a park the
// operator applies by hand under it counts as the hive's.
func (c *Client) parkAppliedByHive(actor string) bool {
	return c.isOwnBotLogin(actor) || c.IsHiveAuthorLogin(actor)
}

// parkSuppressLabel reports whether label is one that parks an issue on a
// person: a built-in hard-suppress label (hardSuppressIssueLabels:
// needs-human, stage/045-needs-decision, the pre-045 stage/085, needs-spec,
// needs-decision, needs-direction, stage/017-possible-existing) or a label
// the operator's project.issue_filter.hard_suppress_labels places in a
// bucket. These are the labels the stage engine derives the decision park
// from (parkedOnPersonLabel). `blocked` is not among them: it does not start
// a decision park (isDecisionParkLabel).
func (c *Client) parkSuppressLabel(label string) bool {
	return hardSuppressIssueLabel([]string{label}) != "" || c.configuredParkingBucket(label) != ""
}

// parkLabelLastAppliedBy returns the login of the newest `labeled` event
// for label in events, and whether the timeline has one.
func parkLabelLastAppliedBy(events []*gh.Timeline, label string) (string, bool) {
	want := strings.TrimSpace(label)
	actor, found := "", false
	var newest time.Time
	for _, ev := range events {
		if ev == nil || ev.Label == nil || !strings.EqualFold(ev.GetEvent(), "labeled") {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(ev.GetLabel().GetName()), want) {
			continue
		}
		// GitHub lists the timeline oldest first; an equal or later time
		// wins, so the last of two events in the same second is the newest.
		if at := ev.GetCreatedAt().Time; !found || !at.Before(newest) {
			actor, found, newest = strings.TrimSpace(ev.GetActor().GetLogin()), true, at
		}
	}
	return actor, found
}

// parkHiveApplied reports whether the issue's current park is the hive's
// own: parkedBy, the account that applied the label the park is dated from
// (parkStartFrom), is a hive identity, and so is the account behind the
// newest `labeled` event of every hard-suppress label the issue carries now
// (labels). A label with no such event in the timeline is not the hive's,
// and an issue that carries no hard-suppress label at all reports false.
//
// So a park that rests on a person's label is not the hive's, although the
// hive wrote the stage label for it: a person's `needs-spec` is still on the
// issue next to the bot's 045. Neither is a park the hive applied and a
// person then removed and re-added inside parkRelabelGap: the park is still
// dated from the hive's event, but the label on the issue is the person's.
func (c *Client) parkHiveApplied(parkedBy string, labels []string, events []*gh.Timeline) bool {
	if !c.parkAppliedByHive(parkedBy) {
		return false
	}
	checked := 0
	for _, label := range labels {
		if !c.parkSuppressLabel(label) {
			continue
		}
		checked++
		actor, found := parkLabelLastAppliedBy(events, label)
		if !found || !c.parkAppliedByHive(actor) {
			return false
		}
	}
	return checked > 0
}

// parkNoteOnlyByFact reports whether the gate would refuse the note on item
// were the park not the hive's: the fact is set, and the same question
// without it is answered no. Decide records no refusal. When this is false
// the note is one the hive posted before the fact existed (the issue carries
// the opt-in label, the hive filed it, the repo's mode is full, no gate is
// installed) and it lists every change, as before.
func (c *Client) parkNoteOnlyByFact(repo string, item writeback.Item) bool {
	if !item.HiveAppliedPark {
		return false
	}
	item.HiveAppliedPark = false
	return !c.WritebackGate().Decide(repo, item, writeback.KindNeedsDecisionComment).Allowed
}

// parkProjectRepo reports whether repo ("owner/name") is issueRepo or one of
// the repositories this hive is configured with (getRepos, project.repos; a
// bare name there takes the client's org through splitRepo). Owner and name
// are compared together, ignoring case.
func (c *Client) parkProjectRepo(repo, issueRepo string) bool {
	want := strings.ToLower(strings.TrimSpace(repo))
	if want == "" {
		return false
	}
	if want == strings.ToLower(strings.TrimSpace(issueRepo)) {
		return true
	}
	for _, entry := range c.getRepos() {
		owner, name := c.splitRepo(strings.TrimSpace(entry))
		if strings.ToLower(owner+"/"+name) == want {
			return true
		}
	}
	return false
}

// parkProjectChanges keeps the changes a note allowed only by the fact may
// list: those the park text names, and those in the project's own
// repositories. A change that is not named is a merged pull request that
// refers to the issue, or the hive's copy of a pull request that does; both
// are kept only when they are in a project repository (a copy is in its
// original's). The order is kept. Dropped changes are counted in the debug
// log, never in the note.
//
// A copy's closing note is kept only when GitHub named who closed the copy
// (closerKnown). Otherwise the quote is the latest comment by any person
// around the close, and it is taken off: the copy is listed as closed, with
// no author, quote or link. A copy the park does not name is a change only
// because of its closing note (parkCopyChanges), so without one it is
// dropped.
func (c *Client) parkProjectChanges(changes []parkChange, issueRepo string, number int) []parkChange {
	kept := make([]parkChange, 0, len(changes))
	for _, ch := range changes {
		if !ch.named && !c.parkProjectRepo(ch.repo, issueRepo) {
			continue
		}
		if ch.quote != "" && !ch.closerKnown {
			ch.by, ch.quote, ch.link = "", "", ""
			if !ch.named {
				continue
			}
		}
		kept = append(kept, ch)
	}
	if dropped := len(changes) - len(kept); dropped > 0 && c.logger != nil {
		c.logger.Debug("park re-check left out changes from outside the project: the note is allowed only because the hive applied the park",
			"repo", issueRepo, "issue", number, "dropped", dropped, "kept", len(kept))
	}
	return kept
}

// parkProjectNamed returns the named references (parkRefKey keys) that are
// in a project repository. A note allowed only by the fact reads a patch,
// and says the change landed, only for these: the pull request that landed
// it is in the named item's own repository (markParkLandings), so an item
// outside the project is still listed as merged or closed, without the
// landed line, the file name or the marker the "What to reply" notice reads.
func (c *Client) parkProjectNamed(named []string, issueRepo string) []string {
	out := make([]string, 0, len(named))
	for _, key := range named {
		if repo, _, ok := splitParkRefKey(key); ok && c.parkProjectRepo(repo, issueRepo) {
			out = append(out, key)
		}
	}
	return out
}
