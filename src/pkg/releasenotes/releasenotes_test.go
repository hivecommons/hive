package releasenotes

import (
	"reflect"
	"testing"
)

const changelogFixture = `# Changelog

## Unreleased

- not a section

## 2026-10-08 (v5.145.0)

### Added

- feature one
  continued here
* feature star

### Fixed

- fix a

## 2026-10-07 (v5.144.1)

### Security

- sec fix

### Notes

- ignored

## 2026-10-07 (v5.144.0)

### Changed

- change a

### Deprecated

- old thing

## 2026-10-01 (v5.143.0)

### Added

- ancient
`

func versions(s []Section) []string {
	var out []string
	for _, x := range s {
		out = append(out, x.Version)
	}
	return out
}

func TestParse(t *testing.T) {
	got := Parse(changelogFixture)
	if want := []string{"v5.145.0", "v5.144.1", "v5.144.0", "v5.143.0"}; !reflect.DeepEqual(versions(got), want) {
		t.Fatalf("versions = %v, want %v", versions(got), want)
	}
	if got[0].Date != "2026-10-08" {
		t.Errorf("date = %q", got[0].Date)
	}
	wantAdded := []string{"feature one continued here", "feature star"}
	if !reflect.DeepEqual(got[0].Categories["added"], wantAdded) {
		t.Errorf("added = %v, want %v", got[0].Categories["added"], wantAdded)
	}
	if _, ok := got[1].Categories["notes"]; ok || len(got[1].Categories["security"]) != 1 {
		t.Errorf("unknown subsection handling wrong: %v", got[1].Categories)
	}
	if len(got[2].Categories["deprecated"]) != 1 {
		t.Errorf("deprecated = %v", got[2].Categories)
	}
}

func TestParseTolerant(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"crlf", "## 2026-01-02 (v1.0.0)\r\n### Added\r\n- a\r\n", []string{"v1.0.0"}},
		{"no v prefix", "## 2026-01-02 (1.2.3)\n### Added\n- a\n", []string{"v1.2.3"}},
		{"missing date", "## (v1.2.3)\n### Added\n- a\n", []string{"v1.2.3"}},
		{"malformed header skipped", "## 2026-01-02 v1.0.0\n### Added\n- lost\n## 2026-01-01 (v0.9.0)\n### Fixed\n- ok\n", []string{"v0.9.0"}},
		{"bullets before subsection", "## 2026-01-02 (v1.0.0)\n- stray\n### Added\n- a\n", []string{"v1.0.0"}},
		{"empty bullet and stray text", "## 2026-01-02 (v1.0.0)\n### Added\n-  \nprose\n\n- a\n", []string{"v1.0.0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse(tt.in)
			if !reflect.DeepEqual(versions(got), tt.want) {
				t.Fatalf("versions = %v, want %v", versions(got), tt.want)
			}
		})
	}
	got := Parse("## 2026-01-02 (v1.0.0)\n### Added\n-  \nprose\n\n- a\n")
	if !reflect.DeepEqual(got[0].Categories["added"], []string{"a"}) {
		t.Errorf("added = %v", got[0].Categories["added"])
	}
	got = Parse("## 2026-01-02 (v1.0.0)\n### Added\n- a\n\n  not a continuation after blank? yes it is\n")
	if len(got[0].Categories["added"]) != 1 {
		t.Errorf("added = %v", got[0].Categories["added"])
	}
}

