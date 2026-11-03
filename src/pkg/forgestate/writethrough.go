package forgestate

import (
	"fmt"
	"strings"
)

// MutationOp is a hive mutation WriteThrough applies to the cache.
type MutationOp string

const (
	OpLabelAdd    MutationOp = "label_add"
	OpLabelRemove MutationOp = "label_remove"
	OpComment     MutationOp = "comment"
	OpMerge       MutationOp = "merge"
	OpClose       MutationOp = "close"
	OpReopen      MutationOp = "reopen"
	OpClaim       MutationOp = "claim"
)

// Mutation is one GitHub write the hive made and GitHub accepted.
type Mutation struct {
	Op     MutationOp
	Kind   Kind
	Number int
	// Labels are added (OpLabelAdd, OpClaim) or removed (OpLabelRemove).
	Labels []string
	// Assignee is added by OpClaim when set.
	Assignee string
}

// WriteThrough applies m to repo's cache immediately, so the next read sees
// the hive's own write without waiting for its webhook or the next
// reconcile. Call it only after GitHub accepted the write. Label, comment
// and claim mutations on an item the cache does not hold are no-ops (the
// item arrives with its webhook); merge and close always remove and leave
// a tombstone; reopen clears the tombstone so the item's webhook re-adds it.
func (s *Store) WriteThrough(repo string, m Mutation) error {
	rs := s.lookup(repo)
	if rs == nil {
		return fmt.Errorf("%w: %s", ErrUnknownRepo, repo)
	}
	if m.Number <= 0 || (m.Kind != KindIssue && m.Kind != KindPR) {
		return fmt.Errorf("forgestate: write-through %s needs an issue or pr number", m.Op)
	}
	switch m.Op {
	case OpLabelAdd, OpLabelRemove, OpComment, OpClaim, OpMerge, OpClose, OpReopen:
	default:
		return fmt.Errorf("forgestate: unknown write-through op %q", m.Op)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	items := rs.items(m.Kind)
	cur, ok := items[m.Number]
	switch m.Op {
	case OpMerge, OpClose:
		at := cur.UpdatedAt
		if !ok {
			at = now
		}
		s.removeLocked(rs, m.Kind, m.Number, at, now, true)
		s.opts.Metrics.WriteThrough(rs.name, string(m.Op))
		return nil
	case OpReopen:
		delete(rs.tombs, itemKey{m.Kind, m.Number})
		s.opts.Metrics.WriteThrough(rs.name, string(m.Op))
		return nil
	}
	if !ok {
		s.opts.Metrics.WriteThrough(rs.name, resultMiss)
		return nil
	}
	cur = cur.clone()
	switch m.Op {
	case OpLabelAdd:
		cur.Labels = addAll(cur.Labels, m.Labels)
	case OpLabelRemove:
		cur.Labels = removeAll(cur.Labels, m.Labels)
	case OpClaim:
		cur.Labels = addAll(cur.Labels, m.Labels)
		if m.Assignee != "" {
			cur.Assignees = addAll(cur.Assignees, []string{m.Assignee})
		}
	}
	cur.Source, cur.FetchedAt = SourceWriteThrough, now
	items[m.Number] = cur
	s.opts.Metrics.WriteThrough(rs.name, string(m.Op))
	return nil
}

func addAll(have, add []string) []string {
	for _, a := range add {
		if a == "" {
			continue
		}
		found := false
		for _, h := range have {
			if strings.EqualFold(h, a) {
				found = true
				break
			}
		}
		if !found {
			have = append(have, a)
		}
	}
	return have
}

func removeAll(have, drop []string) []string {
	out := have[:0]
	for _, h := range have {
		keep := true
		for _, d := range drop {
			if strings.EqualFold(h, d) {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, h)
		}
	}
	return out
}
