package review

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Which perspectives run, what each is told to look for, and -- eventually --
// which perspectives EXIST are the questions an operator is best placed to
// answer and the code is worst placed to guess. A hive reviewing infrastructure
// repos wants security on every PR; a docs fleet wants docs-currency and little
// else; a Kubernetes operator repo may want an "api-compatibility" perspective
// this package has never heard of.
//
// All three were hardcoded: the set was a package var, the focus line was a map
// literal inside the prompt builder, and the vocabulary of valid names was that
// same var reused as a validation check. Adding a perspective therefore meant a
// code change, a release, and a fleet rollout, for what is a per-hive editorial
// choice.
//
// PerspectiveSet makes all three configurable without loosening the one thing
// that must stay strict: a verdict may only claim a perspective this hive
// actually defines. The set is the single authority for that question, resolved
// once from config rather than re-derived at each call site.

// customNamePattern bounds what a hive may invent as a perspective name.
//
// The name is not merely a label. It is interpolated into the review kick, used
// as a map key for routing, and forms part of the verdict filename the relay
// writes on the hive's own filesystem. Lowercase alphanumerics with internal
// dashes are safe and readable in all three without three separate escaping
// rules -- and the relay's verdictSlug stays as the belt to this braces,
// because config is not the only way a name can reach a filename.
var customNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// MaxPerspectiveNameLen bounds a perspective name. Names appear in the kick, in
// comment headings, and in filenames; a long one is unreadable in all three and
// serves no purpose the focus text does not serve better.
const MaxPerspectiveNameLen = 40

// MaxFocusLen bounds one perspective's focus text. The focus line names what to
// look for; the kick around it carries the method. A bound this generous
// rejects only a pasted document, which would crowd out the read and publish
// instructions that make a review usable.
const MaxFocusLen = 2000

// MaxPerspectives bounds how many a hive may define.
//
// Every perspective is a judgment the reviewer must form and a verdict that can
// independently withhold approval, so the cost is paid on every PR, on every
// review. The limit is not a storage concern -- it is a reminder that a
// reviewer asked twenty questions answers all of them worse than one asked
// five.
const MaxPerspectives = 12

// PerspectiveSet is the resolved, ordered set of perspectives one hive reviews
// with, together with the focus text for each.
//
// The zero value is valid and means the built-in default set, so every caller
// not yet taught about configuration keeps working unchanged.
type PerspectiveSet struct {
	order []Perspective
	focus map[Perspective]string
}

// NewPerspectiveSet resolves a configured selection and focus overrides.
//
// names selects which perspectives run, in the order given, and may name
// built-ins or hive-defined perspectives. focus supplies or overrides the focus
// text, keyed by perspective name.
//
// A name that is neither built in nor given focus text is an error rather than
// a silent drop. A typo'd "sekurity" that quietly disappeared would leave an
// operator believing security review was on: the setting reads as enabled
// everywhere it is displayed, and nothing reviews it. That is the worst
// possible outcome of a config mistake, so it fails loudly at load instead.
func NewPerspectiveSet(names []string, focus map[string]string) (PerspectiveSet, error) {
	resolvedFocus, err := parseFocusOverrides(focus)
	if err != nil {
		return PerspectiveSet{}, err
	}

	// No explicit selection: run the built-ins, honouring focus overrides, plus
	// any perspective the hive defined purely by giving it focus text. Defining
	// one and having it silently not run would be its own trap.
	if len(trimmedNames(names)) == 0 {
		order := append([]Perspective(nil), DefaultPerspectives...)
		for _, p := range sortedPerspectiveKeys(resolvedFocus) {
			if !isBuiltinPerspective(p) {
				order = append(order, p)
			}
		}
		if len(order) > MaxPerspectives {
			return PerspectiveSet{}, fmt.Errorf("%d review perspectives defined, over the limit of %d", len(order), MaxPerspectives)
		}
		return PerspectiveSet{order: order, focus: resolvedFocus}, nil
	}

	order := make([]Perspective, 0, len(names))
	seen := map[Perspective]bool{}
	for _, raw := range names {
		name := Perspective(strings.ToLower(strings.TrimSpace(raw)))
		if name == "" || seen[name] {
			continue
		}
		if !isBuiltinPerspective(name) {
			if err := validateCustomName(name); err != nil {
				return PerspectiveSet{}, err
			}
			// A hive-defined perspective with no focus text is a name and
			// nothing else. The reviewer would be told to judge the PR from a
			// perspective never described to it, and would invent one --
			// precisely the ungrounded reviewing this path exists to stop.
			if strings.TrimSpace(resolvedFocus[name]) == "" {
				return PerspectiveSet{}, fmt.Errorf("review perspective %q is not built in and has no focus text: define what it should look for, or remove it", name)
			}
		}
		seen[name] = true
		order = append(order, name)
	}
	if len(order) == 0 {
		return PerspectiveSet{}, nil
	}
	if len(order) > MaxPerspectives {
		return PerspectiveSet{}, fmt.Errorf("%d review perspectives selected, over the limit of %d", len(order), MaxPerspectives)
	}
	return PerspectiveSet{order: order, focus: resolvedFocus}, nil
}

