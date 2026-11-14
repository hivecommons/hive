package connector

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"
)

// Progress phases. A sync moves idle → listing → fetching/writing → done or
// failed; the dashboard shows a progress bar while the phase is active.
const (
	ProgressIdle     = "idle"
	ProgressListing  = "listing"
	ProgressFetching = "fetching"
	ProgressWriting  = "writing"
	ProgressDone     = "done"
	ProgressFailed   = "failed"
)

// progressCurrentMaxRunes caps the current-item title carried in Progress so
// the status JSON stays small and a long upstream title cannot flood the card.
const progressCurrentMaxRunes = 80

// Progress is the live state of a connector's running sync, or of the last
// sync when none is running. It is updated as the sync goes (the Syncer per
// emitted page, connectors such as Plaud after listing and per item) and is
// never persisted: a restart starts from ProgressIdle.
type Progress struct {
	// Phase is one of the Progress* constants.
	Phase string `json:"phase"`
	// ItemsTotal is the number of items this sync will process, when known
	// (Plaud: recordings listed). Zero means unknown.
	ItemsTotal int `json:"items_total,omitempty"`
	// ItemsDone counts items processed so far: pages written plus items
	// the connector skipped.
	ItemsDone int `json:"items_done"`
	// Current is the title of the item being fetched or written, truncated.
	// It never carries credentials: connectors pass page titles only.
	Current string `json:"current,omitempty"`
	// StartedAt is when this sync began; FinishedAt when it ended.
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	// LastError is the error that ended a failed sync, or the partial-sync
	// error of a done one.
	LastError string `json:"last_error,omitempty"`
	// BytesWritten and FactsWritten count page markdown bytes and pages
	// written to the vault so far.
	BytesWritten int64 `json:"bytes_written"`
	FactsWritten int   `json:"facts_written"`
}

// Active reports whether a sync is in progress according to the phase.
func (p Progress) Active() bool {
	switch p.Phase {
	case ProgressListing, ProgressFetching, ProgressWriting:
		return true
	}
	return false
}

// progressReporter applies fn to the running sync's Progress under the
// syncer's lock. It is carried in the Sync context so a connector reports
// progress without a new interface or goroutine.
type progressReporter func(fn func(*Progress))

type progressReporterKey struct{}

// withProgressReporter attaches r to ctx for reportProgress.
func withProgressReporter(ctx context.Context, r progressReporter) context.Context {
	return context.WithValue(ctx, progressReporterKey{}, r)
}

// reportProgress applies fn to the current sync's Progress when ctx carries a
// reporter; without one (a connector run outside the Syncer) it is a no-op.
func reportProgress(ctx context.Context, fn func(*Progress)) {
	if r, ok := ctx.Value(progressReporterKey{}).(progressReporter); ok && r != nil {
		r(fn)
	}
}

// progressTitle trims and truncates an item title for Progress.Current.
func progressTitle(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= progressCurrentMaxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:progressCurrentMaxRunes]) + "…"
}
