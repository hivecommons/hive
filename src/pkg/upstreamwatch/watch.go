package upstreamwatch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// DismissedLabel marks a fork issue whose upstream change should never be
// filed again, alongside closing it as "not planned".
const DismissedLabel = "upstream/dismissed"

// Existing is a fork issue the marker search found for an upstream item.
type Existing struct {
	// Number is the fork issue number.
	Number int
	// Dismissed is true when the issue was closed as "not planned" or
	// carries DismissedLabel.
	Dismissed bool
}

// IssueOutcome is what a reconciliation pass learns about a previously filed
// fork issue's current state.
type IssueOutcome struct {
	// Open is true while the issue is still open: there is nothing to
	// reconcile yet, the ref stays surfaced.
	Open bool
	// Ported is true when the issue was closed as completed.
	Ported bool
	// Dismissed is true when the issue was closed as "not planned" or
	// carries DismissedLabel.
	Dismissed bool
}

// Filer is the write side of the watch against the fork: a marker search
// (the second dedupe guard), issue creation, and the issue lookup a
// reconciliation pass uses to learn whether a previously filed issue has
// since been closed. It only ever opens issues, never PRs. A GitHub-backed
// implementation is in github_filer.go; tests substitute a fake.
type Filer interface {
	// FindMarker returns the fork issue whose body carries marker, if any.
	FindMarker(ctx context.Context, marker string) (Existing, bool, error)
	// File opens issue on the fork and returns its number.
	File(ctx context.Context, issue Issue) (int, error)
	// GetIssue returns the current state of the fork issue numbered number.
	// found is false when the issue no longer exists.
	GetIssue(ctx context.Context, number int) (IssueOutcome, bool, error)
}

// Options configures one repo's Watch.
type Options struct {
	// Repo is the upstream_watch.repos key the state is stored under.
	Repo string
	// Upstream is the resolved owner/repo the fork follows; it qualifies the
	// hidden marker and is recorded in the state.
	Upstream string
	// Label is applied to every filed issue.
	Label string
	// MaxIssuesPerRun caps issues filed per run. Zero or less means no cap.
	MaxIssuesPerRun int
	// StartFrom, when set, is where a first pass starts instead of the
	// fork point (Source.ForkPoint). It is ignored once the repo has
	// history: any saved ref or a non-zero watermark.
	StartFrom time.Time
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Result reports what one run did, by upstream ref.
type Result struct {
	// Filed are refs a new fork issue was opened for.
	Filed []string
	// Skipped are refs judged not applicable to the fork.
	Skipped []string
	// Deduped are refs already recorded in state or found on the fork by
	// their marker.
	Deduped []string
	// Dismissed are refs passed over because they were dismissed, including
	// ones a reconciliation pass found closed as "not planned" or labelled
	// DismissedLabel after having been filed in an earlier run.
	Dismissed []string
	// Ported are previously filed refs a reconciliation pass found closed
	// as completed in this run.
	Ported []string
	// Capped is true when MaxIssuesPerRun stopped the run with items left;
	// Remaining counts them. They stay behind the watermark for next run.
	Capped    bool
	Remaining int
	// FirstPass is true when the repo had no saved refs and a zero
	// watermark, so the run started at StartFrom or the fork point.
	FirstPass bool
	// Start is where a first pass started; zero when FirstPass is false or
	// the start point could not be found.
	Start time.Time
}

// Watch is the upstream watch loop for one fork repo: list upstream items
// since the watermark, classify them, drop the ones that no longer apply,
// file an issue for the rest and advance the watermark.
type Watch struct {
	opts     Options
	source   Source
	contents ForkContents
	store    Store
	filer    Filer
}

// New returns a Watch.
func New(opts Options, source Source, contents ForkContents, store Store, filer Filer) *Watch {
	return &Watch{opts: opts, source: source, contents: contents, store: store, filer: filer}
}

// Run performs one pass: it lists and files new upstream items, then
// reconciles every previously filed ref against its fork issue (ported once
// the issue is completed, dismissed once it is closed as "not planned" or
// labelled DismissedLabel). The state is saved even when either half stops
// early, so every item handled before an error stays handled; the watermark
// never moves past an item that was not filed, skipped or deduped.
//
// A first pass (no saved refs, zero watermark) starts at Options.StartFrom,
// or else at the fork point; when the fork point cannot be found the pass
// files nothing and leaves the watermark zero so the next pass retries. A
// listing the source reports as truncated (*TruncatedError) is never handled:
// the pass files nothing and the watermark stays where it was.
func (w *Watch) Run(ctx context.Context) (Result, error) {
	var res Result
	state, err := w.store.Load()
	if err != nil {
		return res, fmt.Errorf("load upstream watch state: %w", err)
	}
	rs := state.Repo(w.opts.Repo)
	if w.opts.Upstream != "" {
		rs.Upstream = w.opts.Upstream
	}
	rs.LastRunAt = w.opts.now()

	justFiled := make(map[string]bool)
	runErr := w.process(ctx, rs, &res, justFiled)
	if ctx.Err() == nil {
		if err := w.reconcile(ctx, rs, &res, justFiled); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("reconcile filed issues: %w", err))
		}
	}
	if err := w.store.Save(state); err != nil {
		return res, errors.Join(runErr, fmt.Errorf("save upstream watch state: %w", err))
	}
	return res, runErr
}

