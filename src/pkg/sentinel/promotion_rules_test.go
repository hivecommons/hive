package sentinel

import "testing"

// A branch-promotion pull request (auto_merge.promotions) is exempt from
// sensitive_path only: every rule that describes what the content does, and
// owner_self_nomination, still applies. A rule added later is not exempt
// unless someone adds it here on purpose.
func TestPromotionExemptRulesAreSensitivePathOnly(t *testing.T) {
	for _, rule := range AllRules {
		if want := rule == RuleSensitivePath; PromotionExemptRules[rule] != want {
			t.Errorf("PromotionExemptRules[%q] = %v, want %v", rule, PromotionExemptRules[rule], want)
		}
	}
	if len(PromotionExemptRules) != 1 {
		t.Fatalf("PromotionExemptRules = %v, want only sensitive_path", PromotionExemptRules)
	}
	kept, dropped := WithoutPromotionExempt([]Finding{{Rule: RuleSensitivePath}, {Rule: RulePermissionEscalation}, {Rule: RuleOwnerSelfNomination}})
	if len(dropped) != 1 || dropped[0].Rule != RuleSensitivePath || len(kept) != 2 || kept[0].Rule != RulePermissionEscalation || kept[1].Rule != RuleOwnerSelfNomination {
		t.Fatalf("kept=%+v dropped=%+v", kept, dropped)
	}
}
