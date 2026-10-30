package writeback

import "testing"

// hivecommons/hive#15499: the same decision question, reworded by a model
// on each of six heads, must hash to one identity.
func TestDecisionKeyIgnoresRewording(t *testing.T) {
	a := "@clubanderson #15499 has an empty diff against v5 because #15427 already landed the fix. Should this be closed as a no-op?"
	b := "@clubanderson: the diff of #15499 vs v5 is empty (fix landed in #15427 / #15449) — close this PR as a no-op?"
	if DecisionKey(15427, a) != "redundant-of:15427" || DecisionKey(15427, b) != DecisionKey(15427, a) {
		t.Fatalf("redundant-of key: %q / %q", DecisionKey(15427, a), DecisionKey(15427, b))
	}
	if DecisionKey(0, a) == "" || DecisionKey(0, "@someone #1 #2 2026") != "" {
		t.Fatalf("question keys: %q / %q", DecisionKey(0, a), DecisionKey(0, "@someone #1 #2 2026"))
	}
	if DecisionKey(0, "rename flag --foo to --bar?") == DecisionKey(0, "close this as a no-op?") {
		t.Fatal("different questions must not share a key")
	}
}

func TestDecisionKeyFromBodyAndContentDedupe(t *testing.T) {
	body := func(q string) string {
		return "<!-- hive-verdict: needs-decision -->\n<!-- hive-redundant-of: #15427 -->\n<!-- hive-decision-question: " + q + " -->\n**Decision needed**\n\n" + q
	}
	k1, ok1 := DecisionKeyFromBody(body("@clubanderson close #15499 as a no-op? #15427 already landed it."))
	k2, ok2 := DecisionKeyFromBody(body("@clubanderson: #15499's tree matches v5 after #15427 — should it be closed as a no-op?"))
	if !ok1 || !ok2 || k1 != "redundant-of:15427" || k1 != k2 {
		t.Fatalf("keys: %q(%v) %q(%v)", k1, ok1, k2, ok2)
	}
	if normalizeSameItem(body("one wording")) != normalizeSameItem(body("another wording entirely")) {
		t.Fatal("content dedupe must collapse reworded decision bodies with the same redundant-of")
	}
	if _, ok := DecisionKeyFromBody("<!-- hive-redundant-of: #15427 -->\nplain comment"); ok {
		t.Fatal("no needs-decision marker: not a decision body")
	}
	if k, ok := DecisionKeyFromBody("<!-- hive-verdict: needs-decision -->\n<!-- hive-decision-question: keep the helper in the same file or split it out? -->"); !ok || k != "question:keep helper same file split out" {
		t.Fatalf("question-only key = %q (%v)", k, ok)
	}
}
