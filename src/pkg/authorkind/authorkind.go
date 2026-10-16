// Package authorkind classifies who authored a pull request: a person, a
// contributor's hive (a "clanker" relay session posting on the author's
// behalf) or this hive's own App. The stage engine keys its 072 behaviour on
// the kind: only author/clanker PRs get the contributor-nudge interrupt and
// offline adoption; human and hive PRs follow the ordinary transitions and
// simply wait at 072 (docs/labels-and-control-signals.md, "Stage 072 by
// author kind").
//
// This is a leaf package: it imports nothing from the rest of hive so the
// GitHub client, the automerge engine and the dashboard can all use it.
package authorkind

import "strings"

// Kind is an author classification.
type Kind string

const (
	// Unknown means nothing could be decided (e.g. a non-App bot login).
	Unknown Kind = ""
	// Human is a person pushing from their own machine.
	Human Kind = "human"
	// Clanker is a contributor's hive or relay session posting for them.
	Clanker Kind = "clanker"
	// Hive is this hive's own App.
	Hive Kind = "hive"
)

// Input is what Detect may use; every field is optional.
type Input struct {
	// Author is the PR author login.
	Author string
	// AppLogin is this hive's App bot login ([bot] suffix tolerated).
	AppLogin string
	// HiveID is this hive's id (matches `<!-- hive:origin hive=<id> -->`).
	HiveID string
	// Labels are the PR's current labels; only the `agent/<lane>` stamp is
	// read (it means this hive's lane owns the PR).
	Labels []string
	// Body is the PR body: the origin marker, the `— hive:` attribution
	// trailer and the 🐝 identity footer are recognised.
	Body string
	// RelayKnown is true when the author has a contributor profile on this
	// hive (the weakest signal: a relay contributor may also push by hand).
	RelayKnown bool
}

// Names are the label names per kind (governor.labels.author.*).
type Names struct{ Human, Clanker, Hive string }

// DefaultNames are the built-in label names.
var DefaultNames = Names{Human: "author/human", Clanker: "author/clanker", Hive: "author/hive"}

// Label returns the label name for k under n; "" for Unknown.
func (n Names) Label(k Kind) string {
	switch k {
	case Human:
		return n.Human
	case Clanker:
		return n.Clanker
	case Hive:
		return n.Hive
	}
	return ""
}

// Label returns the default label name for k; "" for Unknown.
func (k Kind) Label() string { return DefaultNames.Label(k) }

// Detect classifies the author described by in.
func Detect(in Input) Kind {
	k, _ := Explain(in)
	return k
}

const (
	originMarker       = "<!-- hive:origin"
	attributionTrailer = "— hive:"
	identityFooter     = "🐝"
)

// Explain is Detect with the name of the rule that decided, for logs and
// tests. Detection order: App login → Hive; agent/* label → Hive; origin
// marker for this hive → Hive, for another hive → Clanker; attribution
// trailer or identity footer → Clanker; relay profile → Clanker; other [bot]
// login → Unknown; else Human.
func Explain(in Input) (Kind, string) {
	author := normLogin(in.Author)
	if app := normLogin(in.AppLogin); app != "" && author != "" && author == app {
		return Hive, "app-login"
	}
	for _, l := range in.Labels {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), "agent/") {
			return Hive, "agent-label"
		}
	}
	if id, ok := originHive(in.Body); ok {
		if hive := strings.TrimSpace(in.HiveID); hive != "" && strings.EqualFold(id, hive) {
			return Hive, "origin-local"
		}
		return Clanker, "origin-foreign"
	}
	if strings.Contains(in.Body, attributionTrailer) {
		return Clanker, "attribution-trailer"
	}
	if strings.Contains(in.Body, identityFooter) {
		return Clanker, "identity-footer"
	}
	if in.RelayKnown {
		return Clanker, "registry"
	}
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(in.Author)), "[bot]") {
		return Unknown, "bot"
	}
	return Human, "default-human"
}

// FromLabels reads an already-applied author/<kind> label under n; Unknown
// when none is present.
func FromLabels(labels []string, n Names) Kind {
	for _, l := range labels {
		l = strings.TrimSpace(l)
		switch {
		case n.Hive != "" && strings.EqualFold(l, n.Hive):
			return Hive
		case n.Clanker != "" && strings.EqualFold(l, n.Clanker):
			return Clanker
		case n.Human != "" && strings.EqualFold(l, n.Human):
			return Human
		}
	}
	return Unknown
}

// originHive extracts hive=<id> from the first `<!-- hive:origin ... -->`
// marker in body.
func originHive(body string) (string, bool) {
	i := strings.Index(body, originMarker)
	if i < 0 {
		return "", false
	}
	rest := body[i+len(originMarker):]
	if end := strings.Index(rest, "-->"); end >= 0 {
		rest = rest[:end]
	}
	for _, f := range strings.Fields(rest) {
		if v, ok := strings.CutPrefix(f, "hive="); ok {
			return strings.Trim(v, `"'`), true
		}
	}
	return "", true
}

func normLogin(s string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "[bot]"))
}
