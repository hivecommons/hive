package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/agent"
)

// Table tests for the three relays an agent request crosses on its way to an
// origin (#18993): the plain-HTTP forward (handleConn -> forwardPlainDirect),
// the MITM relay (proxyHTTPHost) and the sandbox read relay. Each is driven
// with raw bytes, exactly as a hostile client would write them, and the origin
// side records what it received WITHOUT net/http's normalisation, so a
// duplicated framing header or a folded line would be visible here even though
// http.ReadRequest would have quietly merged or dropped it.

// rawOriginRequest is one request as the origin received it: the request line
// and the header lines byte-for-byte as written on the wire.
type rawOriginRequest struct {
	line    string
	headers []string
}

// rawOrigin is a fake origin that parses requests by hand.
type rawOrigin struct {
	mu   sync.Mutex
	reqs []rawOriginRequest
}

func (o *rawOrigin) seen() []rawOriginRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]rawOriginRequest(nil), o.reqs...)
}

// serve reads requests off conn, records each before draining its body, and
// answers each with an empty 200 until the connection ends.
func (o *rawOrigin) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		req := rawOriginRequest{line: strings.TrimRight(line, "\r\n")}
		for {
			h, err := br.ReadString('\n')
			if err != nil {
				return
			}
			h = strings.TrimRight(h, "\r\n")
			if h == "" {
				break
			}
			req.headers = append(req.headers, h)
		}
		o.mu.Lock()
		o.reqs = append(o.reqs, req)
		o.mu.Unlock()
		if err := drainRawOriginBody(br, req.headers); err != nil {
			return
		}
		if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"); err != nil {
			return
		}
	}
}

// drainRawOriginBody consumes the body the relay framed, so the next request
// line is read from the right offset.
func drainRawOriginBody(br *bufio.Reader, headers []string) error {
	chunked, length := false, int64(0)
	for _, h := range headers {
		name, value, _ := strings.Cut(h, ":")
		value = strings.TrimSpace(value)
		switch http.CanonicalHeaderKey(strings.TrimSpace(name)) {
		case "Transfer-Encoding":
			chunked = chunked || strings.Contains(strings.ToLower(value), "chunked")
		case "Content-Length":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return err
			}
			length = n
		}
	}
	if chunked {
		if _, err := io.Copy(io.Discard, httputil.NewChunkedReader(br)); err != nil {
			return err
		}
		// The chunked reader stops after the zero-length chunk; the trailer
		// section (normally just the empty line) is still on the wire.
		for {
			l, err := br.ReadString('\n')
			if err != nil {
				return err
			}
			if strings.TrimRight(l, "\r\n") == "" {
				return nil
			}
		}
	}
	_, err := io.CopyN(io.Discard, br, length)
	return err
}

// sendRaw writes raw into the relay as the agent, half-closes so the relay
// sees EOF after the last request, and reads until the relay hangs up.
func sendRaw(t *testing.T, conn net.Conn, raw string) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	// A relay that refuses the framing may hang up before reading it all; a
	// write error then is the refusal, not a test failure.
	_, _ = io.WriteString(conn, raw)
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	_, _ = io.Copy(io.Discard, conn)
}

// agentRelay is one relay under test. send builds the raw agent bytes with
// build (given the request-target spelling and Host the relay expects) and
// returns what the origin received once the relay is done with the client.
type agentRelay struct {
	name string
	path string
	send func(t *testing.T, build func(target func(path string) string, host string) string) []rawOriginRequest
}

