package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestPlanLaysCompanionsOutUnderRoot(t *testing.T) {
	got := PlanCompanionCheckouts("$HIVE_WORKSPACE_DIR/", "hivecommons/spektacular", []string{
		"hivecommons/spektacular-website",
		" HiveCommons/Spektacular-Website ",
		"hivecommons/spektacular",
		"acme/docs/",
		"bare",
		"",
		"../escape",
		"org/../up",
		"a/b/c",
	})
	want := []CompanionCheckout{
		{Repo: "hivecommons/spektacular-website", Path: "$HIVE_WORKSPACE_DIR/hivecommons/spektacular-website"},
		{Repo: "acme/docs", Path: "$HIVE_WORKSPACE_DIR/acme/docs"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Plan = %+v, want %+v", got, want)
	}
}

func TestPlanIsNilWithoutRootOrCompanions(t *testing.T) {
	if got := PlanCompanionCheckouts("", "o/r", []string{"o/c"}); got != nil {
		t.Errorf("no root: %+v", got)
	}
	if got := PlanCompanionCheckouts("/home/dev/workspace", "o/r", nil); got != nil {
		t.Errorf("no companions: %+v", got)
	}
	if got := PlanCompanionCheckouts("/home/dev/workspace", "o/r", []string{"o/r", "junk"}); got != nil {
		t.Errorf("only self and junk: %+v", got)
	}
}

func TestCloneCommandIsShallowIdempotentAndHostAware(t *testing.T) {
	c := CompanionCheckout{Repo: "hivecommons/spektacular-website", Path: "/home/dev/workspace/hivecommons/spektacular-website"}
	got := c.CloneCommand("https://github.ibm.com/")
	want := "[ -d /home/dev/workspace/hivecommons/spektacular-website/.git ] || git clone --depth 1 https://github.ibm.com/hivecommons/spektacular-website /home/dev/workspace/hivecommons/spektacular-website"
	if got != want {
		t.Fatalf("CloneCommand =\n%s\nwant\n%s", got, want)
	}
	if got := c.CloneCommand(""); !strings.Contains(got, "https://github.com/hivecommons/spektacular-website") {
		t.Fatalf("empty host should default to github.com: %s", got)
	}
}

func TestInstructionRendersEveryCloneAndTheNoParkRule(t *testing.T) {
	plan := PlanCompanionCheckouts("$HIVE_WORKSPACE_DIR", "hivecommons/spektacular", []string{"hivecommons/spektacular-website", "hivecommons/spektacular-specs"})
	got := CompanionCheckoutInstruction("hivecommons/spektacular", "https://github.com", plan)
	for _, want := range []string{
		"hivecommons/spektacular registers 2 companion repositories",
		"git clone --depth 1 https://github.com/hivecommons/spektacular-website $HIVE_WORKSPACE_DIR/hivecommons/spektacular-website",
		"git clone --depth 1 https://github.com/hivecommons/spektacular-specs $HIVE_WORKSPACE_DIR/hivecommons/spektacular-specs",
		"never park",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Instruction lacks %q:\n%s", want, got)
		}
	}
	if one := CompanionCheckoutInstruction("o/r", "", plan[:1]); !strings.Contains(one, "1 companion repository ") {
		t.Errorf("singular form: %s", one)
	}
	if CompanionCheckoutInstruction("o/r", "", nil) != "" {
		t.Error("empty plan must render nothing")
	}
}
