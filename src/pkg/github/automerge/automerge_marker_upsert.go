package automerge

// One marker comment per PR, edited in place (hivecommons/hive#17066).
//
// Several lane notes used to key their marker on the head SHA so a new push
// got a fresh copy: on a long-lived PR with many pushes that meant eight
// `hive:signoff-attribution` and five `hive:unauthorized` comments saying
// the same thing (hivecommons/hive#16524). The claim ledger
// (hivecommons/hive#16433) showed the quieter shape: find the hive's own
// comment carrying the marker and rewrite it, which notifies nobody, and
// keep a short, capped "updated" history inside the comment so a reader
// who saw the earlier wording knows what changed.

import (
	"context"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const (
	// markerHistoryCap bounds the "updated" lines kept inside an upserted
	// comment: the five most recent, oldest dropped first.
	markerHistoryCap = 5
	// markerHistoryHeading opens the history block; every line after it
	// that starts with markerHistoryBullet is one history entry.
	markerHistoryHeading = "<sub>Updates (newest last):</sub>"
	markerHistoryBullet  = "- "
	// markerHistoryTimeLayout is the timestamp a history line carries.
	markerHistoryTimeLayout = "2006-01-02 15:04 UTC"
)

// findOwnMarkerComment returns the most recent comment on owner/repo#number
// that match accepts and that one of the hive's own accounts wrote, or nil
// when none exists. Only the hive's App login counts as its own: anyone can
// type a marker, and the hive must never edit a stranger's comment. When
// the hive cannot name its App login, any author is accepted — the same
// fallback the sentinel's marker reads use.
func (c *Engine) findOwnMarkerComment(ctx context.Context, owner, repo string, number int, match func(body string) bool) (*gh.IssueComment, error) {
	if c == nil || c.gh == nil || match == nil {
		return nil, nil
	}
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	app := ""
	if c.transport != nil {
		app = normalizeBotLogin(c.transport.AppBotLogin())
	}
	var found *gh.IssueComment
	for _, cm := range comments {
		if cm == nil || !match(cm.GetBody()) {
			continue
		}
		if app != "" && normalizeBotLogin(cm.GetUser().GetLogin()) != app {
			continue
		}
		found = cm
	}
	return found, nil
}

// editMarkerComment rewrites the hive's own comment id with body. An edit
// notifies nobody, which is the point.
func (c *Engine) editMarkerComment(ctx context.Context, owner, repo string, id int64, body string) error {
	_, _, err := c.gh.Issues.EditComment(ctx, owner, repo, id, &gh.IssueComment{Body: gh.Ptr(body)}) // writeback:gated
	return err
}

// splitMarkerHistory separates an upserted comment body into the part
// before the history block and the history lines themselves (without the
// bullet). A body with no history block returns itself and nil.
func splitMarkerHistory(body string) (core string, history []string) {
	idx := strings.Index(body, markerHistoryHeading)
	if idx < 0 {
		return strings.TrimRight(body, "\n"), nil
	}
	core = strings.TrimRight(body[:idx], "\n")
	for _, line := range strings.Split(body[idx+len(markerHistoryHeading):], "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, markerHistoryBullet) {
			history = append(history, strings.TrimPrefix(line, markerHistoryBullet))
		}
	}
	return core, history
}

// appendMarkerHistory adds line to history, dropping the oldest entries so
// at most markerHistoryCap remain.
func appendMarkerHistory(history []string, line string) []string {
	history = append(history, line)
	if len(history) > markerHistoryCap {
		history = history[len(history)-markerHistoryCap:]
	}
	return history
}

// markerHistoryLine renders one history entry: the time, then what changed.
func markerHistoryLine(at time.Time, change string) string {
	return "updated " + at.UTC().Format(markerHistoryTimeLayout) + ": " + change
}

// renderMarkerHistory appends the history block to core; no history means
// core alone.
func renderMarkerHistory(core string, history []string) string {
	core = strings.TrimRight(core, "\n") + "\n"
	if len(history) == 0 {
		return core
	}
	var b strings.Builder
	b.WriteString(core)
	b.WriteString("\n")
	b.WriteString(markerHistoryHeading)
	b.WriteString("\n")
	for _, h := range history {
		b.WriteString(markerHistoryBullet)
		b.WriteString(h)
		b.WriteString("\n")
	}
	return b.String()
}
