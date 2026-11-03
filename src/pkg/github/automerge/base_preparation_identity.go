package automerge

import (
	"slices"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const preparationObservationTimeout = 10 * time.Second

// A lane result belongs to the evaluated admission identity, not merely to a
// PR number. State/mergeability may change as our mutation completes; mutable
// authorship, head/base, policy labels and review inputs must still agree.
func preparationObservationIdentity(evaluated, current *gh.PullRequest) bool {
	if evaluated == nil || current == nil || evaluated.GetHead().GetSHA() == "" {
		return false
	}
	beforeLabels, afterLabels := labelNames(evaluated.Labels), labelNames(current.Labels)
	slices.Sort(beforeLabels)
	slices.Sort(afterLabels)
	return evaluated.GetNumber() == current.GetNumber() &&
		evaluated.GetHead().GetSHA() == current.GetHead().GetSHA() &&
		evaluated.GetHead().GetRepo().GetFullName() == current.GetHead().GetRepo().GetFullName() &&
		evaluated.GetBase().GetRef() == current.GetBase().GetRef() &&
		evaluated.GetBase().GetSHA() == current.GetBase().GetSHA() &&
		strings.EqualFold(evaluated.GetUser().GetLogin(), current.GetUser().GetLogin()) &&
		evaluated.GetTitle() == current.GetTitle() && evaluated.GetBody() == current.GetBody() &&
		evaluated.GetDraft() == current.GetDraft() && slices.Equal(beforeLabels, afterLabels)
}
