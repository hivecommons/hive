// Package blocker models the structured reason an item is parked on a
// person when the hive itself cannot act — a capability the GitHub App
// lacks (`workflows` permission, repository write, branch-protection admin,
// a secret) or a step only a human may take (a CLA) — as distinct from a
// question a maintainer can answer (hivecommons/hive#15099).
//
// Before this, an agent reporting "the App token cannot push
// .github/workflows/*; needs a human" produced a bare `needs-human`, which
// reads to the dashboard exactly like "which approach do you want?". The
// operator's "Let hive decide" then un-parked the issue, the scanner
// re-verified, re-applied the same `needs-human`, and the loop repeated
// (hivecommons/hive#14976). A Blocker is the machine-readable answer to
// "what would actually unblock this?": the kind names the capability, the
// ledger remembers it per item, and the delegated decision, the stage
// derivation and the tracker all read the same record.
package blocker

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

// Kind is the structured blocker kind. Capability kinds start with
// `capability:` and name something the GitHub App installation can be
// granted; `human-only:` kinds are steps no permission grant unblocks.
type Kind string

const (
	// CapabilityWorkflowsPermission: the App installation has no
	// `workflows: write`, so it cannot push a commit that touches
	// .github/workflows/* (hivecommons/hive#14976).
	CapabilityWorkflowsPermission Kind = "capability:workflows-permission"
	// CapabilityRepoWrite: the App cannot push to the repository at all
	// (no `contents: write`, or the repository is not in the installation).
	CapabilityRepoWrite Kind = "capability:repo-write"
	// CapabilityBranchProtection: the change needs a branch-protection /
	// ruleset edit the App has no `administration` permission for.
	CapabilityBranchProtection Kind = "capability:branch-protection"
	// CapabilitySecret: the change needs a repository or organization
	// secret the App cannot read or set.
	CapabilitySecret Kind = "capability:secret"
	// HumanOnlyCLA: a contributor must sign a CLA / DCO attestation.
	HumanOnlyCLA Kind = "human-only:cla"
	// HumanOnlyReasked is the loop-guard kind: the same needs-human reason
	// came back after a delegated un-park, so whatever the hive decided did
	// not take and asking it again cannot help (hivecommons/hive#15099).
	HumanOnlyReasked Kind = "human-only:re-asked"
)

// Kinds is every known kind, in display order.
var Kinds = []Kind{CapabilityWorkflowsPermission, CapabilityRepoWrite, CapabilityBranchProtection, CapabilitySecret, HumanOnlyCLA, HumanOnlyReasked}

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	for _, known := range Kinds {
		if known == k {
			return true
		}
	}
	return false
}

// IsCapability reports whether a GitHub App permission grant can clear k.
func (k Kind) IsCapability() bool { return strings.HasPrefix(string(k), "capability:") }

// Permission is the GitHub App installation permission k is missing
// ("workflows", "contents", "administration", "secrets"); "" for
// human-only kinds.
func (k Kind) Permission() string {
	switch k {
	case CapabilityWorkflowsPermission:
		return "workflows"
	case CapabilityRepoWrite:
		return "contents"
	case CapabilityBranchProtection:
		return "administration"
	case CapabilitySecret:
		return "secrets"
	}
	return ""
}

// Short is the tracker pill text after "Blocked · ".
func (k Kind) Short() string {
	switch k {
	case CapabilityWorkflowsPermission:
		return "App lacks workflows permission"
	case CapabilityRepoWrite:
		return "App cannot push to this repository"
	case CapabilityBranchProtection:
		return "App cannot change branch protection"
	case CapabilitySecret:
		return "App cannot set the secret"
	case HumanOnlyCLA:
		return "a person must sign the CLA"
	case HumanOnlyReasked:
		return "hive re-asked after a decision"
	}
	return "hive cannot do this"
}

// CantDo is the plain-words first sentence of the popover and of the
// delegated-decision no-effect result: what the hive cannot do and why.
func (k Kind) CantDo() string {
	switch k {
	case CapabilityWorkflowsPermission:
		return "Hive can't do this one: the GitHub App has no `workflows` permission, so it cannot push changes to .github/workflows/*."
	case CapabilityRepoWrite:
		return "Hive can't do this one: the GitHub App cannot push to this repository (no `contents: write`, or the repository is not in the installation)."
	case CapabilityBranchProtection:
		return "Hive can't do this one: the change needs a branch-protection or ruleset edit, and the GitHub App has no `administration` permission."
	case CapabilitySecret:
		return "Hive can't do this one: the change needs a repository secret the GitHub App cannot read or set."
	case HumanOnlyCLA:
		return "Hive can't do this one: a person must sign the CLA before the change can be accepted."
	case HumanOnlyReasked:
		return "Hive can't decide this one: it already un-parked the item on a delegated decision and an agent parked it again for the same reason, so deciding again would only repeat the loop."
	}
	return "Hive can't do this one."
}

