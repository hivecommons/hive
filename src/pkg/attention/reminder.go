package attention

import (
	"sort"
	"strings"
	"time"
)

// DefaultReminderInterval is the non-urgent reminder interval the #16075
// decision set: at most one reminder per unresolved episode per 24 hours.
const DefaultReminderInterval = 24 * time.Hour

// Reminder returns the configured reminder interval, or the default.
func (c Config) Reminder() time.Duration {
	if c.ReminderInterval > 0 {
		return c.ReminderInterval
	}
	return DefaultReminderInterval
}

// LastPing is the latest of the episode's page, its last reminder and the
// recipient's last observed action: the reminder interval runs from it.
func (e Episode) LastPing() time.Time {
	last := e.PagedAt
	for _, t := range []time.Time{e.RemindedAt, e.RecipientActedAt} {
		if t.After(last) {
			last = t
		}
	}
	return last
}

// ReminderDue reports whether e may be reminded at now. Only an episode that
// was actually paged and is still waiting on its recipient qualifies: never a
// resolved, acknowledged, snoozed, uncertain or never-paged one, and never
// before interval has passed since LastPing. Whether the recipient acted on
// the item since LastPing is the caller's check (it needs the forge).
func ReminderDue(e Episode, now time.Time, interval time.Duration) bool {
	if interval <= 0 {
		interval = DefaultReminderInterval
	}
	switch {
	case e.Key == (Key{}), e.State != Notified, e.PagedAt.IsZero(), e.Uncertain:
		return false
	case !e.AckedAt.IsZero(), now.Before(e.SnoozedUntil):
		return false
	}
	return now.Sub(e.LastPing()) >= interval
}

// OpenEpisodes returns copies of repo's unresolved episodes, ordered by item and
// then key hash, for the per-repo attention queue and the reminder pass.
// History-seeded baselines without a key are not listed.
func (s *Store) OpenEpisodes(repo string) []Episode {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Episode
	for _, e := range s.state.Episodes {
		if e.State == Resolved || e.Key == (Key{}) || canonical(e.Key.Repo) != canonical(repo) {
			continue
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.Item != out[j].Key.Item {
			return out[i].Key.Item < out[j].Key.Item
		}
		return out[i].Key.Hash() < out[j].Key.Hash()
	})
	return out
}

// ResolveItem resolves every unresolved episode of repo#item, as when the
// item is closed: nothing is left to remind about, and the item drops out of
// the attention queue. It returns the episodes it resolved.
func (s *Store) ResolveItem(repo string, item int) ([]Episode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	var out []Episode
	for _, e := range s.state.Episodes {
		if e.State == Resolved || e.Key == (Key{}) || canonical(e.Key.Repo) != canonical(repo) || e.Key.Item != item {
			continue
		}
		e.State = Resolved
		e.ResolvedAt = now
		e.WouldNotify = false
		out = append(out, *e)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, s.save()
}

// RecordReminder records that a reminder for key was delivered at at.
func (s *Store) RecordReminder(key Key, at time.Time) (Episode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.state.Episodes[key.Hash()]
	if e == nil || e.State == Resolved {
		return Episode{}, ErrNoEpisode
	}
	e.RemindedAt = at.UTC()
	e.Reminders++
	return *e, s.save()
}

// RecordRecipientAction records that key's recipient acted on the item at
// at, after the last ping; the next reminder interval runs from then.
func (s *Store) RecordRecipientAction(key Key, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.state.Episodes[key.Hash()]
	if e == nil || e.State == Resolved {
		return ErrNoEpisode
	}
	if !at.After(e.RecipientActedAt) {
		return nil
	}
	e.RecipientActedAt = at.UTC()
	return s.save()
}

// RecipientLogin is the episode's recipient as a lower-case GitHub login
// without `@`. ok is false for an empty or team (`org/team`) recipient: a
// team's action cannot be attributed to it, so the caller treats it as none.
func RecipientLogin(e Episode) (login string, ok bool) {
	login = strings.TrimLeft(canonical(e.Key.Recipient), "@")
	return login, login != "" && !strings.Contains(login, "/")
}
