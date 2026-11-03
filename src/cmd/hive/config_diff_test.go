package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigDiffDispatchAndStrictMode(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(map[bool]string{false: "warn", true: "refuse"}[strict], func(t *testing.T) {
			dir := t.TempDir()
			seed, prior := filepath.Join(dir, "seed"), filepath.Join(dir, "prior")
			body := "project: {org: example}\n"
			if strict {
				body += "config: {refuse_gate_regression: true}\n"
			}
			if err := os.WriteFile(seed, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(prior, []byte("review: {require_labels: [hive-review]}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			handled, code := dispatchSubcommand([]string{"config", "diff", "--config", seed, "--json", prior}, &stdout, &stderr)
			if !handled || (code != 0) != strict {
				t.Fatalf("handled=%v code=%d: %s", handled, code, stderr.String())
			}
			var warnings []string
			if err := json.Unmarshal(stdout.Bytes(), &warnings); err != nil || len(warnings) != 1 {
				t.Fatalf("warnings=%v err=%v stdout=%s", warnings, err, stdout.String())
			}
			if !strings.Contains(stderr.String(), "WARN safety gate dropped: review.require_labels") {
				t.Fatal(stderr.String())
			}
			if strings.Contains(stderr.String(), "refusing to start") != strict {
				t.Fatal(stderr.String())
			}
		})
	}
}
