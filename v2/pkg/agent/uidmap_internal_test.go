package agent

import "testing"

// IsInternalUID decides whether a caller bypasses the proxy's agent-mode gate.
// A false positive hands a sandboxed agent an un-gated egress path, so the
// negative cases below matter more than the positive one.

// TestIsInternalUID_HiveProcess is the regression for the outage: the hive runs
// as root, is not in the agent map, and must be recognised as internal.
func TestIsInternalUID_HiveProcess(t *testing.T) {
	u := NewUIDMap()
	u.Agents = map[string]int{"scanner": 2007, "quality": 2006}
	if !u.IsInternalUID(0) {
		t.Error("uid 0 (the hive process) must be internal — otherwise its App-token mint is blocked")
	}
}

// TestIsInternalUID_ProxyUser: the MITM proxy's own upstream dials are internal
// too, mirroring the existing proxy_uid exemption in the iptables chain.
func TestIsInternalUID_ProxyUser(t *testing.T) {
	u := NewUIDMap()
	u.Agents = map[string]int{"scanner": 2007}
	if !u.IsInternalUID(u.ProxyUID) {
		t.Errorf("proxy uid %d must be internal", u.ProxyUID)
	}
}

// TestIsInternalUID_AgentsAreNeverInternal is the positive control. Without it,
// a function that simply returned true would satisfy both tests above while
// handing every agent an exemption.
func TestIsInternalUID_AgentsAreNeverInternal(t *testing.T) {
	u := NewUIDMap()
	u.Agents = map[string]int{
		"architect": 2001, "brainstorm": 2002, "ci-maintainer": 2003,
		"guide": 2004, "outreach": 2005, "quality": 2006,
		"scanner": 2007, "sec-check": 2008, "strategist": 2009, "supervisor": 2010,
	}
	for name, uid := range u.Agents {
		if u.IsInternalUID(uid) {
			t.Errorf("agent %q (uid %d) must NOT be internal — that would bypass the mode gate", name, uid)
		}
	}
}

// TestIsInternalUID_AgentSquattingOnInternalUID is the belt-and-braces case: if
// an agent were ever provisioned at uid 0 or at the proxy uid, it must lose the
// exemption rather than inherit it. This is what keeps the fix safe on a spoke
// where the C6 setuid hole is still open (v2), even though it is closed on v4.
func TestIsInternalUID_AgentSquattingOnInternalUID(t *testing.T) {
	u := NewUIDMap()
	u.Agents = map[string]int{"rogue": 0}
	if u.IsInternalUID(0) {
		t.Error("an agent occupying uid 0 must not be treated as internal")
	}

	u2 := NewUIDMap()
	u2.Agents = map[string]int{"rogue": u2.ProxyUID}
	if u2.IsInternalUID(u2.ProxyUID) {
		t.Error("an agent occupying the proxy uid must not be treated as internal")
	}
}

// TestIsInternalUID_UnknownUID: an unattributable UID stays non-internal, so a
// genuine UID-attribution failure still fails closed and still logs loudly.
func TestIsInternalUID_UnknownUID(t *testing.T) {
	u := NewUIDMap()
	u.Agents = map[string]int{"scanner": 2007}
	for _, uid := range []int{1, 999, 1002, 5000} {
		if u.IsInternalUID(uid) {
			t.Errorf("uid %d is neither the hive nor the proxy and must not be internal", uid)
		}
	}
}
