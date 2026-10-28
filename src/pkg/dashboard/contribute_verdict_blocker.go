package dashboard

import (
	"context"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// A blocked verdict's `blocked` label is a gate only a human lifts unless the
// hive can read what it waits on (hivecommons/hive#15023). When the reason
// names its blocker, the hub records it — a native "blocked by" link for an
// open issue, a blocked-on marker for an open PR — so the existing reconciles
// lift `blocked` once the blocker closes. A reason with no resolvable
// reference, a closed blocker or a failed read records nothing: the block
// stays human-lifted, as before.

// verdictBlockerRecordTimeout bounds the detached GitHub work: one blocker
// read, one blocker-list or comment read, at most one write.
const verdictBlockerRecordTimeout = 30 * time.Second

// recordVerdictBlocker records the blocker a blocked verdict's reason names
// on the task issue. Runs detached from the read loop; callers `go` it. A
// re-delivered verdict naming the same blocker is written at most once per
// hub; GitHub-side checks cover restarts.
func (h *ContributeWSHub) recordVerdictBlocker(task *WSTaskAssign, reason string) {
	if h == nil || task == nil || task.Kind != "issue" || task.Repo == "" || task.Number <= 0 || task.ExternalID != "" {
		return
	}
	blocker, ok := ghpkg.ParseVerdictBlocker(reason, task.Repo, task.Number)
	if !ok {
		return
	}
	key := task.identityKey() + "->" + blocker.String()
	if _, seen := h.verdictBlockersRecorded.LoadOrStore(key, struct{}{}); seen {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), verdictBlockerRecordTimeout)
	defer cancel()
	outcome, err := h.recordVerdictBlockerOnForge(ctx, task.Repo, task.Number, blocker)
	if err != nil {
		// Forget the key so a later re-delivery may try again.
		h.verdictBlockersRecorded.Delete(key)
		h.logger.Warn("[contribute-ws] blocked verdict: could not record the named blocker",
			"repo", task.Repo, "number", task.Number, "blocker", blocker.String(), "error", err)
		return
	}
	h.logger.Info("[contribute-ws] blocked verdict: recorded the named blocker",
		"repo", task.Repo, "number", task.Number, "blocker", blocker.String(), "outcome", string(outcome))
}

func (h *ContributeWSHub) recordVerdictBlockerOnForge(ctx context.Context, repo string, number int, blocker ghpkg.VerdictBlocker) (ghpkg.VerdictBlockerOutcome, error) {
	if h.verdictBlockerRecorder != nil {
		return h.verdictBlockerRecorder(ctx, repo, number, blocker)
	}
	if h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return "", ghpkg.ErrNoGitHubClient
	}
	return h.server.deps.GHClient.RecordVerdictBlocker(ctx, repo, number, blocker)
}
