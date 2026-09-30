package github

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeEnforcedLanes(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nothing enforced", nil, []string{}},
		{"trimmed and lower-cased", []string{" Scanner ", "REVIEWER"}, []string{"reviewer", "scanner"}},
		{"duplicates collapse", []string{"scanner", "scanner", " scanner"}, []string{"scanner"}},
		{"empties dropped", []string{"", "  ", "scanner"}, []string{"scanner"}},
		{"star absorbs the rest", []string{"scanner", "*", "reviewer"}, []string{"*"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeEnforcedLanes(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("NormalizeEnforcedLanes(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("NormalizeEnforcedLanes(%q) = %q, want %q", tc.in, got, tc.want)
				}
			}
		})
	}
}

func TestPublishWriteSurfaceEnforce_WritesReadableDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "write-surface-enforce.json")
	if err := PublishWriteSurfaceEnforce(path, []string{" Scanner ", "reviewer"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var doc WriteSurfaceEnforceFile
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("published document is not JSON: %v (%s)", err, data)
	}
	if doc.Version != WriteSurfaceEnforceFileVersion {
		t.Errorf("version = %d, want %d", doc.Version, WriteSurfaceEnforceFileVersion)
	}
	if doc.UpdatedAt == "" {
		t.Error("updated_at is empty - an operator cannot tell a stale list from a current one")
	}
	if len(doc.Lanes) != 2 || doc.Lanes[0] != "reviewer" || doc.Lanes[1] != "scanner" {
		t.Errorf("lanes = %q, want normalized [reviewer scanner]", doc.Lanes)
	}
	// The gh wrapper parses this file with bash builtins alone, keyed on the
	// version and the lanes array. Encoding them apart (or renaming either)
	// would leave every sandbox check silently un-armed.
	if !strings.Contains(string(data), `"version":1,`) {
		t.Errorf("published document must carry the version the wrapper matches: %s", data)
	}
	if !strings.Contains(string(data), `"lanes":[`) {
		t.Errorf("published document must carry a lanes array the wrapper can read: %s", data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != writeSurfaceEnforceFilePerms {
		t.Errorf("mode = %v, want %v - every agent UID must be able to READ the list and none to write it",
			perm, os.FileMode(writeSurfaceEnforceFilePerms))
	}
}

// Publishing is called at boot and on every config reload, so it must replace
// the previous list rather than accumulate - including when the operator
// removed the last enforced lane, which is how enforcement is turned OFF.
func TestPublishWriteSurfaceEnforce_ReplacesPreviousList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "write-surface-enforce.json")
	if err := PublishWriteSurfaceEnforce(path, []string{"scanner"}); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if err := PublishWriteSurfaceEnforce(path, nil); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var doc WriteSurfaceEnforceFile
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("published document is not JSON: %v (%s)", err, data)
	}
	if len(doc.Lanes) != 0 {
		t.Errorf("lanes = %q, want none - unlisting the last lane must turn enforcement off", doc.Lanes)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the published file - a temp file was left behind", len(entries))
	}
}
