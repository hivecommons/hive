package dashboard

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Getting Started deep links are declared as data: an entry in WELCOME_STEPS or
// WELCOME_MORE_ITEMS names `welcomeShowConfigTab` plus an `actionArg` that is
// supposed to be one of GOVERNOR_CONFIG_TABS. Nothing validates that at
// runtime: welcomeShowConfigTab() hands the value to openConfigDialog(), whose
// override resolution is `tabs.includes(overrideTab) ? overrideTab : ...
// tabs[0]`. A misspelt, stale, or — as in #7452 — never-interpolated
// `'${TRAJECTORY_CONFIG_TAB}'` argument is therefore not an error: the dialog
// silently opens tabs[0] ('General') instead, which is why the Trajectory
// Review link looked like it worked for as long as that section happened to
// live on General.
//
// This test is the missing validation. It resolves every actionArg routed to
// welcomeShowConfigTab — literal or identifier — and requires it to be a member
// of GOVERNOR_CONFIG_TABS, which is itself resolved the same way because that
// list contains an identifier entry (GOVERNOR_BOB_TAB).

var (
	// 'General' / "General" — single- or double-quoted JS string literal.
	jsStringLiteralRe = regexp.MustCompile(`^'([^']*)'$|^"([^"]*)"$`)
	// A bare JS identifier used in place of a literal, e.g. GOVERNOR_BOB_TAB.
	jsIdentifierRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
	// A whole-expression template literal, `${NAME}`, which is the other
	// correct way to spell the deep link.
	jsTemplateIdentRe = regexp.MustCompile("^`\\$\\{\\s*([A-Za-z_$][A-Za-z0-9_$]*)\\s*\\}`$")
	// action: 'welcomeShowConfigTab', ... actionArg: <expr> — same entry, and
	// in this file always the same source line. The alternation matches a
	// complete quoted or templated literal FIRST so a '${X}' argument is not
	// truncated at the brace.
	configTabActionRe = regexp.MustCompile("action:\\s*'welcomeShowConfigTab'\\s*,\\s*actionArg:\\s*('(?:[^'\\\\]|\\\\.)*'|\"(?:[^\"\\\\]|\\\\.)*\"|`[^`]*`|[^,}\\]]+)")
	// Direct calls: welcomeShowConfigTab('Security')
	configTabCallRe = regexp.MustCompile(`(?:^|[^.\w])welcomeShowConfigTab\(\s*([^)]+?)\s*\)`)
)

// resolveJSTabExpr turns an actionArg / tab-list expression into the string it
// evaluates to. A quoted literal yields itself; a bare identifier is looked up
// among the file's `const NAME = '...';` declarations. Anything else (a
// template literal, a call, a concatenation) is reported unresolved so the
// caller can fail loudly rather than guess.
func resolveJSTabExpr(t *testing.T, html, expr string) (string, bool) {
	t.Helper()
	expr = strings.TrimSpace(expr)
	if m := jsStringLiteralRe.FindStringSubmatch(expr); m != nil {
		lit := m[1] + m[2]
		// A single-quoted '${X}' is a literal, not an interpolation — the
		// exact shape of #7452. Resolving it to its own text is correct: the
		// membership assertion below is what rejects it.
		return lit, true
	}
	if m := jsTemplateIdentRe.FindStringSubmatch(expr); m != nil {
		return resolveJSTabExpr(t, html, m[1])
	}
	if jsIdentifierRe.MatchString(expr) {
		decl := regexp.MustCompile(`const\s+` + regexp.QuoteMeta(expr) + `\s*=\s*('[^']*'|"[^"]*")\s*;`)
		m := decl.FindStringSubmatch(html)
		if m == nil {
			return "", false
		}
		v, ok := resolveJSTabExpr(t, html, m[1])
		return v, ok
	}
	return "", false
}

// jsArrayLiteral returns the text between the brackets of `const NAME = [ ... ]`.
func jsArrayLiteral(t *testing.T, html, name string) string {
	t.Helper()
	start := strings.Index(html, "const "+name+" = [")
	if start < 0 {
		t.Fatalf("static/index.html: %s declaration not found", name)
	}
	open := strings.Index(html[start:], "[")
	depth := 0
	for i := start + open; i < len(html); i++ {
		switch html[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return html[start+open+1 : i]
			}
		}
	}
	t.Fatalf("static/index.html: %s array literal is unterminated", name)
	return ""
}

