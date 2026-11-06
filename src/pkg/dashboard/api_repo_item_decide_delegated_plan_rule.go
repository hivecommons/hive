package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// delegatedPlanRule is the rule that decided, in plain words, and its whole
// reason with issue references stripped; the reason is left out when it
// says no more than the dialog's one-line Why.
func delegatedPlanRule(in delegatedecision.Input, d delegatedecision.Decision, why string) (rule, rationale string) {
	if d.Rule != "" {
		rule = delegatedecision.PlainOn(d.Rule, in.Kind)
	}
	rationale = strings.TrimSpace(delegatedPlanIssueRef.ReplaceAllString(strings.TrimSpace(d.Rationale), ""))
	rationale = strings.Join(strings.Fields(rationale), " ")
	if strings.EqualFold(strings.TrimSuffix(rationale, "."), strings.TrimSuffix(strings.TrimSpace(why), ".")) {
		rationale = ""
	}
	return rule, rationale
}
