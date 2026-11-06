package delegatedecision

import (
	"fmt"
	"strconv"
	"strings"
)

// Competing options on a parked issue. An issue that asks its owner to pick
// one of several alternatives (projectbluefin/utah#920: carry the tools in
// the image, a `ujust` recipe, or a -dx flavor) used to fall to
// default-approve, whose "approve the direction" recorded no choice, so the
// claiming agent picked one itself. The delegate now goes with an option only
// when the issue recommends one, and names it in the recorded decision;
// otherwise it leaves the choice to a person.

const (
	// RuleRecommendedOption approves the option the issue itself recommends.
	RuleRecommendedOption = "recommended-option"
	// RuleCompetingOptions refuses: the issue offers options and recommends
	// none, so choosing is the owner's.
	RuleCompetingOptions = "competing-options"
)

// Option is one alternative a parked issue offers its owner: its label as
// the issue writes it ("A", "2") and a one-line summary.
type Option struct {
	Label   string `json:"label"`
	Summary string `json:"summary,omitempty"`
	// Untrusted marks an option read from an issue body a non-hive account
	// wrote: its summary is reporter prose and is never quoted into the
	// owner-attributed decision, which names the option by label alone.
	Untrusted bool `json:"untrusted,omitempty"`
}

// Name is the option as a decision names it: "option 2: A ujust recipe".
func (o Option) Name() string {
	name := "option " + o.Label
	if s := strings.TrimSpace(o.Summary); s != "" && !o.Untrusted {
		name += ": " + s
	}
	return name
}

// competingOptionsDecision decides an issue that offers two or more
// options: it goes with the one the issue recommends, or refuses when the
// issue recommends none. ok is false for an issue offering fewer than two.
func competingOptionsDecision(in Input) (Decision, bool) {
	if len(in.Options) < 2 {
		return Decision{}, false
	}
	ref := "#" + strconv.Itoa(in.Number)
	labels := make([]string, 0, len(in.Options))
	for _, o := range in.Options {
		labels = append(labels, o.Label)
	}
	for _, o := range in.Options {
		if in.RecommendedOption == "" || o.Label != in.RecommendedOption {
			continue
		}
		chosen := o
		d := approveIssue(RuleRecommendedOption, "go with "+chosen.Name(),
			fmt.Sprintf("the issue offers %d options and recommends option %s", len(in.Options), chosen.Label))
		d.Option = &chosen
		return d, true
	}
	return Decision{
		Verdict: VerdictRefused, Rule: RuleCompetingOptions, KeepsPark: true,
		Text: "choose one of " + ref + "'s options with Give direction",
		Rationale: fmt.Sprintf("the issue asks a person to choose between %d options (%s) and recommends none; the hive does not pick one for you",
			len(in.Options), strings.Join(labels, ", ")),
	}, true
}
