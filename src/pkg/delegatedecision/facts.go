package delegatedecision

import (
	"strconv"
	"strings"
)

// Fact is one thing the delegate read about an item, in plain words. The
// "Let hive decide" preview lists them under "What hive saw" so the owner
// can check the decision against its inputs instead of taking the one-line
// reason on trust.
type Fact struct {
	Label string `json:"label"`
	Value string `json:"value"`
	// State is FactOK when the fact does not hold the item back,
	// FactBlocks when it does, and FactInfo when it only steers which
	// decision is made.
	State string `json:"state"`
}

// Fact states.
const (
	FactOK     = "ok"
	FactBlocks = "blocks"
	FactInfo   = "info"
)

// Facts lists what the decision rules read from in, in the order an owner
// checks them: who wrote it, whether the head can merge, what review and
// Sentinel said, then whatever else steered the decision. Only facts the
// rules use are listed, and a fact that does not apply to the item's kind
// is left out.
func (in Input) Facts() []Fact {
	var out []Fact
	add := func(label, value, state string) { out = append(out, Fact{Label: label, Value: value, State: state}) }
	pr := in.Kind == KindPR

	author := "unknown"
	if a := strings.TrimSpace(in.Author); a != "" {
		author = "@" + a
	}
	switch {
	case in.HiveAuthored:
		add("Author", "the hive", FactInfo)
	case in.OwnerAuthor:
		add("Author", author+", an owner of this hive", FactInfo)
	case in.TrustedAuthor:
		add("Author", author+", trusted (write access)", FactInfo)
	default:
		add("Author", author+", not trusted", FactInfo)
	}
	if pr && !in.HiveAuthored && !in.PersonAuthored() {
		who := in.AuthorWord()
		switch {
		case in.ReworkPickedUp:
			add("Contributor", who+" has picked the rework up", FactInfo)
		case in.ContributorOnline:
			add("Contributor", who+" is connected", FactInfo)
		default:
			add("Contributor", in.OfflineWording(), FactInfo)
		}
	}

	if pr {
		switch {
		case in.CIGreen:
			add("CI", "green", FactOK)
		case in.CIPending:
			add("CI", "still running, nothing failed", FactInfo)
		default:
			add("CI", "not green", FactBlocks)
		}
		if in.Conflicting {
			add("Conflicts", "conflicts with the base branch", FactBlocks)
		} else {
			add("Conflicts", "none", FactOK)
		}
		if in.DCOMissing {
			add("Sign-off", "DCO sign-off missing", FactBlocks)
		} else {
			add("Sign-off", "signed off", FactOK)
		}
		switch {
		case in.ChangeRequest() != "":
			add("Review", in.ChangeRequest()+" still stand", FactBlocks)
		case in.HeadNeedsPerson():
			add("Review", "the hive's review asks a person to decide", FactBlocks)
		case strings.TrimSpace(in.HeadReviewVerdict) != "":
			add("Review", "the hive's review on this head: "+strings.ReplaceAll(strings.TrimSpace(in.HeadReviewVerdict), "_", " "), FactOK)
		default:
			add("Review", "no review verdict on this head", FactInfo)
		}
		if f := strings.TrimSpace(in.ReviewerFinding); f != "" {
			if in.SecurityFinding {
				f += " (security review)"
			}
			add("Review finding", f, FactBlocks)
		}
	}

	if in.SentinelAlert {
		reason := strings.TrimSpace(in.SentinelReason)
		if reason == "" {
			reason = "flagged security-relevant changes"
		}
		add("Sentinel", reason, FactBlocks)
	} else {
		add("Sentinel", "no alert", FactOK)
	}

	if pr {
		var shape []string
		if in.Small {
			shape = append(shape, "small (size XS or S)")
		}
		if in.LowRiskPaths {
			shape = append(shape, "only CI config, workflow, docs, text or read-only RBAC files")
		}
		if in.DiffEmpty {
			shape = append(shape, "empty against the base branch")
		}
		if len(shape) > 0 {
			add("Change", strings.Join(shape, "; "), FactInfo)
		}
		if in.WorkflowPath {
			if in.WorkflowsPushable {
				add("Workflow files", "edits .github/workflows; the hive can push them here", FactInfo)
			} else {
				add("Workflow files", "edits .github/workflows; the hive cannot push them here", FactBlocks)
			}
		}
		if c := in.SupersededBy; c != nil && c.PR > 0 {
			add("Already landed", SupersededText(*c), FactInfo)
		}
	}

	if in.DuplicateOf > 0 {
		add("Duplicate", "duplicates #"+strconv.Itoa(in.DuplicateOf), FactInfo)
	}
	if len(in.Overlaps) > 0 {
		add("Overlaps", refList(in.Overlaps), FactInfo)
	}
	var others []int
	for _, c := range in.Competing {
		if !c.Current && c.Number != in.Number {
			others = append(others, c.Number)
		}
	}
	if len(others) > 0 {
		add("Other PRs for the same work", refList(others), FactInfo)
	}
	if in.TrainDuplicateOf > 0 {
		add("Merge queue", "held as a duplicate of #"+strconv.Itoa(in.TrainDuplicateOf), FactBlocks)
	}
	if in.ClaimConflictWith > 0 {
		add("Merge queue", "#"+strconv.Itoa(in.ClaimConflictWith)+" claims the same issue", FactBlocks)
	}

	if pr {
		switch {
		case strings.TrimSpace(in.AdoptedPushRefused) != "":
			add("Adoption", "the hive cannot push this branch: "+strings.TrimSpace(in.AdoptedPushRefused), FactBlocks)
		case in.AdoptedByHive:
			add("Adoption", "adopted by the hive; its fixer owns the branch", FactInfo)
		case in.AdoptionStalled:
			add("Adoption", "adoption stalled on a lane blocker", FactBlocks)
		case in.AdoptionGraceElapsed:
			add("Adoption", "the offline grace has run out", FactInfo)
		}
		if in.ReporterTrustHold {
			add("Reporter trust", "the reporter-trust gate holds this fix", FactBlocks)
		}
	}
	if in.RepoPaused {
		add("Repository", "paused by the operator; the hive only recommends", FactInfo)
	}
	if q := strings.TrimSpace(in.Question); q != "" {
		add("Question for a person", q, FactInfo)
	}
	if len(in.Options) > 0 {
		labels := make([]string, len(in.Options))
		for i, o := range in.Options {
			labels[i] = o.Label
		}
		add("Options offered", strings.Join(labels, ", "), FactInfo)
	}
	if r := strings.TrimSpace(in.RecommendedOption); r != "" {
		add("Recommended option", r, FactInfo)
	}
	return out
}

