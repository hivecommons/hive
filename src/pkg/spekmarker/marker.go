// Package spekmarker reads the hidden publish marker Spek
// (hivecommons/spektacular) writes into every RFC and child issue it files
// into a work source (spektacular#109, #114):
//
//	<!-- spek:publish name=<name> artifact_id=<id> target=<github|jira|linear> [task=<id>] [rfc=<url>] -->
//
// The marker is the hand-off contract between Spek and Hive. An item carrying
// it has already been specified, so Hive keeps it on the direct-fix path
// instead of routing it into a fresh `spec` run, skips the clarity score, and
// carries the RFC link and spek name into the kick so the agent builds against
// the spek's acceptance criteria. A marker WITHOUT `task=` names the RFC
// itself — the umbrella — which Hive treats like an `epic`: never claimed.
//
// Values are read the way pkg/upstreamwatch reads its `upstream-ref` marker:
// derived, never trusted. `rfc=` is echoed into a kick only when it parses as
// an http(s) URL; every other value is a bare token with no whitespace.
package spekmarker

import (
	"net/url"
	"strings"
)

const (
	markerPrefix = "<!-- spek:publish"
	markerSuffix = "-->"
)

// Targets Spek publishes into; every other `target=` is a malformed marker.
const (
	TargetGitHub = "github"
	TargetJira   = "jira"
	TargetLinear = "linear"
)

// Marker is one parsed `<!-- spek:publish … -->` marker.
type Marker struct {
	// Name is the spek artifact's name (`name=`).
	Name string
	// ArtifactID is the spek artifact's stable id (`artifact_id=`), the key
	// Spek re-publishes against.
	ArtifactID string
	// Target is the work source Spek published into (`target=`), lower-cased.
	Target string
	// Task is the plan task / member spec id of a child issue (`task=`);
	// empty on the RFC itself.
	Task string
	// RFC is the RFC's URL (`rfc=`), kept only when it is a well-formed
	// http(s) URL; empty otherwise.
	RFC string
}

// IsUmbrella reports whether the marker names the RFC itself rather than
// one of its children: it carries no `task=`.
func (m Marker) IsUmbrella() bool { return m.Task == "" }

// IsChild reports whether the marker names a child work item (`task=` set).
func (m Marker) IsChild() bool { return m.Task != "" }

// Parse returns the first well-formed publish marker in body. A marker
// missing `name=`, `artifact_id=` or a known `target=` is malformed and
// counts as no marker, so a body cannot half-opt into the hand-off.
func Parse(body string) (Marker, bool) {
	rest := body
	for {
		start := strings.Index(rest, markerPrefix)
		if start < 0 {
			return Marker{}, false
		}
		rest = rest[start+len(markerPrefix):]
		end := strings.Index(rest, markerSuffix)
		if end < 0 {
			return Marker{}, false
		}
		fields := rest[:end]
		rest = rest[end+len(markerSuffix):]
		// `<!-- spek:publishing -->` and the like share the prefix but are
		// not the marker: the keyword must end at whitespace.
		if fields != "" && !isSpace(fields[0]) {
			continue
		}
		if m, ok := parseFields(fields); ok {
			return m, true
		}
	}
}

// Has reports whether body carries a well-formed publish marker.
func Has(body string) bool {
	_, ok := Parse(body)
	return ok
}

// IsUmbrella reports whether body carries a publish marker with no `task=`:
// the item is the Spek-published RFC, the list of work rather than the work.
func IsUmbrella(body string) bool {
	m, ok := Parse(body)
	return ok && m.IsUmbrella()
}

func parseFields(fields string) (Marker, bool) {
	var m Marker
	for _, tok := range strings.Fields(fields) {
		key, val, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		val = strings.Trim(val, `"'`)
		if val == "" {
			continue
		}
		switch strings.ToLower(key) {
		case "name":
			m.Name = val
		case "artifact_id":
			m.ArtifactID = val
		case "target":
			m.Target = strings.ToLower(val)
		case "task":
			m.Task = val
		case "rfc":
			if u, err := url.Parse(val); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
				m.RFC = val
			}
		}
	}
	if m.Name == "" || m.ArtifactID == "" {
		return Marker{}, false
	}
	switch m.Target {
	case TargetGitHub, TargetJira, TargetLinear:
	default:
		return Marker{}, false
	}
	return m, true
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
