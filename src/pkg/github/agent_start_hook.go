package github

import (
	"regexp"
	"strings"
)

// Agent start signals (hivecommons/hive#10527).
//
// A governor kick lists many issues and the agent starts on few of them, so
// the hive no longer claims an issue for being listed. It claims it when the
// agent's own relay request first shows it working the issue: a comment,
// label or claim request on the issue (hive-open-issue), or a pull-request
// request naming it (hive-open-pr --issues). The relays below report those
// requests through AgentStartHook once the request is authorized, so the
// agent name is the verified requester, not text the agent wrote.

// Start signals reported through AgentStartHook.
const (
	AgentStartSignalComment   = "comment"
	AgentStartSignalLabel     = "label"
	AgentStartSignalClaim     = "claim"
	AgentStartSignalPRRequest = "pr_request"
)

// AgentStartHook is told that agent's own request shows it working issue
// number in repo. repo is spelled as the request spelled it (bare or
// owner/repo).
type AgentStartHook func(agent, repo string, number int, signal string)

// SetAgentStartHook installs (or with nil, removes) the start-signal hook.
// Safe to call before or after the watchers start.
func (c *Client) SetAgentStartHook(fn AgentStartHook) {
	if c == nil {
		return
	}
	if fn == nil {
		c.agentStartHook.Store(nil)
		return
	}
	c.agentStartHook.Store(&fn)
}

// notifyAgentStart reports a start signal to the installed hook, if any.
func (c *Client) notifyAgentStart(agent, repo string, number int, signal string) {
	if c == nil || agent == "" || repo == "" || number <= 0 {
		return
	}
	if hook := c.agentStartHook.Load(); hook != nil && *hook != nil {
		(*hook)(agent, repo, number, signal)
	}
}

// commentDeferral matches the deferral forms from #12387: a comment that
// opens with "Deferred"/"Deferring" (markdown emphasis allowed), "blocked
// by/on #N" or a negated start ("Not starting yet", "Can't start"), one
// whose opening clause says "is/still/currently blocked by #N", "waiting
// on/for #N" or "deferring … until/pending", and anywhere "deferred this
// cycle" or "pick(s/ing) up once/after/when". Such a comment is not evidence of
// work starting, even when it names the issue or its blocker. The anchored
// forms deliberately apply to the comment's opening clause only — a comma,
// sentence punctuation or newline ends it — not to every line or clause, so
// a progress comment with an aside ("Working on a fix.\n- Deferring the
// refactor to a follow-up.", "Implementing now. Deferring the rename until
// #N lands.", "Opened PR #N, waiting on #N CI.", "cleanup deferred to #N")
// or a past-tense mention ("previously blocked by #N") still records the
// claim, while a comment that opens with the wait ("Waiting for #N CI, then
// merging.") does not. Explicit label, claim and PR requests remain start
// signals independently of any accompanying comment.
var commentDeferral = regexp.MustCompile(`(?i)(?:` + strings.Join([]string{
	`^\W*deferr(?:ed|ing)\b`,                            // Opening deferral, with optional markdown emphasis.
	`^\W*blocked\s+(?:by|on)\s+#\d+`,                    // Opening blocker, not a historical aside.
	`^\W*(?:not|never|won't|cannot|can't)\s+start\w*\b`, // Explicit non-start.
	`^[^.,;!?\n]*?\b(?:is|am|are|still|currently|remains?)\s+blocked\s+(?:by|on)\s+#\d+`, // Opening-clause blocker.
	`^[^.,;!?\n]*?\bwaiting\s+(?:on|for)\s+#\d+`,                                         // Opening-clause wait.
	`^[^.,;!?\n]*?\bdeferr(?:ed|ing)\s+(?:\w+\s+){0,2}?(?:until|pending)\b`,              // Opening conditional deferral.
	`\bdeferred\s+this\s+cycle\b`,                                                        // Explicit cycle-wide deferral anywhere.
	`\bpick(?:s|ing)?\s+(?:this\s+|it\s+)?up\s+(?:once|after|when)\b`,                    // Conditional pickup, with optional object.
}, `|`) + `)`)

