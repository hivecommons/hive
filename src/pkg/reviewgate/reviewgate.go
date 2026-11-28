// Package reviewgate is the one check every hive path that approves, labels
// `lgtm` / `approved`, queues or merges a pull request runs first
// (hivecommons/hive#20067): a pull request with an open change request from a
// reviewer who has write access or above is not approved, labelled, queued
// or merged by the hive, whatever the ACMM level and whether CI is green.
//
// "Open" follows the forge's own review rules:
//
//   - Only a reviewer's latest opinionated review counts. APPROVED,
//     CHANGES_REQUESTED and DISMISSED are opinions; COMMENTED and PENDING
//     are not, so a later comment never clears a change request.
//   - A change request stays open across new pushes: GitHub and Forgejo
//     never mark a change request stale on their own, so a review left on
//     an older head still blocks.
//   - A dismissed review (state DISMISSED on GitHub, dismissed=true on
//     Forgejo / Gitea) is no longer an opinion, so it releases the block.
//   - A later approval from the same reviewer releases it.
//   - Only reviewers with write access or above count, the same people whose
//     reviews the forge counts toward branch protection. A permission lookup
//     that fails counts as write access: the gate fails closed.
//
// A human operator's explicit dashboard decision may override an open change
// request, but only with a reason that is recorded (RecordOverride) and
// audited by every gated path that honours it.
package reviewgate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Review states the gate reads, normalised to GitHub's spelling.
const (
	StateApproved         = "APPROVED"
	StateChangesRequested = "CHANGES_REQUESTED"
	StateDismissed        = "DISMISSED"
	StateCommented        = "COMMENTED"
	StatePending          = "PENDING"
)

// Audit outcomes a gated attempt records.
const (
	// OutcomeBlocked is an attempt refused because of an open change request.
	OutcomeBlocked = "blocked"
	// OutcomeOverridden is an attempt let through by an operator override.
	OutcomeOverridden = "overridden"
	// OutcomeUnreadable is an attempt refused because the reviews could not
	// be read: the gate fails closed.
	OutcomeUnreadable = "unreadable"
)

// Review is one submitted review, forge-neutral.
type Review struct {
	ID          int64
	Reviewer    string
	State       string
	CommitID    string
	SubmittedAt time.Time
	// Dismissed is Forgejo / Gitea's dismissed flag; GitHub reports a
	// dismissal in State instead. Either releases the review.
	Dismissed bool
	URL       string
}

// Block is one open change request that stops the hive.
type Block struct {
	Reviewer string
	ReviewID int64
	CommitID string
	URL      string
	// PermissionUnverified is set when the reviewer's access could not be
	// read and the request counts as a writer's (fail closed).
	PermissionUnverified bool
}

// String renders the block for logs, errors and the dashboard.
func (b Block) String() string {
	s := fmt.Sprintf("%s (review %d)", b.Reviewer, b.ReviewID)
	if b.PermissionUnverified {
		s += " [access unverified]"
	}
	return s
}

// WriterFunc reports whether login has write access or above on the repo.
type WriterFunc func(ctx context.Context, login string) (bool, error)

// NormalizeState maps a forge review state onto the constants above.
// Forgejo / Gitea spell a change request REQUEST_CHANGES and a comment
// COMMENT.
func NormalizeState(state string, dismissed bool) string {
	if dismissed {
		return StateDismissed
	}
	switch s := strings.ToUpper(strings.TrimSpace(state)); s {
	case "REQUEST_CHANGES", "CHANGES_REQUESTED":
		return StateChangesRequested
	case "APPROVE", "APPROVED":
		return StateApproved
	case "DISMISSED":
		return StateDismissed
	case "COMMENT", "COMMENTED":
		return StateCommented
	default:
		return s
	}
}

func isOpinion(state string) bool {
	switch state {
	case StateApproved, StateChangesRequested, StateDismissed:
		return true
	}
	return false
}

