package proxy

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/crux/agent"
)

// unidentifiedWriterKeyword is the fleetreport.ClassProxyWriterUnidentified
// log-fed keyword. The proxy cannot import pkg/fleetreport (import cycle), so
// the message prefix is pinned here and the class mapping is pinned on the
// fleetreport side (TestRecurringRuntimeMessagesAreClassified); drifting either breaks one.
const unidentifiedWriterKeyword = "proxy: agent could not be identified"

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// An unidentified writer stays BLOCKED (fail closed) and the WARN carries the
// reason, so a shared-UID hive reads as an operator signal (#18798).
func TestProxyHTTP_UnidentifiedWriterBlockedAndReasoned(t *testing.T) {
	cases := []struct {
		name   string
		uidMap *agent.UIDMap
		reason string
	}{
		{"shared uid", &agent.UIDMap{Agents: map[string]int{}}, unidentifiedSharedUID},
		{"no uid map", nil, unidentifiedNoUIDMap},
		{"unusable peer address", &agent.UIDMap{Agents: map[string]int{"scanner": agentUIDFixture}}, unidentifiedNoPeerPort},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestProxy()
			logs := &lockedBuffer{}
			p.logger = slog.New(slog.NewTextHandler(logs, nil))
			p.uidMap = tc.uidMap

			clientConn, proxyClient := net.Pipe()
			upstreamConn, proxyUpstream := net.Pipe()
			defer clientConn.Close()
			defer upstreamConn.Close()

			go p.proxyHTTP(proxyClient, proxyUpstream, "", agent.ModeAdvisory, agent.AgentCapabilities{})
			forwarded := make(chan struct{}, 1)
			go func() {
				buf := make([]byte, 4096)
				if n, _ := upstreamConn.Read(buf); n > 0 {
					forwarded <- struct{}{}
				}
			}()
			go func() {
				fmt.Fprintf(clientConn, "POST /repos/org/repo/issues HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: 0\r\n\r\n")
			}()

			raw := readRefusal(t, clientConn)
			if !strings.Contains(raw, "403") {
				t.Fatalf("an unidentified writer must be blocked, got:\n%s", raw)
			}
			select {
			case <-forwarded:
				t.Fatal("an unidentified write reached upstream")
			default:
			}
			out := logs.String()
			if !strings.Contains(out, unidentifiedWriterKeyword) {
				t.Errorf("WARN lost the fleet-report keyword %q:\n%s", unidentifiedWriterKeyword, out)
			}
			if !strings.Contains(out, "reason="+tc.reason) {
				t.Errorf("WARN missing reason=%s:\n%s", tc.reason, out)
			}
		})
	}
}

func TestUnidentifiedReason(t *testing.T) {
	withAgents := func() *agent.UIDMap {
		return &agent.UIDMap{Agents: map[string]int{"scanner": agentUIDFixture}}
	}
	t.Run("no map", func(t *testing.T) {
		p := &GitHubProxy{}
		if r, _ := p.unidentifiedReason("127.0.0.1:40000"); r != unidentifiedNoUIDMap {
			t.Errorf("got %q", r)
		}
	})
	t.Run("shared uid", func(t *testing.T) {
		p := &GitHubProxy{uidMap: &agent.UIDMap{Agents: map[string]int{}}}
		if r, _ := p.unidentifiedReason("127.0.0.1:40000"); r != unidentifiedSharedUID {
			t.Errorf("got %q", r)
		}
	})
	t.Run("no peer port", func(t *testing.T) {
		p := &GitHubProxy{uidMap: withAgents()}
		if r, _ := p.unidentifiedReason("pipe"); r != unidentifiedNoPeerPort {
			t.Errorf("got %q", r)
		}
	})
	t.Run("port not in socket table", func(t *testing.T) {
		writeProcNetFixture(t, 40000, 8080)
		p := &GitHubProxy{uidMap: withAgents()}
		if r, uid := p.unidentifiedReason("127.0.0.1:41111"); r != unidentifiedPortNotInTable || uid != -1 {
			t.Errorf("got %q uid %d", r, uid)
		}
	})
	t.Run("uid not in map", func(t *testing.T) {
		writeProcNetFixture(t, 40000, 8080)
		p := &GitHubProxy{uidMap: &agent.UIDMap{Agents: map[string]int{"scanner": agentUIDFixture + 1}}}
		if r, uid := p.unidentifiedReason("127.0.0.1:40000"); r != unidentifiedUIDNotInMap || uid != agentUIDFixture {
			t.Errorf("got %q uid %d", r, uid)
		}
	})
}
