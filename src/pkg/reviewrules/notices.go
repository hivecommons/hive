package reviewrules

import (
	"fmt"
	"strings"
)

const (
	// AckMarkerPrefix opens the hidden marker of the acknowledgement comment.
	AckMarkerPrefix = "<!-- hive:review-rule-ack"
	// HomeMarkerPrefix opens the hidden marker of the foreign-home proposal.
	HomeMarkerPrefix = "<!-- hive:review-rule-home"
)

// fenced quotes text as data inside a fenced block that the text cannot close.
func fenced(text string) string {
	return "```text\n" + strings.ReplaceAll(DefuseMentions(text), "```", "'''") + "\n```\n"
}

// AckComment is the reply the reviewer sees on the PR when rules from their
// comments were captured: which rule id, where it applies, the recorded text,
// and how to object. Rule text is quoted as data and raw @handles are defused.
func AckComment(acks []Ack) string {
	var b strings.Builder
	var ids []string
	for _, a := range acks {
		ids = append(ids, a.Rule.ID)
	}
	fmt.Fprintf(&b, "%s ids=%s -->\n", AckMarkerPrefix, strings.Join(ids, ","))
	for _, a := range acks {
		r := a.Rule
		if a.Merged {
			fmt.Fprintf(&b, "This restates rule %s, which the hive already recorded in its knowledge and applies to %s. Your comment (%s) was added to that rule's sources.\n\n", r.ID, r.Describe(), a.URL)
		} else {
			fmt.Fprintf(&b, "Recorded as rule %s in the hive's knowledge, applies to %s. Source: %s\n\n", r.ID, r.Describe(), a.URL)
		}
		b.WriteString(fenced(r.Text))
		b.WriteString("\n")
	}
	b.WriteString("Recorded rules are loaded into the matching agent's next runs, so they hold for future work and not only for this PR. If a rule is not what you meant, reply here and say so; an operator can retract it with `hivectl knowledge state <entry> deprecated` (the entry id is `rule-` followed by the lowercase rule id, for example `rule-r-1`).\n")
	return b.String()
}

// HomeProposalNote is the one needs-human note for a rule whose proper home is
// a repo the hive does not manage. It carries the exact proposed text and the
// source comment; the hive never writes to that repo.
func HomeProposalNote(r *Rule) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s rule=%s -->\n", HomeMarkerPrefix, r.ID)
	fmt.Fprintf(&b, "Needs a human: reviewer feedback here states a rule whose proper home is `%s`'s own files (for example its assessment criteria), and the hive does not write to that repo. The hive has recorded it in its own knowledge as rule %s so its own work follows it. To make it part of the project, a maintainer can add this text where the project keeps such criteria:\n\n", r.ForeignHome, r.ID)
	b.WriteString(fenced(r.Text))
	if len(r.Provenance) > 0 {
		fmt.Fprintf(&b, "\nSource: %s\n", r.Provenance[0].CommentURL)
	}
	return b.String()
}
