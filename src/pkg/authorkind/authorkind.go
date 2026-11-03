// Package authorkind decides WHO wrote a pull request — a person by hand, a
// contributor's own hive under that contributor's GitHub login, or this
// repository's own hive — from the evidence the hive already leaves on a PR
// (hivecommons/hive#12581). The result is rendered as the `author/<kind>`
// label by pkg/stagelabels and gates the stage-072 contributor nudge and
// adoption, which only make sense when there is a hive on the other end.
//
// The kind is orthogonal to the admission tier (`role/<tier>`): a trusted
// contributor whose hive opened the PR is `role/trusted` + `author/clanker`.
//
// This package has no hive imports so anything may use it; the marker
// spellings it recognises are owned by pkg/github (OriginMarker,
// AttributionTrailerPrefix, the gh-wrapper identity footer) and are
// mirrored here by value.
package authorkind

import (
	"regexp"
	"strings"
)

// Kind is the authorship kind. The zero value Unknown means the detector
// had no opinion (today: a non-App bot such as dependabot).
type Kind string

const (
	Unknown Kind = ""
	// Human: a person pushed the PR themselves.
	Human Kind = "human"
	// Clanker: a contributor's own hive (relay) authored the PR under that
	// contributor's GitHub login. "Clanker" is the project's word for a
	// contributor's own agent (clanker), as opposed to the repo's own hive.
	Clanker Kind = "clanker"
	// Hive: this repository's own hive — the App or one of its agent
	// lanes — authored the PR.
	Hive Kind = "hive"
)

// Prefix is the label family every kind name must live under so the stage
// reconciler owns it.
const Prefix = "author/"

// Names are the label names for the three kinds
// (governor.labels.author.{human,clanker,hive}). Zero fields mean the
// defaults.
type Names struct {
	Human, Clanker, Hive string
}

// DefaultNames are the built-in label names.
var DefaultNames = Names{Human: Prefix + "human", Clanker: Prefix + "clanker", Hive: Prefix + "hive"}

// Normalize fills unset names from DefaultNames.
func (n Names) Normalize() Names {
	pick := func(v, d string) string {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
		return d
	}
	return Names{Human: pick(n.Human, DefaultNames.Human), Clanker: pick(n.Clanker, DefaultNames.Clanker), Hive: pick(n.Hive, DefaultNames.Hive)}
}

