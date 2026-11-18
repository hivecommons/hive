// Package customers is the hive's model of a person: one profile per
// customer, the identities that belong to them, their tags, goals and
// satisfaction, and the edges that tie them to artefacts the hive already
// holds. Nothing in this package talks to the network, and nothing it stores
// leaves the hive.
//
// Design record: src/docs/design/customers.md (RFC #18742).
package customers

import (
	"errors"
	"strings"
	"time"
)

// Status is where a person stands with the hive.
type Status string

const (
	// StatusCustomer is a person with a profile.
	StatusCustomer Status = "customer"
	// StatusSuggested is a person the hive has only seen. It is a key,
	// counters and a few evidence references until someone accepts it.
	StatusSuggested Status = "suggested"
)

// Origin is how a customer came to have a profile.
type Origin string

const (
	OriginHiveUser Origin = "hive_user"
	OriginManual   Origin = "manual"
	OriginAccepted Origin = "accepted"
)

// IdentityKind names one way a person is known.
type IdentityKind string

const (
	KindHiveUser IdentityKind = "hive_user"
	KindGitHub   IdentityKind = "github"
	KindEmail    IdentityKind = "email"
	KindGitEmail IdentityKind = "git_email"
	KindDiscord  IdentityKind = "discord"
	KindSlack    IdentityKind = "slack"
	KindTeams    IdentityKind = "teams"
	KindMeet     IdentityKind = "meet"
	KindZoom     IdentityKind = "zoom"
	// KindName is a display name. It is shown on a profile and never used
	// as a key: a name does not link, merge or create an edge.
	KindName IdentityKind = "name"
)

// identityKinds is every kind a caller may store.
var identityKinds = map[IdentityKind]bool{
	KindHiveUser: true, KindGitHub: true, KindEmail: true, KindGitEmail: true,
	KindDiscord: true, KindSlack: true, KindTeams: true, KindMeet: true,
	KindZoom: true, KindName: true,
}

// Source says whether a value was set by the hive or by a person.
type Source string

const (
	SourceAuto   Source = "auto"
	SourceManual Source = "manual"
)

// Bounds on what one person may hold. They keep people.json small and stop
// a sweep or a caller growing a profile without limit.
const (
	MaxNameLen       = 120
	MaxNotesLen      = 4000
	MaxIdentityLen   = 254
	MaxIdentities    = 40
	MaxTagLen        = 40
	MaxTags          = 30
	MaxGoalLen       = 280
	MaxGoals         = 12
	MaxEvidence      = 5
	MaxHistory       = 20
	MaxSuggestionRef = 5
)

// MaxProfileFieldLen bounds one profile field (real name, organization,
// location).
const MaxProfileFieldLen = 120

// Sentinel errors. The dashboard maps them to status codes.
var (
	ErrNotFound  = errors.New("customers: not found")
	ErrInvalid   = errors.New("customers: invalid")
	ErrConflict  = errors.New("customers: conflict")
	ErrForgotten = errors.New("customers: forgotten")
)

