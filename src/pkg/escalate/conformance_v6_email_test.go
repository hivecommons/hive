package escalate

// v6 guard-invariant conformance for the EMAIL surface — outbound escalation
// and digest mail, shipped in #7613 / #7618, plus the inbound reply-to-act
// path that is designed but deliberately unshipped.
//
// src/docs/v6-readiness.md §2 states the line's single non-negotiable: every
// non-dashboard surface routes through the *same* authorization and safety
// machinery, and a surface's conformance row is checked only with a link to a
// test that FAILS if the surface bypasses any of five mechanisms:
//
//  1. ioscan enforcement on inbound reply text;
//  2. the Converse gate/capability on reply paths that affect state;
//  3. canary/secret scrubbing on outbound bodies and headers;
//  4. the dashboard role floor on actor/allowlist authorization;
//  5. the proxy mode ladder/capability check before a reply drives agent work.
//
// Email ships outbound-only today. Inbound reply-to-act is phase 2 by design
// (src/docs/design/escalation-surfaces.md, "Inbound reply-to-act (phase 2,
// explicitly later)"), so mechanisms 1, 2, 4 and 5 are conformed to by
// *unreachability* — which counts only where the unreachability is pinned.
// Email is the sharper case of that than push/on-call: push has no inbound
// design at all, while email has a written one waiting to land, and the design
// doc itself calls inbound mail "the weakest-authenticity surface Hive will
// have". So this file asserts:
//
//   - mechanism 3 behaviourally, over the whole SMTP session (envelope,
//     headers, body) on both outbound paths — the immediate escalation mail
//     and the digest, including the day-long digest buffer it is built from;
//   - mechanisms 1/2/4/5 structurally, by parsing this package and failing
//     when the pieces of an inbound path appear — a mail-reading dependency,
//     a poller/reply entry point, inbound configuration on EmailConfig, or a
//     locally-invented sender authorization. Each failure says the same
//     thing: the invariant has stopped being "unreachable", so the guards
//     have to be wired and asserted here for real.
//
// The state-reaching import check that mechanisms 2/4/5 also need is not
// repeated here: TestV6Conformance_SurfaceCannotDriveAgentWork in
// conformance_v6_test.go already scans every non-test file in this package,
// email.go included.
//
// Deleting a scrub call, or growing an IMAP poller that skips the guards,
// fails this file. Issue: hivecommons/hive#8047. Tracker: hivecommons/hive#7683.

