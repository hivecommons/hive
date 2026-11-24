package intent

import (
	"slices"
	"strings"
	"testing"
)

func TestClassifyPinOnly(t *testing.T) {
	old := "  uses: actions/checkout@" + strings.Repeat("a", 40) + " # v4"
	next := "  uses: actions/checkout@" + strings.Repeat("b", 40) + " # v4.1.0"
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

func TestClassifyPinOnlyActionsReturnsAcceptedPins(t *testing.T) {
	sha := func(c string) string { return strings.Repeat(c, 40) }
	spec := PinOnlySpec{Paths: []string{".github/workflows/*.yml", "Containerfile"}, Kinds: []string{"action-sha", "image-digest"}}
	workflow := ChangedFile{Filename: ".github/workflows/build.yml", Status: "modified", Additions: 3, Deletions: 3, Patch: "@@ -1,5 +1,5 @@\n" +
		"-  - uses: actions/checkout@" + sha("a") + " # v4\n" +
		"+  - uses: actions/checkout@" + strings.ToUpper(sha("b")) + " # v4.2.1\n" +
		" context\n" +
		"-    uses: github/codeql-action/init@" + sha("c") + "\n" +
		"+    uses: github/codeql-action/init@" + sha("d") + "\n" +
		" more context\n" +
		"-  - uses: actions/checkout@" + sha("a") + " # v4\n" +
		"+  - uses: actions/checkout@" + sha("b") + " # v4.2.1"}
	image := ChangedFile{Filename: "Containerfile", Status: "modified", Additions: 1, Deletions: 1,
		Patch: "@@ -1 +1 @@\n-FROM ghcr.io/org/image@sha256:" + strings.Repeat("a", 64) + "\n+FROM ghcr.io/org/image@sha256:" + strings.Repeat("b", 64)}
	ok, why, pins := ClassifyPinOnlyActions([]ChangedFile{workflow, image}, []string{workflow.Filename, image.Filename}, spec)
	if !ok {
		t.Fatalf("refused: %s", why)
	}
	want := []ActionPin{
		{Repo: "actions/checkout", SHA: sha("b"), Version: "v4.2.1"},
		{Repo: "github/codeql-action", SHA: sha("d")},
	}
	if !slices.Equal(pins, want) {
		t.Fatalf("pins = %+v, want %+v", pins, want)
	}
	if ok, _, pins := ClassifyPinOnlyActions([]ChangedFile{image}, []string{image.Filename}, spec); !ok || len(pins) != 0 {
		t.Fatalf("image-only diff: ok=%v pins=%+v, want accepted with no action pins", ok, pins)
	}
	workflow.Patch += "\n+  - run: evil"
	workflow.Additions++
	if ok, _, pins := ClassifyPinOnlyActions([]ChangedFile{workflow}, []string{workflow.Filename}, spec); ok || pins != nil {
		t.Fatalf("refused diff: ok=%v pins=%+v, want no pins", ok, pins)
	}
}

// action-sha-ref: a bare `key: <40-hex>` line is admitted only when it
// carries the same old -> new commit as a `uses:` pin accepted in the same
// diff, in any file. It never admits a line on its own and adds no pin.
func TestClassifyPinOnlyActionSHARef(t *testing.T) {
	const (
		ci        = ".github/workflows/ci.yml"
		release   = ".github/workflows/release.yml"
		unmatched = ": bare SHA line with no matching uses: pin"
		notAPin   = ": non-pin or unsafe replacement"
	)
	oldSHA, newSHA, otherSHA := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	uses := func(sha, version string) string {
		return "    uses: some-org/shared/.github/workflows/test.yml@" + sha + " # " + version
	}
	oldRef, newRef := "      test_ref: "+oldSHA+" # v1", "      test_ref: "+newSHA+" # v1.1.0"
	oldGate, newGate := "      gate_test_ref: "+oldSHA+" # v1", "      gate_test_ref: "+newSHA+" # v1.1.0"
	// ciFile bumps the reusable workflow and the input next to it;
	// releaseFile holds only a bare input line and no uses: change.
	ciFile := func(oldLine, newLine string) ChangedFile {
		return ChangedFile{Filename: ci, Status: "modified", Additions: 2, Deletions: 2, Patch: "@@ -10,4 +10,4 @@\n" +
			"   test:\n" +
			"-" + uses(oldSHA, "v1") + "\n" +
			"+" + uses(newSHA, "v1.1.0") + "\n" +
			"     with:\n" +
			"-" + oldLine + "\n" +
			"+" + newLine}
	}
	releaseFile := func(oldLine, newLine string) ChangedFile {
		return ChangedFile{Filename: release, Status: "modified", Additions: 1, Deletions: 1,
			Patch: "@@ -20 +20 @@\n-" + oldLine + "\n+" + newLine}
	}
	usesOnly := ChangedFile{Filename: ci, Status: "modified", Additions: 1, Deletions: 1,
		Patch: "@@ -11 +11 @@\n-" + uses(oldSHA, "v1") + "\n+" + uses(newSHA, "v1.1.0")}
	both := []string{"action-sha", "action-sha-ref"}
	spec := func(kinds []string) PinOnlySpec {
		return PinOnlySpec{Paths: []string{".github/workflows/*.yml"}, Kinds: kinds}
	}
	wantPins := []ActionPin{{Repo: "some-org/shared", SHA: newSHA, Version: "v1.1.0"}}

	// The pins a caller resolves upstream are those of the uses: lines alone.
	ok, why, pinsAlone := ClassifyPinOnlyActions([]ChangedFile{usesOnly}, []string{ci}, spec([]string{"action-sha"}))
	if !ok || !slices.Equal(pinsAlone, wantPins) {
		t.Fatalf("uses: bump alone: ok=%v (%s) pins=%+v, want %+v", ok, why, pinsAlone, wantPins)
	}

	for _, tc := range []struct {
		name   string
		kinds  []string
		files  []ChangedFile
		reason string // "" means accepted
	}{
		{"companions in two files follow the uses: pin", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile(oldGate, newGate)}, ""},
		{"companion file listed before the uses: file", both,
			[]ChangedFile{releaseFile(oldGate, newGate), ciFile(oldRef, newRef)}, ""},
		{"letter case of the commit does not matter", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile("      gate_test_ref: "+strings.ToUpper(oldSHA), "      gate_test_ref: "+strings.ToUpper(newSHA))}, ""},
		{"no version comment on either line", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile("      gate_test_ref: "+oldSHA, "      gate_test_ref: "+newSHA)}, ""},

		{"new SHA is not the uses: pin's", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile(oldGate, "      gate_test_ref: "+otherSHA+" # v1.1.0")}, release + unmatched},
		{"old SHA is not the uses: pin's", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile("      gate_test_ref: "+otherSHA+" # v1", newGate)}, release + unmatched},
		{"the uses: move reversed", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile(newGate, oldGate)}, release + unmatched},
		{"no uses: pin in the pull request", both,
			[]ChangedFile{releaseFile(oldGate, newGate)}, release + unmatched},
		{"action-sha-ref without action-sha admits nothing", []string{"action-sha-ref"},
			[]ChangedFile{releaseFile(oldGate, newGate)}, release + unmatched},
		{"action-sha-ref without action-sha refuses the uses: line too", []string{"action-sha-ref"},
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile(oldGate, newGate)}, ci + notAPin},
		{"kind not enabled", []string{"action-sha"},
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile(oldGate, newGate)}, ci + notAPin},
		{"kind not enabled, other kinds on", []string{"action-sha", "image-digest", "version-pin"},
			[]ChangedFile{usesOnly, releaseFile(oldGate, newGate)}, release + notAPin},

		{"unsafe key: with", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile("      with: "+oldSHA, "      with: "+newSHA)}, release + notAPin},
		{"unsafe key: env", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile("      env: "+oldSHA, "      env: "+newSHA)}, release + notAPin},
		{"unsafe key: secrets.", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile("      secrets.ref: "+oldSHA, "      secrets.ref: "+newSHA)}, release + notAPin},
		{"unsafe key: signing", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile("      signing_ref: "+oldSHA, "      signing_ref: "+newSHA)}, release + notAPin},
		{"unsafe key: id-token", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile("      id-token: "+oldSHA, "      id-token: "+newSHA)}, release + notAPin},
		{"key changes", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile(oldGate, "      other_ref: "+newSHA+" # v2")}, release + notAPin},
		{"indentation changes", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile(oldGate, "  "+newGate)}, release + notAPin},
		{"version comment appears", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile("      gate_test_ref: "+oldSHA, newGate)}, release + notAPin},
		{"version comment disappears", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile(oldGate, "      gate_test_ref: "+newSHA)}, release + notAPin},
		{"comment that is not a version", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile(oldGate, "      gate_test_ref: "+newSHA+" # v2 and more")}, release + notAPin},
		{"comment not set off by a space", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile("      gate_test_ref: "+oldSHA+"# v1", "      gate_test_ref: "+newSHA+"# v2")}, release + notAPin},
		{"comment changes but the commit does not", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile(oldGate, "      gate_test_ref: "+oldSHA+" # v2")}, release + notAPin},
		{"quoted value", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile(`      gate_test_ref: "`+oldSHA+`"`, `      gate_test_ref: "`+newSHA+`"`)}, release + notAPin},
		{"list item", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile("      - gate_test_ref: "+oldSHA, "      - gate_test_ref: "+newSHA)}, release + notAPin},
		{"short SHA", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile("      gate_test_ref: "+oldSHA[:12], "      gate_test_ref: "+newSHA[:12])}, release + notAPin},
		{"text after the SHA", both,
			[]ChangedFile{ciFile(oldRef, newRef), releaseFile("      gate_test_ref: "+oldSHA, "      gate_test_ref: "+newSHA+" extra")}, release + notAPin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var matches []string
			for _, f := range tc.files {
				matches = append(matches, f.Filename)
			}
			ok, why, pins := ClassifyPinOnlyActions(tc.files, matches, spec(tc.kinds))
			if tc.reason != "" {
				if ok || why != tc.reason || pins != nil {
					t.Fatalf("got (%v, %q, %+v), want refused with %q and no pins", ok, why, pins, tc.reason)
				}
				return
			}
			if !ok {
				t.Fatalf("refused: %s", why)
			}
			// A companion adds nothing for the caller to resolve upstream.
			if !slices.Equal(pins, pinsAlone) {
				t.Fatalf("pins = %+v, want exactly the uses: pin %+v", pins, pinsAlone)
			}
		})
	}

	// A companion must carry one pin's own move, never the old commit of one
	// pin and the new commit of another.
	second := ChangedFile{Filename: ".github/workflows/lint.yml", Status: "modified", Additions: 1, Deletions: 1,
		Patch: "@@ -5 +5 @@\n-    uses: acme/widgets@" + otherSHA + "\n+    uses: acme/widgets@" + strings.Repeat("d", 40)}
	crossed := releaseFile("      gate_test_ref: "+oldSHA, "      gate_test_ref: "+strings.Repeat("d", 40))
	files := []ChangedFile{usesOnly, second, crossed}
	if ok, why := ClassifyPinOnly(files, []string{ci, second.Filename, release}, spec(both)); ok || why != release+unmatched {
		t.Fatalf("crossed move: got (%v, %q), want refused with %q", ok, why, release+unmatched)
	}
}

