package github

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

// The SHA-hold gate exists to get a reproducible build out of an outside
// reporter. It is not meant for the project's own people or for a report that
// already points at the code (hivecommons/hive#18866), and an operator can
// waive it for one issue.

// SHAWaivedLabel is the persistent override: while it is on an issue the SHA
// sweep neither applies nor keeps `needs-sha`. It is applied (and removed) by
// the dashboard's "Waive SHA requirement" row action, which is gated to the
// same roles as releasing a `hold`; a person can also add it by hand.
const SHAWaivedLabel = "sha-waived"

// SHAWaivedLabelDescription is the repo label description for SHAWaivedLabel.
const SHAWaivedLabelDescription = "A maintainer waived the commit-SHA requirement; hive will not hold this issue for a SHA."

const shaWaivedMarkerPrefix = "<!-- hive:sha-waived by="
const shaWaivedMarkerSuffix = " -->"

// SHAWaivedMarker is the hidden marker on the App-authored comment that
// records who waived the requirement.
func SHAWaivedMarker(login string) string {
	return shaWaivedMarkerPrefix + strings.TrimSpace(login) + shaWaivedMarkerSuffix
}

// shaWaivedBy returns the login named by a waiver marker in body, or "".
func shaWaivedBy(body string) string {
	i := strings.Index(body, shaWaivedMarkerPrefix)
	if i < 0 {
		return ""
	}
	rest := body[i+len(shaWaivedMarkerPrefix):]
	j := strings.Index(rest, shaWaivedMarkerSuffix)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// Reasons shaHoldExempt (and the sweep, for the hive's own accounts) give.
const (
	shaExemptInternal   = "internal-author"
	shaExemptWaived     = "waived"
	shaExemptMaintainer = "maintainer-authored"
	shaExemptCodePaths  = "names-code-paths"
)

// defaultSHAExemptAssociations are the author associations treated as the
// project's own people when the caller passes none.
var defaultSHAExemptAssociations = []string{"OWNER", "MEMBER", "COLLABORATOR"}

// repoPathTopDirs are the repository's top-level directories a path must
// start with to count as a concrete code path.
const repoPathTopDirs = `\.github|src|pkg|cmd|internal|docs|bin|config|dashboard|examples|systemd|launchd|githooks`

// repoPathExtensions are the file extensions a code path may end in.
const repoPathExtensions = `go|js|ts|tsx|jsx|py|sh|yaml|yml|json|toml|md|html|css|sql|mod`

// concreteRepoPathPattern matches a repo-relative file path such as
// `src/pkg/github/client.go`: a known top-level directory, one or more
// segments, a known extension, delimited by whitespace or punctuation (so a
// URL or an absolute path like /usr/lib/x.py never matches).
var concreteRepoPathPattern = regexp.MustCompile(
	"(?:^|[\\s`'\"(\\[<])(?:" + repoPathTopDirs + ")/[A-Za-z0-9_./-]*[A-Za-z0-9_-]\\.(?:" + repoPathExtensions + ")(?:[\\s`'\")\\]>:,.;]|$)")

// bodyNamesRepoPaths reports whether an issue body names at least one
// concrete repository file path. It deliberately ignores comments and
// bare file names: a reporter who cites src/... has read the code. The
// signal is self-attested: the text is only pattern-matched and the path is
// never checked to exist, so any reporter can write one to skip the gate.
// That is accepted by design (hivecommons/hive#18866).
func bodyNamesRepoPaths(body string) bool {
	return concreteRepoPathPattern.MatchString(body)
}

// shaHoldExempt reports why the sweep does not gate an issue on a SHA: "" when
// it is gated, else a short reason for the log. Internal authors are handled
// by the caller.
func (cfg SHAHoldConfig) shaHoldExempt(association, body string, labels []string) string {
	switch {
	case labelPresent(labels, SHAWaivedLabel):
		return shaExemptWaived
	case cfg.trustedAssociation(association):
		return shaExemptMaintainer
	case bodyNamesRepoPaths(body):
		return shaExemptCodePaths
	}
	return ""
}

func (cfg SHAHoldConfig) trustedAssociation(association string) bool {
	association = strings.TrimSpace(association)
	if association == "" {
		return false
	}
	set := cfg.TrustedAssociations
	if len(set) == 0 {
		set = defaultSHAExemptAssociations
	}
	for _, a := range set {
		if strings.EqualFold(strings.TrimSpace(a), association) {
			return true
		}
	}
	return false
}

// SHAWaivedBy returns who waived the SHA requirement on repo#number, as the
// sweep last read it from the App-authored waiver comment (or as the
// dashboard recorded it), or "".
func (c *Client) SHAWaivedBy(repo string, number int) string {
	if c == nil {
		return ""
	}
	c.shaWaiversMu.Lock()
	defer c.shaWaiversMu.Unlock()
	if by := c.shaWaivers[shaWaiverKey(repo, number)]; by != shaWaiverUnattributed {
		return by
	}
	return ""
}

// shaWaiverUnattributed marks a waived issue whose waiver comment was read
// and carries no marker (a label added by hand), so the sweep does not list
// its comments again every tick.
const shaWaiverUnattributed = "?"

// shaWaiverKnown reports whether the sweep already resolved who waived the
// issue (or that nobody is named).
func (c *Client) shaWaiverKnown(repo string, number int) bool {
	c.shaWaiversMu.Lock()
	defer c.shaWaiversMu.Unlock()
	_, ok := c.shaWaivers[shaWaiverKey(repo, number)]
	return ok
}

// RecordSHAWaiver remembers who waived the requirement; an empty login
// forgets it.
func (c *Client) RecordSHAWaiver(repo string, number int, login string) {
	if c == nil {
		return
	}
	key := shaWaiverKey(repo, number)
	c.shaWaiversMu.Lock()
	defer c.shaWaiversMu.Unlock()
	if strings.TrimSpace(login) == "" {
		delete(c.shaWaivers, key)
		return
	}
	if c.shaWaivers == nil {
		c.shaWaivers = map[string]string{}
	}
	c.shaWaivers[key] = strings.TrimSpace(login)
}

func shaWaiverKey(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

// refreshSHAWaiver reads the newest App-authored waiver comment so the
// dashboard can name who waived the requirement after a restart.
func (c *Client) refreshSHAWaiver(ctx context.Context, owner, repo string, number int) {
	key := owner + "/" + repo
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return
	}
	by := ""
	for _, comment := range comments {
		if comment == nil || !c.isTrustedAppBotCommentAuthor(comment) {
			continue
		}
		if who := shaWaivedBy(comment.GetBody()); who != "" {
			by = who
		}
	}
	if by == "" {
		by = shaWaiverUnattributed
	}
	c.RecordSHAWaiver(key, number, by)
}