// Identity is one way a person is known.
type Identity struct {
	Kind  IdentityKind `json:"kind"`
	Value string       `json:"value"`
	// Source names what produced the link ("access_list", "calendar_proof",
	// "manual", ...).
	Source     string  `json:"source,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Manual     bool    `json:"manual,omitempty"`
	// Claimed marks an identity its own profile's read-only user added.
	// It is shown on the profile and is not a key: it resolves nobody,
	// produces no edges and blocks nothing until someone with the full
	// view confirms it.
	Claimed bool      `json:"claimed,omitempty"`
	At      time.Time `json:"at"`
	By      string    `json:"by,omitempty"`
}

// Key is the identity's lookup key, "kind:value" over the normalised value.
func (i Identity) Key() string { return IdentityKey(i.Kind, i.Value) }

// IdentityKey is the lookup key for a kind and a raw value.
func IdentityKey(kind IdentityKind, value string) string {
	return string(kind) + ":" + NormalizeIdentity(kind, value)
}

// githubUserPrefix is the provider prefix a dashboard session name may
// carry: "github:alice" and "alice" are the same dashboard user.
const githubUserPrefix = "github:"

// NormalizeIdentity canonicalises a value for its kind. Logins and emails
// compare case-insensitively; a leading "@" on a login is dropped. A
// dashboard user is the same with or without the "github:" provider prefix,
// as the access list treats it. A name is only trimmed: it is never a key.
func NormalizeIdentity(kind IdentityKind, value string) string {
	v := strings.TrimSpace(value)
	switch kind {
	case KindName:
		return v
	case KindGitHub:
		return strings.ToLower(strings.TrimPrefix(v, "@"))
	case KindHiveUser:
		return strings.TrimPrefix(strings.ToLower(v), githubUserPrefix)
	default:
		return strings.ToLower(v)
	}
}

// Change is one entry in the history of a tag, a goal or satisfaction.
type Change struct {
	State  string    `json:"state"`
	Source Source    `json:"source"`
	Rule   string    `json:"rule,omitempty"`
	At     time.Time `json:"at"`
	By     string    `json:"by,omitempty"`
}

// Tag states.
const (
	TagOn  = "on"
	TagOff = "off"
)

// Default tag names. Any other name is a free-form tag.
const (
	TagMaintainer  = "maintainer"
	TagContributor = "contributor"
	TagEndUser     = "end-user"
	TagStakeholder = "stakeholder"
)

// DefaultTags are offered on every profile, in this order.
var DefaultTags = []string{TagMaintainer, TagContributor, TagEndUser, TagStakeholder}

// Tag is one label on a person. A tag a person set or cleared (Source
// manual) is never changed by the sweep; ResetTag hands it back.
type Tag struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Source Source `json:"source"`
	// Rule names the automatic rule that set the tag ("role:owner", ...).
	Rule    string    `json:"rule,omitempty"`
	At      time.Time `json:"at"`
	By      string    `json:"by,omitempty"`
	History []Change  `json:"history,omitempty"`
}

// Goal and satisfaction states.
const (
	StateSuggested = "suggested"
	StateConfirmed = "confirmed"
	StateRejected  = "rejected"
)

// Goal is one thing a person is trying to get done. An inferred goal is a
// suggestion until a person confirms it; a confirmed or manual goal is never
// changed by inference.
type Goal struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	State  string `json:"state"`
	Source Source `json:"source"`
	// Evidence is artefact references, never content.
	Evidence []string  `json:"evidence,omitempty"`
	At       time.Time `json:"at"`
	By       string    `json:"by,omitempty"`
	History  []Change  `json:"history,omitempty"`
}

// Level is the satisfaction ordinal. There is deliberately no numeric
// score: the evidence does not support one.
type Level string

const (
	LevelUnknown   Level = "unknown"
	LevelAtRisk    Level = "at_risk"
	LevelNeutral   Level = "neutral"
	LevelSatisfied Level = "satisfied"
)

// Levels is the ordinal in order, lowest first after unknown.
var Levels = []Level{LevelUnknown, LevelAtRisk, LevelNeutral, LevelSatisfied}

// ValidLevel reports whether l is one of the ordinal's values.
func ValidLevel(l Level) bool {
	for _, v := range Levels {
		if v == l {
			return true
		}
	}
	return false
}

// SatisfactionSuggestion is an inferred level waiting for a person.
type SatisfactionSuggestion struct {
	Level    Level     `json:"level"`
	Evidence []string  `json:"evidence,omitempty"`
	At       time.Time `json:"at"`
}

// Satisfaction is how a person is doing, with the evidence for it. Level is
// the standing value and only a person changes it: by setting it (Source
// manual) or by confirming a suggestion (Source auto). Inference only ever
// writes Suggestion.
type Satisfaction struct {
	Level      Level                   `json:"level"`
	Source     Source                  `json:"source,omitempty"`
	Evidence   []string                `json:"evidence,omitempty"`
	At         time.Time               `json:"at,omitempty"`
	By         string                  `json:"by,omitempty"`
	Suggestion *SatisfactionSuggestion `json:"suggestion,omitempty"`
	// Rejected is the fingerprints of suggestions a person turned down, so
	// the same one is not offered again from the same evidence.
	Rejected []string `json:"rejected,omitempty"`
	History  []Change `json:"history,omitempty"`
}

// MergeRecord remembers a person that was merged into another, so the merge
// can be undone exactly.
type MergeRecord struct {
	// Person is the merged person as it stood before the merge.
	Person Person `json:"person"`
	// Identities, Tags and Goals are what the target gained from the merge
	// (identity keys, tag names, goal ids): exactly what Unmerge takes back.
	Identities []string  `json:"identities,omitempty"`
	Tags       []string  `json:"tags,omitempty"`
	Goals      []string  `json:"goals,omitempty"`
	At         time.Time `json:"at"`
	By         string    `json:"by,omitempty"`
}

// Profile field names an edit may address.
const (
	FieldRealName     = "real_name"
	FieldOrganization = "organization"
	FieldLocation     = "location"
)

// ProfileFields is every profile field, in display order.
var ProfileFields = []string{FieldRealName, FieldOrganization, FieldLocation}

// ProfileSourceGitHub names the automatic source of a profile field: the
// person's own public GitHub profile.
const ProfileSourceGitHub = "github_profile"

// ProfileField is one optional fact about a person with its provenance, in
// the shape of a tag's: Source is auto (filled from a public GitHub profile)
// or manual (a person typed it, possibly to clear it). A manual value always
// wins; "auto" hands the field back to the automatic source.
type ProfileField struct {
	Value  string    `json:"value,omitempty"`
	Source Source    `json:"source,omitempty"`
	Rule   string    `json:"rule,omitempty"`
	At     time.Time `json:"at,omitempty"`
	By     string    `json:"by,omitempty"`
}

// Profile holds the optional descriptive fields of a person. All of it is
// either the person's own public GitHub profile text or typed by a person;
// nothing here comes from the hub.
type Profile struct {
	RealName     ProfileField `json:"real_name,omitempty"`
	Organization ProfileField `json:"organization,omitempty"`
	Location     ProfileField `json:"location,omitempty"`
}

// Field returns a pointer to the named field, or nil for an unknown name.
func (pr *Profile) Field(name string) *ProfileField {
	switch name {
	case FieldRealName:
		return &pr.RealName
	case FieldOrganization:
		return &pr.Organization
	case FieldLocation:
		return &pr.Location
	}
	return nil
}

// Person is one customer.
type Person struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Status       Status       `json:"status"`
	Origin       Origin       `json:"origin"`
	Identities   []Identity   `json:"identities,omitempty"`
	Tags         []Tag        `json:"tags,omitempty"`
	Goals        []Goal       `json:"goals,omitempty"`
	Satisfaction Satisfaction `json:"satisfaction"`
	Notes        string       `json:"notes,omitempty"`
	// Profile is the real name, organization and location, each with its
	// provenance.
	Profile Profile `json:"profile,omitempty"`
	// RejectedGoals is the fingerprints of goal suggestions a person
	// turned down.
	RejectedGoals []string `json:"rejected_goals,omitempty"`
	// InferredAt and InferredHash record the last inference pass over this
	// person and the evidence it read, so an unchanged person is not sent
	// to the model again.
	InferredAt   time.Time     `json:"inferred_at,omitempty"`
	InferredHash string        `json:"inferred_hash,omitempty"`
	MergedFrom   []MergeRecord `json:"merged_from,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

// HasIdentity reports whether p holds the identity with this key as a
// confirmed identity. An unconfirmed claim does not count.
func (p Person) HasIdentity(key string) bool {
	i := p.identityIndex(key)
	return i >= 0 && !p.Identities[i].Claimed
}

// identityIndex is the position of the identity with this key, claimed or
// not, or -1.
func (p Person) identityIndex(key string) int {
	for n, i := range p.Identities {
		if i.Key() == key {
			return n
		}
	}
	return -1
}

// dropIdentity removes the identity at position n.
func (p *Person) dropIdentity(n int) {
	p.Identities = append(p.Identities[:n:n], p.Identities[n+1:]...)
}

// Identity returns p's first identity of a kind, or "".
func (p Person) Identity(kind IdentityKind) string {
	for _, i := range p.Identities {
		if i.Kind == kind {
			return i.Value
		}
	}
	return ""
}

// HasTag reports whether the tag is on.
func (p Person) HasTag(name string) bool {
	for _, t := range p.Tags {
		if t.Name == name {
			return t.State == TagOn
		}
	}
	return false
}

// ActiveTags is the names of the tags that are on, in stored order.
func (p Person) ActiveTags() []string {
	var out []string
	for _, t := range p.Tags {
		if t.State == TagOn {
			out = append(out, t.Name)
		}
	}
	return out
}

// ConfirmedGoals is the goals a person stands behind, in stored order.
func (p Person) ConfirmedGoals() []Goal {
	var out []Goal
	for _, g := range p.Goals {
		if g.State == StateConfirmed {
			out = append(out, g)
		}
	}
	return out
}

// Suggestion is a person the hive has seen but nobody has accepted: an
// identity key, counters and at most MaxSuggestionRef evidence references.
// It carries no name and no content.
type Suggestion struct {
	ID    string       `json:"id"`
	Kind  IdentityKind `json:"kind"`
	Value string       `json:"value"`
	// Counts is how many times each predicate was seen.
	Counts    map[string]int `json:"counts,omitempty"`
	Evidence  []string       `json:"evidence,omitempty"`
	FirstSeen time.Time      `json:"first_seen"`
	LastSeen  time.Time      `json:"last_seen"`
}

// Total is the number of sightings across every predicate.
func (s Suggestion) Total() int {
	n := 0
	for _, c := range s.Counts {
		n += c
	}
	return n
}

// Pin kinds.
const (
	// PinUnlink records that a person removed an identity from a profile:
	// automatic resolution must not put it back.
	PinUnlink = "unlink"
	// PinDistinct records that a person said two profiles are different
	// people: no hint is raised for the pair again.
	PinDistinct = "distinct"
)

// Pin is a manual decision automatic resolution must respect.
type Pin struct {
	Kind string `json:"kind"`
	// Identity is the identity key for an unlink pin.
	Identity string `json:"identity,omitempty"`
	// Person is the profile the pin applies to.
	Person string `json:"person"`
	// Other is the second profile or suggestion of a distinct pin.
	Other string    `json:"other,omitempty"`
	At    time.Time `json:"at"`
	By    string    `json:"by,omitempty"`
}

// Hint is a "Same person?" question for a person to answer. A hint never
// links anything by itself.
type Hint struct {
	ID string `json:"id"`
	// Person is the profile the hint is shown on.
	Person string `json:"person"`
	// Other is the profile or suggestion that may be the same person.
	Other string `json:"other,omitempty"`
	// Identity is set when the hint is about one identity rather than a
	// whole profile (a self-edit that claimed an identity someone else
	// holds).
	Identity *Identity `json:"identity,omitempty"`
	Reason   string    `json:"reason"`
	At       time.Time `json:"at"`
}