// Standing returns each reviewer's latest opinionated review, keyed by the
// lower-cased login. Reviews are ordered by submission time, then by ID, so
// the answer does not depend on the order the forge listed them in.
func Standing(reviews []Review) map[string]Review {
	ordered := make([]Review, 0, len(reviews))
	for _, rv := range reviews {
		rv.State = NormalizeState(rv.State, rv.Dismissed)
		if strings.TrimSpace(rv.Reviewer) == "" || !isOpinion(rv.State) {
			continue
		}
		ordered = append(ordered, rv)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].SubmittedAt.Equal(ordered[j].SubmittedAt) {
			return ordered[i].SubmittedAt.Before(ordered[j].SubmittedAt)
		}
		return ordered[i].ID < ordered[j].ID
	})
	out := make(map[string]Review, len(ordered))
	for _, rv := range ordered {
		out[strings.ToLower(strings.TrimSpace(rv.Reviewer))] = rv
	}
	return out
}

// Evaluate returns the open change requests in reviews: each reviewer whose
// latest opinion is CHANGES_REQUESTED and who has write access or above.
// isWriter is asked only about those reviewers; a nil isWriter, or a lookup
// error, counts the reviewer as a writer (fail closed). Blocks are sorted by
// reviewer.
func Evaluate(ctx context.Context, reviews []Review, isWriter WriterFunc) []Block {
	var out []Block
	for _, rv := range Standing(reviews) {
		if rv.State != StateChangesRequested {
			continue
		}
		block := Block{Reviewer: strings.TrimSpace(rv.Reviewer), ReviewID: rv.ID, CommitID: strings.TrimSpace(rv.CommitID), URL: rv.URL}
		if isWriter != nil {
			writer, err := isWriter(ctx, block.Reviewer)
			if err != nil {
				block.PermissionUnverified = true
			} else if !writer {
				continue
			}
		} else {
			block.PermissionUnverified = true
		}
		out = append(out, block)
	}
	sort.Slice(out, func(i, j int) bool {
		if !strings.EqualFold(out[i].Reviewer, out[j].Reviewer) {
			return strings.ToLower(out[i].Reviewer) < strings.ToLower(out[j].Reviewer)
		}
		return out[i].ReviewID < out[j].ReviewID
	})
	return out
}

// ErrChangesRequested is matched (errors.Is) by every refusal the gate
// returns, so callers can map it onto their own skip reason.
var ErrChangesRequested = errors.New("open change request")

// ErrUnreadable is matched by a refusal caused by a failed review read.
var ErrUnreadable = errors.New("could not read reviews")

// BlockedError is the gate's refusal. It names every open change request.
type BlockedError struct {
	Repo   string
	Number int
	Blocks []Block
	// Cause is set when the reviews could not be read (fail closed).
	Cause error
}

func (e *BlockedError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s#%d: refusing to approve, label or merge: %v: %v", e.Repo, e.Number, ErrUnreadable, e.Cause)
	}
	names := make([]string, 0, len(e.Blocks))
	for _, b := range e.Blocks {
		names = append(names, b.String())
	}
	return fmt.Sprintf("%s#%d: refusing to approve, label or merge: %s from %s; it lifts when that reviewer approves or the review is dismissed", e.Repo, e.Number, ErrChangesRequested, strings.Join(names, ", "))
}

// Is lets errors.Is match ErrChangesRequested (and ErrUnreadable when the
// refusal came from a failed read).
func (e *BlockedError) Is(target error) bool {
	if target == ErrChangesRequested {
		return true
	}
	return target == ErrUnreadable && e.Cause != nil
}

func (e *BlockedError) Unwrap() error { return e.Cause }

// Describe renders blocks as "alice (review 12), bob (review 34)".
func Describe(blocks []Block) string {
	names := make([]string, 0, len(blocks))
	for _, b := range blocks {
		names = append(names, b.String())
	}
	return strings.Join(names, ", ")
}

