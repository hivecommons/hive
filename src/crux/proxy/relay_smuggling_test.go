package proxy

// Request-smuggling and request-target table tests for the agent relays
// (#19223, egress audit section 4): the inspected MITM loop (proxyHTTPHost),
// the plain-HTTP forward (forwardPlainDirect) and the sandbox read relay.
//
// The framing cases do not assert which of "refuse" or "forward" a relay picks
// where net/http is free to do either. They assert the property that matters:
// whatever reaches the upstream carries exactly one unambiguous framing (no
// CL+TE pair, at most one Content-Length, no folded line, no bare LF), so the
// relay's gates and the upstream agree on where each request ends, and a
// request smuggled behind an allowed one is gated, never forwarded as-is.
// Where net/http's behaviour is fixed (RFC 9112 rejections), the case pins it.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/agent"
)

// smuggledDelete is the request hidden behind an allowed one. An ADVISORY
// agent may not DELETE, so it must never reach the upstream.
const smuggledDelete = "DELETE /repos/org/repo HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: 0\r\n\r\n"

// Forward counts for the table: anyForward means net/http may legitimately
// either refuse or forward, and only the framing property is asserted.
const anyForward = -1

type smuggleCase struct {
	name string
	raw  string
	// forwarded is how many requests the upstream must see, or anyForward.
	forwarded int
}

// framingCases are the smuggling inputs shared by the relay tables. Each is a
// GET (allowed at ADVISORY) followed, where it makes sense, by smuggledDelete.
func framingCases() []smuggleCase {
	const head = "GET /repos/org/repo HTTP/1.1\r\nHost: api.github.com\r\n"
	chunkedSmuggle := fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(smuggledDelete), smuggledDelete)
	return []smuggleCase{
		{"baseline", head + "\r\n", 1},
		// CL.TE: an upstream honouring CL would read the terminating chunk as
		// body and the DELETE as a new request; the relay must use TE and gate
		// the DELETE itself.
		{"cl then te", head + "Content-Length: 4\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n" + smuggledDelete, 1},
		// TE.CL: the DELETE rides inside a chunk; an upstream honouring CL=3
		// would read it as a new request, so CL must not be forwarded.
		{"te then cl", head + "Transfer-Encoding: chunked\r\nContent-Length: 3\r\n\r\n" + chunkedSmuggle, 1},
		{"duplicate cl same value", head + "Content-Length: 0\r\nContent-Length: 0\r\n\r\n", 1},
		{"duplicate cl differing", head + "Content-Length: 0\r\nContent-Length: 77\r\n\r\n" + smuggledDelete, 0},
		{"cl list", head + "Content-Length: 0, 77\r\n\r\n" + smuggledDelete, 0},
		{"cl signed", head + "Content-Length: +0\r\n\r\n" + smuggledDelete, 0},
		{"cl negative", head + "Content-Length: -1\r\n\r\n" + smuggledDelete, 0},
		{"cl hex", head + "Content-Length: 0x0\r\n\r\n" + smuggledDelete, 0},
		{"te unknown coding", head + "Transfer-Encoding: gzip\r\n\r\n" + smuggledDelete, 0},
		{"te identity", head + "Transfer-Encoding: identity\r\nContent-Length: 0\r\n\r\n" + smuggledDelete, 0},
		{"te list", head + "Transfer-Encoding: chunked, identity\r\n\r\n0\r\n\r\n" + smuggledDelete, 0},
		{"te duplicated header", head + "Transfer-Encoding: chunked\r\nTransfer-Encoding: identity\r\n\r\n0\r\n\r\n" + smuggledDelete, 0},
		{"te space before colon", head + "Transfer-Encoding : chunked\r\nContent-Length: 0\r\n\r\n0\r\n\r\n" + smuggledDelete, 0},
		{"te folded onto other coding", head + "Transfer-Encoding: identity\r\n chunked\r\n\r\n0\r\n\r\n" + smuggledDelete, 0},
		{"te folded value", head + "Transfer-Encoding:\r\n chunked\r\nContent-Length: 4\r\n\r\n0\r\n\r\n" + smuggledDelete, anyForward},
		{"folded end-to-end header", head + "X-Fold: a\r\n b\r\n\r\n", anyForward},
		{"leading whitespace first header", "GET /repos/org/repo HTTP/1.1\r\n Host: api.github.com\r\n\r\n" + smuggledDelete, 0},
		{"bare lf headers", "GET /repos/org/repo HTTP/1.1\nHost: api.github.com\n\n" + smuggledDelete, anyForward},
		{"bare lf chunk line", head + "Transfer-Encoding: chunked\r\n\r\n0\n\n" + smuggledDelete, anyForward},
		{"http10 te ignored", "GET /repos/org/repo HTTP/1.0\r\nHost: api.github.com\r\nTransfer-Encoding: chunked\r\nContent-Length: 0\r\n\r\n", anyForward},
	}
}

