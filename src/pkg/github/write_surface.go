package github

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/logscrub"
)

// The audited write surface (hivecommons/hive#9587, phase 1).
//
// Every GitHub write an agent asks the hive to perform arrives through one of
// the request relays (pr-, issue-, review-, merge- and push-branch-request
// watchers). Each
// relay operation has a fixed name below. Those names are:
//
//   - the vocabulary of the per-lane allowlist (config write_surface.allowlist),
//     enforced here at the watcher layer, after the file-UID authorizer has
//     proved which agent is speaking;
//   - the "op=" value on the refusal audit entry, so a refused call is
//     findable by operation.
//
// See src/docs/github-write-surface.md for the full inventory.
const (
	// WriteOpOpenPR opens a pull request (pr-request watcher).
	WriteOpOpenPR = "open_pr"
	// WriteOpCreateIssue files an issue (issue-request watcher, kind "issue").
	WriteOpCreateIssue = "create_issue"
	// WriteOpComment comments on an issue or PR (issue-request watcher, kind
	// "comment").
	WriteOpComment = "comment"
	// WriteOpClaim labels an issue as claimed by the agent (issue-request
	// watcher, kind "claim").
	WriteOpClaim = "claim"
	// WriteOpCloseIssue closes an issue, or a PR without merging it
	// (issue-request watcher, kind "close").
	WriteOpCloseIssue = "close_issue"
	// WriteOpLabel adds or removes plain labels on an existing issue or PR
	// (issue-request watcher, kind "label"). Hive-controlled labels (the
	// merge-queue label, hold labels, the hive/ namespace, human-decision
	// labels) are refused by the relay whatever the allowlist says.
	WriteOpLabel = "label"
	// WriteOpRequestReview asks users and/or teams to review an existing PR
	// (issue-request watcher, kind "request_review").
	WriteOpRequestReview = "request_review"
	// WriteOpReview submits a PR review, replies in a review thread, or
	// records a verdict (review-request watcher).
	WriteOpReview = "review"
	// WriteOpResolveThread resolves a bot-opened review thread
	// (review-request watcher, event "resolve_thread").
	WriteOpResolveThread = "resolve_thread"
	// WriteOpMergePR merges a pull request (merge-request watcher).
	WriteOpMergePR = "merge_pr"
	// WriteOpPushBranch pushes an agent's local branch to GitHub
	// (push-branch-request watcher).
	WriteOpPushBranch = "push_branch"
)

// WriteOps returns every relay operation name, in a stable order. Config
// validation and the docs inventory are checked against it.
func WriteOps() []string {
	return []string{
		WriteOpOpenPR,
		WriteOpCreateIssue,
		WriteOpComment,
		WriteOpClaim,
		WriteOpCloseIssue,
		WriteOpLabel,
		WriteOpRequestReview,
		WriteOpReview,
		WriteOpResolveThread,
		WriteOpMergePR,
		WriteOpPushBranch,
	}
}

// IsWriteOp reports whether op names a relay operation.
func IsWriteOp(op string) bool {
	op = strings.ToLower(strings.TrimSpace(op))
	for _, known := range WriteOps() {
		if op == known {
			return true
		}
	}
	return false
}

// AuditActionAgentWriteRefused is recorded when a relay refuses a request
// because the agent's lane allowlist does not include the operation. It is
// deliberately NOT in the activity collector's output set: a refused write
// produced nothing on GitHub.
const AuditActionAgentWriteRefused = "agent_write_refused"

// SetWriteAllowlistFunc installs the per-lane write allowlist predicate
// (#9587). The hive passes config's AgentMayWrite, so an allowlist edited in
// config is in force on the very next relay request; nil clears it, which is
// the unrestricted behaviour every hive had before.
func (c *Client) SetWriteAllowlistFunc(fn func(agent, op string) bool) {
	if c == nil {
		return
	}
	c.reposMu.Lock()
	defer c.reposMu.Unlock()
	c.agentMayWrite = fn
}

// AgentMayWrite reports whether agent's lane may perform op through the relays.
//
// Fails OPEN when no predicate is configured or the agent is unnamed, the same
// contract as AgentServesRepo: the allowlist can only NARROW what an agent may
// already do, and an unnamed request is refused by the authorizer long before
// it gets here.
func (c *Client) AgentMayWrite(agent, op string) bool {
	if c == nil {
		return true
	}
	c.reposMu.RLock()
	may := c.agentMayWrite
	c.reposMu.RUnlock()
	if may == nil || agent == "" {
		return true
	}
	return may(agent, op)
}

// WriteAllowlistReason is the agent- and operator-facing explanation written
// into a relay request's result file when the lane allowlist refuses it.
func WriteAllowlistReason(agent, op string) string {
	return "agent " + agent + " may not perform " + op + " - its lane's write allowlist (write_surface.allowlist) does not include it. Add " + op + " to the agent's allowlist entry to allow this."
}

// refuseWrite applies the lane allowlist to one relay request. It returns the
// refusal reason and true when the request must be refused, and in that case
// has already written the refusal audit entry. Callers write the result file
// and quarantine the request through their own deny path, so a refusal looks
// exactly like every other policy denial to the agent.
//
// Call it only AFTER the file-UID authorizer: the allowlist is keyed on the
// agent name, and that name is only trustworthy once the authorizer has
// matched it to the file's owner.
func (c *Client) refuseWrite(agent, op, repo string, target int) (string, bool) {
	if c.AgentMayWrite(agent, op) {
		return "", false
	}
	reason := WriteAllowlistReason(agent, op)
	c.recordWriteAudit(AuditActionAgentWriteRefused, InvocationMeta{Agent: agent},
		WriteTarget{Repo: repo, Number: target}, "op", op, "outcome", "refused")
	return reason, true
}

