package hub

import (
	"testing"
)

func TestFirstCSV(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"foo,bar,baz", "foo"},
		{" foo , bar", "foo"},
		{",,,hello", "hello"},
		{"", ""},
		{",,,", ""},
		{"single", "single"},
		{" , , spaced ", "spaced"},
	}
	for _, tc := range tests {
		got := firstCSV(tc.input)
		if got != tc.want {
			t.Errorf("firstCSV(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestReplaceFirstCSV(t *testing.T) {
	tests := []struct {
		s, first string
		want     string
	}{
		{"a,b,c", "X", "X,b,c"},
		{" a , b", "X", "X, b"},
		{"a,b,c", "", "b,c"},          // empty replacement removes first
		{"", "X", "X"},                 // empty input returns replacement
		{",,,", "X", "X"},              // all-empty returns replacement
		{"hello", "world", "world"},    // single value replaced
		{" , second", "first", " ,first"},
	}
	for _, tc := range tests {
		got := replaceFirstCSV(tc.s, tc.first)
		if got != tc.want {
			t.Errorf("replaceFirstCSV(%q, %q) = %q, want %q", tc.s, tc.first, got, tc.want)
		}
	}
}

func TestLooksLikeGitHubForgeHost(t *testing.T) {
	tests := []struct {
		label string
		want  bool
	}{
		{"github.com", true},
		{"github.ibm.com", true},
		{"GitHub.Com", true},            // case-insensitive
		{"github.example.org", true},
		{"github.", false},              // trailing dot, no domain
		{"gitlab.com", false},           // not github prefix
		{"my-org", false},               // plain org
		{"", false},
		{"github.com.", false},          // trailing dot
	}
	for _, tc := range tests {
		got := looksLikeGitHubForgeHost(tc.label)
		if got != tc.want {
			t.Errorf("looksLikeGitHubForgeHost(%q) = %v, want %v", tc.label, got, tc.want)
		}
	}
}

func TestNormalizeRepoRef(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://github.ibm.com/z-aiops-unite/ui", "ui"},
		{"http://github.com/org/repo", "repo"},
		{"z-aiops-unite/ui", "ui"},
		{"ui", "ui"},
		{"https://github.com/org/repo.git", "repo"},
		{"  repo  ", "repo"},
		{"", ""},
		{"https://github.com/org/repo/", "repo"},
	}
	for _, tc := range tests {
		got := normalizeRepoRef(tc.input)
		if got != tc.want {
			t.Errorf("normalizeRepoRef(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestIsAvailableRegistryEntry(t *testing.T) {
	tests := []struct {
		name string
		h    RegistryEntry
		want bool
	}{
		{
			name: "explicit available status",
			h:    RegistryEntry{ProvStatus: statusAvailable},
			want: true,
		},
		{
			name: "placeholder org prefix",
			h:    RegistryEntry{Org: placeholderOrgPrefix + "pool-1"},
			want: true,
		},
		{
			name: "assigned hive",
			h:    RegistryEntry{ProvStatus: "assigned", Org: "real-org"},
			want: false,
		},
		{
			name: "empty entry",
			h:    RegistryEntry{},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isAvailableRegistryEntry(tc.h)
			if got != tc.want {
				t.Errorf("isAvailableRegistryEntry(%+v) = %v, want %v", tc.h, got, tc.want)
			}
		})
	}
}

func TestSanitizePublicURLSelfCheck(t *testing.T) {
	tests := []struct {
		name string
		in   *PublicURLSelfCheck
		want *PublicURLSelfCheck
	}{
		{
			name: "nil input",
			in:   nil,
			want: nil,
		},
		{
			name: "valid ok check",
			in:   &PublicURLSelfCheck{Status: PublicURLSelfCheckOK, HTTPStatus: 401, CheckedAt: "2024-01-01T00:00:00Z"},
			want: &PublicURLSelfCheck{Status: PublicURLSelfCheckOK, HTTPStatus: 401, CheckedAt: "2024-01-01T00:00:00Z"},
		},
		{
			name: "valid fail check",
			in:   &PublicURLSelfCheck{Status: PublicURLSelfCheckFail, HTTPStatus: 503, Error: "timeout"},
			want: &PublicURLSelfCheck{Status: PublicURLSelfCheckFail, HTTPStatus: 503, Error: "timeout"},
		},
		{
			name: "invalid status rejected",
			in:   &PublicURLSelfCheck{Status: "hacked<script>"},
			want: nil,
		},
		{
			name: "http status clamped",
			in:   &PublicURLSelfCheck{Status: PublicURLSelfCheckOK, HTTPStatus: 9999},
			want: &PublicURLSelfCheck{Status: PublicURLSelfCheckOK, HTTPStatus: 599},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizePublicURLSelfCheck(tc.in)
			if tc.want == nil {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil result")
			}
			if got.Status != tc.want.Status {
				t.Errorf("Status = %q, want %q", got.Status, tc.want.Status)
			}
			if got.HTTPStatus != tc.want.HTTPStatus {
				t.Errorf("HTTPStatus = %d, want %d", got.HTTPStatus, tc.want.HTTPStatus)
			}
		})
	}
}

func TestSanitizeRouteExistenceCheck(t *testing.T) {
	tests := []struct {
		name string
		in   *RouteExistenceCheck
		want *RouteExistenceCheck
	}{
		{
			name: "nil input",
			in:   nil,
			want: nil,
		},
		{
			name: "valid found check",
			in:   &RouteExistenceCheck{Status: RouteExistenceFound, Host: "myhive.example.com", Kind: "Ingress"},
			want: &RouteExistenceCheck{Status: RouteExistenceFound, Host: "myhive.example.com", Kind: "Ingress"},
		},
		{
			name: "valid missing check",
			in:   &RouteExistenceCheck{Status: RouteExistenceMissing, Error: "not found"},
			want: &RouteExistenceCheck{Status: RouteExistenceMissing, Error: "not found"},
		},
		{
			name: "invalid status rejected",
			in:   &RouteExistenceCheck{Status: "evil<xss>"},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeRouteExistenceCheck(tc.in)
			if tc.want == nil {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil result")
			}
			if got.Status != tc.want.Status {
				t.Errorf("Status = %q, want %q", got.Status, tc.want.Status)
			}
			if got.Kind != tc.want.Kind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.want.Kind)
			}
		})
	}
}