// assertCleanFraming checks one request as the upstream received it.
func assertCleanFraming(t *testing.T, raw string) {
	t.Helper()
	end := strings.Index(raw, "\r\n\r\n")
	if end < 0 {
		t.Errorf("upstream request has no CRLF header terminator (bare LF forwarded?):\n%q", raw)
		return
	}
	cl, te := 0, 0
	for i, line := range strings.Split(raw[:end], "\r\n") {
		if strings.Contains(line, "\n") || strings.Contains(line, "\r") {
			t.Errorf("upstream header line %d carries a bare LF/CR: %q", i, line)
		}
		if i > 0 && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			t.Errorf("upstream header line %d is an obs-fold continuation: %q", i, line)
		}
		name, _, _ := strings.Cut(line, ":")
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "content-length":
			cl++
		case "transfer-encoding":
			te++
		}
	}
	if cl > 1 || te > 1 || (cl > 0 && te > 0) {
		t.Errorf("upstream request framing is ambiguous (Content-Length x%d, Transfer-Encoding x%d):\n%s", cl, te, raw[:end])
	}
}

// capturedUpstream records each request a fake upstream parsed, with the raw
// bytes it consumed for that request.
type capturedUpstream struct {
	mu   sync.Mutex
	reqs []*http.Request
	raws []string
}

func (c *capturedUpstream) snapshot() ([]*http.Request, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*http.Request(nil), c.reqs...), append([]string(nil), c.raws...)
}

// serveCapturingUpstream answers every request on conn with an empty 200,
// recording what it read. The relay writes one request and waits for its
// response, so the bytes consumed between two parses are exactly one request.
func serveCapturingUpstream(conn net.Conn, c *capturedUpstream, closeAfter bool) {
	var raw bytes.Buffer
	br := bufio.NewReader(io.TeeReader(conn, &raw))
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if req.Body != nil {
			_, _ = io.Copy(io.Discard, req.Body)
			_ = req.Body.Close()
		}
		c.mu.Lock()
		c.reqs = append(c.reqs, req)
		c.raws = append(c.raws, raw.String())
		c.mu.Unlock()
		raw.Reset()
		if closeAfter {
			_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}
		if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"); err != nil {
			return
		}
	}
}

// runMITMExchange drives raw through proxyHTTPHost for host and returns what
// the client read back and what the upstream received.
func runMITMExchange(t *testing.T, host string, mode agent.AgentMode, raw string) (string, *capturedUpstream) {
	t.Helper()
	p := newTestProxy()
	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()

	proxyDone := make(chan struct{})
	go func() {
		defer close(proxyDone)
		p.proxyHTTPHost(proxyClient, proxyUpstream, host, "scanner", mode, agent.AgentCapabilities{})
		// proxyHTTPHost leaves both conns to its caller; close them so a
		// refusal unblocks the client's pending write and the fake upstream.
		_ = proxyClient.Close()
		_ = proxyUpstream.Close()
	}()

	up := &capturedUpstream{}
	upstreamDone := make(chan struct{})
	go func() {
		defer close(upstreamDone)
		serveCapturingUpstream(upstreamConn, up, false)
	}()

	go func() { _, _ = io.WriteString(clientConn, raw) }()

	// Read until the proxy hangs up or goes quiet; then hang up ourselves so
	// a proxy waiting for the next keep-alive request returns.
	var got bytes.Buffer
	buf := make([]byte, 4096)
	for {
		_ = clientConn.SetReadDeadline(time.Now().Add(time.Second))
		n, err := clientConn.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	_ = clientConn.Close()
	select {
	case <-proxyDone:
	case <-time.After(exchangeTimeout):
		t.Fatal("proxyHTTPHost did not return after the client hung up")
	}
	_ = upstreamConn.Close()
	<-upstreamDone
	return got.String(), up
}

func TestProxyHTTPHostSmugglingTable(t *testing.T) {
	for _, tc := range framingCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, up := runMITMExchange(t, "api.github.com", agent.ModeAdvisory, tc.raw)
			reqs, raws := up.snapshot()
			for i, r := range reqs {
				if r.Method != http.MethodGet {
					t.Errorf("upstream saw %s %s: a request smuggled behind an allowed one was forwarded ungated", r.Method, r.URL.Path)
				}
				assertCleanFraming(t, raws[i])
			}
			if tc.forwarded != anyForward && len(reqs) != tc.forwarded {
				t.Errorf("upstream saw %d requests, want %d", len(reqs), tc.forwarded)
			}
		})
	}
}

