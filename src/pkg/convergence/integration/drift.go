package integration

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ObserveFiles reuses the merge planner's existing head-keyed file probe.
// The probe is advisory: it does not attest a fresh remote head or grant proof.
func (s *Store) ObserveFiles(repo, head string, files []string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.writerLock()
	if err != nil {
		return err
	}
	defer unlock()
	for _, intent := range s.intents {
		if !strings.EqualFold(intent.Repo, repo) || intent.Head != head {
			continue
		}
		drift := Drift{Key: intent.Key, Repo: repo, Head: head, IntentGeneration: intent.Generation, ObservedAt: now, Source: "existing-head-keyed-overlap-probe-v1", Actual: append([]string(nil), files...)}
		sort.Strings(drift.Actual)
		for _, file := range drift.Actual {
			anticipated := false
			for _, path := range append(append([]string(nil), intent.Paths...), intent.Generated...) {
				if file == path || strings.HasPrefix(file, strings.TrimSuffix(path, "/")+"/") {
					anticipated = true
					break
				}
			}
			if !anticipated {
				drift.OutsideAnticipated = append(drift.OutsideAnticipated, file)
			}
		}
		identity := drift
		identity.ObservedAt = time.Time{}
		id := "files/" + eventDigest(Event{Drift: &identity})
		if _, exists := s.seen[id]; exists {
			continue
		}
		if err := s.appendLocked(Event{Version: SchemaVersion, ID: id, At: now, Drift: &drift}); err != nil {
			return fmt.Errorf("recording overlap scope drift: %w", err)
		}
	}
	return nil
}

func relationDisposition(intent Intent, key, kind string) string {
	for _, assessment := range intent.Relations {
		if assessment.Key == key && assessment.Kind == kind {
			return "; operator assessment=" + assessment.Disposition + " (" + assessment.Provenance + ")"
		}
	}
	return ""
}