// Every kind the classifier accepts is offered, and nothing else is.
func TestPinOnlyKindsListsEveryKind(t *testing.T) {
	want := []string{"action-sha", "image-digest", "version-pin", "action-sha-ref"}
	if got := PinOnlyKinds(); !slices.Equal(got, want) {
		t.Fatalf("PinOnlyKinds() = %v, want %v", got, want)
	}
	// The returned slice is the caller's: changing it must not change the list.
	mine := PinOnlyKinds()
	mine[0] = "anything"
	if got := PinOnlyKinds(); !slices.Equal(got, want) {
		t.Fatalf("PinOnlyKinds() after a caller edit = %v, want %v", got, want)
	}
	file := ChangedFile{Filename: "Containerfile", Status: "modified", Additions: 1, Deletions: 1,
		Patch: "@@ -1 +1 @@\n-ARG TOOL_VERSION=1.2.3\n+ARG TOOL_VERSION=1.2.4"}
	if ok, why := ClassifyPinOnly([]ChangedFile{file}, []string{file.Filename}, PinOnlySpec{Paths: []string{"Containerfile"}, Kinds: want}); !ok {
		t.Fatalf("every known kind together refused: %s", why)
	}
}

// A move to another major version is an upgrade (or a downgrade), not a pin,
// even when only the commit and its version comment change. Every kind that
// carries a version refuses it with a reason that names both versions; the
// same major version, and a line without a version comment, are accepted
// exactly as before.
func TestClassifyPinOnlyRefusesMajorVersionChange(t *testing.T) {
	const (
		workflow      = ".github/workflows/build.yml"
		containerfile = "Containerfile"
	)
	spec := PinOnlySpec{Paths: []string{".github/workflows/*.yml", containerfile}, Kinds: []string{"action-sha", "image-digest", "version-pin"}}
	uses := func(sha, comment string) string {
		return "      - uses: actions/checkout@" + strings.Repeat(sha, 40) + comment
	}
	for _, tc := range []struct {
		name, file, old, next string
		reason                string // "" means accepted
	}{
		{"action-sha upgrade", workflow, uses("a", " # v4"), uses("b", " # v7"), "major version change v4 -> v7 is not a pin"},
		{"action-sha downgrade", workflow, uses("a", " # v7"), uses("b", " # v4"), "major version change v7 -> v4 is not a pin"},
		{"action-sha full versions across a major", workflow, uses("a", " # v4.9.9"), uses("b", " # v5.0.0"), "major version change v4.9.9 -> v5.0.0 is not a pin"},
		{"action-sha same major, comment gains a minor", workflow, uses("a", " # v4"), uses("b", " # v4.2.0"), ""},
		{"action-sha same major, minor bump", workflow, uses("a", " # v4.1.2"), uses("b", " # v4.2.0"), ""},
		{"action-sha same comment", workflow, uses("a", " # v4"), uses("b", " # v4"), ""},
		{"action-sha no comment", workflow, uses("a", ""), uses("b", ""), ""},

		{"version-pin upgrade", containerfile, "version: 1.2.3", "version: 2.0.0", "major version change 1.2.3 -> 2.0.0 is not a pin"},
		{"version-pin downgrade", containerfile, "version: 2.0.0", "version: 1.9.9", "major version change 2.0.0 -> 1.9.9 is not a pin"},
		{"version-pin upgrade with a leading v", containerfile, "ARG TOOL_VERSION=v1.2.3", "ARG TOOL_VERSION=v2.0.0", "major version change v1.2.3 -> v2.0.0 is not a pin"},
		{"version-pin upgrade, v on one side only", containerfile, "version: v1.2.3", "version: 2.0.0", "major version change v1.2.3 -> 2.0.0 is not a pin"},
		{"version-pin quoted upgrade", containerfile, "version: '1.2.3'", "version: '2.0.0'", "major version change 1.2.3 -> 2.0.0 is not a pin"},
		{"version-pin same major", containerfile, "version: 1.2.3", "version: 1.9.0", ""},
		{"version-pin same major with a leading v", containerfile, "ARG TOOL_VERSION=v1.2.3", "ARG TOOL_VERSION=v1.3.0", ""},
		{"version-pin patch bump", containerfile, "version: 1.2.3", "version: 1.2.4", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := ChangedFile{Filename: tc.file, Status: "modified", Additions: 1, Deletions: 1, Patch: "@@ -1 +1 @@\n-" + tc.old + "\n+" + tc.next}
			ok, why := ClassifyPinOnly([]ChangedFile{file}, []string{tc.file}, spec)
			if tc.reason == "" {
				if !ok {
					t.Fatalf("refused: %s", why)
				}
				return
			}
			if want := tc.file + ": " + tc.reason; ok || why != want {
				t.Fatalf("got (%v, %q), want refused with %q", ok, why, want)
			}
		})
	}
}

