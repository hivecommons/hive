package github

import (
	"context"
	"errors"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// Rebase requests to a dependency bot that has stopped maintaining its own
// branch (write-back kind bot_rebase_request). The hive never pushes to a
// dependency bot's branch (DependencyBotBranch); these ask the bot to
// regenerate it through the bot's own mechanism instead. Callers consult
// the write-back gate first. Only Renovate is asked: Dependabot refuses
// comment commands from a GitHub App (dependabot/dependabot-core#9147), so the
// hive never posts a Dependabot command anywhere: a stranded Dependabot PR,
// like an autofix-pushed Dependabot head whose required check never ran, goes
// to a person, whom the hand-off comment asks to post
// DependabotRecreateCommand.

// RenovateRebaseCheckMarker is the HTML comment Renovate puts on the
// rebase/retry checkbox line of every PR description it writes
// (" - [ ] <!-- rebase-check -->If you want to rebase/retry this PR, check
// this box"). Renovate regenerates the branch on its next run once the box
// is ticked, whoever ticked it.
const RenovateRebaseCheckMarker = "<!-- rebase-check -->"

// DependabotRecreateCommand asks Dependabot to regenerate its PR from the
// current base. The hive only names it in hand-off comments for a person
// with push access to post; it never posts it itself. `@dependabot rebase` is refused once anyone else has pushed
// to the branch ("Looks like this PR has been edited by someone other than
// Dependabot"); recreate is the command Dependabot names for that case.
const DependabotRecreateCommand = "@dependabot recreate"

// AuditActionDependencyBotRebaseRequested is recorded when the hive ticks a
// Renovate rebase/retry box.
const AuditActionDependencyBotRebaseRequested = "dependency_bot_rebase_requested"

// ErrDependencyBotHeadMoved means the PR's head is no longer the one the
// caller asked about: the bot (or someone) already moved it, so nothing was
// written.
var ErrDependencyBotHeadMoved = errors.New("dependency-bot PR head moved since it was read")

// RenovateRebaseCheckbox is the state of Renovate's rebase/retry checkbox in
// a PR description.
type RenovateRebaseCheckbox int

const (
	// RenovateRebaseCheckboxAbsent: the description has no rebase-check line.
	RenovateRebaseCheckboxAbsent RenovateRebaseCheckbox = iota
	// RenovateRebaseCheckboxUnticked: `- [ ] <!-- rebase-check -->`.
	RenovateRebaseCheckboxUnticked
	// RenovateRebaseCheckboxTicked: `- [x] <!-- rebase-check -->`; a
	// rebase is already requested and Renovate has not acted on it yet.
	RenovateRebaseCheckboxTicked
)

// TickRenovateRebaseCheckbox returns body with Renovate's rebase/retry box
// ticked and the state the box was in. Only the three bytes "[ ]" on the
// first rebase-check line change; every other byte, line endings and
// indentation included, is returned as it was. A body whose box is absent or
// already ticked is returned unchanged.
func TickRenovateRebaseCheckbox(body string) (string, RenovateRebaseCheckbox) {
	const unticked, ticked, tickedUpper = "- [ ] ", "- [x] ", "- [X] "
	for start := 0; start < len(body); {
		end := strings.IndexByte(body[start:], '\n')
		line := body[start:]
		if end >= 0 {
			line = body[start : start+end]
		}
		rest := strings.TrimLeft(line, " \t")
		indent := len(line) - len(rest)
		switch {
		case strings.HasPrefix(rest, unticked+RenovateRebaseCheckMarker):
			at := start + indent + len("- ")
			return body[:at] + "[x]" + body[at+len("[ ]"):], RenovateRebaseCheckboxUnticked
		case strings.HasPrefix(rest, ticked+RenovateRebaseCheckMarker), strings.HasPrefix(rest, tickedUpper+RenovateRebaseCheckMarker):
			return body, RenovateRebaseCheckboxTicked
		}
		if end < 0 {
			break
		}
		start += end + 1
	}
	return body, RenovateRebaseCheckboxAbsent
}

// RequestRenovateRebase ticks the rebase/retry box in a Renovate PR's
// description, provided the PR's head is still headSHA. It reads the PR
// fresh (the description changes on every Renovate run) and writes the
// description back with only the checkbox line changed. It returns the state
// the box was in: only RenovateRebaseCheckboxUnticked means a write was made.
// The caller must have consulted the write-back gate (bot_rebase_request).
func (c *Client) RequestRenovateRebase(ctx context.Context, repo string, number int, headSHA string) (RenovateRebaseCheckbox, error) {
	if c == nil || c.client == nil {
		return RenovateRebaseCheckboxAbsent, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	ctx = WithRESTCaller(ctx, "hive:dependency_bot_rebase")
	pr, _, err := c.client.PullRequests.Get(ctx, owner, repoName, number)
	if err != nil {
		return RenovateRebaseCheckboxAbsent, err
	}
	if headSHA != "" && !strings.EqualFold(pr.GetHead().GetSHA(), headSHA) {
		return RenovateRebaseCheckboxAbsent, ErrDependencyBotHeadMoved
	}
	body, state := TickRenovateRebaseCheckbox(pr.GetBody())
	if state != RenovateRebaseCheckboxUnticked {
		return state, nil
	}
	_, resp, err := c.client.PullRequests.Edit(ctx, owner, repoName, number, &gh.PullRequest{Body: gh.Ptr(body)}) // writeback:gated — the dependency-bot sweep consults WritebackAllowed (bot_rebase_request) before calling
	c.noteWritebackOutcome(owner+"/"+repoName, resp, err)
	if err != nil {
		return state, err
	}
	c.recordWriteAudit(AuditActionDependencyBotRebaseRequested, hiveWriteMeta(), WriteTarget{Repo: owner + "/" + repoName, Number: number},
		"head", headSHA, "via", "renovate-checkbox")
	return state, nil
}
