package scheduler

import (
	"strings"
	"testing"
)

func TestAddConcreteKickClosingInstructionEndsPrompt(t *testing.T) {
	got := addConcreteKickClosingInstruction("role text")
	if !strings.HasSuffix(got, concreteKickClosingInstruction) {
		t.Fatalf("closing instruction was not the final prompt text:\n%s", got)
	}
	if twice := addConcreteKickClosingInstruction(got); twice != got {
		t.Fatal("closing instruction was duplicated")
	}
}
