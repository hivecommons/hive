package inbox

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// DefaultRFCPublishWait is how long a delegated RFC may stay in progress
// before the share is marked failed (inbox.spek wait; Service.RFCPublishWait).
const DefaultRFCPublishWait = 6 * time.Hour

// RFCFinder looks for the issue an agent published for an item: the first
// issue in repo (owner/repo) whose body contains marker (the item's
// provenance reference, which the agent prompt asks it to put in the RFC).
// It returns "" when none exists yet.
type RFCFinder func(ctx context.Context, repo, marker string) (string, error)

// SetRFCFinder installs the lookup CheckRFCPublications uses.
func (s *Service) SetRFCFinder(f RFCFinder) {
	s.mu.Lock()
	s.rfcFinder = f
	s.mu.Unlock()
}

func (s *Service) rfcPublishWait() time.Duration {
	if s.RFCPublishWait > 0 {
		return s.RFCPublishWait
	}
	return DefaultRFCPublishWait
}

// CheckRFCPublications completes delegated RFC shares: for each RFC record
// still in progress it asks the finder for the published issue and, when it
// exists, records the URL and number (RecordRFCPublished). A share with no
// issue after the wait is marked failed with the reason, and its route fails
// so the Share menu offers a retry. It is called after each sync and on a
// timer.
func (s *Service) CheckRFCPublications(ctx context.Context) (found, expired int) {
	s.mu.RLock()
	find := s.rfcFinder
	s.mu.RUnlock()
	if find == nil {
		return 0, 0
	}
	for _, key := range s.Store.PendingRFCKeys() {
		source, id, _ := strings.Cut(key, "/")
		var started time.Time
		for _, r := range s.Store.History(key) {
			if r.Destination == ShareDestRFC && r.Status == ShareInProgress {
				started = r.StartedAt
			}
		}
		rt, ok := s.Store.Route(key, DestSpekRFC)
		it, have, _ := s.Find(ctx, source, id)
		var marker string
		if ok && have {
			if c, err := s.Content(ctx, it); err == nil {
				marker = c.Provenance
			}
		}
		if marker != "" && rt.Repo != "" {
			url, err := find(ctx, rt.Repo, marker)
			switch {
			case err != nil:
				s.Logger.Warn("inbox: looking for the published RFC", "item", key, "error", err)
			case url != "":
				if err := s.RecordRFCPublished(source, id, url); err != nil {
					s.Logger.Warn("inbox: record published RFC", "item", key, "error", err)
					continue
				}
				found++
				continue
			}
		}
		if !started.IsZero() && s.now().Sub(started) >= s.rfcPublishWait() {
			reason := fmt.Sprintf("no RFC with this item's reference appeared in %s within %s", rt.Repo, s.rfcPublishWait())
			_ = s.Store.FinishLatestShare(key, ShareDestRFC, func(r *ShareRecord) {
				r.Status, r.Reason = ShareFailed, clipReason(reason)
				t := s.now()
				r.FinishedAt = &t
			})
			_, _ = s.Store.Set(key, DestSpekRFC, StatusFailed, Result{}, reason)
			expired++
		}
	}
	return found, expired
}
