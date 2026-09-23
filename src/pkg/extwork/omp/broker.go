package omp

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Broker holds the attached workbench peers by contributor identity. The hub
// attaches a peer when a contributor connection that declared Capability
// authenticates; the standalone Listener attaches from ext_hello. The adapter
// resolves the peer for an admission through PeerSource.
type Broker struct {
	mu    sync.RWMutex
	peers map[string]*Peer
	now   func() time.Time
}

// NewBroker returns an empty broker.
func NewBroker() *Broker { return &Broker{peers: map[string]*Peer{}, now: time.Now} }

// DefaultBroker is the process-wide broker the registry factory uses.
var DefaultBroker = NewBroker()

// PeerSource resolves an attached peer by identity.
type PeerSource interface {
	Peer(identity string) (*Peer, bool)
}

// Attach builds the peer, registers it, and serves its link until the link
// is lost, at which point it is detached. A peer without Capability is
// refused before anything is registered. A second attachment under the same
// identity replaces the first: the old link is closed and its runs are lost
// with it, which the new incarnation makes visible.
func (b *Broker) Attach(ctx context.Context, identity, incarnation, version string, caps Capabilities, link Link) (*Peer, error) {
	p, err := NewPeer(identity, incarnation, version, caps, link, b.now)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	if old, ok := b.peers[identity]; ok {
		_ = old.Close()
	}
	b.peers[identity] = p
	b.mu.Unlock()
	go func() {
		_ = p.Serve(ctx)
		b.Detach(p)
	}()
	return p, nil
}

// Detach removes p if it is still the registered peer for its identity.
func (b *Broker) Detach(p *Peer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cur, ok := b.peers[p.identity]; ok && cur == p {
		delete(b.peers, p.identity)
	}
}

// Peer implements PeerSource.
func (b *Broker) Peer(identity string) (*Peer, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	p, ok := b.peers[identity]
	return p, ok
}

// Identities lists the attached identities, sorted.
func (b *Broker) Identities() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.peers))
	for id := range b.peers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
