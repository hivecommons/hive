package github

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// A PR the hive opens to supersede an adopted contributor PR
// (automerge.AdoptionPRBody) is authored by the App, but its code is the
// contributor's: the branch starts at the adopted head, unchanged. Judging
// it by the PR author turned a blocking alert on the contributor's PR into
// an "author is trusted (hive agent)" notice on the App's copy of the same
// commit, with nothing left to stop a merge lane (projectbluefin/utah#930 →
// #936). The sweep judges such a PR by the author of the code instead.

// adoptedFromRE matches the line AdoptionPRBody opens the superseding PR's
// body with: "Supersedes #N (<cause>; original commits by @login preserved)."
var adoptedFromRE = regexp.MustCompile(`(?m)^Supersedes #(\d+) \([^\n]*; original commits by @([A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\[bot\])?) preserved\)`)

// AdoptedFrom reads the adopted PR's number and author from a superseding
// PR's body; ok is false when the body has no such line.
func AdoptedFrom(body string) (number int, author string, ok bool) {
	m := adoptedFromRE.FindStringSubmatch(body)
	if m == nil {
		return 0, "", false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, "", false
	}
	return n, m[2], true
}

// sentinelCodeAuthor returns the login whose trust decides the PR: the
// adopted PR's author when the PR is the App's superseding copy, else the PR
// author. origin is the adopted PR's number, 0 when the PR is not one.
func (c *Client) sentinelCodeAuthor(pr *gh.PullRequest) (login string, origin int) {
	author := pr.GetUser().GetLogin()
	app := strings.TrimSpace(c.AppBotLogin())
	if app == "" || !strings.EqualFold(author, app) {
		return author, 0
	}
	n, adopted, ok := AdoptedFrom(pr.GetBody())
	if !ok || n == pr.GetNumber() || strings.EqualFold(adopted, app) {
		return author, 0
	}
	return adopted, n
}

// sentinelAdoptedNote is the sentence the marker comment adds on a
// superseding PR, so the alert does not read as a verdict on the App.
func sentinelAdoptedNote(origin int, author string) string {
	return fmt.Sprintf("\nThis PR is the hive's adoption of #%d. Its code is @%s's, so Hive judges it by that author, not by the hive account that opened it.\n", origin, author)
}

// sentinelOriginClearedNote closes the marker comment when a maintainer
// already cleared the alert on the adopted PR at this same head; the
// arguments are the label and the adopted PR's number.
const sentinelOriginClearedNote = "_A maintainer removed the `%s` label from #%d after Hive alerted on this same head there, so Hive treats this head as reviewed and will not re-apply the label unless new commits are pushed._"

// sentinelOriginCleared reports whether the adopted PR carries the hive's
// own blocking alert for head and no longer carries the label: the same
// proof sentinelClearedByMaintainer reads on the PR itself, so a maintainer
// who reviewed the head before the adoption is not asked again. Any read
// failure counts as not cleared, which keeps the label.
func (c *Client) sentinelOriginCleared(ctx context.Context, owner, repo string, origin int, head, label string) bool {
	pr, _, err := c.client.PullRequests.Get(ctx, owner, repo, origin)
	if err != nil || pr == nil || prHasLabel(pr, label) {
		return false
	}
	comment, posted, err := c.findOwnMarkerComment(ctx, owner, repo, origin, SentinelMarker+" "+head)
	if err != nil {
		return false
	}
	return sentinelClearedByMaintainer(comment, posted)
}
