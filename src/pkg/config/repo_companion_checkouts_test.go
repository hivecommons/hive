package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestPlanLaysCompanionsOutUnderRoot(t *testing.T) {
	got := PlanCompanionCheckouts("$HIVE_WORKSPACE_DIR/", "acme/widgets", []string{
		"acme/widgets-docs",
		" Acme/Widgets-Docs ",
		"acme/widgets",
		"acme/docs/",
		"bare",
		"",
		"../escape",
		"org/../up",
		"a/b/c",
	})
	want := []CompanionCheckout{
		{Repo: "acme/widgets-docs", Path: "$HIVE_WORKSPACE_DIR/acme/widgets-docs"},
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
	c := CompanionCheckout{Repo: "acme/widgets-docs", Path: "/home/dev/workspace/acme/widgets-docs"}
	got := c.CloneCommand("https://github.ibm.com/")
	want := "[ -d /home/dev/workspace/acme/widgets-docs/.git ] || git clone --depth 1 https://github.ibm.com/acme/widgets-docs /home/dev/workspace/acme/widgets-docs"
	if got != want {
		t.Fatalf("CloneCommand =\n%s\nwant\n%s", got, want)
	}
	if got := c.CloneCommand(""); !strings.Contains(got, "https://github.com/acme/widgets-docs") {
		t.Fatalf("empty host should default to github.com: %s", got)
	}
}

func TestInstructionRendersEveryCloneAndTheNoParkRule(t *testing.T) {
	plan := PlanCompanionCheckouts("$HIVE_WORKSPACE_DIR", "acme/widgets", []string{"acme/widgets-docs", "acme/widgets-specs"})
	got := CompanionCheckoutInstruction("acme/widgets", "https://github.com", plan)
	for _, want := range []string{
		"acme/widgets registers 2 companion repositories",
		"git clone --depth 1 https://github.com/acme/widgets-docs $HIVE_WORKSPACE_DIR/acme/widgets-docs",
		"git clone --depth 1 https://github.com/acme/widgets-specs $HIVE_WORKSPACE_DIR/acme/widgets-specs",
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
