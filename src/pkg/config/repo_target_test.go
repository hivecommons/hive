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
			want:  "Repo target misconfigured: org 'github.ibm.com' looks like a forge host — expected org/repo. Fix in Settings → Repos.",
		},
		{
			name:  "forge host case variant",
			org:   "GitHub.IBM.com",
			repos: []string{"hive"},
			forge: "github.ibm.com",
			want:  "Repo target misconfigured: org 'GitHub.IBM.com' looks like a forge host — expected org/repo. Fix in Settings → Repos.",
		},
		{
			name:  "url pasted in repo",
			org:   "kubestellar",
			repos: []string{"https://github.com/kubestellar/hive"},
			forge: "github.com",
			want:  "Repo target misconfigured: repo 'https://github.com/kubestellar/hive' is a URL — expected repo name only so the target resolves to org/repo. Fix in Settings → Repos.",
		},
		{
			name:  "url pasted for different org explains migration",
			org:   "kubestellar",
			repos: []string{"https://github.com/hivecommons/hive"},
			forge: "github.com",
			want:  "Repo target misconfigured: repo 'https://github.com/hivecommons/hive' belongs to org 'hivecommons', but this hive is configured for org 'kubestellar' — to migrate, save a repo from the new org in Settings → Repos so the dashboard can adopt it. Fix in Settings → Repos.",
		},
		{
			name:  "host org no repo leaves empty repo",
			org:   "kubestellar",
			repos: []string{""},
			forge: "github.com",
			want:  "Repo target misconfigured: repo is empty — expected org/repo. Fix in Settings → Repos.",
		},
		{
			name:  "empty org",
			org:   "",
			repos: []string{"hive"},
			forge: "github.com",
			want:  "Repo target misconfigured: org is empty — expected org/repo. Fix in Settings → Repos.",
		},
		{
			name:  "empty repo list",
			org:   "kubestellar",
			repos: nil,
			forge: "github.com",
			want:  "Repo target misconfigured: repo is empty — expected org/repo. Fix in Settings → Repos.",
		},
		{
			name:  "trailing slash shape",
			org:   "kubestellar",
			repos: []string{"hive/"},
			forge: "github.com",
			want:  "Repo target misconfigured: repo 'hive/' contains '/' — expected repo name only so the target resolves to org/repo. Fix in Settings → Repos.",
		},
		{
			name:  "repo contains slash",
			org:   "kubestellar",
			repos: []string{"other/hive"},
			forge: "github.com",
			want:  "Repo target misconfigured: repo 'other/hive' belongs to org 'other', but this hive is configured for org 'kubestellar' — to migrate, save a repo from the new org in Settings → Repos so the dashboard can adopt it. Fix in Settings → Repos.",
		},
		{
			name:    "primary qualified with a different org",
			org:     "kubestellar",
			repos:   []string{"hive"},
			primary: "other/hive",
			forge:   "github.com",
			want:    "Repo target misconfigured: repo 'other/hive' belongs to org 'other', but this hive is configured for org 'kubestellar' — to migrate, save a repo from the new org in Settings → Repos so the dashboard can adopt it. Fix in Settings → Repos.",
		},
		// The live regression: an owner pastes the org/repo form GitHub shows.
		// Accepted, because it normalizes to the bare repo under the same org.
		{
			name:    "primary qualified with the configured org",
			org:     "kubestellar",
			repos:   []string{"hive"},
			primary: "kubestellar/hive",
			forge:   "github.com",
		},
		{
			name:    "repos entry qualified with the configured org",
			org:     "zacburns",
			repos:   []string{"zacburns/mlz-manager"},
			primary: "mlz-manager",
			forge:   "github.com",
		},
		{
			name:    "repos entry qualified with a deeper path",
			org:     "kubestellar",
			repos:   []string{"kubestellar/hive/tree/main"},
			primary: "hive",
			forge:   "github.com",
			want:    "Repo target misconfigured: repo 'kubestellar/hive/tree/main' contains '/' — expected repo name only so the target resolves to org/repo. Fix in Settings → Repos.",
		},
		{
			name:  "full url pasted into org",
			org:   "https://github.com/kubestellar",
			repos: []string{"hive"},
			forge: "github.com",
			want:  "Repo target misconfigured: org 'https://github.com/kubestellar' is not an organization name — expected org/repo. Fix in Settings → Repos.",
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

// TestNormalizeRepoForOrg pins the normalizer directly. The bare-name and
// different-org cases are positive controls: an implementation that simply
// stripped everything before the last '/' would pass the org-qualified case
// alone, but must fail these.
func TestNormalizeRepoForOrg(t *testing.T) {
	tests := []struct {
		name         string
		org          string
		repo         string
		want         string
		wantStripped bool
	}{
		// The live regression from hive-hosted-hosted-available-vllmd-04.
		{name: "org qualified matching org", org: "zacburns", repo: "zacburns/mlz-manager", want: "mlz-manager", wantStripped: true},
		{name: "org qualified case insensitive", org: "zacburns", repo: "ZacBurns/mlz-manager", want: "mlz-manager", wantStripped: true},
		{name: "org qualified with surrounding space", org: "zacburns", repo: "  zacburns/mlz-manager  ", want: "mlz-manager", wantStripped: true},

		// Positive controls: nothing may be stripped from these.
		{name: "bare repo unchanged", org: "zacburns", repo: "mlz-manager", want: "mlz-manager"},
		{name: "bare repo containing org as substring", org: "zac", repo: "zacburns", want: "zacburns"},
		{name: "different org not stripped", org: "kubestellar", repo: "other/hive", want: "other/hive"},
		{name: "deeper path not stripped", org: "kubestellar", repo: "kubestellar/hive/tree/main", want: "kubestellar/hive/tree/main"},
		{name: "org prefix with empty repo not stripped", org: "kubestellar", repo: "kubestellar/", want: "kubestellar/"},
		{name: "url left for the url error", org: "kubestellar", repo: "https://github.com/kubestellar/hive", want: "https://github.com/kubestellar/hive"},
		{name: "empty org is a no-op", org: "", repo: "kubestellar/hive", want: "kubestellar/hive"},
		{name: "empty repo is a no-op", org: "kubestellar", repo: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, stripped := NormalizeRepoForOrg(tt.org, tt.repo)
			if got != tt.want || stripped != tt.wantStripped {
				t.Fatalf("NormalizeRepoForOrg(%q, %q) = (%q, %v), want (%q, %v)",
					tt.org, tt.repo, got, stripped, tt.want, tt.wantStripped)
			}
		})
	}
}

