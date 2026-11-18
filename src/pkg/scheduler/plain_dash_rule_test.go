package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/prosefilter"
)

func TestAddPlainDashRuleAddsOnceUnderHeader(t *testing.T) {
	got := addPlainDashRule("# KICK\nbody")
	if !strings.HasPrefix(got, "# KICK\n\n"+plainDashRule+"\n") {
		t.Fatalf("rule not directly under the header:\n%s", got)
	}
	if twice := addPlainDashRule(got); twice != got {
		t.Fatal("rule was duplicated")
	}
	if addPlainDashRule("") != "" {
		t.Fatal("an empty (fail-closed) kick must stay empty")
	}
}

func TestPlainDashRuleIsItselfDashFree(t *testing.T) {
	if prosefilter.HasDash(plainDashRule) {
		t.Fatalf("the no-em-dash rule must not contain a dash itself: %q", plainDashRule)
	}
}
