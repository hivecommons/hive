package omp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

const (
	helloTimeout       = 5 * time.Second
	listenerReadLimit  = 4 << 20
	listenerCloseGrace = 2 * time.Second
	// DefaultListen is the loopback address the listener binds when none is
	// given; port 0 picks a free one.
	DefaultListen = "127.0.0.1:0"
)

// ErrNotLoopback rejects a listen address that is not loopback: this listener
// carries no authentication of its own, so it exists only for loopback tests
// and examples. In production the hub's authenticated contributor WebSocket is
// the channel and attaches peers through Broker.Attach directly.
var ErrNotLoopback = errors.New("omp: listener binds loopback addresses only")

// Listener accepts workbench connections on a loopback WebSocket, reads the
// ext_hello frame, and attaches the peer to a Broker.
type Listener struct {
	broker   *Broker
	srv      *http.Server
	ln       net.Listener
	upgrader websocket.Upgrader
	ctx      context.Context
	cancel   context.CancelFunc
}

// Listen binds addr (loopback only) and starts accepting.
func Listen(addr string, broker *Broker) (*Listener, error) {
	if addr == "" {
		addr = DefaultListen
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("omp listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("%w: %s", ErrNotLoopback, addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &Listener{broker: broker, ln: ln, ctx: ctx, cancel: cancel}
	l.srv = &http.Server{Handler: l, ReadHeaderTimeout: helloTimeout}
	go func() { _ = l.srv.Serve(ln) }()
	return l, nil
}

// URL is the ws:// address a workbench dials.
func (l *Listener) URL() string { return "ws://" + l.ln.Addr().String() }

// Close stops accepting and drops every attached link.
func (l *Listener) Close() error {
	l.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), listenerCloseGrace)
	defer cancel()
	return l.srv.Shutdown(ctx)
}

// ServeHTTP upgrades, reads ext_hello, and attaches.
func (l *Listener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := l.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(listenerReadLimit)
	link := NewWSLink(conn)
	helloCtx, cancel := context.WithTimeout(l.ctx, helloTimeout)
	hello, err := link.Recv(helloCtx)
	cancel()
	if err != nil || hello.Type != MsgHello {
		_ = link.Send(l.ctx, Message{Type: MsgRefused, Reason: "expected " + MsgHello})
		_ = link.Close()
		return
	}
	var caps Capabilities
	if hello.Capabilities != nil {
		caps = *hello.Capabilities
	}
	if _, err := l.broker.Attach(l.ctx, hello.ContributorID, hello.Incarnation, hello.WorkbenchVersion, caps, link); err != nil {
		_ = link.Send(l.ctx, Message{Type: MsgRefused, Reason: err.Error()})
		_ = link.Close()
		return
	}
	_ = link.Send(l.ctx, Message{Type: MsgHelloOK, ContributorID: hello.ContributorID})
}
