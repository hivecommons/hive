package knowledge

import (
	"os"
	"sort"
	"strings"
	"sync"
)

const (
	// PinTag marks a vault entry the primer carries on every kick whose
	// context its scope admits, whatever the kick's keywords are. Reviewer
	// rules captured from review feedback (hivecommons/hive#19046) use it: a
	// rule a reviewer stated for an agent's future work must reach that
	// agent's next run, not only the runs whose issue titles happen to share
	// words with the rule.
	PinTag = "pin"
	// PinnedFactType is the section the primer renders pinned entries under.
	// It is its own type so the section is rendered first and the byte cap
	// reaches the pinned entries before it reaches ranked ones.
	PinnedFactType FactType = "reviewer-rule"
	// maxPinnedFacts bounds how many pinned entries one kick carries, so a
	// large rule set cannot crowd out ranked knowledge.
	maxPinnedFacts = 20
)

// pinnedSource is one pinned vault: an open store, or a directory the primer
// opens the first time it exists (AddPinnedDir).
type pinnedSource struct {
	name   string
	dir    string
	layer  LayerType
	onOpen func(*FileStore)

	mu    sync.Mutex
	store *FileStore
}

// open returns the source's store, opening its directory when it has appeared
// since the last kick. A directory that is still missing yields nil.
func (ps *pinnedSource) open(p *Primer) *FileStore {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.store != nil || ps.dir == "" {
		return ps.store
	}
	if info, err := os.Stat(ps.dir); err != nil || !info.IsDir() {
		return nil
	}
	store, err := NewFileStore(ps.dir, ps.name, p.logger)
	if err != nil {
		p.logger.Warn("primer: pinned directory not opened", "name", ps.name, "dir", ps.dir, "error", err)
		return nil
	}
	ps.store = store
	p.logger.Info("primer: pinned directory opened", "name", ps.name, "dir", ps.dir)
	if ps.onOpen != nil {
		ps.onOpen(store)
	}
	return store
}

// AddPinnedFileStore registers a vault whose entries tagged PinTag are always
// primed when their scope admits the kick (FactScope.Admits; an entry without
// a scope admits every kick). The usual lifecycle filter and the agent's
// knowledge scope still apply, so a draft, deprecated or out-of-scope entry is
// never pinned. Like AddFileStore it is not safe for concurrent use with Prime.
func (p *Primer) AddPinnedFileStore(name string, store *FileStore, layer LayerType) {
	p.pinned = append(p.pinned, &pinnedSource{name: name, layer: layer, store: store})
	p.logger.Info("primer: added pinned file store", "name", name, "layer", layer)
}

// AddPinnedDir is AddPinnedFileStore for a directory that may not exist yet
// (#19094): the review-feedback sweep creates the review-rules channel on its
// first capture, and its rules must reach kicks without a restart. The
// directory is opened on the first kick after it appears, and onOpen (when
// set) is called once with the opened store, so the caller can also connect it
// where people browse knowledge.
func (p *Primer) AddPinnedDir(name, dir string, layer LayerType, onOpen func(*FileStore)) {
	p.pinned = append(p.pinned, &pinnedSource{name: name, dir: dir, layer: layer, onOpen: onOpen})
	p.logger.Info("primer: added pinned directory", "name", name, "dir", dir, "layer", layer)
}

// HasPinned reports whether any pinned store is registered.
func (p *Primer) HasPinned() bool { return p != nil && len(p.pinned) > 0 }

// pinnedFacts returns the pinned entries a kick must carry, oldest slug first
// so the result is stable. Bodies are read in full.
func (p *Primer) pinnedFacts(scope *TOCScope, kc KickContext, draftAuthors []string) []Fact {
	var out []Fact
	for _, ps := range p.pinned {
		store := ps.open(p)
		if store == nil {
			continue
		}
		for _, f := range store.ListPages(PinTag) {
			full, err := store.ReadPage(f.Slug)
			if err != nil || full == nil {
				continue
			}
			full.Layer = ps.layer
			full.Type = PinnedFactType
			out = append(out, *full)
		}
	}
	if len(out) == 0 {
		return nil
	}
	out = FilterByLifecycleFor(out, p.config.IncludeStates, draftAuthors)
	if scope != nil {
		out = scope.Filter(out)
	}
	out, _ = FilterByKickContext(out, kc)
	sort.Slice(out, func(i, j int) bool { return strings.Compare(out[i].Slug, out[j].Slug) < 0 })
	if len(out) > maxPinnedFacts {
		out = out[:maxPinnedFacts]
	}
	return out
}

// withPinned puts pinned first and drops any ranked copy of the same slug,
// then trims the ranked remainder so the total stays within maxFacts. Pinned
// entries are never trimmed.
func withPinned(pinned, ranked []Fact, maxFacts int) []Fact {
	if len(pinned) == 0 {
		return ranked
	}
	have := make(map[string]bool, len(pinned))
	for _, f := range pinned {
		have[f.Slug] = true
	}
	room := maxFacts - len(pinned)
	out := append([]Fact(nil), pinned...)
	for _, f := range ranked {
		if have[f.Slug] || room <= 0 {
			continue
		}
		out = append(out, f)
		room--
	}
	return out
}