func TestProxyHTTPHostRequestTargetTable(t *testing.T) {
	cases := []struct {
		name, raw string
		refused   bool
	}{
		{"matching host", "GET /repos/org/repo HTTP/1.1\r\nHost: api.github.com\r\n\r\n", false},
		{"matching host with port", "GET /repos/org/repo HTTP/1.1\r\nHost: api.github.com:443\r\n\r\n", false},
		{"matching host other case and trailing dot", "GET /repos/org/repo HTTP/1.1\r\nHost: API.GitHub.com.\r\n\r\n", false},
		{"absolute form same host", "GET https://api.github.com/repos/org/repo HTTP/1.1\r\nHost: api.github.com\r\n\r\n", false},
		{"query carrying dots", "GET /repos/org/repo/issues?q=../x HTTP/1.1\r\nHost: api.github.com\r\n\r\n", false},
		{"dots inside a segment", "GET /repos/org/repo..x/issues HTTP/1.1\r\nHost: api.github.com\r\n\r\n", false},
		{"host header mismatch", "GET /repos/org/repo HTTP/1.1\r\nHost: github.com\r\n\r\n", true},
		{"host header other service", "GET /repos/org/repo HTTP/1.1\r\nHost: api.linear.app\r\n\r\n", true},
		{"absolute form other host", "GET https://evil.example/repos/org/repo HTTP/1.1\r\nHost: api.github.com\r\n\r\n", true},
		{"absolute form other host matching header", "GET http://github.com/repos/org/repo HTTP/1.1\r\nHost: github.com\r\n\r\n", true},
		{"dot dot segment", "GET /repos/org/repo/../../other/repo HTTP/1.1\r\nHost: api.github.com\r\n\r\n", true},
		{"dot segment", "GET /repos/org/./repo HTTP/1.1\r\nHost: api.github.com\r\n\r\n", true},
		{"encoded dot dot", "GET /repos/org/repo/%2e%2e/x HTTP/1.1\r\nHost: api.github.com\r\n\r\n", true},
		{"mixed encoded dot dot", "GET /repos/org/repo/.%2E/x HTTP/1.1\r\nHost: api.github.com\r\n\r\n", true},
		{"dot dot with encoded slash", "GET /repos/org/repo/..%2Fx HTTP/1.1\r\nHost: api.github.com\r\n\r\n", true},
		{"trailing dot dot", "GET /repos/org/repo/.. HTTP/1.1\r\nHost: api.github.com\r\n\r\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, up := runMITMExchange(t, "api.github.com", agent.ModeAdvisory, tc.raw+smuggledDelete)
			reqs, raws := up.snapshot()
			for i, r := range reqs {
				if r.Method != http.MethodGet {
					t.Errorf("upstream saw %s %s", r.Method, r.URL.Path)
				}
				assertCleanFraming(t, raws[i])
				if h := canonicalHost(r.Host); h != "api.github.com" && !strings.HasPrefix(h, "api.github.com:") {
					t.Errorf("upstream saw Host %q on the api.github.com session", r.Host)
				}
			}
			if tc.refused {
				if !strings.HasPrefix(got, "HTTP/1.1 400 ") {
					t.Errorf("client read %q, want a 400 refusal", got)
				}
				if len(reqs) != 0 {
					t.Errorf("a refused request target reached the upstream: %s %s", reqs[0].Method, reqs[0].URL.String())
				}
				return
			}
			if len(reqs) != 1 {
				t.Errorf("upstream saw %d requests, want the allowed GET only", len(reqs))
			}
		})
	}
}

