package hub

import "testing"

func TestSplitCSV(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"empty", "", nil},
		{"single", "foo", []string{"foo"}},
		{"multiple", "a,b,c", []string{"a", "b", "c"}},
		{"whitespace trimmed", " a , b , c ", []string{"a", "b", "c"}},
		{"empty entries skipped", "a,,b,,,c", []string{"a", "b", "c"}},
		{"all empty", ",,,", nil},
		{"single with spaces", "  hello  ", []string{"hello"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitCSV(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("splitCSV(%q) = %v (len %d); want %v (len %d)", tt.input, got, len(got), tt.want, len(tt.want))
			}
			for i, v := range got {
				if v != tt.want[i] {
					t.Errorf("splitCSV(%q)[%d] = %q; want %q", tt.input, i, v, tt.want[i])
				}
			}
		})
	}
}

func TestRepoTargetForgeHost(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty defaults to github.com", "", "github.com"},
		{"whitespace defaults to github.com", "   ", "github.com"},
		{"full https URL", "https://github.com/hivecommons/hive", "github.com"},
		{"GHE URL", "https://github.example.com/org/repo", "github.example.com"},
		{"http URL", "http://gitlab.local/org/repo", "gitlab.local"},
		{"URL with port", "https://git.example.com:8443/repo", "git.example.com:8443"},
		{"bare host with https prefix", "https://git.example.com", "git.example.com"},
		// Without a scheme, url.Parse returns empty Host, so the fallback
		// path returns the full string lowered with slashes trimmed.
		{"no scheme falls through to fallback", "git.example.com/org/repo", "git.example.com/org/repo"},
		{"trailing slash stripped", "https://github.com/", "github.com"},
		{"mixed case lowered", "https://GitHub.COM/org/repo", "github.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := repoTargetForgeHost(tt.input)
			if got != tt.want {
				t.Errorf("repoTargetForgeHost(%q) = %q; want %q", tt.input, got, tt.want)
			}
		})
	}
}