import (
	"bufio"
	"context"
	"crypto/tls"
	"go/ast"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- mechanism 3: canary/secret scrubbing ----------------------------------

// emailCapture is a throwaway SMTP peer that records the WHOLE session — every
// command line as well as the DATA block — so a secret is caught wherever it
// landed: an envelope address, a header, or the body. ServeSMTPFake
// (email_test.go) keeps only DATA; conformance has to see the envelope too,
// because Subject and recipients are as operator-visible as the body is.
type emailCapture struct {
	ln net.Listener
	ch chan string
}

func newEmailCapture(t *testing.T) (*emailCapture, EmailConfig) {
	t.Helper()
	serverTLS, clientTLS := testTLSConfig(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	c := &emailCapture{ln: ln, ch: make(chan string, 4)}
	go c.serve()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("addr: %v", err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	return c, EmailConfig{
		Host:      "127.0.0.1",
		Port:      p,
		From:      "hive@example.com",
		To:        []string{"ops@example.com"},
		DigestTo:  []string{"team@example.com"},
		HiveName:  "conformance",
		Spoke:     "org",
		Version:   "v6",
		tlsConfig: clientTLS,
	}
}

func (c *emailCapture) serve() {
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			return
		}
		go c.handle(conn)
	}
}

func (c *emailCapture) handle(conn net.Conn) {
	defer conn.Close()
	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	write := func(s string) { _, _ = rw.WriteString(s + "\r\n"); _ = rw.Flush() }
	write("220 fake")

	var session strings.Builder
	inData := false
	for {
		line, err := rw.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		session.WriteString(line + "\n")
		if inData {
			if line == "." {
				select {
				case c.ch <- session.String():
				default:
				}
				session.Reset()
				inData = false
				write("250 ok")
			}
			continue
		}
		cmd := ""
		if fields := strings.Fields(line); len(fields) > 0 {
			cmd = strings.ToUpper(fields[0])
		}
		switch cmd {
		case "EHLO", "HELO":
			write("250-fake")
			write("250 OK")
		case "DATA":
			write("354 go")
			inData = true
		case "QUIT":
			write("221 bye")
			return
		default:
			write("250 ok")
		}
	}
}

func (c *emailCapture) session(t *testing.T) string {
	t.Helper()
	select {
	case got := <-c.ch:
		return got
	case <-time.After(10 * time.Second):
		t.Fatal("SMTP peer received no message")
		return ""
	}
}

// TestV6ConformanceEmail_EscalationMailIsScrubbed covers mechanism 3 on the
// immediate `decision`/`page` path — the one that carries a HUMAN DECISION
// NEEDED escalation to an operator's inbox.
func TestV6ConformanceEmail_EscalationMailIsScrubbed(t *testing.T) {
	rec, cfg := newEmailCapture(t)
	sink := NewEmailSink(cfg)

	if err := sink.Deliver(context.Background(), leakyEvent()); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	wire := rec.session(t)

	assertNoSecrets(t, sink.Name(), wire)

	// Guard against a scrubber so broad it eats the mail: the Subject header
	// is built from Event.Title, and the provenance footer is configuration
	// (hive name / spoke / version) that must survive intact, exactly as a
	// push sink's bearer credential does.
	if !strings.Contains(wire, "Subject: ") {
		t.Fatalf("no Subject header in the SMTP session; the scrub assertion would be vacuous:\n%s", wire)
	}
	for _, want := range []string{"name=conformance", "spoke=org", "version=v6"} {
		if !strings.Contains(wire, want) {
			t.Errorf("escalation mail lost %q; scrubbing must cover the Event, not the sink configuration", want)
		}
	}
}

// TestV6ConformanceEmail_DigestIsScrubbedInBufferAndOnTheWire covers mechanism
// 3 on the second outbound path, and on the buffer behind it.
//
// The digest is the one place this surface *retains* escalation text: `info`
// and `decision` events sit in EmailSink.dig until the configured send time,
// up to a day later. Scrubbing only on the way out would leave secrets live in
// process memory for that whole window and would put them on the wire for any
// future reader of that buffer, so the buffer is asserted directly.
func TestV6ConformanceEmail_DigestIsScrubbedInBufferAndOnTheWire(t *testing.T) {
	ctx := context.Background()
	rec, cfg := newEmailCapture(t)
	sink := NewEmailSink(cfg)

	ev := leakyEvent()
	ev.Severity = SeverityInfo
	if err := sink.Deliver(ctx, ev); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	sink.mu.Lock()
	buffered := append([]Event(nil), sink.dig...)
	sink.mu.Unlock()
	if len(buffered) != 1 {
		t.Fatalf("digest buffer holds %d events, want 1; the buffer assertion would be vacuous", len(buffered))
	}
	assertNoSecrets(t, "email digest buffer", buffered[0].Title+"\n"+buffered[0].Body+"\n"+buffered[0].Link)

	if err := sink.SendDigest(ctx); err != nil {
		t.Fatalf("send digest: %v", err)
	}
	wire := rec.session(t)
	if !strings.Contains(wire, "Hive escalation digest") {
		t.Fatalf("captured session is not the digest mail:\n%s", wire)
	}
	assertNoSecrets(t, "email digest", wire)
}

// --- structural invariants: mechanisms 1, 2, 4 and 5 -----------------------

// mailReadingImports are dependency shapes that only a surface which *reads*
// mail needs. An outbound SMTP sink needs none of them, so one appearing is
// the earliest visible sign that inbound reply-to-act has started to land.
// Matched as substrings so a new IMAP or message-parsing library is caught
// without this list being updated first.
var mailReadingImports = []string{"imap", "go-message", "go-sasl", "net/mail", "mailbox"}

// emailInboundMarkers is the vocabulary an inbound mail path is written in.
// The push surface's inbound signals (an http.ResponseWriter, a "webhook"
// name) do not fire for email: an IMAP poller dials *out* and is named for
// polling, fetching, or replying, which is why this surface needs its own
// list rather than reusing inboundReason alone.
var emailInboundMarkers = []string{"imap", "inbound", "mailbox", "inbox", "poll", "fetch", "reply", "consume", "dkim"}

// surfaceImports reports whether any non-test file in this package imports
// path.
func surfaceImports(files map[string]*ast.File, path string) bool {
	for _, file := range files {
		for _, spec := range file.Imports {
			if strings.Trim(spec.Path.Value, `"`) == path {
				return true
			}
		}
	}
	return false
}

// TestV6ConformanceEmail_NoUnscannedInboundMailPath covers mechanism 1.
//
// Nothing inbound exists here yet: no IMAP poller, no reply parser, no verb
// dispatch. This test fails the moment one appears without pkg/ioscan
// alongside it, so "we have no inbound path" cannot quietly become "we have an
// unscanned inbound path" — the failure mode the design doc calls out when it
// says inbound mail ships last and defaults off.
func TestV6ConformanceEmail_NoUnscannedInboundMailPath(t *testing.T) {
	files := parseSurface(t)
	scansInbound := surfaceImports(files, ioscanImport)
	if scansInbound {
		return
	}

	const remedy = "Inbound reply text must pass ioscan.EnforceInput before it can drive anything, and this " +
		"test must be extended to assert that — along with the Converse gate, the dashboard role floor, and " +
		"the proxy mode ladder — on the new path (src/docs/v6-readiness.md §2, hivecommons/hive#8047)"

	for path, file := range files {
		for _, spec := range file.Imports {
			imported := strings.Trim(spec.Path.Value, `"`)
			for _, marker := range mailReadingImports {
				if strings.Contains(imported, marker) {
					t.Errorf("v6 conformance (ioscan enforcement): %s imports %q, which only a surface that "+
						"reads mail needs, but the package does not import %s. %s",
						path, imported, ioscanImport, remedy)
				}
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			if reason := inboundReason(fn); reason != "" {
				t.Errorf("v6 conformance (ioscan enforcement): %s declares %s, which %s, but the package does "+
					"not import %s. %s", path, fn.Name.Name, reason, ioscanImport, remedy)
				return true
			}
			lower := strings.ToLower(fn.Name.Name)
			for _, marker := range emailInboundMarkers {
				if strings.Contains(lower, marker) {
					t.Errorf("v6 conformance (ioscan enforcement): %s declares %s, which is named like an "+
						"inbound mail path (%s), but the package does not import %s. %s",
						path, fn.Name.Name, marker, ioscanImport, remedy)
					return true
				}
			}
			return true
		})
	}
}

// TestV6ConformanceEmail_InboundConfigCannotShipWithoutGuards covers
// mechanisms 1, 2, 4 and 5 at the config surface.
//
// Reply-to-act is configuration-first: per the design doc it arrives as a
// mailbox to poll, a sender allowlist, and a DKIM requirement, and it is
// disabled when the allowlist is empty. So the config struct is where the
// feature becomes visible earliest — before any handler is written. A field
// naming any of that, in a package that cannot even scan the text it would
// admit, means the guards have not been wired.
func TestV6ConformanceEmail_InboundConfigCannotShipWithoutGuards(t *testing.T) {
	files := parseSurface(t)
	scansInbound := surfaceImports(files, ioscanImport)

	found := false
	for path, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok || spec.Name.Name != "EmailConfig" {
				return true
			}
			structType, ok := spec.Type.(*ast.StructType)
			if !ok || structType.Fields == nil {
				return true
			}
			found = true
			if scansInbound {
				return true
			}
			for _, field := range structType.Fields.List {
				for _, name := range field.Names {
					lower := strings.ToLower(name.Name)
					for _, marker := range emailInboundMarkers {
						if !strings.Contains(lower, marker) {
							continue
						}
						t.Errorf("v6 conformance (ioscan / Converse / role floor / mode ladder): %s gives "+
							"EmailConfig an inbound field %s (%s) while the package does not import %s. "+
							"Inbound mail is the weakest-authenticity surface this project has "+
							"(src/docs/design/escalation-surfaces.md): before it configures, its replies must "+
							"pass ioscan.EnforceInput, require the Converse capability, authorize the sender "+
							"against the dashboard role floor rather than an allowlist invented here, and "+
							"clear the proxy mode ladder before driving agent work — each asserted in this "+
							"file (src/docs/v6-readiness.md §2, hivecommons/hive#8047)",
							path, name.Name, marker, ioscanImport)
					}
				}
			}
			return true
		})
	}
	if !found {
		t.Fatal("EmailConfig not found in the package sources; this conformance check would pass vacuously")
	}
}