func (w *Watch) process(ctx context.Context, rs *RepoState, res *Result, justFiled map[string]bool) error {
	since := rs.Watermark
	if rs.firstPass() {
		res.FirstPass = true
		start, err := w.startPoint(ctx)
		if err != nil {
			return err
		}
		res.Start = start
		since = start
	}
	items, err := w.source.List(ctx, since)
	if err != nil {
		return fmt.Errorf("list upstream items: %w", err)
	}
	// Written directly, and only once the listing is complete: a failed or
	// truncated first pass keeps a zero watermark, so the next pass is a
	// first pass again and honours a start_from set in the meantime.
	// AdvanceWatermark is not used because it only moves forward.
	if res.FirstPass {
		rs.Watermark = since
	}
	filed := 0
	for i, item := range items {
		if err := ctx.Err(); err != nil {
			holdBehind(rs, item)
			return err
		}
		if rs.Dismissed(item.Ref) {
			res.Dismissed = append(res.Dismissed, item.Ref)
			rs.AdvanceWatermark(item.Timestamp)
			continue
		}
		if rs.Seen(item.Ref) {
			res.Deduped = append(res.Deduped, item.Ref)
			rs.AdvanceWatermark(item.Timestamp)
			continue
		}
		if w.opts.MaxIssuesPerRun > 0 && filed >= w.opts.MaxIssuesPerRun {
			res.Capped = true
			res.Remaining = len(items) - i
			holdBehind(rs, item)
			return nil
		}
		if err := w.handle(ctx, rs, res, item, &filed, justFiled); err != nil {
			holdBehind(rs, item)
			return fmt.Errorf("%s: %w", item.Ref, err)
		}
	}
	return nil
}

