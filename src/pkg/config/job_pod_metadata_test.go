package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJobPodMetadataRejectedOnLoad(t *testing.T) {
	seed := "project:\n  org: testorg\ngithub:\n  token: ghp_test123456789\n"
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"reserved global", "agent_sandbox:\n  job:\n    pod_labels:\n      job-name: stolen\n", "job-name"},
		{"invalid agent value", "agents:\n  quality:\n    sandbox:\n      job:\n        pod_labels:\n          example.com/access: 'not valid'\n", "example.com/access"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hive.yaml")
			if err := os.WriteFile(path, []byte(seed+tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadWithOverrides(path, "-"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("load error = %v, want %q", err, tc.want)
			}
		})
	}
}