func refList(ns []int) string {
	refs := make([]string, len(ns))
	for i, n := range ns {
		refs[i] = "#" + strconv.Itoa(n)
	}
	return strings.Join(refs, ", ")
}

// WhyNot is one option the ladder ruled out and the fact that ruled it out.
type WhyNot struct {
	Option  string `json:"option"`
	Because string `json:"because"`
}

// rungOptions names each ladder rung as the option an owner would have
// picked by hand.
var rungOptions = map[string]string{
	RuleAlreadyFixed:        "Close as already fixed",
	RuleMergeableAsIs:       "Approve as it is",
	RuleRebaseThenMerge:     "Rebase, then merge",
	RuleReturnToContributor: "Send it back to the contributor",
	RuleContributorOffline:  "Adopt it",
}

// WhyNot turns RuledOut ("rung: reason") into the options it ruled out, in
// the owner's words. Nil for a nil ladder.
func (l *Ladder) WhyNot() []WhyNot {
	if l == nil {
		return nil
	}
	out := make([]WhyNot, 0, len(l.RuledOut))
	for _, r := range l.RuledOut {
		rung, because, _ := strings.Cut(r, ": ")
		option, ok := rungOptions[rung]
		if !ok {
			option = Plain(rung)
		}
		out = append(out, WhyNot{Option: option, Because: strings.TrimSpace(because)})
	}
	return out
}
