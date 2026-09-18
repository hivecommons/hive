package mention

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Store struct {
	mu    sync.Mutex
	path  string
	state storeState
}

type storeState struct {
	Watermarks map[string]time.Time `json:"watermarks"`
	Seen       map[string]bool      `json:"seen"`
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

func NewStore(path string) (*Store, error) {
	s := &Store{path: path, state: storeState{Watermarks: map[string]time.Time{}, Seen: map[string]bool{}, Pending: map[string][]Context{}, Active: map[string][]Context{}}}
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
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s.state); err != nil {
			return nil, err
		}
	}
	if s.state.Watermarks == nil {
		s.state.Watermarks = map[string]time.Time{}
	}
	if s.state.Seen == nil {
		s.state.Seen = map[string]bool{}
	}
	if s.state.Pending == nil {
		s.state.Pending = map[string][]Context{}
	}
	if s.state.Active == nil {
		s.state.Active = map[string][]Context{}
	}
	return s, nil
}

func (s *Store) Watermark(repo string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Watermarks[repo]
}

func (s *Store) Seen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return id != "" && s.state.Seen[id]
}

func (s *Store) Mark(repo, id string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != "" {
		s.state.Seen[id] = true
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

func contextIndex(queue []Context, source string) int {
	source = strings.TrimSpace(source)
	for i, ctx := range queue {
		if source == "" || ctx.KickSource == source {
			return i
		}
	}
	return -1
}

func (s *Store) RequeueActiveFront(agent string, ctx Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue := s.state.Active[agent]
	s.state.Active[agent] = append([]Context{ctx}, queue...)
	return s.saveLocked()
}

func (s *Store) ClearActive(agent string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if t.After(s.state.Watermarks[repo]) {
		s.state.Watermarks[repo] = t
	}
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}
