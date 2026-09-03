package advisory

import (
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// staleCloseReason is stamped into a bead's metadata when staleness pruning
// retires it. Named so logs, tests and the bead trail agree on the wording.
const staleCloseReason = "auto-closed: finding not re-reported within staleness window"

// PruneStaleAdvisoryBeads closes any open advisory bead whose LastSeenAt is
// older than the staleness window, returning the closed titles for logging.
//
// This is the general answer to "advisory beads never close": only a handful of
// findings (app-auth access, PR-linked fixes) can be actively proven healed, but
// EVERY finding is re-reported by its agent for as long as the condition holds.
// So absence of a re-report within the window is the evidence a finding is gone,
// and the digest stops carrying it.
//
// Beads with a nil LastSeenAt are never pruned: they were filed before Upsert
// existed, so "not re-reported" cannot be distinguished from "never stamped",
// and closing them would silently retire findings that may well still hold.
func PruneStaleAdvisoryBeads(stores map[string]*beads.Store, window time.Duration) []string {
	if window <= 0 {
		return nil
	}
	var closed []string
	for _, store := range stores {
		if store == nil {
			continue
		}
		for _, b := range store.List(beads.ListFilter{}) {
			if b.Status != beads.StatusOpen && b.Status != beads.StatusInProgress {
				continue
			}
			if !isAdvisoryBeadType(b.Type) {
				continue
			}
			if b.LastSeenAt == nil {
				continue
			}
			if time.Since(b.LastSeenAt.Time) <= window {
				continue
			}
			title := b.Title
			if err := store.Update(b.ID, func(bd *beads.Bead) {
				bd.Status = beads.StatusClosed
			}); err != nil {
				continue
			}
			_ = store.SetMetadata(b.ID, closeReasonMetadataKey, staleCloseReason)
			closed = append(closed, title)
		}
	}
	return closed
}