// List returns the perspectives to review with, in dispatch order. Dispatch
// spends its slot budget in list order, so this order is load-bearing: it
// decides which perspective a capped hive runs first.
func (s PerspectiveSet) List() []Perspective {
	if len(s.order) == 0 {
		return DefaultPerspectives
	}
	return s.order
}

// Len is how many perspectives a PR is eligible to receive.
func (s PerspectiveSet) Len() int { return len(s.List()) }

// Focus returns what this hive tells the reviewer to look for.
//
// A blank override falls back to the built-in rather than producing "Focus ONLY
// on ." -- a cleared textarea in the settings dialog is how an operator says
// "use the default", and it is indistinguishable at this layer from a key that
// was never set.
func (s PerspectiveSet) Focus(p Perspective) string {
	if custom := strings.TrimSpace(s.focus[p]); custom != "" {
		return custom
	}
	if builtin := defaultFocus[p]; builtin != "" {
		return builtin
	}
	return "the named review perspective"
}

// Known reports whether this hive defines the perspective.
//
// This is the authority behind verdict validation, and it is deliberately the
// SELECTED set rather than every name that has ever existed. A verdict naming a
// perspective the hive does not review is not a routing input: nothing
// dispatched it and nothing is waiting on it, so accepting it would let a
// reviewer manufacture a judgment no one asked for.
func (s PerspectiveSet) Known(p Perspective) bool {
	for _, known := range s.List() {
		if known == p {
			return true
		}
	}
	return false
}

// Names renders the set for an error message or a prompt.
func (s PerspectiveSet) Names() string { return joinPerspectives(s.List()) }

// Overrides returns the focus text this hive has customised, for display and
// export. Built-in defaults are excluded: the point of an export is what THIS
// hive changed.
func (s PerspectiveSet) Overrides() map[Perspective]string {
	out := make(map[Perspective]string, len(s.focus))
	for p, f := range s.focus {
		out[p] = f
	}
	return out
}

// defaultFocus is what each built-in perspective is told to look for.
var defaultFocus = map[Perspective]string{
	PerspectiveCorrectness:     "correctness, regressions, edge cases, data races, and test adequacy",
	PerspectiveSecurity:        "exploitable vulnerabilities, unsafe permissions, injection, secrets, and trust-boundary regressions",
	PerspectiveIntentAlignment: "whether the diff solves the linked issue without unrelated scope creep",
	PerspectiveStyle:           "maintainability, conventions, readability, and repository idioms",
	PerspectiveDocsCurrency:    "documentation, examples, generated docs, and operator-facing text that must change with behavior",
	PerspectivePlanMatch:       "whether the diff implements the approved plan wave named by its Hive-Run / Hive-Plan trailers: files or behaviour the plan never asked for (scope creep), and planned items the diff leaves out",
}