// localAuthorizationMarkers name the decision this surface must never make for
// itself. §2 is explicit that no surface grows its own authz; for email the
// tempting shape is a private sender allowlist, because the design doc lists
// one among the inbound guards. An allowlist here is only conformant if it is
// *derived from* the dashboard role floor rather than invented beside it.
var localAuthorizationMarkers = []string{"authoriz", "allowlist", "allowed", "permit", "authenticat", "rolefloor", "capabilit"}

// TestV6ConformanceEmail_NoLocalSenderAuthorization covers mechanisms 2, 4
// and 5 from the other direction than the import scan in
// conformance_v6_test.go does.
//
// That test fails when the surface *reaches* pkg/dashboard, pkg/proxy or
// pkg/agent — the way a bypass gets built by calling past the guards. This one
// fails when the surface *reimplements* them locally, which is how a bypass
// gets built without importing anything at all: an `isAllowedSender` that
// compares addresses is an authorization decision, and a decision made here is
// one the role floor, the Converse gate and the mode ladder never saw.
func TestV6ConformanceEmail_NoLocalSenderAuthorization(t *testing.T) {
	for path, file := range parseSurface(t) {
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			lower := strings.ToLower(fn.Name.Name)
			for _, marker := range localAuthorizationMarkers {
				if !strings.Contains(lower, marker) {
					continue
				}
				t.Errorf("v6 conformance (Converse / role floor / mode ladder): %s declares %s, which decides "+
					"authorization (%s) inside the escalation surface. No surface grows its own authz: the "+
					"actor behind an email reply is authorized by the dashboard role floor, the reply itself "+
					"needs the Converse capability, and anything it drives clears the proxy mode ladder. "+
					"Derive the decision from those, and replace this unreachability check with positive "+
					"assertions that each of them gates the path "+
					"(src/docs/v6-readiness.md §2, hivecommons/hive#8047)", path, fn.Name.Name, marker)
				return true
			}
			return true
		})
	}
}
