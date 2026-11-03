package intent

import (
	"strings"
	"testing"
)

func TestClassifyPinOnly(t *testing.T) {
	old := "  uses: actions/checkout@" + strings.Repeat("a", 40) + " # v4"
	next := "  uses: actions/checkout@" + strings.Repeat("b", 40) + " # v5"
	patch := func(a, b string) string { return "@@ -1 +1 @@\n-" + a + "\n+" + b }
	file := ChangedFile{Filename: ".github/workflows/build.yml", Status: "modified", Additions: 1, Deletions: 1, Patch: patch(old, next)}
	spec := PinOnlySpec{Paths: []string{".github/workflows/*.yml", "Containerfile"}, Kinds: []string{"action-sha", "image-digest", "version-pin"}}
	for _, tc := range []struct {
		name   string
		mutate func(*ChangedFile, *PinOnlySpec)
		want   bool
	}{
		{"pure action SHA", func(_ *ChangedFile, _ *PinOnlySpec) {}, true},
		{"blank context lines", func(f *ChangedFile, _ *PinOnlySpec) {
			f.Patch = "@@ -1,4 +1,4 @@\n\n-" + old + "\n+" + next + "\n\n context"
		}, true},
		{"blank line before hunk", func(f *ChangedFile, _ *PinOnlySpec) { f.Patch = "\n" + f.Patch }, false},
		{"image digest", func(f *ChangedFile, _ *PinOnlySpec) {
			f.Filename = "Containerfile"
			f.Patch = patch("FROM ghcr.io/org/image@sha256:"+strings.Repeat("a", 64), "FROM ghcr.io/org/image@sha256:"+strings.Repeat("b", 64))
		}, true},
		{"version literal", func(f *ChangedFile, _ *PinOnlySpec) {
			f.Filename = "Containerfile"
			f.Patch = patch("ARG TOOL_VERSION=1.2.3", "ARG TOOL_VERSION=1.2.4")
		}, true},
		{"added step", func(f *ChangedFile, _ *PinOnlySpec) {
			f.Additions++
			f.Patch = strings.Replace(f.Patch, "@@ -1 +1 @@", "@@ -1 +1,2 @@", 1) + "\n+- run: evil"
		}, false},
		{"permission change", func(f *ChangedFile, _ *PinOnlySpec) {
			f.Additions++
			f.Deletions++
			f.Patch += "\n@@ -4 +4 @@\n-permissions: read-all\n+permissions: write-all"
		}, false},
		{"missing patch", func(f *ChangedFile, _ *PinOnlySpec) { f.Patch = "" }, false},
		{"truncated patch", func(f *ChangedFile, _ *PinOnlySpec) { f.Patch += "\n@@ -4,3 +4,3 @@\n context" }, false},
		{"counts disagree", func(f *ChangedFile, _ *PinOnlySpec) { f.Additions++ }, false},
		{"added file", func(f *ChangedFile, _ *PinOnlySpec) { f.Status = "added" }, false},
		{"renamed file", func(f *ChangedFile, _ *PinOnlySpec) { f.Status = "renamed" }, false},
		{"outside exception paths", func(_ *ChangedFile, s *PinOnlySpec) { s.Paths = []string{"Containerfile"} }, false},
		{"kind disabled", func(_ *ChangedFile, s *PinOnlySpec) { s.Kinds = []string{"image-digest"} }, false},
		{"unknown kind", func(_ *ChangedFile, s *PinOnlySpec) { s.Kinds = append(s.Kinds, "anything") }, false},
		{"changed source", func(f *ChangedFile, _ *PinOnlySpec) {
			f.Patch = patch(old, strings.Replace(next, "actions/checkout", "attacker/checkout", 1))
		}, false},
		{"SHA-shaped command", func(f *ChangedFile, _ *PinOnlySpec) { f.Patch = patch("run: echo "+old, "run: echo "+next) }, false},
		{"arbitrary comment", func(f *ChangedFile, _ *PinOnlySpec) { f.Patch = patch(old, next+" execute something") }, false},
		{"trust boundary pin", func(f *ChangedFile, _ *PinOnlySpec) {
			f.Patch = patch(strings.Replace(old, "actions/checkout", "sigstore/cosign", 1), strings.Replace(next, "actions/checkout", "sigstore/cosign", 1))
		}, false},
		{"owners file", func(f *ChangedFile, s *PinOnlySpec) { f.Filename = "CODEOWNERS"; s.Paths = []string{"*"} }, false},
		{"metadata only", func(f *ChangedFile, _ *PinOnlySpec) {
			f.Patch = "@@ -1 +1 @@\n unchanged"
			f.Additions = 0
			f.Deletions = 0
		}, false},
		{"malformed line", func(f *ChangedFile, _ *PinOnlySpec) { f.Patch += "\n" + "not a diff" }, false},
		{"extra diff lines", func(f *ChangedFile, _ *PinOnlySpec) { f.Patch += "\n extra" }, false},
		{"unpaired removal", func(f *ChangedFile, _ *PinOnlySpec) { f.Patch = "@@ -1 +1,0 @@\n-" + old; f.Additions = 0 }, false},
		{"invalid hunk count", func(f *ChangedFile, _ *PinOnlySpec) {
			f.Patch = "@@ -1,99999999999999999999999999999999 +1 @@\n-" + old + "\n+" + next
		}, false},
		{"no hunk", func(f *ChangedFile, _ *PinOnlySpec) { f.Patch = "-" + old + "\n+" + next }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s := file, PinOnlySpec{Paths: append([]string(nil), spec.Paths...), Kinds: append([]string(nil), spec.Kinds...)}
			tc.mutate(&f, &s)
			ok, reason := ClassifyPinOnly([]ChangedFile{f}, []string{f.Filename}, s)
			if ok != tc.want || reason == "" {
				t.Fatalf("got (%v, %q), want %v", ok, reason, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name    string
		files   []ChangedFile
		matches []string
	}{
		{"empty files", nil, []string{file.Filename}},
		{"no guarded paths", []ChangedFile{file}, nil},
		{"missing matched file", []ChangedFile{file}, []string{file.Filename, "Containerfile"}},
		{"duplicate file", []ChangedFile{file, file}, []string{file.Filename}},
		{"unguarded file", []ChangedFile{file, {Filename: "other.yml", Status: "modified", Additions: 1, Deletions: 1, Patch: file.Patch}}, []string{file.Filename}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ok, why := ClassifyPinOnly(tc.files, tc.matches, spec); ok {
				t.Fatalf("unexpected eligibility: %s", why)
			}
		})
	}
}

func TestPinReplacementNarrowGrammar(t *testing.T) {
	kinds := map[string]bool{"version-pin": true, "image-digest": true}
	for _, tc := range []struct {
		old, next string
		want      bool
	}{
		{"version: '1.2.3'", "version: '1.2.4'", true},
		{"version: 1.2.3", "version: 1.2.4'", false},
		{"version: '1.2.3'", "version: \"1.2.4\"", false},
		{"ARG TOOL_VERSION=1.2.3", "ARG TOOL_VERSION=1.2.4; evil", false},
		{"env: 1.2.3", "env: 1.2.4", false},
		{"with: 1.2.3", "with: 1.2.4", false},
		{"version: 1.2.3", "version: 1.2.3", false},
		{"version: 1.2.3", "version: 1.2.4", true},
		{"ARG SIGNING_VERSION=1.2.3", "ARG SIGNING_VERSION=1.2.4", false},
	} {
		if got := pinReplacement(tc.old, tc.next, kinds); got != tc.want {
			t.Errorf("%q -> %q: got %v", tc.old, tc.next, got)
		}
	}
}