// An empty host is the GitHub-implied proxyHTTP form: there is no CONNECT
// target to compare a Host against, but the path check still applies.
func TestRelayRequestTargetRefusalEmptyHost(t *testing.T) {
	for _, tc := range []struct {
		target, hostHeader string
		refused            bool
	}{
		{"/repos/org/repo", "api.github.com", false},
		{"/repos/org/repo", "github.com", false},
		{"/repos/org/repo/../x", "api.github.com", true},
	} {
		req, err := http.ReadRequest(bufio.NewReader(strings.NewReader("GET " + tc.target + " HTTP/1.1\r\nHost: " + tc.hostHeader + "\r\n\r\n")))
		if err != nil {
			t.Fatal(err)
		}
		if _, refused := relayRequestTargetRefusal(req, ""); refused != tc.refused {
			t.Errorf("%s (Host %s): refused = %v, want %v", tc.target, tc.hostHeader, refused, tc.refused)
		}
	}
}

func TestRelayPathHasDotSegment(t *testing.T) {
	for path, want := range map[string]bool{
		"":                                  false,
		"/":                                 false,
		"/repos/org/repo":                   false,
		"/repos/org/repo.git/info/refs":     false,
		"/org/repo.git/git-upload-pack":     false,
		"/repos/org/repo/contents/a..b":     false,
		"/repos/org/repo/contents/...":      false,
		"/repos/org/repo/contents/.github":  false,
		"/repos/org/r%2Fx":                  false,
		"/.":                                true,
		"/..":                               true,
		"/repos/../x":                       true,
		"/repos/./x":                        true,
		"/repos/%2e/x":                      true,
		"/repos/%2E%2E/x":                   true,
		"/repos/.%2e/x":                     true,
		"/repos/..%2fx":                     true,
		"/repos/x%2F..":                     true,
		"/repos/%zz/x":                      true, // undecodable: fail closed
		"/org/repo.git/git-receive-pack/..": true,
	} {
		if got := relayPathHasDotSegment(path); got != want {
			t.Errorf("relayPathHasDotSegment(%q) = %v, want %v", path, got, want)
		}
	}
}

// forwardPlainDirect receives a request handleConn already parsed. The table
// drives the same parse, then checks the forwarded bytes: a framing net/http
// refuses never reaches forwardPlainDirect, and one it accepts goes out with
// regenerated framing and nothing smuggled after it.
func TestForwardPlainDirectSmugglingTable(t *testing.T) {
	for _, tc := range framingCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ln.Close() }()
			up := &capturedUpstream{}
			go func() {
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					go func() {
						defer func() { _ = conn.Close() }()
						serveCapturingUpstream(conn, up, true)
					}()
				}
			}()

			// Rewrite the request line into the absolute form a proxy client
			// sends, keeping every framing byte as the case wrote it.
			raw := strings.Replace(tc.raw, "GET /repos/org/repo", "GET http://"+ln.Addr().String()+"/repos/org/repo", 1)
			req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
			if err != nil {
				if tc.forwarded > 0 {
					t.Fatalf("net/http refused a request the table expects forwarded: %v", err)
				}
				return // handleConn drops it before forwardPlainDirect.
			}
			if tc.forwarded == 0 {
				t.Fatalf("net/http accepted an ambiguous framing the table expects refused")
			}

			p := newTestProxy()
			p.allowPrivateForward = true // the upstream under test is a loopback listener
			resp := plainForwardRoundTrip(t, p, req)
			_, _ = io.Copy(io.Discard, resp.Body)

			reqs, raws := up.snapshot()
			// A 200 is written only after the upstream recorded the request.
			// A body net/http refuses mid-stream (a bare-LF chunk line, if the
			// reader is strict) fails the forward instead; whatever part of it
			// was sent must still be cleanly framed.
			if resp.StatusCode != http.StatusOK {
				for i := range reqs {
					assertCleanFraming(t, raws[i])
				}
				if tc.forwarded > 0 {
					t.Fatalf("status = %d, want 200", resp.StatusCode)
				}
				return
			}
			if len(reqs) != 1 {
				t.Fatalf("upstream saw %d requests, want 1", len(reqs))
			}
			if reqs[0].Method != http.MethodGet {
				t.Errorf("upstream saw %s", reqs[0].Method)
			}
			assertCleanFraming(t, raws[0])
			if strings.Contains(raws[0], "DELETE /repos/org/repo") && !strings.Contains(tc.name, "te then cl") {
				t.Errorf("smuggled request bytes reached the upstream:\n%q", raws[0])
			}
		})
	}
}