// action-sha-ref: a companion line whose version comment changes the major
// version is refused even though a uses: pin in the same diff carries the
// same commit move, and a uses: pin that changes the major version is refused
// with action-sha-ref enabled too.
func TestClassifyPinOnlyActionSHARefRefusesMajorVersionChange(t *testing.T) {
	const ci = ".github/workflows/ci.yml"
	oldSHA, newSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	file := func(usesOld, usesNew, refOld, refNew string) ChangedFile {
		return ChangedFile{Filename: ci, Status: "modified", Additions: 2, Deletions: 2, Patch: "@@ -10,4 +10,4 @@\n" +
			"   test:\n" +
			"-    uses: some-org/shared/.github/workflows/test.yml@" + oldSHA + usesOld + "\n" +
			"+    uses: some-org/shared/.github/workflows/test.yml@" + newSHA + usesNew + "\n" +
			"     with:\n" +
			"-      test_ref: " + oldSHA + refOld + "\n" +
			"+      test_ref: " + newSHA + refNew}
	}
	spec := PinOnlySpec{Paths: []string{".github/workflows/*.yml"}, Kinds: []string{"action-sha", "action-sha-ref"}}
	for _, tc := range []struct {
		name                             string
		usesOld, usesNew, refOld, refNew string
		reason                           string // "" means accepted
	}{
		{"companion upgrade", " # v1", " # v1.1.0", " # v1", " # v2", "major version change v1 -> v2 is not a pin"},
		{"companion downgrade", " # v3", " # v3.0.1", " # v3", " # v1", "major version change v3 -> v1 is not a pin"},
		{"uses: upgrade with a same-major companion", " # v1", " # v2", " # v1", " # v1.1.0", "major version change v1 -> v2 is not a pin"},
		{"both upgrade", " # v1", " # v2", " # v1", " # v2", "major version change v1 -> v2 is not a pin"},
		{"companion same major", " # v1", " # v1.1.0", " # v1.0.0", " # v1.1.0", ""},
		{"companion without a comment", " # v1", " # v1.1.0", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := file(tc.usesOld, tc.usesNew, tc.refOld, tc.refNew)
			ok, why, pins := ClassifyPinOnlyActions([]ChangedFile{f}, []string{ci}, spec)
			if tc.reason == "" {
				if !ok || len(pins) != 1 || pins[0].SHA != newSHA {
					t.Fatalf("got (%v, %q, %+v), want accepted with the uses: pin", ok, why, pins)
				}
				return
			}
			if want := ci + ": " + tc.reason; ok || why != want || pins != nil {
				t.Fatalf("got (%v, %q, %+v), want refused with %q and no pins", ok, why, pins, want)
			}
		})
	}
}

