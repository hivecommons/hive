package pushbroker

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/hivecommons/hive/crux/prosefilter"
)

// sanitizeHeadCommitMessage applies the shared prose filter
// (hivecommons/hive#19103) to the outgoing HEAD commit's message and amends
// it when a dash was rewritten. It reports whether it amended HEAD.
//
// A commit message travels inside the pushed git objects, so the egress proxy
// cannot rewrite it in transit without changing object ids. The broker is
// the one place hive touches an agent's commits before they leave, and it
// already amends HEAD for the trailing-newline rule, so it is where the
// message is fixed. Only HEAD is rewritten: an older outgoing commit would
// need a rebase of the agent's branch, which the broker never does. Hive
// squash-merges, and the squash commit message hive authors is filtered, so
// what lands on the default branch is clean either way.
//
// The amend keeps the tree, the author and every trailer (Signed-off-by
// included); --cleanup=verbatim keeps the message byte-for-byte apart from
// the rewritten dashes. The switch is prosefilter.AgentWritesEnv.
//
// It is best-effort: a dash rewrite never blocks a push. A failure to read or
// amend the message is logged and the commit leaves as the agent wrote it.
func (b *Broker) sanitizeHeadCommitMessage(ctx context.Context) bool {
	amended, err := b.rewriteHeadCommitMessage(ctx)
	if err != nil && b.Logger != nil {
		b.Logger.Warn("pushbroker could not rewrite dashes in the HEAD commit message; pushing it unchanged",
			"repo", b.Repo, "branch", b.Branch, "error", err)
	}
	return amended && err == nil
}

func (b *Broker) rewriteHeadCommitMessage(ctx context.Context) (amended bool, err error) {
	if !prosefilter.AgentWritesEnabled() {
		return false, nil
	}
	// cat-file gives the message exactly as stored; `git log --format=%B`
	// appends a newline, which --cleanup=verbatim would then keep.
	raw, err := b.git(ctx, "cat-file", "commit", "HEAD")
	if err != nil {
		return false, fmt.Errorf("reading HEAD commit message: %w", err)
	}
	_, msg, found := strings.Cut(string(raw), "\n\n")
	if !found {
		return false, nil
	}
	if !prosefilter.HasDash(msg) {
		return false, nil
	}
	clean := sanitizeCommitMessage(msg)
	if clean == msg {
		return false, nil
	}
	f, err := os.CreateTemp("", "hive-commit-msg-*")
	if err != nil {
		return false, fmt.Errorf("staging commit message: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.WriteString(clean); err != nil {
		_ = f.Close()
		return false, fmt.Errorf("staging commit message: %w", err)
	}
	if err := f.Close(); err != nil {
		return false, fmt.Errorf("staging commit message: %w", err)
	}
	// --only with no paths amends the message alone; the agent's staged
	// changes stay staged and out of the commit.
	if _, err := b.git(ctx, "commit", "--amend", "--only", "--cleanup=verbatim", "-F", f.Name()); err != nil {
		return false, err
	}
	if b.Logger != nil {
		b.Logger.Info("pushbroker rewrote dashes in the HEAD commit message", "repo", b.Repo, "branch", b.Branch,
			"dashes", prosefilter.DashCount(msg)-prosefilter.DashCount(clean))
	}
	return true, nil
}

// sanitizeCommitMessage filters a commit message: the subject line as a
// title, the rest as prose.
func sanitizeCommitMessage(msg string) string {
	subject, rest, hasRest := strings.Cut(msg, "\n")
	subject = prosefilter.SanitizeTitle(subject)
	if !hasRest {
		return subject
	}
	return subject + "\n" + prosefilter.Sanitize(rest)
}
