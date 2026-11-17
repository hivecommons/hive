package customers

import (
	"fmt"
	"strings"
)

// An artefact reference is an id, never content. The part before the first
// colon is its kind.
const (
	RefGitHub  = "gh"
	RefInbox   = "inbox"
	RefMeeting = "meeting"
	RefEvent   = "event"
	RefFact    = "fact"
	RefBead    = "bead"
	RefAudit   = "audit"
)

// IssueRef is the reference for a GitHub issue ("gh:issue:org/repo#N").
func IssueRef(repo string, number int) string {
	return fmt.Sprintf("%s:issue:%s#%d", RefGitHub, repo, number)
}

// PRRef is the reference for a GitHub pull request ("gh:pr:org/repo#N").
func PRRef(repo string, number int) string {
	return fmt.Sprintf("%s:pr:%s#%d", RefGitHub, repo, number)
}

// InboxRef is the reference for an Inbox item ("inbox:<source>:<id>").
func InboxRef(source, id string) string { return RefInbox + ":" + source + ":" + id }

// MeetingRef is the reference for a meeting ("meeting:<id>").
func MeetingRef(id string) string { return RefMeeting + ":" + id }

// EventRef is the reference for a calendar event ("event:<id>").
func EventRef(id string) string { return RefEvent + ":" + id }

// FactRef is the reference for a knowledge fact ("fact:<layer>/<slug>").
func FactRef(layer, slug string) string { return RefFact + ":" + layer + "/" + slug }

// BeadRef is the reference for a bead ("bead:<store>:<id>").
func BeadRef(store, id string) string { return RefBead + ":" + store + ":" + id }

// AuditRef is the reference for one day of the audit log
// ("audit:<yyyy-mm-dd>"). Activity edges are one per person per day.
func AuditRef(day string) string { return RefAudit + ":" + day }

// RefKind is the kind of a reference, or "" when it has none.
func RefKind(ref string) string {
	kind, _, ok := strings.Cut(ref, ":")
	if !ok {
		return ""
	}
	return kind
}

// validRef reports whether ref can be stored: it has a kind and holds no
// key delimiter or control character.
func validRef(ref string) bool {
	if RefKind(ref) == "" || len(ref) > maxRefLen {
		return false
	}
	for _, r := range ref {
		if r < ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// Viewer is who is looking. Full is the owner and read-write view; without
// it the viewer sees only their own profile. Owner adds forget and export.
type Viewer struct {
	User  string
	Full  bool
	Owner bool
}

// Resolver answers for one kind of artefact. The customers store keeps
// references only, so whether an artefact still exists and who may see it
// is always the source's answer.
type Resolver interface {
	// Exists reports whether the artefact is still held. An edge to an
	// artefact that is gone is pruned.
	Exists(ref string) bool
	// Visible reports whether v may see the artefact where it lives.
	Visible(ref string, v Viewer) bool
}

// Resolvers maps a reference kind to its resolver. A kind with no resolver
// is taken to exist and to be visible: with nobody to ask, the edge is kept
// rather than silently dropped.
type Resolvers map[string]Resolver

// Exists reports whether the artefact behind ref is still held.
func (r Resolvers) Exists(ref string) bool {
	res, ok := r[RefKind(ref)]
	if !ok || res == nil {
		return true
	}
	return res.Exists(ref)
}

// Visible reports whether v may see the artefact behind ref.
func (r Resolvers) Visible(ref string, v Viewer) bool {
	res, ok := r[RefKind(ref)]
	if !ok || res == nil {
		return true
	}
	return res.Visible(ref, v)
}
