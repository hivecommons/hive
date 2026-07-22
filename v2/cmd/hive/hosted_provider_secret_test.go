package main

import (
	"testing"

	"github.com/kubestellar/hive/v2/pkg/integrated"
)

func TestHostedAutomationRequiresProviderSecret(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		automation integrated.Automation
		want       bool
	}{
		{name: "advisory", automation: integrated.AutomationAdvisory},
		{name: "issues", automation: integrated.AutomationIssues},
		{name: "repair pr", automation: integrated.AutomationRepairPR, want: true},
		{name: "auto merge", automation: integrated.AutomationAutoMerge, want: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := hostedAutomationRequiresProviderSecret(test.automation); got != test.want {
				t.Fatalf("hostedAutomationRequiresProviderSecret(%q) = %t, want %t", test.automation, got, test.want)
			}
		})
	}
}
