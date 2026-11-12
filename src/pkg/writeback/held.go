package writeback

// A writer that asks the gate again on every sweep for the same item (the
// blocked sweep's repair, every few minutes) would otherwise report the same
// refusal each time. The gate keeps the reason it last refused each such item
// for, so the writer can say so once and again only when the reason changes.
// It lives here rather than with the writer because the gate is the one
// object a process shares across GitHub clients and rebuilds of them. It is
// in memory only: a restart reports each hold once more.

// heldKey names one item a kind is being held on.
type heldKey struct {
	repo   string
	number int
	kind   Kind
}

// NoteHeld records that kind on repo#number was refused for reason and
// reports whether that is news: the item's first hold, or a reason other
// than the one last noted. A nil gate refuses nothing and has nothing to
// remember, so every call on it is news.
func (g *Gate) NoteHeld(repo string, number int, kind Kind, reason string) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	key := heldKey{repo: repoKey(repo), number: number, kind: kind}
	if last, ok := g.held[key]; ok && last == reason {
		return false
	}
	if g.held == nil {
		g.held = map[heldKey]string{}
	}
	g.held[key] = reason
	return true
}

// ForgetHeld drops what NoteHeld kept for kind on repo#number, because the
// write went through or the item is no longer one the writer would write to.
// A later hold of the item is news again.
func (g *Gate) ForgetHeld(repo string, number int, kind Kind) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.held, heldKey{repo: repoKey(repo), number: number, kind: kind})
}

// RetainHeld forgets every hold of kind on an item keep does not report,
// which bounds the memory by the writer's current targets: an item that was
// closed, or whose label a person removed, never reaches ForgetHeld. keep is
// given the repo lower-cased and trimmed; it runs under the gate's lock and
// must not call the gate.
func (g *Gate) RetainHeld(kind Kind, keep func(repo string, number int) bool) {
	if g == nil || keep == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for key := range g.held {
		if key.kind == kind && !keep(key.repo, key.number) {
			delete(g.held, key)
		}
	}
}
