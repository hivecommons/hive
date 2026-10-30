package dashboard

import (
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The activity monitor only ever lists open work, so a PR that closed
// without merging simply drops out of Items; the SPA could not tell that
// from a merge whose audit entry had not landed yet, and said nothing
// (hivecommons/hive#15808: #15793, superseded by #15783, "vanished"). Every
// closed-unmerged PR snapshot that reaches applyWebhookItemSnapshots — a
// webhook's embedded state or confirming read, the closed sweep, a
// hive-mediated close (ApplyPRTerminalAudit) — is remembered here for
// stageTrackerMergedWindow and served as recently_closed, the closed twin
// of recently_merged.

// stageTrackerRecentlyClosedCap bounds the in-memory list.
const stageTrackerRecentlyClosedCap = 200

// noteClosedSnapshots remembers every closed, unmerged PR among snaps.
func (s *Server) noteClosedSnapshots(snaps []github.WebhookItemSnapshot) {
	if s == nil || len(snaps) == 0 {
		return
	}
	now := time.Now()
	qualify := s.mergedRepoQualifier()
	s.recentlyClosedMu.Lock()
	defer s.recentlyClosedMu.Unlock()
	for _, snap := range snaps {
		if !snap.IsPR || snap.Open() || snap.Merged || snap.Number <= 0 || snap.Repo == "" {
			continue
		}
		repo := qualify(snap.Repo)
		at := snap.UpdatedAt
		if at.IsZero() || at.After(now) {
			at = now
		}
		dup := false
		for i := range s.recentlyClosed {
			if s.recentlyClosed[i].Number == snap.Number && strings.EqualFold(s.recentlyClosed[i].Repo, repo) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		s.recentlyClosed = append(s.recentlyClosed, StageTrackerClosed{Repo: repo, Number: snap.Number, ClosedAt: at.UTC()})
	}
	s.pruneRecentlyClosedLocked(now)
}

// pruneRecentlyClosedLocked drops entries older than the window and, past
// the cap, the oldest. Caller holds recentlyClosedMu.
func (s *Server) pruneRecentlyClosedLocked(now time.Time) {
	cut := now.Add(-stageTrackerMergedWindow)
	kept := s.recentlyClosed[:0]
	for _, e := range s.recentlyClosed {
		if !e.ClosedAt.Before(cut) {
			kept = append(kept, e)
		}
	}
	if len(kept) > stageTrackerRecentlyClosedCap {
		kept = kept[len(kept)-stageTrackerRecentlyClosedCap:]
	}
	s.recentlyClosed = kept
}

// stageTrackerRecentlyClosed is the list as the payload carries it: newest
// first, within stageTrackerMergedWindow of now.
func (s *Server) stageTrackerRecentlyClosed(now time.Time) []StageTrackerClosed {
	if s == nil {
		return nil
	}
	s.recentlyClosedMu.Lock()
	defer s.recentlyClosedMu.Unlock()
	s.pruneRecentlyClosedLocked(now)
	if len(s.recentlyClosed) == 0 {
		return nil
	}
	out := append([]StageTrackerClosed(nil), s.recentlyClosed...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ClosedAt.After(out[j].ClosedAt) })
	return out
}

// stageTrackerMergedHold is dashboard.stage_tracker.merged_hold_s as the
// server reads it (the config default when no config is wired).
func (s *Server) stageTrackerMergedHold() time.Duration {
	if s != nil && s.deps != nil && s.deps.Config != nil {
		return s.deps.Config.StageTrackerMergedHold()
	}
	return (*config.Config)(nil).StageTrackerMergedHold()
}
