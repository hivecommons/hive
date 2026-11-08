package climemory

import (
	"strings"
	"testing"
)

// TestValidateCreate pins the create-path provenance guard (#17056): a fact
// tagged cli-memory must be a draft carrying only the tags the drain itself
// writes; anything else passes through untouched.
func TestValidateCreate(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	tests := []struct {
		name    string
		state   string
		tags    []string
		wantErr string
	}{
		{"untagged approved fact passes", "approved", []string{"deploy", "agent:scanner"}, ""},
		{"untagged fact with no state passes", "", nil, ""},
		{"full drain tag set as draft", "draft", []string{SourceTag, "agent:scanner", "cli:claude", HashKey + ":" + hash, "repo:hivecommons/hive"}, ""},
		{"source tag matched case-insensitively and trimmed", "DRAFT", []string{" CLI-Memory ", "cli:codex"}, ""},
		{"gemini cli and short memhash", "draft", []string{SourceTag, "cli:gemini", HashKey + ":" + hash[:16]}, ""},
		{"approved drained fact refused", "approved", []string{SourceTag}, "must be created with state draft"},
		{"missing state refused", "", []string{SourceTag}, "must be created with state draft"},
		{"unknown state refused", "pending", []string{SourceTag}, "must be created with state draft"},
		{"foreign plain tag refused", "draft", []string{SourceTag, "deploy"}, `tag "deploy" is not allowed`},
		{"unknown key refused", "draft", []string{SourceTag, "scope:org"}, `tag "scope:org" is not allowed`},
		{"path-like agent refused", "draft", []string{SourceTag, "agent:../etc"}, "not allowed"},
		{"leading-dot agent refused", "draft", []string{SourceTag, "agent:.hidden"}, "not allowed"},
		{"unknown cli refused", "draft", []string{SourceTag, "cli:copilot"}, "not allowed"},
		{"uppercase memhash refused", "draft", []string{SourceTag, HashKey + ":" + strings.ToUpper(hash)}, "not allowed"},
		{"too-short memhash refused", "draft", []string{SourceTag, HashKey + ":abc123"}, "not allowed"},
		{"repo without owner refused", "draft", []string{SourceTag, "repo:hive"}, "not allowed"},
		{"repo with extra segment refused", "draft", []string{SourceTag, "repo:a/b/c"}, "not allowed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCreate(tc.state, tc.tags)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateCreate(%q, %v) = %v, want nil", tc.state, tc.tags, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateCreate(%q, %v) = %v, want error containing %q", tc.state, tc.tags, err, tc.wantErr)
			}
		})
	}
}