// Blocker is one recorded blocker on an item.
type Blocker struct {
	Kind Kind `json:"kind"`
	// Detail is the reporting sentence ("the remaining retry wrappers live
	// in .github/workflows/…"), trimmed, for the popover.
	Detail string `json:"detail,omitempty"`
	// Source is how the kind was learned: "marker" (the agent wrote the
	// `<!-- hive:blocker -->` marker), "inferred" (from the prose) or
	// "loop-guard".
	Source string    `json:"source,omitempty"`
	Since  time.Time `json:"since,omitempty"`
}

const (
	SourceMarker    = "marker"
	SourceInferred  = "inferred"
	SourceLoopGuard = "loop-guard"
)

// markerPrefix opens the agent-report marker; kept distinct from every
// other hive:* marker so each parser reads only its own.
const markerPrefix = "<!-- hive:blocker kind="

var markerRE = regexp.MustCompile(`<!--\s*hive:blocker\s+kind=([A-Za-z0-9:_-]+)\s*-->`)

// Marker renders the machine-readable marker an agent (or the gh wrapper
// on its behalf) appends to a needs-human report.
func Marker(k Kind) string { return markerPrefix + string(k) + " -->" }

// ParseMarker returns the kind named by the first `<!-- hive:blocker
// kind=… -->` marker in text; false when there is none or it names an
// unknown kind.
func ParseMarker(text string) (Kind, bool) {
	m := markerRE.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	k := Kind(strings.ToLower(m[1]))
	return k, k.Valid()
}

// inferRules map prose to a kind, first match wins. They are deliberately
// narrow: a sentence has to say both that the App/token/hive *cannot* do
// something and name the capability, so an issue that merely discusses
// workflows is never classified.
var inferRules = []struct {
	kind Kind
	re   *regexp.Regexp
}{
	{CapabilityWorkflowsPermission, regexp.MustCompile("(?is)" +
		"(no|lacks?|lacking|missing|without)\\s+(the\\s+)?`?workflows?`?(:\\s*write)?\\s+permission" +
		"|" + cannotRE + "\\s+(push|modify|change|edit|write|update|touch|commit)[^.\\n]{0,80}(\\.github/workflows|workflow files?|workflows?/)" +
		"|(\\.github/workflows|workflow files?)[^.\\n]{0,80}" + cannotRE + "\\s+(push|modify|change|edit|write|update|touch|commit)" +
		"|workflows?\\s+permission[^.\\n]{0,40}(missing|lacking|not granted|needed|required)")},
	{CapabilityBranchProtection, regexp.MustCompile("(?is)" +
		cannotRE + "[^.\\n]{0,60}(branch[ -]protection|rulesets?|protected branch)" +
		"|(branch[ -]protection|rulesets?|protected branch)[^.\\n]{0,60}(" + cannotRE + "|needs|requires)[^.\\n]{0,20}(make|change|edit|update|admin|administration|maintainer)?")},
	{CapabilitySecret, regexp.MustCompile("(?is)" +
		cannotRE + "\\s+(set|add|create|read|rotate|update|configure)[^.\\n]{0,60}\\b(secrets?|credentials?)\\b" +
		"|\\b(repository|organization|org|actions)\\s+secrets?[^.\\n]{0,60}(" + cannotRE + "|needs a (human|person|maintainer))")},
	{CapabilityRepoWrite, regexp.MustCompile("(?is)" +
		cannotRE + "\\s+(push|write|commit)[^.\\n]{0,60}\\b(repository|repo|branch|fork)\\b" +
		"|(no|lacks?|missing|without)\\s+(the\\s+)?`?(contents|write|push)`?(:\\s*write)?\\s+(permission|access)")},
	{HumanOnlyCLA, regexp.MustCompile("(?is)\\b(sign|signing|signed)\\b[^.\\n]{0,30}\\b(cla|contributor license agreement)\\b|\\bcla\\b[^.\\n]{0,40}(must|needs?|required)")},
}

// cannotRE is the "the App / token / hive cannot" fragment the rules share.
const cannotRE = "(cannot|can't|can not|unable to|not able to|no permission to|not permitted to|refuses? to|is not allowed to|lacks the permission to)"

// Infer classifies prose with no marker. It returns the kind and true when
// a rule matches; the `needs-human` hint is not required (a PR body's
// "Refs #N (needs-human: …)" remainder and a scanner's verification comment
// both qualify) but the capability wording is.
func Infer(prose string) (Kind, bool) {
	prose = stripMarkers(prose)
	for _, r := range inferRules {
		if r.re.MatchString(prose) {
			return r.kind, true
		}
	}
	return "", false
}

