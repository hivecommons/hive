// Package spekmarker parses the hidden provenance marker Spek
// (hivecommons/spektacular) writes into every RFC and child issue it
// publishes into a work source:
//
//	<!-- spek:publish name=<name> artifact_id=<id> target=<t> [task=<id>] [rfc=<url>] -->
//
// The marker is how a published item says "this body was produced from a
// finished spec" (hivecommons/spektacular#114). Crux reads it on the proxy's
// issue-create path so a Spek child filed from inside a hive is treated as
// already specified, and the hive's triage pass reads the same marker to keep
// the child on the direct-fix path instead of re-routing it into a new spec
// run. It is a stdlib-only Crux ring leaf; hive-side packages may re-export or
// copy it, Crux never imports them.
package spekmarker

import (
	"regexp"
	"strings"
)

// Prefix is the marker's opening token.
const Prefix = "<!-- spek:publish "

var (
	markerRE = regexp.MustCompile(`<!--\s*spek:publish\s+([^>]*?)\s*-->`)
	// keyRE is the shape of a marker key: a lowercase identifier.
	keyRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// Marker is one parsed `<!-- spek:publish … -->` comment.
type Marker struct {
	// Name is the spek artifact name (`name=`). Required.
	Name string
	// ArtifactID is the spek's stable artifact id (`artifact_id=`). Required;
	// it is what makes re-publish idempotent.
	ArtifactID string
	// Target is the work source the item was published to (`target=`):
	// github, jira or linear. Required.
	Target string
	// Task is the plan task / member spec id a child carries (`task=`). Empty
	// on the RFC itself.
	Task string
	// RFC is the URL of the parent RFC a child links back to (`rfc=`).
	RFC string
}

// Valid reports whether the marker carries every required field.
func (m Marker) Valid() bool {
	return m.Name != "" && m.ArtifactID != "" && m.Target != ""
}

// IsChild reports whether the marker belongs to a child issue (carries a
// task id) rather than the RFC.
func (m Marker) IsChild() bool { return m.Task != "" }

// Parse returns the first valid spek:publish marker in text. ok is false
// when text carries no marker, or only markers missing a required field —
// a malformed marker is not evidence of a spec, so callers fall back to
// their default treatment of the body.
func Parse(text string) (Marker, bool) {
	for _, match := range markerRE.FindAllStringSubmatch(text, -1) {
		if len(match) != 2 {
			continue
		}
		m, ok := parseFields(match[1])
		if ok && m.Valid() {
			return m, true
		}
	}
	return Marker{}, false
}

// Has reports whether text carries a valid spek:publish marker.
func Has(text string) bool {
	_, ok := Parse(text)
	return ok
}

// parseFields decodes the `key=value key=value` run inside a marker. Values
// run to the next whitespace; a value wrapped in double quotes may contain
// spaces. An unknown key is ignored so a newer Spek can add fields without
// an older Crux refusing the marker; a key with an empty value or a value
// that still looks like a template token (`<name>`) fails the parse.
func parseFields(s string) (Marker, bool) {
	var m Marker
	rest := strings.TrimSpace(s)
	for rest != "" {
		eq := strings.IndexByte(rest, '=')
		if eq <= 0 {
			return Marker{}, false
		}
		key := rest[:eq]
		if !keyRE.MatchString(key) {
			return Marker{}, false
		}
		rest = rest[eq+1:]
		var val string
		if strings.HasPrefix(rest, `"`) {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				return Marker{}, false
			}
			val = rest[1 : end+1]
			rest = rest[end+2:]
		} else {
			end := strings.IndexAny(rest, " \t\r\n")
			if end < 0 {
				val, rest = rest, ""
			} else {
				val, rest = rest[:end], rest[end:]
			}
		}
		rest = strings.TrimSpace(rest)
		if val == "" || (strings.HasPrefix(val, "<") && strings.HasSuffix(val, ">")) {
			return Marker{}, false
		}
		switch key {
		case "name":
			m.Name = val
		case "artifact_id":
			m.ArtifactID = val
		case "target":
			m.Target = strings.ToLower(val)
		case "task":
			m.Task = val
		case "rfc":
			m.RFC = val
		}
	}
	return m, true
}