func agentRelays() []agentRelay {
	return []agentRelay{
		{
			name: "forwardPlainDirect",
			path: "/ok",
			send: func(t *testing.T, build func(func(string) string, string) string) []rawOriginRequest {
				origin := &rawOrigin{}
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("listen: %v", err)
				}
				var connsMu sync.Mutex
				var conns []net.Conn
				t.Cleanup(func() {
					_ = ln.Close()
					connsMu.Lock()
					defer connsMu.Unlock()
					for _, c := range conns {
						_ = c.Close()
					}
				})
				go func() {
					for {
						c, err := ln.Accept()
						if err != nil {
							return
						}
						connsMu.Lock()
						conns = append(conns, c)
						connsMu.Unlock()
						go origin.serve(c)
					}
				}()
				addr := ln.Addr().String()

				p := newTestProxy()
				p.allowPrivateForward = true // the origin under test is on loopback
				client, server := tcpPair(t)
				done := make(chan struct{})
				go func() {
					defer close(done)
					p.handleConn(server)
				}()
				sendRaw(t, client, build(func(path string) string { return "http://" + addr + path }, addr))
				<-done
				return origin.seen()
			},
		},
		{
			name: "proxyHTTPHost",
			path: "/repos/org/repo",
			send: func(t *testing.T, build func(func(string) string, string) string) []rawOriginRequest {
				origin := &rawOrigin{}
				upClient, upServer := net.Pipe()
				originDone := make(chan struct{})
				go func() {
					defer close(originDone)
					origin.serve(upServer)
				}()

				p := newTestProxy()
				client, server := tcpPair(t)
				done := make(chan struct{})
				go func() {
					defer close(done)
					p.proxyHTTPHost(server, upClient, "", "scanner", agent.ModeAdvisory, agent.AgentCapabilities{})
					_ = server.Close()
				}()
				sendRaw(t, client, build(func(path string) string { return path }, "api.github.com"))
				<-done
				_ = upClient.Close()
				<-originDone
				return origin.seen()
			},
		},
		{
			name: "sandboxReadRelay",
			path: "/repos/o/r/tags",
			send: func(t *testing.T, build func(func(string) string, string) string) []rawOriginRequest {
				origin := &rawOrigin{}
				h := newRelayHarness(t, func() (string, error) { return testReadToken, nil })
				var wg sync.WaitGroup
				h.relay.dial = func(string) (net.Conn, error) {
					c, s := net.Pipe()
					wg.Add(1)
					go func() {
						defer wg.Done()
						origin.serve(s)
					}()
					return c, nil
				}
				user, release := enrollKey(t, h.relay, "deps")
				defer release()
				tc, status := h.connect(t, user, "api.github.com:443")
				if status != http.StatusOK {
					t.Fatalf("CONNECT status = %d, want 200", status)
				}
				sendRaw(t, tc, build(func(path string) string { return path }, "api.github.com"))
				wg.Wait()
				return origin.seen()
			},
		},
	}
}

// agentHopIdentityHeaderLines are the hop and hive identity assertions an
// agent can put on a request, in the spellings a hand-rolled client uses.
var agentHopIdentityHeaderLines = []string{
	"Proxy-Authorization: Basic c2Nhbm5lcjpodW50ZXIy",
	"Proxy-Connection: keep-alive",
	"X-Forwarded-For: 10.0.0.1",
	"X-Forwarded-Host: dashboard.hive.svc",
	"X-Forwarded-Proto: https",
	"x-forwarded-user: owner",
	"Forwarded: for=10.0.0.1;proto=https",
	"X-Hive-User: owner",
	"X-Hive-Role: owner",
	"x-hive-agent: quality",
}

func TestAgentRelaysStripHopIdentityHeaders(t *testing.T) {
	for _, relay := range agentRelays() {
		t.Run(relay.name, func(t *testing.T) {
			seen := relay.send(t, func(target func(string) string, host string) string {
				return "GET " + target(relay.path) + " HTTP/1.1\r\nHost: " + host + "\r\n" +
					strings.Join(agentHopIdentityHeaderLines, "\r\n") + "\r\n" +
					"X-Keep: me\r\n\r\n"
			})
			if len(seen) != 1 {
				t.Fatalf("origin saw %d requests, want 1", len(seen))
			}
			stripped := map[string]bool{}
			for _, l := range agentHopIdentityHeaderLines {
				name, _, _ := strings.Cut(l, ":")
				stripped[http.CanonicalHeaderKey(name)] = true
			}
			kept := false
			for _, l := range seen[0].headers {
				name, value, _ := strings.Cut(l, ":")
				canon := http.CanonicalHeaderKey(strings.TrimSpace(name))
				if stripped[canon] {
					t.Errorf("origin received %q; agent hop and hive identity headers must be stripped", l)
				}
				if canon == "X-Keep" && strings.TrimSpace(value) == "me" {
					kept = true
				}
			}
			if !kept {
				t.Errorf("origin headers %q lack X-Keep: me; the request's own headers must be forwarded", seen[0].headers)
			}
		})
	}
}

// smuggledSuffix is a complete second request hidden where one reading of the
// framing ends the first request and another does not.
func smuggledSuffix(target func(string) string, host string) string {
	return "GET " + target("/smuggled") + " HTTP/1.1\r\nHost: " + host + "\r\n\r\n"
}

