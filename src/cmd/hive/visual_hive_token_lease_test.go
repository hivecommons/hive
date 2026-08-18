package main

import "testing"

func TestVisualHiveGitHubAppBrokerRequiresExplicitPerInstanceOptIn(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
	}{
		{value: "", want: false},
		{value: "false", want: false},
		{value: "1", want: false},
		{value: " true ", want: true},
		{value: "TRUE", want: true},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv(visualHiveGitHubAppEnabledEnv, test.value)
			if got := visualHiveGitHubAppBrokerEnabled(); got != test.want {
				t.Fatalf("enabled=%t, want %t for %q", got, test.want, test.value)
			}
		})
	}
}
