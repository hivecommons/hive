package advisory

import (
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// staleUnverifiedReason is stamped into a bead's metadata when the staleness
// window passes without a fresh report. That absence is not proof the finding
// healed, so the digest must keep the bead open and caption it as unverified
// instead of moving it to Recently Resolved.
const staleUnverifiedReason = "not re-reported within staleness window"

const staleUnverifiedMetadataKey = "stale_unverified"

// MarkStaleAdvisoryBeads marks any open advisory bead whose LastSeenAt is older
// than the staleness window, returning the marked titles for logging.
//
// Silence from an advisory agent is not positive evidence that a finding is
// gone: runs can be partial, truncated, non-deterministic, or absent. Keep the
// finding open unless another path has proof of resolution (for example a
// referenced GitHub issue actually closed).
//
// Beads with a nil LastSeenAt are never marked: they were filed before Upsert
// existed, so "not re-reported" cannot be distinguished from "never stamped",
// and marking them would warn on findings that may never have had a clock.
func MarkStaleAdvisoryBeads(stores map[string]*beads.Store, window time.Duration) []string {
	if window <= 0 {
		return nil
	}
	var marked []string
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
			if b.Meta(staleUnverifiedMetadataKey) != "" {
				continue
			}
			title := b.Title
			if err := store.SetMetadata(b.ID, staleUnverifiedMetadataKey, staleUnverifiedReason); err != nil {
				continue
			}
			marked = append(marked, title)
		}
	}
	return marked
}

// PruneStaleAdvisoryBeads is kept for older callers. It no longer closes beads:
// absence of a report is only an unverified/stale signal, never a resolution.
func PruneStaleAdvisoryBeads(stores map[string]*beads.Store, window time.Duration) []string {
	return MarkStaleAdvisoryBeads(stores, window)
}
