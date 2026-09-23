package omp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/extwork"
)

// Peer errors.
var (
	// ErrCapabilityMissing: the peer did not declare Capability. It is refused
	// as an OMP host and never attached as anything else.
	ErrCapabilityMissing = fmt.Errorf("%w: peer lacks capability %s", extwork.ErrRefused, Capability)
	// ErrPeerGone: the workbench disconnected; the native state of its runs
	// is unknown, never stopped.
	ErrPeerGone = fmt.Errorf("%w: workbench disconnected", extwork.ErrTransport)
	// ErrNotAccepted: a start was attempted for a key the peer never accepted.
	ErrNotAccepted = fmt.Errorf("%w: offer was not accepted; no context is delivered", extwork.ErrRefused)
	// ErrOfferPending: the offer was withdrawn (context ended) before the
	// workbench answered; it counts as declined.
	ErrOfferPending = errors.New("omp: offer withdrawn before the workbench answered")
)

// detailDisconnected is the observation detail after the peer went away.
const detailDisconnected = "workbench disconnected; native state unknown"

type decision struct {
	accepted bool
	reason   string
}

type startReply struct {
	remoteRunID  string
	deduplicated bool
	refused      string
}

type cancelReply struct {
	acknowledged bool
	stopped      bool
	detail       string
}

// run is the adapter-side record of one execution key on this peer.
type run struct {
	accepted    bool
	digest      string
	remoteRunID string
	state       extwork.State
	stage       string
	detail      string
	observedAt  time.Time
	receipt     *extwork.ReceiptRef
	artifacts   map[string][]byte
	startAck    chan startReply
	cancelAck   chan cancelReply
}

// Peer is one attached workbench: its identity, declared posture, link, and
// the runs it holds. Serve drives the read loop; every inbound frame updates
// the run it names, so Observe never blocks on the network.
type Peer struct {
	identity    string
	incarnation string
	version     string
	caps        Capabilities
	link        Link
	now         func() time.Time

	mu     sync.Mutex
	seq    int
	runs   map[extwork.ExecutionKey]*run
	offers map[extwork.ExecutionKey]chan decision
	gone   chan struct{}
	err    error
}

// NewPeer builds a peer over link. It refuses a posture without Capability
// (never downgrades) and an empty identity.
func NewPeer(identity, incarnation, version string, caps Capabilities, link Link, now func() time.Time) (*Peer, error) {
	if identity == "" {
		return nil, fmt.Errorf("%w: peer identity is required", extwork.ErrRefused)
	}
	if !caps.Declares(Capability) {
		return nil, ErrCapabilityMissing
	}
	if link == nil {
		return nil, fmt.Errorf("%w: peer link is required", extwork.ErrRefused)
	}
	if now == nil {
		now = time.Now
	}
	return &Peer{identity: identity, incarnation: incarnation, version: version, caps: caps, link: link, now: now, runs: map[extwork.ExecutionKey]*run{}, offers: map[extwork.ExecutionKey]chan decision{}, gone: make(chan struct{})}, nil
}

// Identity is the contributor identity the lease is held under.
func (p *Peer) Identity() string { return p.identity }

// Incarnation is the workbench session identity.
func (p *Peer) Incarnation() string { return p.incarnation }

// Version is the workflow version the workbench declared.
func (p *Peer) Version() string { return p.version }

// Gone is closed once the link is lost.
func (p *Peer) Gone() <-chan struct{} { return p.gone }

// Err is the read-loop error that ended the link, or nil while it is up.
func (p *Peer) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// goneErr wraps ErrPeerGone with the read-loop error so an observation
// after disconnect says why the state is unknown. Callers hold p.mu.
func (p *Peer) goneErrLocked() error {
	if p.err == nil {
		return ErrPeerGone
	}
	return fmt.Errorf("%w: %v", ErrPeerGone, p.err)
}

// Alive reports whether the link is still up.
func (p *Peer) Alive() bool {
	select {
	case <-p.gone:
		return false
	default:
		return true
	}
}

func (p *Peer) nextSeq() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	return p.seq
}

func (p *Peer) send(ctx context.Context, msg Message) error {
	if !p.Alive() {
		return ErrPeerGone
	}
	msg.Seq = p.nextSeq()
	if err := p.link.Send(ctx, msg); err != nil {
		return fmt.Errorf("%w: %v", ErrPeerGone, err)
	}
	return nil
}

// Serve reads frames until the link fails or ctx ends, then marks every
// non-terminal run unknown. It returns the link error.
func (p *Peer) Serve(ctx context.Context) error {
	for {
		msg, err := p.link.Recv(ctx)
		if err != nil {
			p.markGone(err)
			return err
		}
		p.handle(msg)
	}
}

// Close drops the link, which ends Serve.
func (p *Peer) Close() error { return p.link.Close() }

