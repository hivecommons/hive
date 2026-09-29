package releasesentinel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Retag after merge (hivecommons/hive#9585, phase 2).
//
// The issue's loop is "push the fix and move the tag together". The design
// here keeps the blast radius as small as it can be:
//
//   - The hive never pushes a branch. The agent's fix goes through the normal
//     PR path, so branch protection, required checks and review all apply
//     exactly as they do to any other change.
//   - The agent marks the PR with one body line, FixMarker(tag). Once a marked
//     PR is merged into the release branch after the round started, the
//     sentinel moves the tag to the PR's merge commit.
//   - The tag move is ONE atomic push leased on the tag's expected old commit
//     (git push --atomic --force-with-lease=refs/tags/<tag>:<old>), so a tag
//     someone else moved in the meantime is never overwritten.
//   - The new commit must be a fast-forward of the old tag commit and be
//     contained in the release branch. By default every commit between the
//     two must belong to the fix PR, so the tag only ever becomes "old release
//     + the fix"; RetagAllowIntervening relaxes that for busy release
//     branches.
//   - Anything the retagger refuses is recorded and never retried for that PR;
//     the release then falls back to the phase-1 behavior (the merged fix
//     ships as the next patch release).
//
// After a move the record goes back to awaiting_ci on the new commit, and the
// next pass evaluates that commit's CI under the same rules as any tag move.

// FixMarkerKey is the key of the PR body line that hands a merged fix back to
// the sentinel: "Release-Sentinel: v1.2.3".
const FixMarkerKey = "Release-Sentinel"

// retagSettleWindow is how long after its own tag move the sentinel treats a
// tag listing that still shows the old commit as a lagging read rather than a
// deliberate move back.
const retagSettleWindow = 10 * time.Minute

// FixMarker is the exact line a repair PR carries in its body.
func FixMarker(tag string) string {
	return FixMarkerKey + ": " + tag
}

// HasFixMarker reports whether body carries FixMarker(tag) on a line of its
// own (surrounding whitespace and case ignored).
func HasFixMarker(body, tag string) bool {
	want := FixMarker(tag)
	for _, line := range strings.Split(body, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), want) {
			return true
		}
	}
	return false
}

// FixPR is a merged repair PR.
type FixPR struct {
	Number   int
	URL      string
	MergeSHA string
	MergedAt time.Time
	// Commits are the SHAs of the PR's own commits.
	Commits []string
}

// FixSource finds merged repair PRs. It only reads.
type FixSource interface {
	// DefaultBranch is the repository's default branch, used as the release
	// branch when none is configured.
	DefaultBranch(ctx context.Context) (string, error)
	// MergedFixPR returns the most recently merged PR into branch whose body
	// carries FixMarker(tag) and that merged at or after since.
	MergedFixPR(ctx context.Context, branch, tag string, since time.Time) (FixPR, bool, error)
}

// RetagRequest asks a Retagger to move Tag from OldSHA to NewSHA.
type RetagRequest struct {
	Repo   string
	Tag    string
	Branch string
	OldSHA string
	NewSHA string
	// FixCommits are the commits allowed between OldSHA and NewSHA (the fix
	// PR's own commits; NewSHA itself is always allowed).
	FixCommits []string
	// AllowIntervening skips the FixCommits check.
	AllowIntervening bool
}

// ErrRetagRefused marks a refusal: moving the tag would be unsafe, so it was
// not attempted (or the lease was lost). A refusal is final for that fix PR;
// any other Retag error is transient and retried next pass.
var ErrRetagRefused = errors.New("release sentinel refused to move the tag")

// refused wraps ErrRetagRefused with a reason.
func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRetagRefused, fmt.Sprintf(format, args...))
}

// Retagger moves a release tag. Implementations must be compare-and-swap on
// OldSHA and must never write a branch.
type Retagger interface {
	Retag(ctx context.Context, req RetagRequest) error
}

// tryRetag moves the tag to a merged fix PR's commit when one is ready. It
// reports whether the tag moved. A refusal is recorded on rec and is not an
// error; a transient failure is.
func (s *Sentinel) tryRetag(ctx context.Context, now time.Time, rec *Record, res *Result) (bool, error) {
	branch := strings.TrimSpace(s.opts.ReleaseBranch)
	if branch == "" {
		b, err := s.fixes.DefaultBranch(ctx)
		if err != nil {
			return false, fmt.Errorf("resolve release branch: %w", err)
		}
		branch = strings.TrimSpace(b)
	}
	if branch == "" {
		return false, errors.New("resolve release branch: repository reports no default branch")
	}
	pr, ok, err := s.fixes.MergedFixPR(ctx, branch, rec.Tag, rec.RoundStartedAt)
	if err != nil {
		return false, fmt.Errorf("look up merged fix PR for %s: %w", rec.Tag, err)
	}
	if !ok || pr.MergeSHA == "" || pr.Number == rec.RetagRefusedPR {
		return false, nil
	}
	err = s.retagger.Retag(ctx, RetagRequest{
		Repo:             rec.Repo,
		Tag:              rec.Tag,
		Branch:           branch,
		OldSHA:           rec.SHA,
		NewSHA:           pr.MergeSHA,
		FixCommits:       pr.Commits,
		AllowIntervening: s.opts.RetagAllowIntervening,
	})
	if errors.Is(err, ErrRetagRefused) {
		rec.RetagRefusedPR = pr.Number
		rec.RetagNote = fmt.Sprintf("fix PR #%d: %v", pr.Number, err)
		rec.UpdatedAt = now
		res.RetagRefused = rec.RetagNote
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("move %s to fix PR #%d merge commit %s: %w", rec.Tag, pr.Number, shortSHA(pr.MergeSHA), err)
	}
	from := rec.SHA
	rec.RetagFromSHA = from
	rec.RetaggedAt = now
	rec.RetagPR = pr.Number
	rec.RetagNote = ""
	rec.SHA = pr.MergeSHA
	rec.BlockingRuns = nil
	rec.transition(now, StateAwaitingCI, fmt.Sprintf("round %d fix PR #%d merged; tag moved %s -> %s",
		rec.Round, pr.Number, shortSHA(from), shortSHA(pr.MergeSHA)))
	return true, nil
}