// projectbluefin/dakota-iso#337: Renovate moved actions/checkout from v4 to
// v7 in two protected workflows, changing only the commit and the `# vN`
// comment, and the hive merged it as a pin. It is a major upgrade and stays a
// person's merge; the same diff within v4 is still a pin bump.
func TestClassifyPinOnlyDakotaMajorUpgrade(t *testing.T) {
	const (
		oldSHA = "34e114876b0b11c390a56381ad16ebd13914f8d5"
		newSHA = "3d3c42e5aac5ba805825da76410c181273ba90b1"
	)
	diff := func(version string) []ChangedFile {
		var files []ChangedFile
		for _, name := range []string{".github/workflows/build.yml", ".github/workflows/release.yml"} {
			files = append(files, ChangedFile{Filename: name, Status: "modified", Additions: 1, Deletions: 1, Patch: "@@ -20 +20 @@\n" +
				"-        uses: actions/checkout@" + oldSHA + " # v4\n" +
				"+        uses: actions/checkout@" + newSHA + " # " + version})
		}
		return files
	}
	matches := []string{".github/workflows/build.yml", ".github/workflows/release.yml"}
	spec := PinOnlySpec{Paths: []string{".github/workflows/*.yml"}, Kinds: []string{"action-sha", "image-digest", "version-pin"}}
	const want = ".github/workflows/build.yml: major version change v4 -> v7 is not a pin"

	if ok, why := ClassifyPinOnly(diff("v7"), matches, spec); ok || why != want {
		t.Fatalf("ClassifyPinOnly v4 -> v7: got (%v, %q), want refused with %q", ok, why, want)
	}
	if ok, why, pins := ClassifyPinOnlyActions(diff("v7"), matches, spec); ok || why != want || pins != nil {
		t.Fatalf("ClassifyPinOnlyActions v4 -> v7: got (%v, %q, %+v), want refused with %q and no pins", ok, why, pins, want)
	}
	ok, why, pins := ClassifyPinOnlyActions(diff("v4.3.1"), matches, spec)
	wantPins := []ActionPin{{Repo: "actions/checkout", SHA: newSHA, Version: "v4.3.1"}}
	if !ok || !slices.Equal(pins, wantPins) {
		t.Fatalf("v4 -> v4.3.1: got (%v, %q, %+v), want accepted with %+v", ok, why, pins, wantPins)
	}
}