func (p *Peer) markGone(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.gone:
		return
	default:
	}
	p.err = err
	close(p.gone)
	for _, r := range p.runs {
		if r.state != extwork.StateTerminal {
			r.state = extwork.StateUnknown
			r.detail = detailDisconnected
			r.observedAt = p.now()
		}
	}
}

func (p *Peer) runLocked(key extwork.ExecutionKey) *run {
	r, ok := p.runs[key]
	if !ok {
		r = &run{artifacts: map[string][]byte{}, startAck: make(chan startReply, 1), cancelAck: make(chan cancelReply, 1)}
		p.runs[key] = r
	}
	return r
}

func (p *Peer) handle(msg Message) {
	key := extwork.ExecutionKey(msg.ExecutionKey)
	if key == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch msg.Type {
	case MsgAccept, MsgDecline:
		ch, ok := p.offers[key]
		if !ok {
			ch = make(chan decision, 1)
			p.offers[key] = ch
		}
		select {
		case ch <- decision{accepted: msg.Type == MsgAccept, reason: msg.Reason}:
		default:
		}
	case MsgStarted:
		r := p.runLocked(key)
		select {
		case r.startAck <- startReply{remoteRunID: msg.RemoteRunID, deduplicated: msg.Deduplicated}:
		default:
		}
	case MsgStartRefused:
		r := p.runLocked(key)
		select {
		case r.startAck <- startReply{refused: msg.Reason}:
		default:
		}
	case MsgProgress:
		r := p.runLocked(key)
		if r.state == extwork.StateTerminal {
			return
		}
		state := extwork.State(msg.State)
		detail := msg.Detail
		if !extwork.ValidState(state) {
			detail = "workbench reported state " + msg.State
			state = extwork.StateUnknown
		}
		r.state, r.detail, r.observedAt = state, detail, p.now()
		if msg.Stage != "" {
			// A frame that omits the stage keeps the last one reported, or
			// the stage the offer named; a progress event never loses it.
			r.stage = msg.Stage
		}
		if msg.RemoteRunID != "" {
			r.remoteRunID = msg.RemoteRunID
		}
	case MsgCancelAck:
		r := p.runLocked(key)
		select {
		case r.cancelAck <- cancelReply{acknowledged: msg.Acknowledged, stopped: msg.Stopped, detail: msg.Detail}:
		default:
		}
	case MsgReceipt:
		if msg.Artifact == nil {
			return
		}
		r := p.runLocked(key)
		if _, err := extwork.CleanArtifactPath(msg.Artifact.Path); err != nil {
			r.detail = "workbench sent an artifact with an unsafe path"
			return
		}
		r.artifacts[msg.Artifact.Path] = append([]byte(nil), msg.Artifact.Body...)
		r.receipt = &extwork.ReceiptRef{Path: msg.Artifact.Path, Digest: msg.Artifact.Digest, Size: msg.Artifact.Size}
		r.state, r.observedAt = extwork.StateTerminal, p.now()
		if msg.RemoteRunID != "" {
			r.remoteRunID = msg.RemoteRunID
		}
		if msg.Stage != "" {
			r.stage = msg.Stage
		}
	}
}

// Offer sends the summary and waits for the workbench's answer. Nothing but
// the Offer fields leaves Hive before an answer arrives. A lost link or an
// ended context is a decline, never an acceptance.
func (p *Peer) Offer(ctx context.Context, offer extwork.Offer, gen uint64) (extwork.OfferDecision, string, error) {
	p.mu.Lock()
	ch, ok := p.offers[offer.ExecutionKey]
	if !ok {
		ch = make(chan decision, 1)
		p.offers[offer.ExecutionKey] = ch
	}
	p.mu.Unlock()
	msg := Message{Type: MsgOffer, ExecutionKey: string(offer.ExecutionKey), WorkKey: offer.WorkKey, TaskID: offer.AssignmentID, TaskGen: gen, Stage: offer.Stage, Summary: offer.Summary}
	if err := p.send(ctx, msg); err != nil {
		return extwork.OfferDeclined, err.Error(), err
	}
	select {
	case d := <-ch:
		p.mu.Lock()
		delete(p.offers, offer.ExecutionKey)
		if d.accepted {
			r := p.runLocked(offer.ExecutionKey)
			r.accepted = true
			r.stage = offer.Stage
		}
		p.mu.Unlock()
		if d.accepted {
			return extwork.OfferAccepted, d.reason, nil
		}
		return extwork.OfferDeclined, d.reason, nil
	case <-p.gone:
		return extwork.OfferDeclined, ErrPeerGone.Error(), ErrPeerGone
	case <-ctx.Done():
		return extwork.OfferDeclined, ErrOfferPending.Error(), ErrOfferPending
	}
}

// Accepted reports whether the workbench accepted the key's offer.
func (p *Peer) Accepted(key extwork.ExecutionKey) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.runs[key]
	return ok && r.accepted
}