// Label renders the configured label for k, or "" for Unknown.
func (n Names) Label(k Kind) string {
	n = n.Normalize()
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

// All lists the configured names in kind order (human, clanker, hive).
func (n Names) All() []string {
	n = n.Normalize()
	return []string{n.Human, n.Clanker, n.Hive}
}

// Label renders the default label for k ("author/human", …), or "" for Unknown.
func (k Kind) Label() string { return DefaultNames.Label(k) }

// String is the bare kind ("human", "clanker", "hive", "").
func (k Kind) String() string { return string(k) }

func rank(k Kind) int {
	switch k {
	case Hive:
		return 3
	case Clanker:
		return 2
	case Human:
		return 1
	}
	return 0
}

// FromLabels reads an already-applied `author/<kind>` label under names, or
// Unknown. hive beats clanker beats human should an item transiently carry
// two. It never falls back to Detect: labels are the engine's output, and a
// reader that wants a fresh opinion should call Detect.
func FromLabels(labels []string, names Names) Kind {
	names = names.Normalize()
	out := Unknown
	for _, l := range labels {
		var k Kind
		switch strings.TrimSpace(l) {
		case names.Hive:
			k = Hive
		case names.Clanker:
			k = Clanker
		case names.Human:
			k = Human
		default:
			continue
		}
		if rank(k) > rank(out) {
			out = k
		}
	}
	return out
}

// Input is the evidence Detect may use. Every field is optional; the more
// that is filled in, the more specific the rule that fires.
type Input struct {
	// Author is the PR author's GitHub login.
	Author string
	// AppLogin is this hive's App bot login; the `[bot]` suffix is tolerated
	// on either side.
	AppLogin string
	// HiveID is this hive's id, compared with the `hive=` field of the
	// `<!-- hive:origin … -->` marker.
	HiveID string
	// Labels are the PR's current labels. Only the `agent/<lane>` ownership
	// stamp is read; an existing `author/*` label is NOT consulted.
	Labels []string
	// Body is the PR body: the origin marker, the `— hive:` attribution
	// trailer and the 🐝 identity footer are read from it.
	Body string
	// RelayKnown reports that the author has a contributor profile on this
	// hive, i.e. runs a relay that targets it. It is the weakest signal (a
	// registered contributor can still push by hand) and only fires when
	// the body carries no marker.
	RelayKnown bool
	// Forge is the forge the PR lives on ("github", "gitlab", "gitea";
	// pkg/forge.Kind by value). Empty means GitHub. It selects how a bot
	// login is recognised: GitHub Apps carry a `[bot]` suffix, GitLab
	// project/group access-token users are `project_<id>_bot…` /
	// `group_<id>_bot…`, and Gitea/Forgejo Actions run as `gitea-actions` /
	// `forgejo-actions`. The marker and trailer rules are forge-neutral.
	Forge string
}

// Rule names Explain returns, in detection order.
const (
	RuleAppLogin           = "app-login"
	RuleAgentLabel         = "agent-label"
	RuleOriginLocal        = "origin-local"
	RuleOriginForeign      = "origin-foreign"
	RuleAttributionTrailer = "attribution-trailer"
	RuleIdentityFooter     = "identity-footer"
	RuleRegistry           = "registry"
	RuleBot                = "bot"
	RuleDefaultHuman       = "default-human"
)

// Marker spellings, mirrored from pkg/github by value (see package doc).
const (
	originMarkerName = "hive:origin"
	// attributionTrailerPrefix opens the visible trailer line pkg/github
	// AppendTrailer writes and the hub's ReconcilePRAttribution stamps on
	// relay-reported PRs.
	attributionTrailerPrefix = "— hive:"
	// identityFooterMarker is the visible prefix of the footer
	// bin/gh-wrapper.sh appends to agent-authored bodies.
	identityFooterMarker = "🐝 **Hive Agent**"
	// agentLabelPrefix is the lane routing/ownership stamp.
	agentLabelPrefix = "agent/"
	botSuffix        = "[bot]"
)

// gitlabBotLoginPattern matches the users GitLab creates for project and group
// access tokens (project_<id>_bot, project_<id>_bot_<hash>, group_<id>_bot…).
var gitlabBotLoginPattern = regexp.MustCompile(`^(project|group)_\d+_bot(_[0-9a-z]+)?$`)

// giteaBotLogins are the pseudo-users Gitea and Forgejo Actions act as.
var giteaBotLogins = map[string]bool{"gitea-actions": true, "forgejo-actions": true}

var originMarkerPattern = regexp.MustCompile(`<!--\s*` + regexp.QuoteMeta(originMarkerName) + `\s+([^>]*?)\s*-->`)

// Detect returns the authorship kind for in. See Explain for the rule.
func Detect(in Input) Kind {
	k, _ := Explain(in)
	return k
}

// Explain returns the kind and the name of the rule that decided it. Rules
// are tried in order; the first that applies wins:
//
//  1. author is the App login → Hive
//  2. an `agent/<lane>` label is present → Hive
//  3. `<!-- hive:origin hive=<id> -->`: id == HiveID → Hive, else → Clanker
//  4. the `— hive:` attribution trailer → Clanker
//  5. the 🐝 identity footer → Clanker
//  6. RelayKnown → Clanker
//  7. a non-App bot login (see Input.Forge) → Unknown
//  8. otherwise → Human
func Explain(in Input) (Kind, string) {
	author, app := normLogin(in.Author), normLogin(in.AppLogin)
	if author != "" && app != "" && author == app {
		return Hive, RuleAppLogin
	}
	for _, l := range in.Labels {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), agentLabelPrefix) {
			return Hive, RuleAgentLabel
		}
	}
	if origin, ok := originHive(in.Body); ok {
		if local := strings.Join(strings.Fields(in.HiveID), "-"); local != "" && strings.EqualFold(origin, local) {
			return Hive, RuleOriginLocal
		}
		return Clanker, RuleOriginForeign
	}
	if strings.Contains(in.Body, attributionTrailerPrefix) {
		return Clanker, RuleAttributionTrailer
	}
	if strings.Contains(in.Body, identityFooterMarker) {
		return Clanker, RuleIdentityFooter
	}
	if in.RelayKnown {
		return Clanker, RuleRegistry
	}
	if isBotLogin(in.Author, in.Forge) {
		return Unknown, RuleBot
	}
	return Human, RuleDefaultHuman
}

// isBotLogin reports whether login is an automation account on forge. The
// GitHub `[bot]` suffix is honoured on every forge (a mirrored PR may carry
// one); the GitLab and Gitea conventions only on their own forge.
func isBotLogin(login, forge string) bool {
	l := strings.ToLower(strings.TrimSpace(login))
	if strings.HasSuffix(l, botSuffix) {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(forge)) {
	case "gitlab":
		return gitlabBotLoginPattern.MatchString(l)
	case "gitea":
		return giteaBotLogins[l]
	}
	return false
}

func normLogin(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), botSuffix)
}

// originHive returns the hive= field of the first origin marker in body.
func originHive(body string) (string, bool) {
	m := originMarkerPattern.FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	for _, field := range strings.Fields(m[1]) {
		if v, ok := strings.CutPrefix(field, "hive="); ok && v != "" {
			return v, true
		}
	}
	return "", false
}
