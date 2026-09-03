package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeEligibleFixture points mergeEligiblePath at a temp file containing the
// given merge-eligible.json body and restores the original path on cleanup.
func writeEligibleFixture(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "merge-eligible.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	orig := mergeEligiblePath
	mergeEligiblePath = path
	t.Cleanup(func() { mergeEligiblePath = orig })
}

// TestMergeTargetEligible_SHABinding covers M4 (CWE-367): the governor-observed
// head_sha stored in merge-eligible.json must equal the relay's expected SHA.
// An exact match authorizes; a moved head (mismatch), a missing stored SHA, or
// a target absent from the list all fail closed.
func TestMergeTargetEligible_SHABinding(t *testing.T) {
	const goodSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const movedSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	body := `{
		"generated_at": "2026-08-06T00:00:00Z",
		"merge_eligible": [
			{"number": 42, "repo": "hivecommons/hive", "head_sha": "` + goodSHA + `"},
			{"number": 7, "repo": "kubestellar/console", "head_sha": ""}
		]
	}`
	writeEligibleFixture(t, body)

	tests := []struct {
		name      string
		repo      string
		number    int
		expectSHA string
		want      bool
	}{
		{"exact match allows (fully-qualified repo)", "hivecommons/hive", 42, goodSHA, true},
		{"exact match allows (bare repo)", "hive", 42, goodSHA, true},
		{"moved head denied (SHA mismatch)", "hivecommons/hive", 42, movedSHA, false},
		{"empty expected SHA denied", "hivecommons/hive", 42, "", false},
		{"whitespace expected SHA denied", "hivecommons/hive", 42, "   ", false},
		{"stored SHA empty denied", "kubestellar/console", 7, movedSHA, false},
		{"number not in list denied", "hivecommons/hive", 999, goodSHA, false},
		{"repo mismatch denied", "kubestellar/other", 42, goodSHA, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeTargetEligible(tt.repo, tt.number, tt.expectSHA); got != tt.want {
				t.Errorf("mergeTargetEligible(%q, %d, %q) = %v, want %v",
					tt.repo, tt.number, tt.expectSHA, got, tt.want)
			}
		})
	}
}

// TestMergeTargetEligible_FailClosed confirms the fail-closed reads: a missing
// or unparseable list authorizes nothing, even with a plausible expected SHA.
func TestMergeTargetEligible_FailClosed(t *testing.T) {
	const someSHA = "cccccccccccccccccccccccccccccccccccccccc"

	t.Run("missing file denies", func(t *testing.T) {
		orig := mergeEligiblePath
		mergeEligiblePath = filepath.Join(t.TempDir(), "does-not-exist.json")
		t.Cleanup(func() { mergeEligiblePath = orig })
		if mergeTargetEligible("hivecommons/hive", 42, someSHA) {
			t.Error("missing merge-eligible.json must deny (fail closed)")
		}
	})

	t.Run("unparseable file denies", func(t *testing.T) {
		writeEligibleFixture(t, "{ this is not json")
		if mergeTargetEligible("hivecommons/hive", 42, someSHA) {
			t.Error("unparseable merge-eligible.json must deny (fail closed)")
		}
	})
}