// handle judges one unseen item and files, skips or dedupes it.
func (w *Watch) handle(ctx context.Context, rs *RepoState, res *Result, item Item, filed *int, justFiled map[string]bool) error {
	now := w.opts.now()
	j, err := Judge(ctx, w.contents, item)
	if err != nil {
		return fmt.Errorf("judge: %w", err)
	}
	if !j.Applicable {
		rs.Put(Outcome{Ref: item.Ref, Status: StatusSkipped, ItemTime: item.Timestamp, Reason: j.Reason}, now)
		res.Skipped = append(res.Skipped, item.Ref)
		return nil
	}
	existing, found, err := w.filer.FindMarker(ctx, Marker(w.opts.Upstream, item))
	if err != nil {
		return fmt.Errorf("search fork for marker: %w", err)
	}
	if found {
		if existing.Dismissed {
			rs.Put(Outcome{Ref: item.Ref, Status: StatusDismissed, IssueNumber: existing.Number,
				Reason: fmt.Sprintf("fork issue #%d was dismissed", existing.Number)}, now)
			rs.AdvanceWatermark(item.Timestamp)
			res.Dismissed = append(res.Dismissed, item.Ref)
			return nil
		}
		rs.Put(Outcome{Ref: item.Ref, Status: StatusFiled, ItemTime: item.Timestamp, IssueNumber: existing.Number}, now)
		res.Deduped = append(res.Deduped, item.Ref)
		return nil
	}
	number, err := w.filer.File(ctx, RenderIssue(w.opts.Upstream, item, j, w.opts.Label))
	if err != nil {
		return fmt.Errorf("file issue: %w", err)
	}
	rs.Put(Outcome{Ref: item.Ref, Status: StatusFiled, ItemTime: item.Timestamp, IssueNumber: number}, now)
	res.Filed = append(res.Filed, item.Ref)
	justFiled[item.Ref] = true
	*filed++
	return nil
}

// reconcile checks every previously filed ref against its fork issue and
// updates the ones the fork has since closed: completed becomes ported,
// not-planned or DismissedLabel becomes dismissed. It skips refs filed
// earlier in this very call to process, since a freshly opened issue is
// certainly still open, and stops at the first error so the rest are
// retried next run; the watermark is left alone, since it already moved
// past these refs when they were first filed.
func (w *Watch) reconcile(ctx context.Context, rs *RepoState, res *Result, justFiled map[string]bool) error {
	refs := make([]string, 0, len(rs.Refs))
	for ref, rec := range rs.Refs {
		if rec == nil || rec.Status != StatusFiled || rec.IssueNumber == 0 || justFiled[ref] {
			continue
		}
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	now := w.opts.now()
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec, ok := rs.Record(ref)
		if !ok {
			continue
		}
		outcome, found, err := w.filer.GetIssue(ctx, rec.IssueNumber)
		if err != nil {
			return fmt.Errorf("%s (issue #%d): %w", ref, rec.IssueNumber, err)
		}
		if !found || outcome.Open {
			continue
		}
		switch {
		case outcome.Dismissed:
			rs.Put(Outcome{Ref: ref, Status: StatusDismissed, IssueNumber: rec.IssueNumber,
				Reason: fmt.Sprintf("fork issue #%d was dismissed", rec.IssueNumber)}, now)
			res.Dismissed = append(res.Dismissed, ref)
		case outcome.Ported:
			rs.Put(Outcome{Ref: ref, Status: StatusPorted, IssueNumber: rec.IssueNumber}, now)
			res.Ported = append(res.Ported, ref)
		}
	}
	return nil
}

// startPoint returns where a first pass starts: Options.StartFrom when set,
// else the fork's last commit in common with its upstream. It never returns
// a zero time without an error, so a first pass never scans from zero.
func (w *Watch) startPoint(ctx context.Context) (time.Time, error) {
	if !w.opts.StartFrom.IsZero() {
		return w.opts.StartFrom, nil
	}
	at, err := w.source.ForkPoint(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("find fork point: %w", err)
	}
	if at.IsZero() {
		return time.Time{}, errors.New("find fork point: no commit date for the merge base")
	}
	return at, nil
}

// holdBehind keeps the watermark strictly before an unhandled item. The
// source lists items strictly after the watermark, so an unhandled item that
// shares a timestamp with the last handled one would otherwise never be
// listed again; stepping back is safe because the handled ones are deduped.
func holdBehind(rs *RepoState, item Item) {
	if !item.Timestamp.IsZero() && !item.Timestamp.After(rs.Watermark) {
		rs.Watermark = item.Timestamp.Add(-time.Nanosecond)
	}
}
