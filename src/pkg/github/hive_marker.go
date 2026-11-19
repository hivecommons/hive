package github

import (
	"regexp"
	"strings"
)

// Hive markers on items the hive wrote under a human login
// (hivecommons/hive#18199).
//
// On a remote repo the hive writes as the hive owner's account, so the author
// login alone cannot say "the hive opened this": the owner may also file
// things there by hand. What can is the marker every hive-authored body
// carries: the "— hive:" attribution trailer the hive's own writers append
// (AppendTrailer), or the 🐝 **Hive Agent** identity footer bin/gh-wrapper.sh
// adds to what an agent files through gh. pkg/authorkind reads the same two
// marks; this file exposes them to the remote-item poller together with the
// agent name they carry.

// identityFooterAgent captures the agent display name the gh-wrapper footer
// names, when it names one: `🐝 **Hive Agent**: \`name\“.
var identityFooterAgent = regexp.MustCompile("🐝 \\*\\*Hive Agent\\*\\*:[ \\t]*`([^`\\n]+)`")

// HasHiveIdentityFooter reports whether body ends with the gh-wrapper identity
// footer.
func HasHiveIdentityFooter(body string) bool {
	return attributionIdentityFooter.MatchString(strings.ReplaceAll(body, "\r\n", "\n"))
}

// HiveMarkerAgent reports whether body carries a hive marker (attribution
// trailer or identity footer) and the agent it names: the trailer's agent=
// first, else the footer's display name, "" when neither names one. The
// trailer's governor pseudo-agent counts as no agent: it is the hive's
// control plane, not something a kick can reach.
func HiveMarkerAgent(body string) (agent string, marked bool) {
	if meta, ok := ParseAttributionTrailer(body); ok {
		marked = true
		if a := strings.TrimSpace(meta.Agent); a != "" && a != AttributionAgentGovernor {
			agent = a
		}
	}
	if HasHiveIdentityFooter(body) {
		marked = true
		if agent == "" {
			if m := identityFooterAgent.FindStringSubmatch(body); m != nil {
				agent = strings.TrimSpace(m[1])
			}
		}
	}
	return agent, marked
}
