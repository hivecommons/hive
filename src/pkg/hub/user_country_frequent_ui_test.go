package hub

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The Country dropdown in the admin contact editor floats countries already
// assigned to MORE THAN ONE loaded user to a "frequent" group at the top, so
// an admin filing new users into the same handful of countries stops paging
// through 250 alphabetical rows. The grouping lives in inline dashboardHTML
// JavaScript (frequentCountryCodes / countrySelectOptionsHTML), which no Go
// test can execute directly — so, exactly like upgrade_filter_agreement_test.go,
// these tests extract the REAL shipped functions and run them under node
// rather than re-implementing the logic in Go, which could agree with itself
// while the shipped dashboard stayed broken.

// countryJSDecl slices one top-level `var NAME = ...;` line out of
// dashboardHTML, so the harness runs against the shipped constant rather than
// a copy that could drift.
func countryJSDecl(t *testing.T, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^ {4}(var ` + regexp.QuoteMeta(name) + ` = .*;)`)
	m := re.FindStringSubmatch(dashboardHTML)
	if m == nil {
		t.Fatalf("declaration of %s not found in dashboardHTML — renamed or removed; "+
			"update this test deliberately, do not delete it", name)
	}
	return m[1]
}

// countryHarness is the shipped dropdown logic plus the globals it closes
// over. esc/escAttr are DOM-backed in the browser (document.createElement), so
// the harness substitutes equivalent string-only escapers — everything the
// grouping logic itself does is the extracted, shipped source.
func countryHarness(t *testing.T, allUsersJS string) string {
	t.Helper()
	return strings.Join([]string{
		countryJSDecl(t, "COUNTRY_CODE_LEN"),
		countryJSDecl(t, "COUNTRY_FREQUENT_MIN_USERS"),
		countryJSDecl(t, "COUNTRY_FREQUENT_SEPARATOR"),
		countryJSDecl(t, "ISO_COUNTRY_CODES"),
		"function esc(s) { return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }",
		"function escAttr(s) { return esc(s).replace(/\"/g, '&quot;').replace(/'/g, '&#39;'); }",
		"var _allUsers = " + allUsersJS + ";",
		jsFunc(t, "normalizeCountryCode"),
		jsFunc(t, "countryDisplayName"),
		jsFunc(t, "frequentCountryCodes"),
		jsFunc(t, "countrySelectOptionsHTML"),
	}, "\n")
}

// runCountryJS evaluates body (which must assign to `result`) against the
// harness and unmarshals the JSON it prints.
func runCountryJS(t *testing.T, allUsersJS, body string, out interface{}) {
	t.Helper()
	src := countryHarness(t, allUsersJS) + "\nvar result;\n" + body +
		"\nprocess.stdout.write(JSON.stringify(result));\n"
	cmd := exec.Command("node", "-e", src)
	stdout, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node failed: %v\nstderr:\n%s", err, ee.Stderr)
		}
		t.Skipf("node unavailable: %v", err)
	}
	if err := json.Unmarshal(stdout, out); err != nil {
		t.Fatalf("bad JSON from node: %v\ngot: %s", err, stdout)
	}
}

// TestFrequentCountryCodesGrouping drives the shipped frequency computation
// through its contract: only counts above one float, ordered by count
// descending then name, with empty and malformed countries never counting.
func TestFrequentCountryCodesGrouping(t *testing.T) {
	cases := []struct {
		name  string
		users string
		want  []string
	}{
		{
			// The screenshot's roster shape: US dominates, GB repeats, DE is a
			// one-off. Only the repeated two float, most-used first.
			name:  "counts above one float ordered by count desc",
			users: `[{country:'US'},{country:'US'},{country:'US'},{country:'GB'},{country:'GB'},{country:'DE'}]`,
			want:  []string{"US", "GB"},
		},
		{
			// Equal counts fall back to the localized name: France before
			// Germany, regardless of the order users happen to arrive in.
			name:  "ties ordered by display name",
			users: `[{country:'DE'},{country:'FR'},{country:'DE'},{country:'FR'}]`,
			want:  []string{"FR", "DE"},
		},
		{
			// A single assignment is not a pattern — nothing floats.
			name:  "singletons never float",
			users: `[{country:'US'},{country:'GB'},{country:'DE'}]`,
			want:  []string{},
		},
		{
			// Country-less and malformed values must not count toward any
			// group: '' is the deliberate "no flag" state, 'USA'/'u' would
			// have been rejected by normalizeCountryCode everywhere else.
			name:  "empty and malformed countries ignored",
			users: `[{country:''},{country:''},{},{country:null},{country:'USA'},{country:'USA'},{country:'u'},{country:'u'}]`,
			want:  []string{},
		},
		{
			// Lowercase codes are the same country — normalizeCountryCode
			// uppercases before counting, so 'us' + 'US' is two users of US.
			name:  "case-insensitive counting",
			users: `[{country:'us'},{country:'US'}]`,
			want:  []string{"US"},
		},
		{
			name:  "no users no group",
			users: `[]`,
			want:  []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			runCountryJS(t, tc.users, "result = frequentCountryCodes(_allUsers);", &got)
			if len(got) != len(tc.want) {
				t.Fatalf("frequentCountryCodes = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("frequentCountryCodes = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestCountrySelectOptionsFrequentGroup pins the rendered <option> structure:
// "— none —" stays first, the frequent group sits directly under it, a
// disabled separator divides it from the full alphabetical list, and frequent
// codes stay DUPLICATED in that list so alphabetical muscle memory still works.
func TestCountrySelectOptionsFrequentGroup(t *testing.T) {
	users := `[{country:'US'},{country:'US'},{country:'US'},{country:'GB'},{country:'GB'},{country:'DE'}]`
	var html string
	runCountryJS(t, users, "result = countrySelectOptionsHTML('');", &html)

	if !strings.HasPrefix(html, `<option value="" selected>— none —</option>`) {
		t.Fatalf("options must start with the selected — none — clearer, got prefix %q", html[:80])
	}
	sep := `<option value="" disabled>──────────</option>`
	sepAt := strings.Index(html, sep)
	if sepAt < 0 {
		t.Fatalf("disabled separator missing from options:\n%s", html[:200])
	}
	head, tail := html[:sepAt], html[sepAt+len(sep):]

	// Frequent group above the separator: US then GB (US has more users), and
	// the one-off DE must NOT be up there.
	usAt := strings.Index(head, `<option value="US"`)
	gbAt := strings.Index(head, `<option value="GB"`)
	if usAt < 0 || gbAt < 0 || usAt > gbAt {
		t.Errorf("frequent group must list US before GB above the separator (US at %d, GB at %d)", usAt, gbAt)
	}
	if strings.Contains(head, `<option value="DE"`) {
		t.Error("DE has only one user and must not float above the separator")
	}

	// The full alphabetical list below the separator still carries the
	// frequent codes — floating is an addition, not a move.
	for _, code := range []string{"US", "GB", "DE", "AF"} {
		if !strings.Contains(tail, `<option value="`+code+`"`) {
			t.Errorf("full list below the separator is missing %s", code)
		}
	}
}

// TestCountrySelectOptionsNoFrequentNoSeparator: with no repeated countries
// the dropdown must look exactly as it always has — no group, no separator.
func TestCountrySelectOptionsNoFrequentNoSeparator(t *testing.T) {
	var html string
	runCountryJS(t, `[{country:'US'}]`, "result = countrySelectOptionsHTML('');", &html)
	if strings.Contains(html, "disabled") {
		t.Error("no country repeats, so there must be no separator")
	}
	if c := strings.Count(html, `<option value="US"`); c != 1 {
		t.Errorf("US must appear exactly once without a frequent group, got %d", c)
	}
}

// TestCountrySelectOptionsSelectedOnce: when the current code is itself
// frequent it appears twice, and only the FREQUENT copy may carry selected —
// duplicate selected attributes would make the browser pick the later,
// alphabetical copy and scroll the closed select away from the top group.
func TestCountrySelectOptionsSelectedOnce(t *testing.T) {
	users := `[{country:'US'},{country:'US'}]`
	var html string
	runCountryJS(t, users, "result = countrySelectOptionsHTML('US');", &html)
	if c := strings.Count(html, " selected"); c != 1 {
		t.Fatalf("exactly one option may be selected, got %d:\n%s", c, html[:300])
	}
	sepAt := strings.Index(html, "disabled")
	selAt := strings.Index(html, `<option value="US" selected`)
	if selAt < 0 || sepAt < 0 || selAt > sepAt {
		t.Errorf("selected must be the frequent copy above the separator (selected at %d, separator at %d)", selAt, sepAt)
	}

	// A current code that is NOT frequent is still marked selected, in the
	// alphabetical list where its only copy lives.
	runCountryJS(t, users, "result = countrySelectOptionsHTML('JP');", &html)
	if c := strings.Count(html, `<option value="JP" selected`); c != 1 {
		t.Errorf("non-frequent current code must be selected in the main list exactly once, got %d", c)
	}
	if c := strings.Count(html, " selected"); c != 1 {
		t.Errorf("exactly one option may be selected, got %d", c)
	}
}

// TestCountryFrequentUsesNamedConstants guards the repo's no-magic-numbers
// rule for the two values this feature introduced.
func TestCountryFrequentUsesNamedConstants(t *testing.T) {
	for _, decl := range []string{
		"var COUNTRY_FREQUENT_MIN_USERS = 2;",
		"var COUNTRY_FREQUENT_SEPARATOR =",
	} {
		if !strings.Contains(dashboardHTML, decl) {
			t.Errorf("missing named constant %q — thresholds must never be inline literals", decl)
		}
	}
	// The threshold must stay "more than one": a lone assignment reordering
	// the list for every admin would be noise, not a pattern.
	if !strings.Contains(dashboardHTML, "counts[c] >= COUNTRY_FREQUENT_MIN_USERS") {
		t.Error("frequentCountryCodes must gate on COUNTRY_FREQUENT_MIN_USERS")
	}
}
