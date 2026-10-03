package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestTaskStatusPushEnabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		env  string
		want bool
	}{
		{"default", "{}", "", true},
		{"explicit on", "task_status_push: true", "", true},
		{"explicit off", "task_status_push: false", "", false},
		{"env off overrides on", "task_status_push: true", "false", false},
		{"env on overrides off", "task_status_push: false", "true", true},
		{"env off overrides default", "{}", "off", false},
		{"invalid env uses config", "task_status_push: false", "typo", false},
		{"invalid env uses default", "{}", "typo", true},
		{"normalized env", "task_status_push: true", " NO ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(TaskStatusPushEnvVar, tc.env)
			var h HubConfig
			if err := yaml.Unmarshal([]byte(tc.yaml), &h); err != nil {
				t.Fatal(err)
			}
			if got := h.TaskStatusPushEnabled(); got != tc.want {
				t.Fatalf("TaskStatusPushEnabled() = %v, want %v", got, tc.want)
			}
			// Saving and reloading must preserve explicit false, not turn it
			// into the default-on absent value.
			data, err := yaml.Marshal(h)
			if err != nil {
				t.Fatal(err)
			}
			var reloaded HubConfig
			if err := yaml.Unmarshal(data, &reloaded); err != nil {
				t.Fatal(err)
			}
			if got := reloaded.TaskStatusPushEnabled(); got != tc.want {
				t.Fatalf("after YAML round trip = %v, want %v", got, tc.want)
			}
		})
	}
}