func governorConfigTabs(t *testing.T, html string) map[string]bool {
	t.Helper()
	body := jsArrayLiteral(t, html, "GOVERNOR_CONFIG_TABS")
	tabs := map[string]bool{}
	for _, raw := range strings.Split(body, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		v, ok := resolveJSTabExpr(t, html, entry)
		if !ok {
			t.Fatalf("GOVERNOR_CONFIG_TABS entry %q could not be resolved to a tab name", entry)
		}
		tabs[v] = true
	}
	if len(tabs) < 5 {
		t.Fatalf("GOVERNOR_CONFIG_TABS parsed to only %d tabs (%v) — the parser, not the list, is probably wrong", len(tabs), tabs)
	}
	return tabs
}

func TestWelcomeConfigTabDeepLinksNameRealTabs(t *testing.T) {
	body, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	tabs := governorConfigTabs(t, html)

	checked := 0
	check := func(where, expr string) {
		v, ok := resolveJSTabExpr(t, html, expr)
		if !ok {
			t.Errorf("%s: welcomeShowConfigTab argument %s does not resolve to a tab name. "+
				"Pass a quoted tab name or a const declared as a quoted tab name — openConfigDialog() "+
				"silently falls back to GOVERNOR_CONFIG_TABS[0] for anything it cannot match.", where, expr)
			return
		}
		if !tabs[v] {
			t.Errorf("%s: welcomeShowConfigTab argument %s resolves to %q, which is not in GOVERNOR_CONFIG_TABS. "+
				"openConfigDialog() will silently open the remembered tab or GOVERNOR_CONFIG_TABS[0] instead (#7452).", where, expr, v)
			return
		}
		checked++
	}

	for _, name := range []string{"WELCOME_STEPS", "WELCOME_MORE_ITEMS"} {
		arr := jsArrayLiteral(t, html, name)
		for _, m := range configTabActionRe.FindAllStringSubmatch(arr, -1) {
			check(name, strings.TrimSpace(m[1]))
		}
	}
	if checked < 5 {
		t.Fatalf("only %d welcomeShowConfigTab deep links were validated — the extraction is probably broken", checked)
	}

	// Same guarantee for imperative call sites, which take the identical path
	// through openConfigDialog().
	for _, m := range configTabCallRe.FindAllStringSubmatch(html, -1) {
		arg := strings.TrimSpace(m[1])
		// Skip the function declaration and any pass-through of a variable
		// that is not a tab constant (e.g. welcomeShowConfigTab(tab)).
		if arg == "tab" || strings.HasPrefix(arg, "function") {
			continue
		}
		if jsIdentifierRe.MatchString(arg) {
			if _, ok := resolveJSTabExpr(t, html, arg); !ok {
				continue
			}
		}
		check("welcomeShowConfigTab() call site", arg)
	}
}

// TestTrajectoryReviewLivesOnFeaturesTab pins the placement half of #7452:
// Trajectory Review is a named, default-off, opt-in capability lane, so it
// belongs with the other lanes on Settings → Features, not among General's
// always-in-force baseline settings. The deep-link constant must agree.
func TestTrajectoryReviewLivesOnFeaturesTab(t *testing.T) {
	body, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)

	tab, ok := resolveJSTabExpr(t, html, "TRAJECTORY_CONFIG_TAB")
	if !ok {
		t.Fatal("TRAJECTORY_CONFIG_TAB is not declared as a quoted tab name")
	}
	if tab != "Features" {
		t.Errorf("TRAJECTORY_CONFIG_TAB = %q, want \"Features\"", tab)
	}

	general := jsFunctionBody(t, html, "function renderGovGeneral()")
	features := jsFunctionBody(t, html, "function renderGovFeatures(f, am, rv)")
	for _, marker := range []string{
		"toggleTrajectoryReview",
		"saveTrajectoryField",
		`data-arg0="onDivergence"`,
	} {
		if strings.Contains(general, marker) {
			t.Errorf("renderGovGeneral() still renders Trajectory Review control %q", marker)
		}
		if !strings.Contains(features, marker) {
			t.Errorf("renderGovFeatures() is missing Trajectory Review control %q", marker)
		}
	}
}