// WriteTarget names what one hive-mediated GitHub write touched. Write sites
// pass it explicitly (#9587 phase 2) so the audit entry's typed repo and target
// come from the value the write actually used, never from re-parsing a string.
type WriteTarget struct {
	// Repo is the repository written to (bare or "owner/repo").
	Repo string
	// Number is the issue or PR number written to; zero when the write has no
	// numbered target (a label created on a repo, a refused open_pr).
	Number int
}

// recordWriteAudit writes the audit entry for one hive-mediated GitHub write,
// with the typed repo/target taken from target. The legacy "repo=" and
// "number=" detail pairs are still written first, in the same order as
// before, so every reader of the detail string keeps working.
//
// Any "repo" or "number" pair in extra is dropped: target is the one source
// of both, so the typed field and the detail pair can never disagree.
func (c *Client) recordWriteAudit(action string, m InvocationMeta, target WriteTarget, extra ...string) {
	if c == nil {
		return
	}
	c.deliverAuditRecord(writeAuditRecord(action, m, target, extra...))
}

// writeAuditRecord builds the redacted record recordWriteAudit delivers.
func writeAuditRecord(action string, m InvocationMeta, target WriteTarget, extra ...string) AuditRecord {
	repo := strings.TrimSpace(target.Repo)
	pairs := make([]string, 0, len(extra)+auditTargetPairLen)
	pairs = append(pairs, auditPairRepo, repo)
	if target.Number > 0 {
		pairs = append(pairs, auditPairNumber, strconv.Itoa(target.Number))
	}
	for i := 0; i+1 < len(extra); i += 2 {
		if extra[i] == auditPairRepo || extra[i] == auditPairNumber {
			continue
		}
		pairs = append(pairs, extra[i], extra[i+1])
	}
	rec := AuditRecord{
		Action: action,
		Detail: redactAuditText(m.AuditDetail(pairs...)),
		Agent:  m.Agent,
		Repo:   redactAuditText(repo),
	}
	if target.Number > 0 {
		rec.Target = target.Number
	}
	return rec
}

// Detail pair keys that carry the typed repo/target in the legacy format.
const (
	auditPairRepo   = "repo"
	auditPairNumber = "number"
	// auditTargetPairLen is the room the repo and number pairs take.
	auditTargetPairLen = 4
)

// AuditRecord is one typed audit entry for a hive-mediated GitHub write. Repo
// and Target are first-class so consumers (the activity collector, per-repo
// cost attribution #4836) read them directly instead of parsing Detail. Detail
// keeps the "k=v, k=v" string, including the repo= and number= pairs, for every
// existing reader of that format.
type AuditRecord struct {
	Action string
	Detail string
	Agent  string
	// Repo is the repository written to, as the request named it (bare or
	// "owner/repo"). Empty when the write has no repository.
	Repo string
	// Target is the issue or PR number written to. Zero when the write has no
	// numbered target (e.g. a refused open_pr, which never got a number).
	Target int
}

// authorizationValuePattern matches an HTTP Authorization header (or a
// JSON/k=v rendering of one) and captures everything up to its value, so the
// value itself can be masked. logscrub already masks "Bearer <token>" and
// GitHub-prefixed tokens; this closes the "Authorization: token <x>" and
// "Authorization: Basic <x>" forms, whose values carry no recognisable prefix.
var authorizationValuePattern = regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?)(?:(?:bearer|token|basic)\s+)?[^\s,"']+`)

// auditRedactedValue replaces a masked Authorization header value. It matches
// logscrub's default replacement so every scrubbed span reads the same.
const auditRedactedValue = "[REDACTED]"

// redactAuditText masks credential material in text bound for the audit log.
// Audit details carry agent-supplied values (repo names, override reasons,
// URLs), so they are scrubbed on the way in rather than trusted.
func redactAuditText(s string) string {
	if s == "" {
		return s
	}
	s = logscrub.ScrubString(s)
	return authorizationValuePattern.ReplaceAllString(s, "${1}"+auditRedactedValue)
}

// auditRecordFor builds the typed record for one audit call. Repo and Target
// are taken from the call's own structured "repo" and "number" pairs, the
// arguments every write site already passes, so the typed fields and the
// legacy detail pairs can never disagree.
func auditRecordFor(action string, m InvocationMeta, extra ...string) AuditRecord {
	rec := AuditRecord{
		Action: action,
		Detail: redactAuditText(m.AuditDetail(extra...)),
		Agent:  m.Agent,
	}
	for i := 0; i+1 < len(extra); i += 2 {
		switch extra[i] {
		case auditPairRepo:
			if rec.Repo == "" {
				rec.Repo = redactAuditText(strings.TrimSpace(extra[i+1]))
			}
		case auditPairNumber:
			if rec.Target == 0 {
				if n, err := strconv.Atoi(strings.TrimSpace(extra[i+1])); err == nil && n > 0 {
					rec.Target = n
				}
			}
		}
	}
	return rec
}