// RFC 9112 section 3.2.2: a proxy receiving absolute-form MUST ignore the Host
// header and use the request target's authority. The destination guard keys
// on the dialed address, so a Host header naming somewhere else neither moves
// the dial nor reaches the upstream.
func TestForwardPlainDirectAbsoluteFormOverridesHost(t *testing.T) {
	t.Run("upstream sees the target authority", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		up := &capturedUpstream{}
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			serveCapturingUpstream(conn, up, true)
		}()
		raw := "GET http://" + ln.Addr().String() + "/x HTTP/1.1\r\nHost: metadata.google.internal\r\n\r\n"
		req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
		if err != nil {
			t.Fatal(err)
		}
		p := newTestProxy()
		p.allowPrivateForward = true
		resp := plainForwardRoundTrip(t, p, req)
		_, _ = io.Copy(io.Discard, resp.Body)
		reqs, _ := up.snapshot()
		if len(reqs) != 1 {
			t.Fatalf("upstream saw %d requests, want 1", len(reqs))
		}
		if reqs[0].Host != ln.Addr().String() {
			t.Errorf("upstream Host = %q, want the request target's authority %q", reqs[0].Host, ln.Addr().String())
		}
	})
	t.Run("guard keys on the target, not the Host header", func(t *testing.T) {
		raw := "GET http://169.254.169.254/latest/meta-data/ HTTP/1.1\r\nHost: example.com\r\n\r\n"
		req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
		if err != nil {
			t.Fatal(err)
		}
		resp := plainForwardRoundTrip(t, newTestProxy(), req)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403 for a link-local target behind a public Host header", resp.StatusCode)
		}
	})
}

// The sandbox read relay parses with the same net/http reader. Its framing
// table asserts that nothing smuggled behind a read reaches GitHub, and its
// path table that a dot segment is refused before the policy's suffix match
// can be steered.
func TestSandboxReadRelaySmugglingTable(t *testing.T) {
	for _, tc := range framingCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRelayHarness(t, func() (string, error) { return testReadToken, nil })
			user, release := enrollKey(t, h.relay, "deps")
			defer release()
			conn, status := h.connect(t, user, "api.github.com:443")
			if status != 200 {
				t.Fatalf("CONNECT status = %d", status)
			}
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			_, _ = io.WriteString(conn, tc.raw)
			// The relay closes after any refusal; drain until it does, or until
			// a keep-alive tunnel goes quiet.
			_, _ = io.Copy(io.Discard, conn)
			for _, r := range h.seen() {
				if r.Method != http.MethodGet {
					t.Errorf("upstream saw %s %s: a request smuggled behind a read reached GitHub", r.Method, r.URL.Path)
				}
			}
			if tc.forwarded == 0 && len(h.seen()) != 0 {
				t.Errorf("upstream saw %d requests for a framing net/http refuses", len(h.seen()))
			}
		})
	}
}

func TestSandboxReadRelayRefusesDotSegments(t *testing.T) {
	h := newRelayHarness(t, func() (string, error) { return testReadToken, nil })
	user, release := enrollKey(t, h.relay, "deps")
	defer release()
	for _, tc := range []struct{ host, method, path string }{
		{"github.com", "POST", "/org/repo.git/git-receive-pack/../git-upload-pack"},
		{"github.com", "GET", "/org/repo.git/git-receive-pack/%2e%2e/info/refs"},
		{"api.github.com", "GET", "/repos/o/r/../../x"},
		{"api.github.com", "GET", "/repos/o/./r"},
	} {
		t.Run(tc.method+" "+tc.host+tc.path, func(t *testing.T) {
			conn, status := h.connect(t, user, tc.host+":443")
			if status != 200 {
				t.Fatalf("CONNECT status = %d", status)
			}
			_, _ = fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: %s\r\nContent-Length: 0\r\n\r\n", tc.method, tc.path, tc.host)
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
			if len(h.seen()) != 0 {
				t.Fatal("a dot-segment path reached GitHub")
			}
		})
	}
}
