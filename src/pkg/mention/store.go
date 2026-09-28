package mention

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const contextTTL = 24 * time.Hour

// seenTTL bounds how long a dedupe key stays in the seen set. Mention node
// IDs only need to outlive the per-repo watermark (the poller drops comments
// created before it) and GitHub's 3-day webhook redelivery window, but the
// set also carries Actions run keys (repo:run_id:run_attempt) and OIDC jti
// keys, so the TTL must exceed GitHub's 35-day maximum workflow-run
// lifetime: a later job of the same run attempt must still be recognised as
// a replay.
const seenTTL = 45 * 24 * time.Hour

type Store struct {
	mu    sync.Mutex
	path  string
	state storeState
}

type storeState struct {
	Watermarks map[string]time.Time `json:"watermarks"`
	// Seen maps each dedupe key to when it was first marked so entries age
	// out after seenTTL. It persists under "seen_at", not the legacy "seen"
	// key, so an older build reading a newer file sees an empty legacy set
	// instead of failing to parse.
	Seen map[string]time.Time `json:"seen_at"`
	// LegacySeen is the pre-TTL bool set: read on load, migrated into Seen,
	// never written back.
	LegacySeen map[string]bool      `json:"seen,omitempty"`
	Pending    map[string][]Context `json:"pending,omitempty"`
	Active     map[string][]Context `json:"active,omitempty"`
}

type Context struct {
	Agent      string    `json:"agent"`
	KickSource string    `json:"kick_source,omitempty"`
	Repo       string    `json:"repo"`
	Kind       string    `json:"kind,omitempty"`
	Number     int       `json:"number"`
	NodeID     string    `json:"node_id"`
	CommentID  int64     `json:"comment_id"`
	HTMLURL    string    `json:"html_url"`
	Author     string    `json:"author"`
	Accepted   time.Time `json:"accepted"`
}

// NewStore loads the store at path (an empty path is an in-memory store). A
// file that cannot be parsed — a torn write from before saves were atomic, or
// any other corruption — is moved aside to a timestamped ".corrupt-*" backup
// and the store starts empty instead of failing: the poller re-seeds each
// repo's watermark to now, so older comments are not replayed, and the
// mention feature and Actions dispatch dedupe stay available.
func NewStore(path string) (*Store, error) {
	s := &Store{path: path, state: emptyStoreState()}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	now := time.Now()
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s.state); err != nil {
			backup := fmt.Sprintf("%s.corrupt-%d", path, now.UnixNano())
			if rerr := os.Rename(path, backup); rerr != nil {
				return nil, errors.Join(fmt.Errorf("parse mention store %s: %w", path, err), fmt.Errorf("back up corrupt mention store: %w", rerr))
			}
			slog.Default().Error("mention store corrupt; moved aside and starting empty", "path", path, "backup", backup, "error", err)
			s.state = emptyStoreState()
			return s, nil
		}
	}
	if s.state.Watermarks == nil {
		s.state.Watermarks = map[string]time.Time{}
	}
	if s.state.Seen == nil {
		s.state.Seen = map[string]time.Time{}
	}
	migrated := len(s.state.LegacySeen) > 0
	for id, ok := range s.state.LegacySeen {
		if _, exists := s.state.Seen[id]; ok && id != "" && !exists {
			s.state.Seen[id] = now
		}
	}
	s.state.LegacySeen = nil
	if s.state.Pending == nil {
		s.state.Pending = map[string][]Context{}
	}
	if s.state.Active == nil {
		s.state.Active = map[string][]Context{}
	}
	if s.pruneExpiredLocked(now, "load") || migrated {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func emptyStoreState() storeState {
	return storeState{Watermarks: map[string]time.Time{}, Seen: map[string]time.Time{}, Pending: map[string][]Context{}, Active: map[string][]Context{}}
}

func (s *Store) Watermark(repo string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Watermarks[repo]
}

func (s *Store) Seen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		return false
	}
	_, ok := s.state.Seen[id]
	return ok
}

func (s *Store) Mark(repo, id string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.pruneExpiredLocked(now, "mutation")
	if _, exists := s.state.Seen[id]; id != "" && !exists {
		s.state.Seen[id] = now
	}
	if t.After(s.state.Watermarks[repo]) {
		s.state.Watermarks[repo] = t
	}
	return s.saveLocked()
}

func (s *Store) RecordActive(agent string, ev Event, accepted time.Time) error {
	return s.recordContext(agent, ev, mentionKickSource(ev), accepted, false)
}

func (s *Store) RecordPending(agent string, ev Event, source string, accepted time.Time) error {
	return s.recordContext(agent, ev, source, accepted, true)
}

func (s *Store) recordContext(agent string, ev Event, source string, accepted time.Time, pending bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(time.Now(), "mutation")
	if agent == "" {
		return s.saveLocked()
	}
	ctx := Context{
		Agent:      agent,
		KickSource: strings.TrimSpace(source),
		Repo:       ev.Repo,
		Kind:       ev.Kind,
		Number:     ev.Number,
		NodeID:     ev.NodeID,
		CommentID:  ev.CommentID,
		HTMLURL:    ev.HTMLURL,
		Author:     ev.Author,
		Accepted:   accepted,
	}
	if pending {
		if s.state.Pending == nil {
			s.state.Pending = map[string][]Context{}
		}
		s.state.Pending[agent] = append(s.state.Pending[agent], ctx)
	} else {
		if s.state.Active == nil {
			s.state.Active = map[string][]Context{}
		}
		s.state.Active[agent] = append(s.state.Active[agent], ctx)
	}
	return s.saveLocked()
}

