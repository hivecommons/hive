package nousdeps

import (
	"errors"
	"strings"
	"testing"
)

const (
	testCommit  = "8de28905b87474b870c56e1ecbd3c970532683b8"
	testCommit2 = "34193512f9aecca8244c3a99c67873e578193319"
	testSHA256  = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

func TestDependencyValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		dep  Dependency
		want error // nil = valid; otherwise errors.Is target
	}{
		{"repo shorthand pinned", Dependency{Repo: "inference-sim/inference-sim", Commit: testCommit}, nil},
		{"repo url pinned", Dependency{Repo: "https://github.com/org/repo.git", Commit: testCommit}, nil},
		{"repo uppercase commit accepted", Dependency{Repo: "org/repo", Commit: strings.ToUpper(testCommit)}, nil},
		{"file pinned", Dependency{URL: "https://huggingface.co/datasets/o/n/resolve/abc/trace.jsonl", SHA256: testSHA256}, nil},
		{"file with explicit role", Dependency{URL: "https://example.org/a.bin", SHA256: testSHA256, Role: "Data"}, nil},
		{"neither source", Dependency{Commit: testCommit}, ErrInvalidDependency},
		{"both sources", Dependency{Repo: "o/r", Commit: testCommit, URL: "https://x.org/f", SHA256: testSHA256}, ErrInvalidDependency},
		{"repo unpinned", Dependency{Repo: "o/r"}, ErrUnpinned},
		{"repo branch name", Dependency{Repo: "o/r", Commit: "main"}, ErrUnpinned},
		{"repo short sha", Dependency{Repo: "o/r", Commit: testCommit[:12]}, ErrUnpinned},
		{"repo pinned by sha256", Dependency{Repo: "o/r", Commit: testCommit, SHA256: testSHA256}, ErrInvalidDependency},
		{"file unpinned", Dependency{URL: "https://x.org/f"}, ErrUnpinned},
		{"file short hash", Dependency{URL: "https://x.org/f", SHA256: testSHA256[:40]}, ErrUnpinned},
		{"file pinned by commit", Dependency{URL: "https://x.org/f", SHA256: testSHA256, Commit: testCommit}, ErrInvalidDependency},
		{"file http", Dependency{URL: "http://x.org/f", SHA256: testSHA256}, ErrInvalidDependency},
		{"file credentials", Dependency{URL: "https://u:p@x.org/f", SHA256: testSHA256}, ErrInvalidDependency},
		{"file fragment", Dependency{URL: "https://x.org/f#frag", SHA256: testSHA256}, ErrInvalidDependency},
		{"file odd port", Dependency{URL: "https://x.org:8443/f", SHA256: testSHA256}, ErrInvalidDependency},
		{"file port 443 ok", Dependency{URL: "https://x.org:443/f", SHA256: testSHA256}, nil},
		{"file no host", Dependency{URL: "https:///f", SHA256: testSHA256}, ErrInvalidDependency},
		{"file unparseable", Dependency{URL: "https://x.org/%zz", SHA256: testSHA256}, ErrInvalidDependency},
		{"file no derivable name", Dependency{URL: "https://x.org/", SHA256: testSHA256}, ErrInvalidDependency},
		{"file no derivable name but named", Dependency{Name: "root", URL: "https://x.org/", SHA256: testSHA256}, nil},
		{"repo ssh scheme", Dependency{Repo: "git@github.com:o/r.git", Commit: testCommit}, ErrInvalidDependency},
		{"unknown role", Dependency{Repo: "o/r", Commit: testCommit, Role: "runtime"}, ErrInvalidDependency},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.dep.Validate()
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestDependencyDerivations(t *testing.T) {
	d := Dependency{Repo: "inference-sim/inference-sim", Commit: testCommit}
	if d.Kind() != KindRepo || d.SourceURL() != "https://github.com/inference-sim/inference-sim" || d.Pin() != testCommit {
		t.Fatalf("repo derivations: %q %q %q", d.Kind(), d.SourceURL(), d.Pin())
	}
	if d.EffectiveName() != "inference-sim" || d.EnvName() != "NOUS_DEP_INFERENCE_SIM" {
		t.Fatalf("repo name: %q env %q", d.EffectiveName(), d.EnvName())
	}
	f := Dependency{URL: "https://huggingface.co/d/o/n/resolve/r/trace.jsonl", SHA256: strings.ToUpper(testSHA256)}
	if f.Kind() != KindFile || f.SourceURL() != f.URL || f.Pin() != testSHA256 {
		t.Fatalf("file derivations: %q %q %q", f.Kind(), f.SourceURL(), f.Pin())
	}
	if f.EffectiveName() != "trace.jsonl" || f.EnvName() != "NOUS_DEP_TRACE_JSONL" {
		t.Fatalf("file name: %q env %q", f.EffectiveName(), f.EnvName())
	}
	if (Dependency{Repo: "https://github.com/o/r.git", Commit: testCommit}).EffectiveName() != "r" {
		t.Fatal(".git suffix not stripped")
	}
	if (Dependency{Name: " custom ", URL: "https://x.org/f", SHA256: testSHA256}).EffectiveName() != "custom" {
		t.Fatal("explicit name not honoured")
	}
	if (Dependency{Name: "--odd name--"}).EnvName() != "NOUS_DEP_ODD_NAME" {
		t.Fatalf("EnvName = %q", (Dependency{Name: "--odd name--"}).EnvName())
	}
}

func TestValidateAll(t *testing.T) {
	t.Run("defaults roles and names", func(t *testing.T) {
		got, err := ValidateAll([]Dependency{
			{Repo: "o/target", Commit: testCommit},
			{URL: "https://x.org/trace.jsonl", SHA256: testSHA256},
			{Repo: "o/helper", Commit: testCommit2},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Role != RoleTarget || got[1].Role != RoleData || got[2].Role != RoleTool {
			t.Fatalf("roles = %q %q %q", got[0].Role, got[1].Role, got[2].Role)
		}
		if got[0].Name != "target" || got[1].Name != "trace.jsonl" || got[2].Name != "helper" {
			t.Fatalf("names = %q %q %q", got[0].Name, got[1].Name, got[2].Name)
		}
	})
	t.Run("explicit roles normalised", func(t *testing.T) {
		got, err := ValidateAll([]Dependency{{Repo: "o/r", Commit: testCommit, Role: " TOOL "}})
		if err != nil || got[0].Role != RoleTool {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("empty is valid", func(t *testing.T) {
		got, err := ValidateAll(nil)
		if err != nil || len(got) != 0 {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	for _, tc := range []struct {
		name string
		deps []Dependency
		msg  string
	}{
		{"invalid entry indexed", []Dependency{{Repo: "o/r", Commit: testCommit}, {Repo: "o/x"}}, "dependencies[1]"},
		{"two targets", []Dependency{{Repo: "o/a", Commit: testCommit, Role: RoleTarget}, {Repo: "o/b", Commit: testCommit2, Role: RoleTarget}}, "more than one"},
		{"file as target", []Dependency{{URL: "https://x.org/f", SHA256: testSHA256, Role: RoleTarget}}, "only a repo"},
		{"duplicate names", []Dependency{{Repo: "o/r", Commit: testCommit}, {Repo: "p/r", Commit: testCommit2}}, "duplicate name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateAll(tc.deps)
			if err == nil || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("ValidateAll() = %v, want error containing %q", err, tc.msg)
			}
		})
	}
}

func TestAllowlist(t *testing.T) {
	al := NewAllowlist([]string{" GitHub.com ", "huggingface.co", "github.com", ""})
	if len(al) != 2 || al[0] != "github.com" || al[1] != "huggingface.co" {
		t.Fatalf("NewAllowlist = %q", al)
	}
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"github.com", true},
		{"GITHUB.COM", true},
		{" huggingface.co ", true},
		{"api.github.com", false},
		{"github.com.evil.example", false},
		{"evil.example", false},
		{"", false},
	} {
		if got := al.Allows(tc.host); got != tc.want {
			t.Errorf("Allows(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
	if Allowlist(nil).Allows("github.com") || (Allowlist{}).Allows("github.com") {
		t.Fatal("nil/empty allowlist must deny everything")
	}
	for _, tc := range []struct {
		name string
		raw  string
		want error
	}{
		{"allowed", "https://github.com/o/r", nil},
		{"allowed with 443", "https://huggingface.co:443/x", nil},
		{"denied host", "https://objects.githubusercontent.com/x", ErrNotAllowlisted},
		{"denied subdomain", "https://raw.github.com/x", ErrNotAllowlisted},
		{"http rejected before allowlist", "http://github.com/o/r", ErrInvalidDependency},
		{"credentials rejected", "https://tok@github.com/o/r", ErrInvalidDependency},
		{"ip rejected", "https://140.82.112.3/o/r", ErrNotAllowlisted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := al.CheckURL(tc.raw)
			if tc.want == nil && err != nil {
				t.Fatalf("CheckURL(%q) = %v", tc.raw, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("CheckURL(%q) = %v, want %v", tc.raw, err, tc.want)
			}
		})
	}
	if _, err := (Allowlist{}).CheckURL("https://github.com/o/r"); err == nil || !strings.Contains(err.Error(), "fetch_allowlist is empty") {
		t.Fatalf("empty allowlist error = %v", err)
	}
}

func TestAllowlistFromEnv(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	if got := AllowlistFromEnv(env("")); len(got) != 1 || got[0] != "github.com" {
		t.Fatalf("unset env → %q, want default", got)
	}
	if got := AllowlistFromEnv(env("huggingface.co, GitHub.com")); len(got) != 2 || got[0] != "huggingface.co" || got[1] != "github.com" {
		t.Fatalf("env → %q", got)
	}
	// The hive exports an explicitly empty yaml list as the bare separator.
	if got := AllowlistFromEnv(env(",")); len(got) != 0 {
		t.Fatalf("deny-all env → %q, want empty", got)
	}
	t.Setenv("HIVE_NOUS_FETCH_ALLOWLIST", "mirror.example")
	if got := AllowlistFromEnv(nil); len(got) != 1 || got[0] != "mirror.example" {
		t.Fatalf("nil getenv → %q", got)
	}
}

func TestParse(t *testing.T) {
	t.Run("nested under campaign", func(t *testing.T) {
		got, err := Parse([]byte(`
campaign:
  name: blis-routing-v1
  mode: observe
  dependencies:
    - repo: inference-sim/inference-sim
      commit: ` + testCommit + `
      role: target
    - url: https://huggingface.co/datasets/o/n/resolve/r/trace.jsonl
      sha256: ` + testSHA256 + `
      role: data
`))
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != "blis-routing-v1" || len(got.Dependencies) != 2 || got.Dependencies[0].Role != RoleTarget {
			t.Fatalf("Parse = %+v", got)
		}
	})
	t.Run("top level as in the RFC", func(t *testing.T) {
		got, err := Parse([]byte("dependencies:\n  - repo: o/r\n    commit: " + testCommit + "\n"))
		if err != nil || len(got.Dependencies) != 1 || got.Dependencies[0].Name != "r" {
			t.Fatalf("Parse = %+v, %v", got, err)
		}
	})
	t.Run("nested wins over top level", func(t *testing.T) {
		got, err := Parse([]byte("dependencies:\n  - repo: o/top\n    commit: " + testCommit + "\ncampaign:\n  dependencies:\n    - repo: o/nested\n      commit: " + testCommit + "\n"))
		if err != nil || len(got.Dependencies) != 1 || got.Dependencies[0].Name != "nested" {
			t.Fatalf("Parse = %+v, %v", got, err)
		}
	})
	t.Run("no dependencies", func(t *testing.T) {
		got, err := Parse([]byte("campaign:\n  name: plain\n  mode: observe\n"))
		if err != nil || got.Name != "plain" || len(got.Dependencies) != 0 {
			t.Fatalf("Parse = %+v, %v", got, err)
		}
	})
	t.Run("invalid yaml", func(t *testing.T) {
		if _, err := Parse([]byte("campaign: [")); err == nil {
			t.Fatal("want parse error")
		}
	})
	t.Run("unpinned refused", func(t *testing.T) {
		_, err := Parse([]byte("dependencies:\n  - repo: o/r\n    commit: main\n"))
		if !errors.Is(err, ErrUnpinned) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if _, err := Load(t.TempDir() + "/nope.yaml"); err == nil {
			t.Fatal("want error")
		}
	})
}