func TestAgentRelaysRejectOrNormaliseSmugglingFraming(t *testing.T) {
	cases := []struct {
		name string
		// framing is the header lines (each ending in CRLF) and body placed
		// after the Host line of the first request.
		framing func(target func(string) string, host string) string
	}{
		{"CL.TE", func(target func(string) string, host string) string {
			return "Content-Length: 4\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n" + smuggledSuffix(target, host)
		}},
		{"TE.CL", func(target func(string) string, host string) string {
			s := smuggledSuffix(target, host)
			return "Transfer-Encoding: chunked\r\nContent-Length: " + strconv.Itoa(5+len(s)) + "\r\n\r\n0\r\n\r\n" + s
		}},
		{"conflicting Content-Length", func(target func(string) string, host string) string {
			s := smuggledSuffix(target, host)
			return "Content-Length: 0\r\nContent-Length: " + strconv.Itoa(len(s)) + "\r\n\r\n" + s
		}},
		{"signed Content-Length", func(target func(string) string, host string) string {
			s := smuggledSuffix(target, host)
			return "Content-Length: +" + strconv.Itoa(len(s)) + "\r\n\r\n" + s
		}},
		{"unknown Transfer-Encoding", func(target func(string) string, host string) string {
			return "Transfer-Encoding: xchunked\r\nContent-Length: 5\r\n\r\n0\r\n\r\n" + smuggledSuffix(target, host)
		}},
		{"duplicated Transfer-Encoding", func(target func(string) string, host string) string {
			return "Transfer-Encoding: chunked\r\nTransfer-Encoding: identity\r\n\r\n0\r\n\r\n" + smuggledSuffix(target, host)
		}},
		{"Transfer-Encoding list", func(target func(string) string, host string) string {
			return "Transfer-Encoding: chunked, identity\r\n\r\n0\r\n\r\n" + smuggledSuffix(target, host)
		}},
		{"whitespace before colon", func(target func(string) string, host string) string {
			return "Transfer-Encoding : chunked\r\nContent-Length: 5\r\n\r\n0\r\n\r\n" + smuggledSuffix(target, host)
		}},
		{"obs-fold into Transfer-Encoding", func(target func(string) string, host string) string {
			return "X-Keep: a\r\n Transfer-Encoding: chunked\r\nContent-Length: 5\r\n\r\n0\r\n\r\n" + smuggledSuffix(target, host)
		}},
	}
	for _, relay := range agentRelays() {
		for _, tc := range cases {
			t.Run(relay.name+"/"+tc.name, func(t *testing.T) {
				seen := relay.send(t, func(target func(string) string, host string) string {
					return "GET " + target(relay.path) + " HTTP/1.1\r\nHost: " + host + "\r\n" + tc.framing(target, host)
				})
				// The relay may refuse the request outright (nothing reaches
				// the origin) or accept one reading of it. What it must never
				// do is pass the ambiguity on: every request the origin sees
				// is one the relay parsed itself, framed one way only.
				allowedLines := map[string]bool{
					"GET " + relay.path + " HTTP/1.1": true,
					"GET /smuggled HTTP/1.1":          true,
				}
				for _, req := range seen {
					if !allowedLines[req.line] {
						t.Errorf("origin received request line %q, which the relay never parsed as a request", req.line)
					}
					assertUnambiguousFraming(t, req)
				}
			})
		}
	}
}

func assertUnambiguousFraming(t *testing.T, req rawOriginRequest) {
	t.Helper()
	var contentLength, transferEncoding int
	for _, h := range req.headers {
		if h[0] == ' ' || h[0] == '\t' {
			t.Errorf("%s: origin received an obs-fold continuation line %q", req.line, h)
			continue
		}
		name, _, ok := strings.Cut(h, ":")
		if !ok || name == "" || name != strings.TrimSpace(name) {
			t.Errorf("%s: origin received a malformed header line %q", req.line, h)
			continue
		}
		switch http.CanonicalHeaderKey(name) {
		case "Content-Length":
			contentLength++
		case "Transfer-Encoding":
			transferEncoding++
		}
	}
	if contentLength > 1 || transferEncoding > 1 || (contentLength > 0 && transferEncoding > 0) {
		t.Errorf("%s: origin received %d Content-Length and %d Transfer-Encoding headers; a forwarded request must be framed exactly one way (headers %q)",
			req.line, contentLength, transferEncoding, req.headers)
	}
}