func TestDiff(t *testing.T) {
	all := Parse(changelogFixture)
	tests := []struct {
		name     string
		from, to []Section
		want     []string
	}{
		{"tagged to tagged", all[3:], all, []string{"v5.145.0", "v5.144.1", "v5.144.0"}},
		{"same revision", all, all, nil},
		{"downgrade is empty", all, all[2:], nil},
		{"from empty", nil, all[:1], []string{"v5.145.0"}},
		{"newest first regardless of file order", nil,
			[]Section{{Version: "v1.0.0", Date: "2026-01-01"}, {Version: "v1.0.2", Date: "2026-01-03"}, {Version: "v1.0.1", Date: "2026-01-03"}},
			[]string{"v1.0.2", "v1.0.1", "v1.0.0"}},
		{"duplicate versions collapse", nil, []Section{{Version: "v1.0.0"}, {Version: "v1.0.0"}}, []string{"v1.0.0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Diff(tt.from, tt.to)
			if !reflect.DeepEqual(versions(got), tt.want) {
				t.Fatalf("Diff = %v, want %v", versions(got), tt.want)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.3", 0},
		{"v1.2.4", "v1.2.3", 1},
		{"v1.10.0", "v1.9.0", 1},
		{"v1.2", "v1.2.1", -1},
		{"v1.2.1", "v1.2", 1},
		{"v1.2.3-rc1", "v1.2.3", 0},
		{"vx.y", "v0.0", 0},
	}
	for _, tt := range tests {
		if got := compareVersions(tt.a, tt.b); got != tt.want {
			t.Errorf("compareVersions(%q,%q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestLatest(t *testing.T) {
	if got := Latest(nil); got != "" {
		t.Errorf("Latest(nil) = %q", got)
	}
	if got := Latest(Parse(changelogFixture)); got != "v5.145.0" {
		t.Errorf("Latest = %q", got)
	}
	if got := Latest([]Section{{Version: "v1.0.0", Date: "2026-01-01"}, {Version: "v2.0.0", Date: "2026-02-01"}}); got != "v2.0.0" {
		t.Errorf("Latest = %q", got)
	}
}

func TestFragmentCategory(t *testing.T) {
	tests := map[string]string{
		"added-1-foo.md":          "added",
		"changelog.d/fixed-2.md":  "fixed",
		"Security-x.md":           "security",
		"deprecated-old-thing.md": "deprecated",
		"changed.md":              "changed",
		"README.md":               "",
		"feature-x.md":            "",
		"":                        "",
	}
	for in, want := range tests {
		if got := FragmentCategory(in); got != want {
			t.Errorf("FragmentCategory(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFragmentGap(t *testing.T) {
	to := []Fragment{
		{Name: "added-2-b.md", Body: "- b one\n- b two\n"},
		{Name: "added-1-a.md", Body: "- a\r\n"},
		{Name: "fixed-3-c.md", Body: "<!-- release: patch -->\n- c\n"},
		{Name: "changed-4-old.md", Body: "- already at from\n"},
		{Name: "README.md", Body: "- readme"},
		{Name: "notes.txt", Body: "- txt"},
		{Name: "weird-5.md", Body: "- weird"},
		{Name: "security-6.md", Body: "\n"},
	}
	got := FragmentGap([]string{"changed-4-old.md"}, to)
	if got == nil {
		t.Fatal("expected a section")
	}
	if got.Title != UnreleasedTitle {
		t.Errorf("title = %q", got.Title)
	}
	want := map[string][]string{"added": {"a", "b one", "b two"}, "fixed": {"c"}}
	if !reflect.DeepEqual(got.Categories, want) {
		t.Errorf("categories = %v, want %v", got.Categories, want)
	}
	if FragmentGap([]string{"added-1-a.md"}, to[1:2]) != nil {
		t.Error("expected nil when every fragment is present at from")
	}
	if FragmentGap(nil, nil) != nil {
		t.Error("expected nil for no fragments")
	}
}

func bullets(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "b"
	}
	return out
}

func TestTruncate(t *testing.T) {
	secs := []Section{
		{Version: "v2", Categories: map[string][]string{"added": bullets(3), "fixed": bullets(2)}},
		{Version: "v1", Categories: map[string][]string{"added": bullets(4)}},
	}
	tests := []struct {
		name      string
		max       int
		wantCount int
		wantVers  []string
		wantTrunc bool
	}{
		{"under cap", 100, 9, []string{"v2", "v1"}, false},
		{"exact cap", 9, 9, []string{"v2", "v1"}, false},
		{"cut inside category", 2, 2, []string{"v2"}, true},
		{"cut across sections", 6, 6, []string{"v2", "v1"}, true},
		{"default cap", 0, 9, []string{"v2", "v1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, trunc := Truncate(secs, tt.max)
			n := 0
			for _, s := range got {
				n += s.entryCount()
			}
			if n != tt.wantCount || trunc != tt.wantTrunc || !reflect.DeepEqual(versions(got), tt.wantVers) {
				t.Errorf("count=%d trunc=%v vers=%v, want %d %v %v", n, trunc, versions(got), tt.wantCount, tt.wantTrunc, tt.wantVers)
			}
		})
	}
	if len(secs[0].Categories["added"]) != 3 {
		t.Error("Truncate mutated its input")
	}
}

func TestBuild(t *testing.T) {
	from := "## 2026-10-07 (v5.144.0)\n### Added\n- old\n"
	to := "## 2026-10-08 (v5.145.0)\n### Added\n- new\n" + from
	frags := []Fragment{{Name: "added-9-x.md", Body: "- unreleased x\n"}}

	t.Run("tagged", func(t *testing.T) {
		r := Build(from, to, nil, frags, false, 0)
		if len(r.Sections) != 1 || r.Sections[0].Version != "v5.145.0" || r.Unreleased != nil || r.Truncated {
			t.Errorf("unexpected result %+v", r)
		}
	})
	t.Run("untagged adds unreleased", func(t *testing.T) {
		r := Build(from, to, nil, frags, true, 0)
		if r.Unreleased == nil || len(r.Sections) != 1 {
			t.Fatalf("unexpected result %+v", r)
		}
		if got := r.Unreleased.Categories["added"]; !reflect.DeepEqual(got, []string{"unreleased x"}) {
			t.Errorf("unreleased = %v", got)
		}
	})
	t.Run("same revision is empty", func(t *testing.T) {
		r := Build(to, to, []string{"added-9-x.md"}, frags, true, 0)
		if len(r.Sections) != 0 || r.Unreleased != nil || r.Truncated {
			t.Errorf("unexpected result %+v", r)
		}
	})
	t.Run("unreleased consumes the cap first", func(t *testing.T) {
		r := Build(from, to, nil, frags, true, 1)
		if r.Unreleased == nil || len(r.Sections) != 0 || !r.Truncated {
			t.Errorf("unexpected result %+v", r)
		}
	})
}