func (s *Store) PromotePending(agent, source string) (Context, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(time.Now(), "mutation")
	queue := s.state.Pending[agent]
	idx := contextIndex(queue, source)
	if idx < 0 {
		return Context{}, false, nil
	}
	ctx := queue[idx]
	if len(queue) == 1 {
		delete(s.state.Pending, agent)
	} else {
		next := append([]Context(nil), queue[:idx]...)
		next = append(next, queue[idx+1:]...)
		s.state.Pending[agent] = next
	}
	s.state.Active[agent] = append(s.state.Active[agent], ctx)
	if err := s.saveLocked(); err != nil {
		return Context{}, false, err
	}
	return ctx, true, nil
}

func (s *Store) ClearPending(agent, source string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(time.Now(), "mutation")
	queue := s.state.Pending[agent]
	idx := contextIndex(queue, source)
	if idx < 0 {
		return s.saveLocked()
	}
	if len(queue) == 1 {
		delete(s.state.Pending, agent)
	} else {
		next := append([]Context(nil), queue[:idx]...)
		next = append(next, queue[idx+1:]...)
		s.state.Pending[agent] = next
	}
	return s.saveLocked()
}

func (s *Store) ActiveForAgent(agent string) (Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue := s.state.Active[agent]
	if len(queue) == 0 {
		return Context{}, false
	}
	return queue[0], true
}

func (s *Store) ClaimActive(agent string) (Context, bool, error) {
	return s.ClaimActiveSource(agent, "")
}

func (s *Store) ClaimActiveSource(agent, source string) (Context, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(time.Now(), "mutation")
	queue := s.state.Active[agent]
	idx := contextIndex(queue, source)
	if idx < 0 {
		return Context{}, false, nil
	}
	ctx := queue[idx]
	if len(queue) == 1 {
		delete(s.state.Active, agent)
	} else {
		next := append([]Context(nil), queue[:idx]...)
		next = append(next, queue[idx+1:]...)
		s.state.Active[agent] = next
	}
	if err := s.saveLocked(); err != nil {
		return Context{}, false, err
	}
	return ctx, true, nil
}

func (s *Store) DropSource(agent, source string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(time.Now(), "mutation")
	s.state.Pending = dropFromQueue(s.state.Pending, agent, source)
	s.state.Active = dropFromQueue(s.state.Active, agent, source)
	return s.saveLocked()
}

func contextIndex(queue []Context, source string) int {
	source = strings.TrimSpace(source)
	for i, ctx := range queue {
		if source == "" || ctx.KickSource == source {
			return i
		}
	}
	return -1
}

func dropFromQueue(queues map[string][]Context, agent, source string) map[string][]Context {
	queue := queues[agent]
	idx := contextIndex(queue, source)
	if idx < 0 {
		return queues
	}
	if len(queue) == 1 {
		delete(queues, agent)
		return queues
	}
	next := append([]Context(nil), queue[:idx]...)
	next = append(next, queue[idx+1:]...)
	queues[agent] = next
	return queues
}

func (s *Store) RequeueActiveFront(agent string, ctx Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(time.Now(), "mutation")
	queue := s.state.Active[agent]
	s.state.Active[agent] = append([]Context{ctx}, queue...)
	return s.saveLocked()
}

func (s *Store) ClearActive(agent string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(time.Now(), "mutation")
	queue := s.state.Active[agent]
	if len(queue) <= 1 {
		delete(s.state.Active, agent)
	} else {
		s.state.Active[agent] = append([]Context(nil), queue[1:]...)
	}
	return s.saveLocked()
}

func (s *Store) Advance(repo string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(time.Now(), "mutation")
	if t.After(s.state.Watermarks[repo]) {
		s.state.Watermarks[repo] = t
	}
	return s.saveLocked()
}

func (s *Store) pruneExpiredLocked(now time.Time, reason string) bool {
	changed := false
	s.state.Pending, changed = pruneContextQueues(s.state.Pending, now, reason, changed)
	s.state.Active, changed = pruneContextQueues(s.state.Active, now, reason, changed)
	for id, marked := range s.state.Seen {
		if now.Sub(marked) > seenTTL {
			delete(s.state.Seen, id)
			changed = true
		}
	}
	return changed
}

func pruneContextQueues(queues map[string][]Context, now time.Time, reason string, changed bool) (map[string][]Context, bool) {
	for agent, queue := range queues {
		kept := queue[:0]
		for _, ctx := range queue {
			if !ctx.Accepted.IsZero() && now.Sub(ctx.Accepted) > contextTTL {
				slog.Default().Debug("audit: mention context expired", "agent", agent, "source", ctx.KickSource, "repo", ctx.Repo, "number", ctx.Number, "reason", reason)
				changed = true
				continue
			}
			kept = append(kept, ctx)
		}
		if len(kept) == 0 {
			delete(queues, agent)
			continue
		}
		queues[agent] = append([]Context(nil), kept...)
	}
	return queues, changed
}

// saveLocked replaces the store file atomically: a crash or OOM kill mid-save
// leaves either the previous complete document or the new one, never a torn
// file that would fail to parse on the next boot.
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return err
	}
	committed = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
