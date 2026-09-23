package omp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

const (
	// Engine is the registry name.
	Engine = "omp"
	// Capability is the contributor-protocol token a workbench must declare to
	// be offered OMP-bound work. A peer that opts in without it is refused.
	Capability = "ext-exec/omp"
	// ReceiptArtifact is the artifact path a workbench publishes its stage
	// receipt under.
	ReceiptArtifact = "receipt.json"
)

// Message types on the relay channel. Every type carries the ext_ prefix so a
// relay implementation can route them beside the existing task_* frames
// without ambiguity. Field names reuse the contributor protocol's where the
// meaning is the same (task_id, task_gen, stage, reason, contributor_id).
const (
	// MsgHello (peer to hub) identifies the workbench on the standalone
	// listener: contributor_id, capabilities, workbench_version, incarnation.
	// On the hub's own contributor WebSocket the auth_response frame already
	// carries identity and capabilities, so the hub attaches the peer from
	// those and no ext_hello is exchanged.
	MsgHello = "ext_hello"
	// MsgHelloOK (hub to peer) acknowledges attachment.
	MsgHelloOK = "ext_hello_ok"
	// MsgRefused (hub to peer) refuses attachment; reason names why. The peer
	// is never attached as an ordinary contributor.
	MsgRefused = "ext_refused"
	// MsgOffer (hub to peer) is the assignment summary: execution_key,
	// work_key, task_id, task_gen, stage, summary. Nothing else.
	MsgOffer = "ext_offer"
	// MsgAccept / MsgDecline (peer to hub) answer an offer by execution_key.
	MsgAccept  = "ext_accept"
	MsgDecline = "ext_decline"
	// MsgStart (hub to peer) delivers the bounded context bundle (payload)
	// for an ACCEPTED execution key.
	MsgStart = "ext_start"
	// MsgStarted (peer to hub) acknowledges the start with remote_run_id and
	// deduplicated (true when the key was already running the same payload).
	MsgStarted = "ext_started"
	// MsgStartRefused (peer to hub) refuses a start; reason ReasonConflict
	// means the key is running a different payload.
	MsgStartRefused = "ext_start_refused"
	// MsgProgress (peer to hub) reports state (accepted, running, waiting,
	// terminal, unknown), stage, and detail for an execution key.
	MsgProgress = "ext_progress"
	// MsgCancel (hub to peer) requests cancellation of an execution key.
	MsgCancel = "ext_cancel"
	// MsgCancelAck (peer to hub) reports acknowledged and stopped separately.
	MsgCancelAck = "ext_cancel_ack"
	// MsgReceipt (peer to hub) publishes the stage receipt artifact for an
	// execution key and marks the run terminal.
	MsgReceipt = "ext_receipt"

	// ReasonConflict is the start-refusal reason for a changed payload under
	// an unchanged execution key.
	ReasonConflict = "conflict"
	// ReasonNotAccepted is the start-refusal reason for a key the workbench
	// never accepted.
	ReasonNotAccepted = "not_accepted"
)

// Capabilities is the workbench's declared posture on ext_hello. It mirrors
// the relay_capabilities list of the contributor protocol's auth_response.
type Capabilities struct {
	RelayCapabilities []string `json:"relay_capabilities,omitempty"`
}

// Declares reports whether token is in the declared list.
func (c *Capabilities) Declares(token string) bool {
	if c == nil {
		return false
	}
	for _, t := range c.RelayCapabilities {
		if strings.TrimSpace(t) == token {
			return true
		}
	}
	return false
}

// Artifact is one artifact carried inline on ext_receipt. Body is the exact
// bytes; Digest and Size are what the binding verifies them against.
type Artifact struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	Body   []byte `json:"body"`
}

// Message is one frame on the relay channel.
type Message struct {
	Type          string        `json:"type"`
	Seq           int           `json:"seq,omitempty"`
	ContributorID string        `json:"contributor_id,omitempty"`
	Capabilities  *Capabilities `json:"capabilities,omitempty"`
	// WorkbenchVersion is the workflow version the workbench runs; the
	// adapter refuses a peer whose version is not the pinned one.
	WorkbenchVersion string `json:"workbench_version,omitempty"`
	// Incarnation is the workbench session identity. A reconnect under a new
	// incarnation is a different instance whose runs are never adopted.
	Incarnation  string `json:"incarnation,omitempty"`
	ExecutionKey string `json:"execution_key,omitempty"`
	TaskID       string `json:"task_id,omitempty"`
	TaskGen      uint64 `json:"task_gen,omitempty"`
	WorkKey      string `json:"work_key,omitempty"`
	Stage        string `json:"stage,omitempty"`
	Summary      string `json:"summary,omitempty"`
	Reason       string `json:"reason,omitempty"`
	State        string `json:"state,omitempty"`
	Detail       string `json:"detail,omitempty"`
	RemoteRunID  string `json:"remote_run_id,omitempty"`
	Deduplicated bool   `json:"deduplicated,omitempty"`
	Acknowledged bool   `json:"acknowledged,omitempty"`
	Stopped      bool   `json:"stopped,omitempty"`
	// Payload is the bounded context bundle; present only on ext_start.
	Payload  []byte    `json:"payload,omitempty"`
	Artifact *Artifact `json:"artifact,omitempty"`
}

// Link is a bidirectional message channel to one peer. The hub backs it with
// the contributor WebSocket; tests and the fixture back it with WSLink over a
// loopback socket.
type Link interface {
	Send(ctx context.Context, msg Message) error
	Recv(ctx context.Context) (Message, error)
	Close() error
}

// ErrLinkClosed is returned once a Link is closed.
var ErrLinkClosed = errors.New("omp: link closed")

// WSLink is a Link over a gorilla WebSocket connection. Writes are serialised
// per gorilla's one-concurrent-writer contract.
type WSLink struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	once    sync.Once
	closed  chan struct{}
}

// NewWSLink wraps an established connection.
func NewWSLink(conn *websocket.Conn) *WSLink {
	return &WSLink{conn: conn, closed: make(chan struct{})}
}

// Send writes one JSON frame.
func (l *WSLink) Send(ctx context.Context, msg Message) error {
	select {
	case <-l.closed:
		return ErrLinkClosed
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	if err := l.conn.WriteJSON(msg); err != nil {
		return fmt.Errorf("%w: %v", ErrLinkClosed, err)
	}
	return nil
}

// Recv reads one JSON frame. It returns ErrLinkClosed once the connection is
// gone; ctx cancellation closes the connection to unblock the read.
func (l *WSLink) Recv(ctx context.Context) (Message, error) {
	stop := context.AfterFunc(ctx, func() { _ = l.Close() })
	defer stop()
	var msg Message
	if err := l.conn.ReadJSON(&msg); err != nil {
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		return Message{}, fmt.Errorf("%w: %v", ErrLinkClosed, err)
	}
	return msg, nil
}

// Close closes the connection; safe to call more than once.
func (l *WSLink) Close() error {
	var err error
	l.once.Do(func() {
		close(l.closed)
		err = l.conn.Close()
	})
	return err
}
