package knowledge

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// LifecycleState is the curation state of a knowledge fact. Only approved
// facts reach agents by default; draft, deprecated and superseded facts are
// excluded from primers and agent-facing search unless an operator explicitly
// includes them (#11102).
type LifecycleState string

const (
	StateDraft      LifecycleState = "draft"
	StateApproved   LifecycleState = "approved"
	StateDeprecated LifecycleState = "deprecated"
	StateSuperseded LifecycleState = "superseded"
)

// AllLifecycleStates lists every valid lifecycle state.
var AllLifecycleStates = []LifecycleState{StateDraft, StateApproved, StateDeprecated, StateSuperseded}

// ParseLifecycleState validates s (case-insensitive) as a lifecycle state.
func ParseLifecycleState(s string) (LifecycleState, error) {
	st := LifecycleState(strings.ToLower(strings.TrimSpace(s)))
	for _, valid := range AllLifecycleStates {
		if st == valid {
			return st, nil
		}
	}
	return "", fmt.Errorf("invalid lifecycle state %q (want draft, approved, deprecated or superseded)", s)
}

// ParseLifecycleStates parses a comma-separated include list such as
// "deprecated,superseded". "all" expands to every state; empty input yields nil
// (the approved-only default).
func ParseLifecycleStates(csv string) ([]LifecycleState, error) {
	var out []LifecycleState
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.EqualFold(part, "all") {
			return append([]LifecycleState(nil), AllLifecycleStates...), nil
		}
		st, err := ParseLifecycleState(part)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// resolveLifecycleState returns the effective state of a fact. An explicit,
// valid `state` wins. Legacy facts written before lifecycle states existed
// carry no state and default to approved, except that a superseded_by link or
// a legacy `status: deprecated|superseded` (e.g. connector tombstones) maps to
// the matching state so stale context stays out of prompts.
func resolveLifecycleState(state LifecycleState, status, supersededBy string) LifecycleState {
	if st, err := ParseLifecycleState(string(state)); err == nil {
		return st
	}
	if strings.TrimSpace(supersededBy) != "" {
		return StateSuperseded
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case string(StateDeprecated):
		return StateDeprecated
	case string(StateSuperseded):
		return StateSuperseded
	}
	return StateApproved
}

// EffectiveState returns the fact's lifecycle state, defaulting legacy facts
// (no state recorded) to approved.
func (f Fact) EffectiveState() LifecycleState {
	return resolveLifecycleState(f.State, f.Status, f.SupersededBy)
}

// lifecycleAllowed reports whether st passes a filter that always admits
// approved facts plus any explicitly included states.
func lifecycleAllowed(st LifecycleState, include []LifecycleState) bool {
	if st == StateApproved {
		return true
	}
	for _, in := range include {
		if in == st {
			return true
		}
	}
	return false
}

// FilterByLifecycle keeps approved facts plus any facts whose effective state
// is listed in include. With a nil/empty include only approved (current)
// knowledge survives. Retained facts have State populated.
func FilterByLifecycle(facts []Fact, include []LifecycleState) []Fact {
	out := make([]Fact, 0, len(facts))
	for _, f := range facts {
		st := f.EffectiveState()
		if !lifecycleAllowed(st, include) {
			continue
		}
		f.State = st
		out = append(out, f)
	}
	return out
}

// SetLifecycleState records state on the fact identified by slug by rewriting
// its frontmatter. Setting a state other than superseded clears any
// superseded_by link so the fact is not implicitly re-superseded on load.
func (s *FileStore) SetLifecycleState(slug string, state LifecycleState) error {
	st, err := ParseLifecycleState(string(state))
	if err != nil {
		return err
	}
	if st == StateSuperseded {
		return fmt.Errorf("use Supersede to mark %s superseded so the replacement is linked", slug)
	}
	if err := s.rewriteFrontmatter(slug, map[string]string{"state": string(st), "superseded_by": ""}); err != nil {
		return err
	}
	s.reindex()
	return nil
}

// Supersede marks oldSlug as superseded by newSlug: the old fact gets
// state superseded and superseded_by newSlug, and the new fact records
// supersedes oldSlug. Both facts must live in this store.
func (s *FileStore) Supersede(oldSlug, newSlug string) error {
	if oldSlug == "" || newSlug == "" {
		return fmt.Errorf("both superseded and superseding slugs are required")
	}
	if oldSlug == newSlug {
		return fmt.Errorf("fact %s cannot supersede itself", oldSlug)
	}
	s.refreshIfStale()
	s.mu.RLock()
	_, okNew := s.pages[newSlug]
	s.mu.RUnlock()
	if !okNew {
		return fmt.Errorf("page not found: %s", newSlug)
	}
	if err := s.rewriteFrontmatter(oldSlug, map[string]string{"state": string(StateSuperseded), "superseded_by": newSlug}); err != nil {
		return err
	}
	if err := s.rewriteFrontmatter(newSlug, map[string]string{"supersedes": oldSlug}); err != nil {
		return err
	}
	s.reindex()
	return nil
}

// rewriteFrontmatter upserts frontmatter keys on the page's markdown file.
// An empty value removes the key. Values are sanitized to a single line.
func (s *FileStore) rewriteFrontmatter(slug string, kv map[string]string) error {
	s.mu.RLock()
	p, ok := s.pages[slug]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("page not found: %s", slug)
	}
	data, err := os.ReadFile(p.Path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", slug, err)
	}
	updated := upsertFrontmatter(string(data), kv)
	tmpPath := p.Path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", slug, err)
	}
	if err := os.Rename(tmpPath, p.Path); err != nil {
		return fmt.Errorf("renaming %s: %w", slug, err)
	}
	return nil
}

// upsertFrontmatter sets (or, for empty values, removes) top-level keys in a
// markdown file's YAML frontmatter, creating the block when absent. Keys are
// written in sorted order when appended so output is deterministic.
func upsertFrontmatter(content string, kv map[string]string) string {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var lines []string
	body := content
	if strings.HasPrefix(content, "---\n") {
		if endIdx := strings.Index(content[4:], "\n---"); endIdx >= 0 {
			if endIdx > 0 {
				lines = strings.Split(content[4:4+endIdx], "\n")
			}
			body = content[4+endIdx+4:]
		}
	} else {
		body = "\n" + content
	}

	done := make(map[string]bool, len(kv))
	out := make([]string, 0, len(lines)+len(kv))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		matched := ""
		for _, k := range keys {
			if strings.HasPrefix(trimmed, k+":") {
				matched = k
				break
			}
		}
		if matched == "" {
			out = append(out, line)
			continue
		}
		if done[matched] || kv[matched] == "" {
			continue
		}
		done[matched] = true
		out = append(out, matched+": "+sanitizeFrontmatterValue(kv[matched]))
	}
	for _, k := range keys {
		if done[k] || kv[k] == "" {
			continue
		}
		out = append(out, k+": "+sanitizeFrontmatterValue(kv[k]))
	}
	if len(out) == 0 {
		return strings.TrimPrefix(body, "\n")
	}
	return "---\n" + strings.Join(out, "\n") + "\n---" + body
}