// Renovate keeps the text after a pin's version comment when it rewrites the
// pin, so a workflow that annotates a pin (`# v2.90.6 (just)`, or a linter
// suppression such as `# v1 # zizmor: ignore[ref-version-mismatch]`) had
// every later bump refused as a non-pin (projectbluefin/dakota-iso#400). A
// short note is accepted only when it is byte-identical on both lines and
// follows a version comment; it never becomes part of the version the
// upstream tag check resolves.
func TestClassifyPinOnlyTrailingNote(t *testing.T) {
	const (
		ci      = ".github/workflows/ci.yml"
		notAPin = ci + ": non-pin or unsafe replacement"
		zizmor  = " # zizmor: ignore[ref-version-mismatch]"
	)
	oldSHA, newSHA := strings.Repeat("a", 40), "25f51f6b1bf4e18876107cece836f4f77ac17c8c"
	uses := func(sha, suffix string) string {
		return "      - uses: taiki-e/install-action@" + sha + suffix
	}
	spec := PinOnlySpec{Paths: []string{".github/workflows/*.yml"}, Kinds: []string{"action-sha", "action-sha-ref"}}
	for _, tc := range []struct {
		name, old, next string
		version         string // the accepted pin's Version
		reason          string // "" means accepted
	}{
		{"note kept, version bumped", uses(oldSHA, " # v2.90.6 (just)"), uses(newSHA, " # v2.90.7 (just)"), "v2.90.7", ""},
		{"linter suppression kept, version unchanged", uses(oldSHA, " # v1"+zizmor), uses(newSHA, " # v1"+zizmor), "v1", ""},
		{"no note, as before", uses(oldSHA, " # v2.90.6"), uses(newSHA, " # v2.90.7"), "v2.90.7", ""},
		{"note of 80 characters", uses(oldSHA, " # v2 "+strings.Repeat("n", 80)), uses(newSHA, " # v2 "+strings.Repeat("n", 80)), "v2", ""},
		{"note kept after a tab", uses(oldSHA, " # v2\t(just)"), uses(newSHA, " # v2\t(just)"), "v2", ""},

		{"note changes", uses(oldSHA, " # v2.90.6 (just)"), uses(newSHA, " # v2.90.7 (cargo)"), "", notAPin},
		{"note appears", uses(oldSHA, " # v2.90.6"), uses(newSHA, " # v2.90.7 (just)"), "", notAPin},
		{"note disappears", uses(oldSHA, " # v2.90.6 (just)"), uses(newSHA, " # v2.90.7"), "", notAPin},
		{"note separator changes", uses(oldSHA, " # v2 (just)"), uses(newSHA, " # v2  (just)"), "", notAPin},
		{"only the note changes", uses(oldSHA, " # v2 (just)"), uses(oldSHA, " # v2 (cargo)"), "", notAPin},
		{"note of 81 characters", uses(oldSHA, " # v2 "+strings.Repeat("n", 81)), uses(newSHA, " # v2 "+strings.Repeat("n", 81)), "", notAPin},
		{"unsafe word secrets. in the note", uses(oldSHA, " # v2 (secrets.X)"), uses(newSHA, " # v2 (secrets.X)"), "", notAPin},
		{"unsafe word permissions in the note", uses(oldSHA, " # v2 (permissions)"), uses(newSHA, " # v2 (permissions)"), "", notAPin},
		{"expression in the note", uses(oldSHA, " # v2 ${{ github.token }}"), uses(newSHA, " # v2 ${{ github.token }}"), "", notAPin},
		{"dollar sign in the note", uses(oldSHA, " # v2 $HOME"), uses(newSHA, " # v2 $HOME"), "", notAPin},
		{"backtick in the note", uses(oldSHA, " # v2 `id`"), uses(newSHA, " # v2 `id`"), "", notAPin},
		{"control character in the note", uses(oldSHA, " # v2 (a\x01b)"), uses(newSHA, " # v2 (a\x01b)"), "", notAPin},
		{"note without a version comment", uses(oldSHA, " (just)"), uses(newSHA, " (just)"), "", notAPin},
		{"note after a comment not set off by a space", uses(oldSHA, "# v2 (just)"), uses(newSHA, "# v2 (just)"), "", notAPin},
		{"note after a version that is not vN[.N[.N]]", uses(oldSHA, " # v2.90.6.1 (just)"), uses(newSHA, " # v2.90.7.1 (just)"), "", notAPin},
		{"note kept, major version changes", uses(oldSHA, " # v2.90.6 (just)"), uses(newSHA, " # v3.0.0 (just)"), "", ci + ": major version change v2.90.6 -> v3.0.0 is not a pin"},

		// The same rule for an action-sha-ref companion of the uses: pin.
		{"companion note kept", "      test_ref: " + oldSHA + " # v2.90.6" + zizmor, "      test_ref: " + newSHA + " # v2.90.7" + zizmor, "v2.90.7", ""},
		{"companion note changes", "      test_ref: " + oldSHA + " # v2.90.6 (just)", "      test_ref: " + newSHA + " # v2.90.7 (cargo)", "", notAPin},
		{"companion note without a version comment", "      test_ref: " + oldSHA + " (just)", "      test_ref: " + newSHA + " (just)", "", notAPin},
		{"companion note with an expression", "      test_ref: " + oldSHA + " # v2 ${{ x }}", "      test_ref: " + newSHA + " # v2 ${{ x }}", "", notAPin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patch := "@@ -3 +3 @@\n-" + tc.old + "\n+" + tc.next
			file := ChangedFile{Filename: ci, Status: "modified", Additions: 1, Deletions: 1, Patch: patch}
			if strings.Contains(tc.old, "test_ref:") {
				// A companion needs the uses: pin that moves the same commit.
				file = ChangedFile{Filename: ci, Status: "modified", Additions: 2, Deletions: 2, Patch: "@@ -3,3 +3,3 @@\n" +
					"-" + uses(oldSHA, " # v2.90.6") + "\n" +
					"+" + uses(newSHA, " # v2.90.7") + "\n" +
					"     with:\n" +
					"-" + tc.old + "\n" +
					"+" + tc.next}
			}
			ok, why, pins := ClassifyPinOnlyActions([]ChangedFile{file}, []string{ci}, spec)
			if tc.reason != "" {
				if ok || why != tc.reason || pins != nil {
					t.Fatalf("got (%v, %q, %+v), want refused with %q and no pins", ok, why, pins, tc.reason)
				}
				return
			}
			want := []ActionPin{{Repo: "taiki-e/install-action", SHA: newSHA, Version: tc.version}}
			if !ok || !slices.Equal(pins, want) {
				t.Fatalf("got (%v, %q, %+v), want accepted with %+v", ok, why, pins, want)
			}
		})
	}
}