// ReviewIDs lists the blocks' review IDs, comma separated, for audit detail.
func ReviewIDs(blocks []Block) string {
	ids := make([]string, 0, len(blocks))
	for _, b := range blocks {
		ids = append(ids, fmt.Sprintf("%d", b.ReviewID))
	}
	return strings.Join(ids, ",")
}

// Reviewers lists the blocks' reviewers, comma separated, for audit detail.
func Reviewers(blocks []Block) string {
	names := make([]string, 0, len(blocks))
	for _, b := range blocks {
		names = append(names, b.Reviewer)
	}
	return strings.Join(names, ",")
}

// Override is an operator's audited decision to proceed despite the open
// change requests it names. It covers exactly those review IDs: a new change
// request, or a new review from the same reviewer, is not covered.
type Override struct {
	Repo      string
	Number    int
	ReviewIDs []int64
	Actor     string
	Reason    string
	At        time.Time
}

// OverrideTTL bounds how long an override stands. The operator decided on
// the review state they saw; a merge that has not happened within this
// window asks again.
const OverrideTTL = 24 * time.Hour

var (
	overrideMu sync.Mutex
	overrides  = map[string]Override{}
	nowFn      = time.Now
)

func overrideKey(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + fmt.Sprintf("%d", number)
}

// ErrOverrideIncomplete is returned by RecordOverride without an actor, a
// reason or the change requests it covers.
var ErrOverrideIncomplete = errors.New("an override needs the operator, a reason and the change requests it covers")

// RecordOverride records an operator's override for repo#number. It refuses
// an override without an actor, a reason or at least one blocking review.
func RecordOverride(repo string, number int, actor, reason string, blocks []Block) (Override, error) {
	actor, reason = strings.TrimSpace(actor), strings.TrimSpace(reason)
	if actor == "" || reason == "" || len(blocks) == 0 || strings.TrimSpace(repo) == "" || number <= 0 {
		return Override{}, ErrOverrideIncomplete
	}
	ov := Override{Repo: strings.TrimSpace(repo), Number: number, Actor: actor, Reason: reason, At: nowFn()}
	for _, b := range blocks {
		ov.ReviewIDs = append(ov.ReviewIDs, b.ReviewID)
	}
	overrideMu.Lock()
	overrides[overrideKey(repo, number)] = ov
	overrideMu.Unlock()
	return ov, nil
}

// OverrideFor returns the standing override that covers every block, if any.
func OverrideFor(repo string, number int, blocks []Block) (Override, bool) {
	if len(blocks) == 0 {
		return Override{}, false
	}
	overrideMu.Lock()
	defer overrideMu.Unlock()
	key := overrideKey(repo, number)
	ov, ok := overrides[key]
	if !ok {
		return Override{}, false
	}
	if nowFn().Sub(ov.At) > OverrideTTL {
		delete(overrides, key)
		return Override{}, false
	}
	covered := make(map[int64]bool, len(ov.ReviewIDs))
	for _, id := range ov.ReviewIDs {
		covered[id] = true
	}
	for _, b := range blocks {
		if !covered[b.ReviewID] {
			return Override{}, false
		}
	}
	return ov, true
}

// ClearOverride drops any override for repo#number.
func ClearOverride(repo string, number int) {
	overrideMu.Lock()
	delete(overrides, overrideKey(repo, number))
	overrideMu.Unlock()
}

// Decision is what a gated path does with the blocks it read.
type Decision struct {
	Blocks   []Block
	Override Override
	// Overridden is set when an operator override covers every block.
	Overridden bool
}

// Allowed reports whether the path may proceed.
func (d Decision) Allowed() bool { return len(d.Blocks) == 0 || d.Overridden }

// Decide applies any standing override to blocks.
func Decide(repo string, number int, blocks []Block) Decision {
	d := Decision{Blocks: blocks}
	if len(blocks) == 0 {
		return d
	}
	if ov, ok := OverrideFor(repo, number, blocks); ok {
		d.Override, d.Overridden = ov, true
	}
	return d
}
