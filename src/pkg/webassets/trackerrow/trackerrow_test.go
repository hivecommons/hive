package trackerrow

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestFilesMatchEmbed pins that Files names exactly the embedded assets, so a
// file added to the directory is either listed or fails here.
func TestFilesMatchEmbed(t *testing.T) {
	embedded, err := fs.Glob(FS, "*.js")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, name := range Files {
		listed[name] = true
	}
	for _, name := range embedded {
		if !listed[name] {
			t.Errorf("%s is embedded but not listed in Files", name)
		}
	}
	if len(embedded) != len(Files) {
		t.Errorf("embedded %v, Files %v", embedded, Files)
	}
}

// TestAssetsDefineRowFunctions pins the functions each asset provides and that
// none of them draws an action control: the hub embeds these and stays
// visibility-only.
func TestAssetsDefineRowFunctions(t *testing.T) {
	want := map[string][]string{
		"tracker-row-anchor.js": {
			"function stageTrackerAnchor(stage, stages, kind, item) {",
		},
		"tracker-row-title.js": {
			"function stageTrackerSplitTitleTag(title) {",
			"function stageTrackerTitleHtml(title, extraClass) {",
		},
	}
	for _, name := range Files {
		b, err := fs.ReadFile(FS, name)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		decls, ok := want[name]
		if !ok {
			t.Errorf("%s has no expected functions in this test", name)
		}
		for _, d := range decls {
			if !strings.Contains(src, d) {
				t.Errorf("%s does not define %q", name, d)
			}
		}
		for _, banned := range []string{"data-action", "addEventListener", "fetch(", "localStorage", "document."} {
			if strings.Contains(src, banned) {
				t.Errorf("%s contains %q; the shared row asset must stay pure presentation", name, banned)
			}
		}
	}
}

// TestRequiredGlobals pins RequiredGlobals to the STAGE_TRACKER_* names the
// embedded assets reference, so a new input cannot be added without listing it.
func TestRequiredGlobals(t *testing.T) {
	re := regexp.MustCompile(`\bSTAGE_TRACKER_[A-Z_]+\b`)
	seen := map[string]bool{}
	for _, name := range Files {
		b, err := fs.ReadFile(FS, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllString(string(b), -1) {
			seen[m] = true
		}
	}
	var got []string
	for m := range seen {
		got = append(got, m)
	}
	sort.Strings(got)
	want := append([]string(nil), RequiredGlobals...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("assets reference %v, RequiredGlobals is %v", got, want)
	}
}
