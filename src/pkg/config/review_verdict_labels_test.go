package config

import (
	"strings"
	"testing"
)

func TestReviewVerdictLabelsValidate(t *testing.T) {
	ok := ReviewVerdictLabelsConfig{
		Reviewing:        "hive-review/in-progress",
		Approve:          "hive-review/approved",
		ChangesRequested: "hive-review/changes-requested",
		RequiresHuman:    "hive-review/needs-human",
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid labels rejected: %v", err)
	}
	if !ok.Enabled() || len(ok.Labels()) != 4 {
		t.Fatalf("Enabled=%v Labels=%v", ok.Enabled(), ok.Labels())
	}
	if (ReviewVerdictLabelsConfig{}).Enabled() {
		t.Fatal("empty block must be off")
	}
	if err := (ReviewVerdictLabelsConfig{Approve: "  "}).Validate(); err != nil {
		t.Fatalf("blank key should be ignored: %v", err)
	}
	for name, bad := range map[string]ReviewVerdictLabelsConfig{
		"duplicate": {Approve: "done", RequiresHuman: "Done"},
		"comma":     {Reviewing: "a,b"},
		"too long":  {Approve: strings.Repeat("x", 51)},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