// builtinPerspectives is every perspective this package defines. It is
// DefaultPerspectives plus the opt-in ones: a name here is accepted without
// focus text and validated as a built-in, whether or not it runs by default.
var builtinPerspectives = append(append([]Perspective(nil), DefaultPerspectives...), PerspectivePlanMatch)

// WithPlanMatch returns the set with plan_match appended, if it is not already
// selected. It is how review.plan_match.enabled reaches the set: the toggle is
// a separate switch from review.perspectives so that turning the perspective
// on does not require an operator to spell out the whole selection.
//
// A set already at MaxPerspectives is returned unchanged: the cap is a
// reminder that every perspective costs every review, and the toggle does not
// get to exceed it silently.
func (s PerspectiveSet) WithPlanMatch() PerspectiveSet {
	if s.Known(PerspectivePlanMatch) || s.Len() >= MaxPerspectives {
		return s
	}
	order := append(append([]Perspective(nil), s.List()...), PerspectivePlanMatch)
	return PerspectiveSet{order: order, focus: s.focus}
}

// acceptingBuiltins widens the set to every built-in perspective plus the
// hive's own selection, for validating reports already written to disk. It is
// deliberately not what the relay validates against: a live verdict must name
// a perspective the hive actually dispatched.
func (s PerspectiveSet) acceptingBuiltins() PerspectiveSet {
	order := append([]Perspective(nil), builtinPerspectives...)
	for _, p := range s.List() {
		if !isBuiltinPerspective(p) {
			order = append(order, p)
		}
	}
	return PerspectiveSet{order: order, focus: s.focus}
}

// DefaultFocus returns the built-in focus line, so the settings UI can show an
// operator what they are editing away from and offer a way back. Without it an
// override is a one-way door: nothing remembers what the default said. Empty
// for a hive-defined perspective, which has no default.
func DefaultFocus(p Perspective) string { return defaultFocus[p] }

// DefaultFocusAll returns the focus line of every perspective in the default
// set, for the settings UI. plan_match is deliberately absent: it is toggled
// from the Features panel (review.plan_match.enabled), not selected from this
// list, and listing it here would render it as on by default when it is not.
// DefaultFocus still answers for it.
func DefaultFocusAll() map[Perspective]string {
	out := make(map[Perspective]string, len(DefaultPerspectives))
	for _, p := range DefaultPerspectives {
		out[p] = defaultFocus[p]
	}
	return out
}

func isBuiltinPerspective(p Perspective) bool {
	for _, want := range builtinPerspectives {
		if p == want {
			return true
		}
	}
	return false
}

func validateCustomName(p Perspective) error {
	name := string(p)
	if len(name) > MaxPerspectiveNameLen {
		return fmt.Errorf("review perspective %q is %d characters, over the %d limit", name, len(name), MaxPerspectiveNameLen)
	}
	if !customNamePattern.MatchString(name) {
		return fmt.Errorf("review perspective %q must be lowercase letters, digits and single dashes (e.g. api-compatibility)", name)
	}
	return nil
}

func parseFocusOverrides(raw map[string]string) (map[Perspective]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[Perspective]string, len(raw))
	for _, k := range sortedStringKeys(raw) {
		name := Perspective(strings.ToLower(strings.TrimSpace(k)))
		if name == "" {
			continue
		}
		if !isBuiltinPerspective(name) {
			if err := validateCustomName(name); err != nil {
				return nil, err
			}
		}
		text := strings.TrimSpace(raw[k])
		if text == "" {
			// Cleared means "back to the default", so drop the key rather than
			// storing an empty override that renders as one.
			continue
		}
		if len(text) > MaxFocusLen {
			return nil, fmt.Errorf("focus for %q is %d characters, over the %d limit", name, len(text), MaxFocusLen)
		}
		out[name] = text
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func trimmedNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

func sortedPerspectiveKeys(m map[Perspective]string) []Perspective {
	out := make([]Perspective, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedStringKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
