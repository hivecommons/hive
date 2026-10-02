package upstreamwatch

import (
	"context"
	"errors"
	"fmt"
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

// Filer is the write side of the watch against the fork: a marker search
// (the second dedupe guard) and issue creation. It only ever opens issues,
// never PRs. A GitHub-backed implementation is in github_filer.go; tests
// substitute a fake.
type Filer interface {
	// FindMarker returns the fork issue whose body carries marker, if any.
	FindMarker(ctx context.Context, marker string) (Existing, bool, error)
	// File opens issue on the fork and returns its number.
	File(ctx context.Context, issue Issue) (int, error)
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
	// Dismissed are refs passed over because they were dismissed.
	Dismissed []string
	// Capped is true when MaxIssuesPerRun stopped the run with items left;
	// Remaining counts them. They stay behind the watermark for next run.
	Capped    bool
	Remaining int
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

// Run performs one pass. The state is saved even when the pass stops early,
// so every item handled before an error stays handled; the watermark never
// moves past an item that was not filed, skipped or deduped.
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

	runErr := w.process(ctx, rs, &res)
	if err := w.store.Save(state); err != nil {
		return res, errors.Join(runErr, fmt.Errorf("save upstream watch state: %w", err))
	}
	return res, runErr
}

func (w *Watch) process(ctx context.Context, rs *RepoState, res *Result) error {
	items, err := w.source.List(ctx, rs.Watermark)
	if err != nil {
		return fmt.Errorf("list upstream items: %w", err)
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
		if err := w.handle(ctx, rs, res, item, &filed); err != nil {
			holdBehind(rs, item)
			return fmt.Errorf("%s: %w", item.Ref, err)
		}
	}
	return nil
}

// handle judges one unseen item and files, skips or dedupes it.
func (w *Watch) handle(ctx context.Context, rs *RepoState, res *Result, item Item, filed *int) error {
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
	*filed++
	return nil
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
