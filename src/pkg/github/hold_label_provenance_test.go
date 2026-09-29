package github

import "testing"

// A provenance label whose hive ID happens to contain "hold" must not read as
// a hold label. Live case: hive/hosted-available-oke-11-placeholder-r05x
// parked 69 items on one spoke (2026-09-28).
func TestHasHoldLabel_ProvenanceLabelIsNeverHold(t *testing.T) {
	hiveID := "hosted-available-oke-11-placeholder-r05x"
	prov := HiveProvenanceLabel(hiveID)
	if HasHoldLabel([]string{prov, "kind/bug"}) {
		t.Fatalf("%s must not classify as hold", prov)
	}
	if HasHoldLabelWith([]string{prov}, []string{CanonicalHiveHoldLabel(hiveID)}) {
		t.Fatalf("%s must not classify as hold even with the hive-pause label configured", prov)
	}
	// The real hold spellings still work beside it.
	if !HasHoldLabel([]string{prov, "hold"}) {
		t.Fatal("bare hold label must still classify")
	}
	if !HasHoldLabel([]string{prov, "do-not-merge/hold"}) {
		t.Fatal("substring hold label must still classify")
	}
	if !HasHoldLabelWith([]string{prov, CanonicalHiveHoldLabel(hiveID)}, []string{CanonicalHiveHoldLabel(hiveID)}) {
		t.Fatal("configured hive-pause label must still classify")
	}
}
