package automerge

import (
	"context"
	"strconv"
	"strings"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// The hive's own review threads at a person's merge.
//
// unresolvedThreadsRefusal resolves the hive's own non-blocking threads
// only after GitHub refused a merge the hive attempted. A head the stage
// pass hands to a person at 090-ready-for-human-merge is never attempted
// (a human-merge path, a workflow fence),
// so on a base that requires conversation resolution the person's merge was
// refused for the hive's own low-severity notes until they replied to and
// resolved each one by hand (projectbluefin/actions#779 and #783).
//
// The stage pass now asks the client to resolve them once per head when it
// writes 090 (hgithub.Client.ResolveOwnReviewThreadsForHumanMerge): only on
// a base it reads as requiring conversation resolution, only when every
// unresolved thread is the hive's own and marked non-blocking, with the
// rules of the merge-refusal path. It runs from ApplyStageLabels, the pass
// that writes the 090 label and entry comment for a trust-blocked head and
// for a human-merge path alike. A held 090 (sentinel alert, hold label)
// waits for the gate to lift.
//
// The resolve and the replies are unsolicited writes on the PR, so the
// write-back gate decides them first, under the kind the 090 / 085 entry
// comment is gated under (writeback.KindNeedsDecisionComment, a comment
// kind: a labels-only or issues-only repository refuses it). A refusal
// costs no GitHub call. In quiet notification mode (the mode in which a
// plain 090 entry folds into the status card and emails nobody) the
// threads are resolved without replies. The 090 entry comment is left as
// it is: a thread that still needs a person is logged, not added to it.

// readyOwnReviewThreadResolver is implemented by the GitHub client; a
// transport without it resolves nothing at 090. reply asks for one reply
// in each thread before it is resolved.
type readyOwnReviewThreadResolver interface {
	ResolveOwnReviewThreadsForHumanMerge(ctx context.Context, repo string, number int, base string, reply bool) (hgithub.OwnReviewThreadsOutcome, error)
}

func readyOwnThreadsKey(repo string, number int, head string) string {
	return "ready-threads|" + strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number) + "@" + strings.TrimSpace(head)
}

// readyOwnThreadsRetry reports whether an outcome leaves the head to be
// asked again on a later pass: nothing about its threads was read (a paused
// repository, a rule that could not be read; the client caches the rule
// read, so a retry costs no call until that cache expires), or every
// resolve call failed (nothing was resolved; a reply already posted is not
// repeated, the client skips a thread that carries it).
func readyOwnThreadsRetry(out hgithub.OwnReviewThreadsOutcome) bool {
	switch out.Skipped {
	case "paused", hgithub.OwnThreadsSkippedRuleUnknown:
		return true
	}
	return out.Failed > 0 && out.Resolved == 0
}

// resolveOwnThreadsAtReady runs the 090 resolution for obs once per head.
// route is the route the stage pass wrote.
func (c *Engine) resolveOwnThreadsAtReady(ctx context.Context, obs stageObservation, route stagelabels.Route) {
	if c == nil || route.Stage != stagelabels.ReadyForHumanMerge || len(route.Gates) > 0 || obs.Merged || obs.Err || obs.Number <= 0 {
		return
	}
	head, base := strings.TrimSpace(obs.HeadSHA), strings.TrimSpace(obs.BaseRef)
	if head == "" || base == "" {
		return
	}
	r, ok := c.transport.(readyOwnReviewThreadResolver)
	if !ok {
		return
	}
	repo := obs.Owner + "/" + obs.Repo
	key := readyOwnThreadsKey(repo, obs.Number, head)
	if !c.markReadyOwnThreads(key) {
		return
	}
	// Asked once per head and before any GitHub call: a refusal is final
	// for the head, so the gate records it once rather than on every pass.
	item := writebackItemForObservation(obs)
	app := normalizeBotLogin(c.transport.AppBotLogin())
	item.HiveAuthored = app != "" && normalizeBotLogin(obs.Author) == app
	if d := c.writebackAllowed(obs.Display, item, writeback.KindNeedsDecisionComment, ""); !d.Allowed {
		c.info("stage labels: resolving the hive's own review threads at 090 withheld by the write-back gate", "repo", obs.Display, "pr", obs.Number, "head", head, "reason", d.Reason)
		return
	}
	reply := !c.stagePolicy().QuietNotifications
	out, err := r.ResolveOwnReviewThreadsForHumanMerge(ctx, repo, obs.Number, base, reply)
	switch {
	case err != nil:
		c.warn("stage labels: could not read review threads for a person's merge", "repo", obs.Display, "pr", obs.Number, "head", head, "error", err)
	case out.Failed > 0 && out.Resolved == 0:
		c.forgetReadyOwnThreads(key)
		c.warn("stage labels: could not resolve the hive's own review threads for a person's merge; a later pass tries again", "repo", obs.Display, "pr", obs.Number, "head", head, "failed", out.Failed)
	case readyOwnThreadsRetry(out):
		// Paused, or a rule the client could not read: it logged that once,
		// when it cached the answer. Nothing is logged per pass.
		c.forgetReadyOwnThreads(key)
	case out.Resolved > 0:
		c.info("stage labels: resolved the hive's own non-blocking review threads for a person's merge", "repo", obs.Display, "pr", obs.Number, "head", head,
			"resolved", out.Resolved, "failed", out.Failed, "remaining", out.Remaining(), "replied", reply)
	case out.NeedsPerson():
		c.info("stage labels: review threads at 090 still need a person", "repo", obs.Display, "pr", obs.Number, "head", head,
			"unresolved", out.Unresolved, "others", out.Others, "own_answered", out.OwnAnswered, "own_blocking", out.OwnBlocking, "skipped", out.Skipped)
	default:
		c.debug("stage labels: no review threads to resolve at 090", "repo", obs.Display, "pr", obs.Number, "head", head, "unresolved", out.Unresolved, "skipped", out.Skipped)
	}
}

// readyOwnThreadsCap bounds the per-head marks; past it the set starts
// over, which at worst asks a head once more.
const readyOwnThreadsCap = 4096

// markReadyOwnThreads records key and reports whether this is its first
// use.
func (c *Engine) markReadyOwnThreads(key string) bool {
	c.readyOwnThreadsMu.Lock()
	defer c.readyOwnThreadsMu.Unlock()
	if c.readyOwnThreads == nil {
		c.readyOwnThreads = map[string]bool{}
	}
	if c.readyOwnThreads[key] {
		return false
	}
	if len(c.readyOwnThreads) >= readyOwnThreadsCap {
		c.readyOwnThreads = map[string]bool{}
	}
	c.readyOwnThreads[key] = true
	return true
}

// forgetReadyOwnThreads drops key, so the next pass asks the head again.
func (c *Engine) forgetReadyOwnThreads(key string) {
	c.readyOwnThreadsMu.Lock()
	defer c.readyOwnThreadsMu.Unlock()
	delete(c.readyOwnThreads, key)
}
