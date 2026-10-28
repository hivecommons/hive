package proxy

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/hivecommons/hive/crux/agent"
)

// identifyAgentFromReq determines the agent name for a request. It always
// tries UID-based identification first (unforgeable, works for any client
// including native binaries that don't send Proxy-Authorization); see
// fallbackAgentName for what happens when that fails.
func (p *GitHubProxy) identifyAgentFromReq(r *http.Request) string {
	if p.uidMap != nil {
		if name := p.identifyAgentByUID(r.RemoteAddr); name != "" {
			return name
		}
	}
	return p.fallbackAgentName(r)
}

// IdentifyAgentByUID names the agent behind an http.Server-delivered request
// from the socket owner's UID ALONE. Unlike identifyAgentFromReq it never
// falls back to the caller-controlled Proxy-Authorization header, not even
// under HIVE_PROXY_ADVISORY_OK: see fallbackAgentName for why a self-asserted
// name is impersonation, and for the loopback services beside the proxy (the
// Jev decision endpoint, hivecommons/hive#8939) there is no "degraded but
// functional" mode worth that trade — an unidentified caller is refused.
// Returns "" when no UID map was loaded or the connection's UID is unmapped.
func (p *GitHubProxy) IdentifyAgentByUID(r *http.Request) string {
	if p.uidMap == nil || r == nil {
		return ""
	}
	return p.identifyAgentByUID(r.RemoteAddr)
}

// identifyAgentFromConn identifies the calling agent for a request read off a
// raw connection. It MUST be used instead of identifyAgentFromReq on any path
// where the request came from http.ReadRequest rather than http.Server:
// ReadRequest does not populate Request.RemoteAddr (only http.Server does), so
// the UID lookup silently gets an empty address, finds no agent, and the caller
// falls back to ADVISORY — downgrading every write-capable agent to read-only.
// Taking the peer address from the connection itself removes that trap.
func (p *GitHubProxy) identifyAgentFromConn(conn net.Conn, r *http.Request) string {
	if p.uidMap != nil && conn != nil {
		if name := p.identifyAgentByUID(conn.RemoteAddr().String()); name != "" {
			return name
		}
	}
	return p.fallbackAgentName(r)
}

// fallbackAgentName is reached whenever unforgeable UID-based identification
// did NOT succeed — either p.uidMap is nil (never loaded: no entrypoint root
// phase ran to write uid-map.json, e.g. local dev, a native/systemd install,
// or a genuine boot-ordering failure) or this specific connection's UID has
// no entry in a map that DID load (N7, #3841).
//
// Before this fix, both cases fell through to extractAgentName — the
// Proxy-Authorization header, which the CALLER sets. Any process that can
// reach the proxy's loopback port can send `Proxy-Authorization: hive
// <any-name>` and be identified as that name: a spoke-agent process claiming
// to be a MORE privileged agent bypasses the whole capability model this
// proxy exists to enforce, and #1861 (inject GitHub auth at the proxy so
// agents never hold tokens) would turn that spoofed identity directly into a
// minted token for the impersonated agent. That is strictly worse than
// simply refusing to identify the caller.
//
// The safe default is therefore to NOT trust the header: return "", which
// readAgentMode already treats as ModeAdvisory (the most restrictive mode —
// writes blocked, per the existing "agent could not be identified" WARN a few
// lines below in proxyHTTP). This is not new plumbing; it is the same path an
// unidentified caller already hits today, now reached deliberately instead of
// only when the header happens to be absent.
//
// The one deliberate exception is proxyAdvisoryOK (HIVE_PROXY_ADVISORY_OK=true)
// — the SAME explicit, operator-set escape hatch entrypoint.sh already uses
// for "this deployment does not enforce the full forced-egress gate" (local
// dev, a native/systemd install with no per-agent UID separation at all,
// platforms without NET_ADMIN). An operator who has already accepted a
// degraded capability model there is not newly exposed by also accepting a
// self-asserted identity within it — and without this exception, those
// deployments would lose agent identification (and therefore write access)
// entirely, which would be a functional regression, not a security fix.
func (p *GitHubProxy) fallbackAgentName(r *http.Request) string {
	if !p.proxyAdvisoryOK {
		return ""
	}
	return extractAgentName(r)
}

// identifyAgentByUID reads /proc/net/tcp to find the UID of the process
// that owns the socket connected to the proxy, then looks up the agent name.
func (p *GitHubProxy) identifyAgentByUID(remoteAddr string) string {
	uid, err := uidForRemoteAddr(remoteAddr)
	if err != nil {
		return ""
	}
	return p.uidMap.LookupByUID(uid)
}

// uidForRemoteAddr resolves the UID of the process that owns the local socket
// behind remoteAddr ("host:port" as http.Server and net.Conn report it) via
// /proc/net/tcp. It is the shared first half of every UID-attribution path.
func uidForRemoteAddr(remoteAddr string) (int, error) {
	_, portStr, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return -1, err
	}
	port := 0
	const maxPort = 65535
	for _, c := range portStr {
		if c < '0' || c > '9' {
			return -1, fmt.Errorf("remote port %q is not numeric", portStr)
		}
		port = port*10 + int(c-'0')
		if port > maxPort {
			return -1, fmt.Errorf("remote port %q out of range", portStr)
		}
	}
	return LookupUIDByLocalPort(port)
}

// extractAgentName reads the agent name from the Proxy-Authorization header.
// Supports "hive <name>" (custom) and "Basic <b64>" (standard HTTP proxy auth
// sent automatically when the proxy URL contains userinfo, e.g. http://quality@host:port).
func extractAgentName(r *http.Request) string {
	auth := r.Header.Get("Proxy-Authorization")
	if auth == "" {
		return ""
	}
	if strings.HasPrefix(auth, "hive ") {
		return strings.TrimPrefix(auth, "hive ")
	}
	if strings.HasPrefix(auth, "Basic ") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
		if err != nil {
			return ""
		}
		// Format is "username:password" — agent name is the username portion.
		user, _, _ := strings.Cut(string(decoded), ":")
		return user
	}
	return ""
}

// readAgentMode reads the mode from the hot-reloadable mode file.
// readAgentCaps loads the agent's orthogonal capability set (#4492) from
// /tmp/.hive-caps-<agent>. Every failure path returns the zero value — no
// capabilities — so an unreadable, missing or malformed file degrades toward
// the tier check alone, which is the deny direction.
func readAgentCaps(agentName string) agent.AgentCapabilities {
	if agentName == "" {
		return agent.AgentCapabilities{}
	}
	data, err := os.ReadFile(capsFilePrefix + agentName)
	if err != nil {
		return agent.AgentCapabilities{}
	}
	return agent.ParseCapabilities(strings.TrimSpace(string(data)))
}

func readAgentMode(agentName string) agent.AgentMode {
	if agentName == "" {
		return agent.ModeAdvisory
	}
	data, err := os.ReadFile(modeFilePrefix + agentName)
	if err != nil {
		return agent.ModeAdvisory
	}
	mode, ok := agent.ParseAgentMode(strings.TrimSpace(string(data)))
	if !ok {
		return agent.ModeAdvisory
	}
	return mode
}