// Detect reads a comment or body: the marker wins, prose inference is the
// fallback for reports written before the marker existed. Detail is the
// first sentence that cites `needs-human` / "needs a human", or the first
// sentence the kind was inferred from.
func Detect(text string, at time.Time) (Blocker, bool) {
	if k, ok := ParseMarker(text); ok {
		return Blocker{Kind: k, Detail: ReasonSentence(text), Source: SourceMarker, Since: at}, true
	}
	if k, ok := Infer(text); ok {
		return Blocker{Kind: k, Detail: ReasonSentence(text), Source: SourceInferred, Since: at}, true
	}
	return Blocker{}, false
}

var (
	htmlCommentRE = regexp.MustCompile(`(?s)<!--.*?-->`)
	needsHumanRE  = regexp.MustCompile(`(?i)needs[- ]human|needs a (human|person|maintainer)|a maintainer must|a person must`)
	spaceRE       = regexp.MustCompile(`\s+`)
)

func stripMarkers(s string) string { return htmlCommentRE.ReplaceAllString(s, " ") }

// ReasonSentence is the reporting sentence of a needs-human comment: the
// sentence containing the needs-human hint, else the first non-empty line
// that is not a footer. Markers, the 🐝 footer and code fences are dropped;
// the result is capped so it fits a popover.
func ReasonSentence(text string) string {
	const maxLen = 280
	clean := stripMarkers(text)
	var first string
	for _, line := range strings.Split(clean, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "🐝") || strings.HasPrefix(line, "<sub>") || strings.HasPrefix(line, "```") || strings.HasPrefix(line, "_This comment") {
			continue
		}
		if first == "" {
			first = line
		}
		if needsHumanRE.MatchString(line) {
			first = line
			break
		}
	}
	first = strings.TrimLeft(first, "-*> ")
	first = spaceRE.ReplaceAllString(first, " ")
	if len(first) > maxLen {
		first = first[:maxLen-1] + "…"
	}
	return first
}

// NormalizeReason is the loop guard's comparison key for a needs-human
// reason: lower-cased, markers and punctuation-level noise dropped, so the
// scanner re-phrasing a PR number or a timestamp still counts as the same
// reason.
func NormalizeReason(text string) string {
	s := strings.ToLower(ReasonSentence(text))
	s = regexp.MustCompile(`#\d+|\d{4}-\d{2}-\d{2}[^\s]*|https?://\S+`).ReplaceAllString(s, "")
	s = regexp.MustCompile("[`*_\"'()\\[\\]:;,.?!]").ReplaceAllString(s, "")
	s = spaceRE.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// Options are the plain-words choices the popover and the delegated
// decision offer for a blocker, in order. Each maps to an action id the
// dashboard knows; the dashboard decides which are enabled.
type Option struct {
	Action string
	Label  string
}

const (
	// ActionAppSettings opens the GitHub App's permissions page.
	ActionAppSettings = "open:app-settings"
	// ActionRecheck probes the installation's permissions and clears the
	// blocker when the missing one is granted.
	ActionRecheck = "blocker:recheck"
	// ActionPatch asks the hive to prepare the change as a diff / branch a
	// person pushes.
	ActionPatch = "blocker:patch"
	// ActionClosePartial marks the item hive/partially-addressed and
	// closes it as completed.
	ActionClosePartial = "blocker:close-partial"
	// ActionWait is "keep waiting" — a no-op the popover lists so the
	// operator sees it is a legitimate choice.
	ActionWait = "blocker:wait"
)

// OptionsFor lists the options for k in the order the popover shows them.
func OptionsFor(k Kind) []Option {
	if k.IsCapability() {
		perm := k.Permission()
		return []Option{
			{ActionAppSettings, "Grant the App `" + perm + ": write`"},
			{ActionRecheck, "Re-check permissions"},
			{ActionPatch, "Prepare the patch for a person to push"},
			{ActionClosePartial, "Mark partially-addressed and close"},
			{ActionWait, "Keep waiting"},
		}
	}
	return []Option{
		{ActionClosePartial, "Mark partially-addressed and close"},
		{ActionWait, "Keep waiting"},
	}
}

// MissingPermissions lists the distinct App permissions a set of blockers
// is waiting on, sorted.
func MissingPermissions(blockers []Blocker) []string {
	seen := map[string]bool{}
	for _, b := range blockers {
		if p := b.Kind.Permission(); p != "" && !seen[p] {
			seen[p] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