func TestNormalizeProjectRepos(t *testing.T) {
	t.Run("mixed list normalizes only the qualified entries", func(t *testing.T) {
		got, changed := NormalizeProjectRepos("zacburns", []string{"zacburns/mlz-manager", "other-repo"})
		if !changed {
			t.Fatalf("changed = false, want true")
		}
		want := []string{"mlz-manager", "other-repo"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})

	// Positive control: an already-correct list must report no change so callers
	// do not log or persist a spurious repair.
	t.Run("bare list unchanged", func(t *testing.T) {
		in := []string{"mlz-manager", "other-repo"}
		got, changed := NormalizeProjectRepos("zacburns", in)
		if changed {
			t.Fatalf("changed = true for an already-bare list %v", in)
		}
		for i := range in {
			if got[i] != in[i] {
				t.Fatalf("got %v, want %v", got, in)
			}
		}
	})

	t.Run("different org left for validation to reject", func(t *testing.T) {
		got, changed := NormalizeProjectRepos("kubestellar", []string{"other/hive"})
		if changed || got[0] != "other/hive" {
			t.Fatalf("got (%v, %v), want ([other/hive], false)", got, changed)
		}
		if issue := ValidateProjectRepoTargets("kubestellar", got, "hive", "github.com"); issue == nil {
			t.Fatalf("a repo qualified with a different org must still be rejected")
		}
	})
}

// TestApplyDefaultsNormalizesRepos covers the load path: the exact live config
// shape from the degraded hive must come out of applyDefaults with a bare repos
// list, and ValidateRepoTargets must then report no issue.
func TestApplyDefaultsNormalizesRepos(t *testing.T) {
	cfg := &Config{}
	cfg.Project.Org = "zacburns"
	cfg.Project.Repos = []string{"zacburns/mlz-manager"}
	cfg.Project.PrimaryRepo = "mlz-manager"
	cfg.applyDefaults()

	if len(cfg.Project.Repos) != 1 || cfg.Project.Repos[0] != "mlz-manager" {
		t.Fatalf("Project.Repos = %v, want [mlz-manager]", cfg.Project.Repos)
	}
	if issue := ValidateRepoTargets(cfg); issue != nil {
		t.Fatalf("ValidateRepoTargets = %q, want nil", issue.Message)
	}

	// Positive control: a already-bare config must survive applyDefaults intact.
	bare := &Config{}
	bare.Project.Org = "zacburns"
	bare.Project.Repos = []string{"mlz-manager"}
	bare.Project.PrimaryRepo = "mlz-manager"
	bare.applyDefaults()
	if len(bare.Project.Repos) != 1 || bare.Project.Repos[0] != "mlz-manager" {
		t.Fatalf("bare Project.Repos = %v, want [mlz-manager]", bare.Project.Repos)
	}

	// A mismatched org must NOT be normalized away on load — it stays broken and
	// visibly reported rather than silently retargeting the hive.
	mismatch := &Config{}
	mismatch.Project.Org = "zacburns"
	mismatch.Project.Repos = []string{"someoneelse/mlz-manager"}
	mismatch.Project.PrimaryRepo = "mlz-manager"
	mismatch.applyDefaults()
	if mismatch.Project.Repos[0] != "someoneelse/mlz-manager" {
		t.Fatalf("mismatched org was rewritten to %v", mismatch.Project.Repos)
	}
	if issue := ValidateRepoTargets(mismatch); issue == nil {
		t.Fatalf("ValidateRepoTargets = nil, want a repo_target issue for a mismatched org")
	}
}
