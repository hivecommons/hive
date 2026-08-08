package config

import "testing"

func TestValidateProjectRepoTargets(t *testing.T) {
	tests := []struct {
		name    string
		org     string
		repos   []string
		primary string
		forge   string
		want    string
	}{
		{name: "public org repo", org: "kubestellar", repos: []string{"hive"}, primary: "hive", forge: "github.com"},
		{name: "ghe org repo", org: "kcp-dev", repos: []string{"hive"}, primary: "hive", forge: "github.ibm.com"},
		{
			name:  "forge host as org",
			org:   "github.ibm.com",
			repos: []string{"hive"},
			forge: "github.ibm.com",
			want:  "Repo target misconfigured: org 'github.ibm.com' looks like a forge host — expected org/repo. Fix in Governor Config → Repos.",
		},
		{
			name:  "forge host case variant",
			org:   "GitHub.IBM.com",
			repos: []string{"hive"},
			forge: "github.ibm.com",
			want:  "Repo target misconfigured: org 'GitHub.IBM.com' looks like a forge host — expected org/repo. Fix in Governor Config → Repos.",
		},
		{
			name:  "url pasted in repo",
			org:   "kubestellar",
			repos: []string{"https://github.com/kubestellar/hive"},
			forge: "github.com",
			want:  "Repo target misconfigured: repo 'https://github.com/kubestellar/hive' is a URL — expected repo name only so the target resolves to org/repo. Fix in Governor Config → Repos.",
		},
		{
			name:  "host org no repo leaves empty repo",
			org:   "kubestellar",
			repos: []string{""},
			forge: "github.com",
			want:  "Repo target misconfigured: repo is empty — expected org/repo. Fix in Governor Config → Repos.",
		},
		{
			name:  "empty org",
			org:   "",
			repos: []string{"hive"},
			forge: "github.com",
			want:  "Repo target misconfigured: org is empty — expected org/repo. Fix in Governor Config → Repos.",
		},
		{
			name:  "empty repo list",
			org:   "kubestellar",
			repos: nil,
			forge: "github.com",
			want:  "Repo target misconfigured: repo is empty — expected org/repo. Fix in Governor Config → Repos.",
		},
		{
			name:  "trailing slash shape",
			org:   "kubestellar",
			repos: []string{"hive/"},
			forge: "github.com",
			want:  "Repo target misconfigured: repo 'hive/' contains '/' — expected repo name only so the target resolves to org/repo. Fix in Governor Config → Repos.",
		},
		{
			name:  "repo contains slash",
			org:   "kubestellar",
			repos: []string{"other/hive"},
			forge: "github.com",
			want:  "Repo target misconfigured: repo 'other/hive' contains '/' — expected repo name only so the target resolves to org/repo. Fix in Governor Config → Repos.",
		},
		{
			name:    "primary contains slash",
			org:     "kubestellar",
			repos:   []string{"hive"},
			primary: "kubestellar/hive",
			forge:   "github.com",
			want:    "Repo target misconfigured: repo 'kubestellar/hive' contains '/' — expected repo name only so the target resolves to org/repo. Fix in Governor Config → Repos.",
		},
		{
			name:  "full url pasted into org",
			org:   "https://github.com/kubestellar",
			repos: []string{"hive"},
			forge: "github.com",
			want:  "Repo target misconfigured: org 'https://github.com/kubestellar' is not an organization name — expected org/repo. Fix in Governor Config → Repos.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidateProjectRepoTargets(tt.org, tt.repos, tt.primary, tt.forge)
			if tt.want == "" {
				if got != nil {
					t.Fatalf("got issue %q, want nil", got.Message)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil issue, want %q", tt.want)
			}
			if got.Message != tt.want {
				t.Fatalf("message = %q, want %q", got.Message, tt.want)
			}
		})
	}
}