// commentStartOverride matches an explicit statement that work is starting
// or resuming, or that the blocker/deferral it opens with is in the past
// ("Blocked by #N earlier; …"). It wins over commentDeferral so a resume
// comment such as "Previously blocked by #N; that merged, starting now."
// still records the claim that keeps a second agent off the issue (#10527).
// The historical form stops at the same clause punctuation as
// commentDeferral's opening forms plus ':' (which those forms deliberately
// do not stop at, so "Status: still blocked by #N" stays a deferral), so a
// time word in the reason of "Deferred this cycle: #N opened yesterday …"
// does not make the deferral historical.
// Occurrences that commentNonStart matches are removed before this runs, so
// a start word inside the deferral clause does not override it. A past
// "started" only overrides with an explicit "now", not a third-party start
// or an old spike.
var commentStartOverride = regexp.MustCompile(`(?i)(?:` + strings.Join([]string{
	`\b(?:starting|started\s+(?:work\s+)?now|resuming|resumed|picking\s+(?:this\s+|it\s+)?up|picked\s+(?:this\s+|it\s+)?up|back\s+on\s+(?:this|it)|no\s+longer\s+blocked|unblocked)\b`, // Present start or explicit resume.
	`\bworking\s+on\s+(?:this|it)\s+now\b`, // Current work, not "working on it means waiting".
	`^\W*(?:was\s+|were\s+)?(?:blocked|deferred)\b[^.,:;!?\n]*?\b(?:earlier|previously|yesterday|last\s+(?:week|cycle|time|month))\b`, // Historical opening blocker.
}, `|`) + `)`)

// commentNonStart matches start words that do not describe work starting
// now: a negated start ("not starting until #N merges"), a start conditional
// on something else within the same clause ("once #N merges and this is
// unblocked", "resuming after that", "will resume once it merges" — the
// rest of that clause is consumed with it), and a third party's start,
// past ("#N was started by scanner", "#N already started this refactor",
// "the scanner picked this up in #N") or
// in progress ("#N is starting the same refactor", "scanner is picking this
// up in #N", "#N is unblocked"). "am" is deliberately absent from the
// present-tense form, and a bare "I"/"we" subject from the past form, so a
// first-person "I am picking this up" or "I picked this up" stays a
// start. Each match is blanked before commentStartOverride is evaluated.
var commentNonStart = regexp.MustCompile(`(?i)(?:` + strings.Join([]string{
	`\b(?:not|never|won't|cannot|can't|before|without)\s+(?:be\s+)?(?:start|resum|pick|unblock|work)\w*`,     // Negated start.
	`\b(?:once|after|when|until|while|unless|if|pending)\b[^.;!?\n]*?\b(?:start|resum|pick|unblock|work)\w*`, // Condition before a start.
	`\b(?:start|resum|unblock)\w*\s+(?:once|after|when|until|unless|if|pending)\b[^.;!?\n]*`,                 // Conditional start clause.
	`\bpick\w*\s+(?:this\s+|it\s+)?up\s+(?:once|after|when|until|unless|if|pending)\b[^.;!?\n]*`,             // Conditional pickup clause.
	`\b(?:was|were|been|already|has|had|#\d+)\s+(?:started|resumed|picked\s+(?:this\s+|it\s+)?up)\b`,         // Third-party past start.
	`\b(?:the\s+\w+|\w+(?:-\w+)+|\w+\s+(?:agent|bot))\s+(?:started|resumed|picked\s+(?:this\s+|it\s+)?up)\b`, // Named third-party past start.
	`\b(?:is|are)\s+(?:starting|resuming|picking\s+(?:this\s+|it\s+)?up|unblocked|working)\b`,                // Third-party present start.
}, `|`) + `)`)

// commentIsDeferral reports whether a comment body is an explicit deferral
// that carries no overriding statement of work starting.
func commentIsDeferral(body string) bool {
	if !commentDeferral.MatchString(body) {
		return false
	}
	return !commentStartOverride.MatchString(commentNonStart.ReplaceAllString(body, " "))
}

// issueRequestStartSignal maps a successful issue request to its start
// signal, or "" when it does not show work starting. Creating an issue,
// closing one, asking for a review, and deferral comments are not starts.
func issueRequestStartSignal(kind, body string) string {
	switch kind {
	case "comment":
		if commentIsDeferral(body) {
			return ""
		}
		return AgentStartSignalComment
	case "label":
		return AgentStartSignalLabel
	case "claim":
		return AgentStartSignalClaim
	}
	return ""
}