// Start delivers the bundle for an accepted key and waits for the workbench's
// start acknowledgement. The same key with a different payload is a conflict
// here, before anything is sent; the same key and payload is re-sent so a
// workbench that already runs it can answer deduplicated.
func (p *Peer) Start(ctx context.Context, key extwork.ExecutionKey, gen uint64, stage string, payload []byte) (extwork.StartResult, error) {
	digest := extwork.RequestDigest(payload)
	p.mu.Lock()
	r, ok := p.runs[key]
	if !ok || !r.accepted {
		p.mu.Unlock()
		return extwork.StartResult{}, ErrNotAccepted
	}
	if r.digest != "" && r.digest != digest {
		p.mu.Unlock()
		return extwork.StartResult{}, fmt.Errorf("%w: execution key already started with a different payload", extwork.ErrConflict)
	}
	r.digest = digest
	// Drain a stale acknowledgement so this start waits for its own.
	select {
	case <-r.startAck:
	default:
	}
	p.mu.Unlock()
	if err := p.send(ctx, Message{Type: MsgStart, ExecutionKey: string(key), TaskGen: gen, Stage: stage, Payload: payload}); err != nil {
		return extwork.StartResult{}, err
	}
	select {
	case reply := <-r.startAck:
		if reply.refused != "" {
			if reply.refused == ReasonConflict {
				return extwork.StartResult{}, fmt.Errorf("%w: workbench refused: %s", extwork.ErrConflict, reply.refused)
			}
			return extwork.StartResult{}, fmt.Errorf("%w: workbench refused start: %s", extwork.ErrRefused, reply.refused)
		}
		if reply.remoteRunID == "" {
			return extwork.StartResult{}, fmt.Errorf("%w: workbench acknowledged the start without a run id", extwork.ErrRefused)
		}
		p.mu.Lock()
		r.remoteRunID = reply.remoteRunID
		if r.state == "" {
			r.state, r.observedAt = extwork.StateAccepted, p.now()
		}
		p.mu.Unlock()
		return extwork.StartResult{RemoteRunID: reply.remoteRunID, RemoteIncarnation: p.incarnation, Deduplicated: reply.deduplicated}, nil
	case <-p.gone:
		return extwork.StartResult{}, ErrPeerGone
	case <-ctx.Done():
		return extwork.StartResult{}, fmt.Errorf("%w: %v", extwork.ErrTransport, ctx.Err())
	}
}

// Observe reads the last state the workbench reported for key. A key the
// workbench never started is ErrNotFound. Once the link is gone every
// non-terminal run is unknown with ErrPeerGone; a terminal run stays terminal
// because its receipt is already in hand.
func (p *Peer) Observe(key extwork.ExecutionKey) (extwork.Observation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.runs[key]
	if !ok || r.remoteRunID == "" {
		return extwork.Observation{State: extwork.StateUnknown}, extwork.ErrNotFound
	}
	obs := extwork.Observation{State: r.state, RemoteRunID: r.remoteRunID, RemoteIncarnation: p.incarnation, Stage: r.stage, Detail: r.detail, Receipt: r.receipt, ObservedAt: r.observedAt}
	if obs.State != extwork.StateTerminal && !p.Alive() {
		obs.State, obs.Detail = extwork.StateUnknown, detailDisconnected
		return obs, p.goneErrLocked()
	}
	return obs, nil
}

// Cancel sends the request and waits for the acknowledgement. Requested is
// true only once the frame left; Acknowledged and Stopped are exactly what
// the workbench reported. A lost link leaves Stopped false.
func (p *Peer) Cancel(ctx context.Context, key extwork.ExecutionKey) (extwork.CancelFacts, error) {
	p.mu.Lock()
	r, ok := p.runs[key]
	if !ok || r.remoteRunID == "" {
		p.mu.Unlock()
		return extwork.CancelFacts{}, extwork.ErrNotFound
	}
	select {
	case <-r.cancelAck:
	default:
	}
	p.mu.Unlock()
	if err := p.send(ctx, Message{Type: MsgCancel, ExecutionKey: string(key)}); err != nil {
		return extwork.CancelFacts{Detail: err.Error()}, err
	}
	select {
	case reply := <-r.cancelAck:
		return extwork.CancelFacts{Requested: true, Acknowledged: reply.acknowledged, Stopped: reply.stopped, Detail: reply.detail}, nil
	case <-p.gone:
		return extwork.CancelFacts{Requested: true, Detail: detailDisconnected}, ErrPeerGone
	case <-ctx.Done():
		return extwork.CancelFacts{Requested: true, Detail: "no acknowledgement before the deadline"}, fmt.Errorf("%w: %v", extwork.ErrTransport, ctx.Err())
	}
}

// OpenArtifact returns the bytes the workbench published under path for key.
func (p *Peer) OpenArtifact(key extwork.ExecutionKey, path string) (io.ReadCloser, error) {
	clean, err := extwork.CleanArtifactPath(path)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.runs[key]
	if !ok || r.remoteRunID == "" {
		return nil, extwork.ErrNotFound
	}
	raw, ok := r.artifacts[clean]
	if !ok {
		return nil, extwork.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}
