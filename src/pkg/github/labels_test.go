package github

import "testing"

func TestEscalationIssueLabelDefinitions(t *testing.T) {
	for _, name := range []string{"needs-direction", "needs-spec", "needs-signal", "meta"} {
		t.Run(name, func(t *testing.T) {
			def, ok := escalationIssueLabelDefinition(name)
			if !ok {
				t.Fatalf("missing definition for %q", name)
			}
			if def.color == "" {
				t.Fatalf("%q has empty color", name)
			}
			if def.description == "" {
				t.Fatalf("%q has empty description", name)
			}
		})
	}
}
