package recommend

import (
	"fmt"
	"strings"
)

// batchBlock renders a copy-pasteable command that acts on a whole section at
// once.
//
// This is the part that actually reduces a maintainer's workload. A list of
// ten ready pull requests still costs ten page loads, ten scrolls and ten
// clicks; on a queue of several hundred that is why the list goes unworked no
// matter how well it is sorted. One block they can paste -- and edit first --
// turns that into a single action.
//
// The commands name every pull request explicitly instead of merging whatever
// a search returns. That is the whole safety design: the reader can delete any
// line they disagree with, and the command can never act on something the
// digest did not show them. A query-driven mass merge would be shorter and
// would be the wrong thing to hand anyone.
func batchBlock(repo string, bucket Bucket, items []Item, cap int) string {
	if repo == "" || len(items) == 0 {
		return ""
	}
	shown := items
	if cap > 0 && len(shown) > cap {
		shown = shown[:cap]
	}

	var cmds []string
	switch bucket {
	case BucketReady:
		for _, it := range shown {
			cmds = append(cmds, fmt.Sprintf("gh pr merge %d --repo %s --squash", it.Number, repo))
		}
	case BucketStaleDraft:
		for _, it := range shown {
			cmds = append(cmds, fmt.Sprintf("gh pr close %d --repo %s --comment %q", it.Number, repo, staleDraftCloseNote))
		}
	case BucketConflicts:
		// Rebasing is not something to fire blindly at a list, and for fork
		// PRs the maintainer cannot push at all. Offer the read-only step
		// that tells them which are worth saving.
		for _, it := range shown {
			cmds = append(cmds, fmt.Sprintf("gh pr view %d --repo %s --json title,author,updatedAt,files", it.Number, repo))
		}
	default:
		return ""
	}

	var b strings.Builder
	b.WriteString("\n<details>\n<summary>")
	b.WriteString(batchSummary[bucket])
	b.WriteString("</summary>\n\n```bash\n")
	b.WriteString(strings.Join(cmds, "\n"))
	b.WriteString("\n```\n\n")
	b.WriteString(batchCaution[bucket])
	b.WriteString("\n</details>\n")
	return b.String()
}

const staleDraftCloseNote = "Closing to keep the review queue workable — please reopen if this is still live."

var batchSummary = map[Bucket]string{
	BucketReady:      "Merge these in one paste",
	BucketStaleDraft: "Close these in one paste",
	BucketConflicts:  "Inspect these before deciding",
}

var batchCaution = map[Bucket]string{
	BucketReady:      "Every pull request is listed separately on purpose — delete any line you do not want before running it.",
	BucketStaleDraft: "Closing a draft is reversible; each one can be reopened by its author.",
	BucketConflicts:  "Read-only. Nothing here changes a pull request.",
}
