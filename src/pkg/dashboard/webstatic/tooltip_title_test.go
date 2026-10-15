package webstatic

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The dashboard's custom help tooltip is a `.config-tooltip` nested inside a
// `.config-info` trigger and rendered into a body-level portal on hover/focus.
// If that trigger, or any ancestor in the same markup, also carries a
// non-empty native `title=`, the browser draws its own tooltip alongside the
// styled one and the user sees the same text twice (#12017: the Delivery SLA
// tiles). The guard below scans index.html — static markup and the JS template
// literals that build markup at runtime — for that double-tooltip shape.
//
// A trigger may opt out of an ancestor's title with an explicit `title=""`,
// which is how the section help marks suppress the header's collapse hint.

var (
	tooltipTagRe         = regexp.MustCompile(`<(/?)([a-zA-Z][\w-]*)([^<>]*?)(/?)>`)
	tooltipTitleAttrRe   = regexp.MustCompile(`\btitle="([^"]*)"`)
	tooltipClassAttrRe   = regexp.MustCompile(`\bclass="([^"]*)"`)
	tooltipFuncDeclRe    = regexp.MustCompile(`function\s+([A-Za-z_$][\w$]*)\s*\(`)
	tooltipVoidElements  = map[string]bool{"br": true, "hr": true, "img": true, "input": true, "path": true, "circle": true, "rect": true, "line": true, "polyline": true, "use": true, "meta": true, "link": true}
	tooltipCustomClass   = "config-tooltip"
	tooltipFuncBodyLimit = 8000
)

type tooltipOffender struct {
	Line    int
	Trigger string
	TitleOn string
}

// tooltipHelperFuncs returns the names of JS functions whose body emits
// `.config-tooltip` markup, keyed to whether that markup suppresses ancestor
// titles with its own `title=""`.
func tooltipHelperFuncs(src string) map[string]bool {
	helpers := map[string]bool{}
	for _, loc := range tooltipFuncDeclRe.FindAllStringSubmatchIndex(src, -1) {
		name := src[loc[2]:loc[3]]
		start := loc[0]
		end := len(src)
		if next := strings.Index(src[start+1:], "\n    function "); next >= 0 {
			end = start + 1 + next
		}
		if end-start > tooltipFuncBodyLimit {
			end = start + tooltipFuncBodyLimit
		}
		body := src[start:end]
		if !hasCustomTooltipClass(body) {
			continue
		}
		helpers[name] = strings.Contains(body, `title=""`)
	}
	return helpers
}

func hasCustomTooltipClass(markup string) bool {
	for _, m := range tooltipClassAttrRe.FindAllStringSubmatch(markup, -1) {
		for _, c := range strings.Fields(m[1]) {
			if c == tooltipCustomClass {
				return true
			}
		}
	}
	return false
}

type tooltipOpenTag struct {
	tag        string
	title      string
	emptyTitle bool
}

type tooltipEvent struct {
	pos    int
	kind   string // open | close | helper
	tag    string
	attrs  string
	self   bool
	helper string
}

// findDoubleTooltips reports every custom-tooltip trigger that would also
// inherit a non-empty native title from itself or an enclosing element on the
// same line. Templates are scanned line by line because the dashboard keeps
// each template literal's markup on one line.
func findDoubleTooltips(src string) []tooltipOffender {
	helpers := tooltipHelperFuncs(src)
	var offenders []tooltipOffender
	for i, line := range strings.Split(src, "\n") {
		if !tooltipLineRelevant(line, helpers) {
			continue
		}
		var events []tooltipEvent
		for _, m := range tooltipTagRe.FindAllStringSubmatchIndex(line, -1) {
			tag := line[m[4]:m[5]]
			ev := tooltipEvent{pos: m[0], kind: "open", tag: tag, attrs: line[m[6]:m[7]]}
			if m[2] != m[3] {
				ev.kind = "close"
			}
			ev.self = m[8] != m[9] || tooltipVoidElements[strings.ToLower(tag)]
			events = append(events, ev)
		}
		for name := range helpers {
			needle := "${" + name + "("
			from := 0
			for {
				idx := strings.Index(line[from:], needle)
				if idx < 0 {
					break
				}
				events = append(events, tooltipEvent{pos: from + idx, kind: "helper", helper: name})
				from += idx + len(needle)
			}
		}
		sort.Slice(events, func(a, b int) bool { return events[a].pos < events[b].pos })

		var stack []tooltipOpenTag
		report := func(trigger string, suppressed bool) {
			if suppressed {
				return
			}
			for k := len(stack) - 1; k >= 0; k-- {
				if stack[k].emptyTitle {
					return
				}
				if stack[k].title != "" {
					offenders = append(offenders, tooltipOffender{Line: i + 1, Trigger: trigger, TitleOn: "<" + stack[k].tag + " title=\"" + stack[k].title + "\">"})
					return
				}
			}
		}
		for _, ev := range events {
			switch ev.kind {
			case "open":
				entry := tooltipOpenTag{tag: ev.tag}
				if t := tooltipTitleAttrRe.FindStringSubmatch(ev.attrs); t != nil {
					entry.title = t[1]
					entry.emptyTitle = t[1] == ""
				}
				if hasCustomTooltipClass("<x " + ev.attrs + ">") {
					report("<"+ev.tag+" class=\"…config-tooltip…\">", false)
				}
				if !ev.self {
					stack = append(stack, entry)
				}
			case "close":
				for k := len(stack) - 1; k >= 0; k-- {
					if stack[k].tag == ev.tag {
						stack = stack[:k]
						break
					}
				}
			case "helper":
				report("${"+ev.helper+"(…)}", helpers[ev.helper])
			}
		}
	}
	return offenders
}

func tooltipLineRelevant(line string, helpers map[string]bool) bool {
	if strings.Contains(line, tooltipCustomClass) {
		return true
	}
	for name := range helpers {
		if strings.Contains(line, "${"+name+"(") {
			return true
		}
	}
	return false
}

func TestNoDoubleTooltipsInDashboardIndex(t *testing.T) {
	src := readStyleRatchetFile(t, "../static/index.html")
	helpers := tooltipHelperFuncs(src)
	if len(helpers) == 0 {
		t.Fatal("no JS helpers emitting .config-tooltip found; the scanner's helper discovery is broken")
	}
	for _, o := range findDoubleTooltips(src) {
		t.Errorf("index.html:%d: custom tooltip %s also inherits a native %s — drop the title (or set title=\"\" on the trigger) so the ⓘ shows one tooltip", o.Line, o.Trigger, o.TitleOn)
	}
}

func TestFindDoubleTooltipsScanner(t *testing.T) {
	const fixture = "    function tipMark(id, tip) {\n" +
		"      return `<span class=\"config-info\" aria-label=\"details\">ⓘ<span id=\"${id}\" class=\"config-tooltip\" role=\"tooltip\">${tip}</span></span>`;\n" +
		"    }\n" +
		"    function quietMark(id, tip) {\n" +
		"      return `<a class=\"config-info\" title=\"\" aria-label=\"help\">?<span id=\"${id}\" class=\"config-tooltip\">${tip}</span></a>`;\n" +
		"    }\n" +
		"    function plain() {\n" +
		"      return `<div title=\"only native\">n=3</div>`;\n" +
		"    }\n" +
		"    function render() {\n" +
		"      const bad = `<div class=\"tile\" title=\"${esc(tip)}\"><div class=\"lbl\">p50${tipMark('a', tip)}</div></div>`;\n" +
		"      const inline = `<div title=\"hint\"><span class=\"config-info\">ⓘ<span class=\"config-tooltip\">x</span></span></div>`;\n" +
		"      const suppressed = `<div title=\"Collapse\">${quietMark('b', tip)}</div>`;\n" +
		"      const sibling = `<div><button title=\"Hide\">▾</button>${tipMark('c', tip)}</div>`;\n" +
		"      const closedBefore = `<div title=\"n=3\">3</div><span class=\"config-info\">ⓘ<span class=\"config-tooltip\">y</span></span>`;\n" +
		"      const selfEmpty = `<div title=\"outer\"><span class=\"config-info\" title=\"\">ⓘ<span class=\"config-tooltip\">z</span></span></div>`;\n" +
		"    }\n"

	helpers := tooltipHelperFuncs(fixture)
	if suppress, ok := helpers["tipMark"]; !ok || suppress {
		t.Fatalf("tipMark helper = (%v, %v), want discovered and non-suppressing", suppress, ok)
	}
	if suppress, ok := helpers["quietMark"]; !ok || !suppress {
		t.Fatalf("quietMark helper = (%v, %v), want discovered and suppressing", suppress, ok)
	}
	if _, ok := helpers["plain"]; ok {
		t.Fatal("plain emits no config-tooltip markup yet was classified as a helper")
	}

	got := findDoubleTooltips(fixture)
	wantLines := []int{11, 12}
	if len(got) != len(wantLines) {
		t.Fatalf("offenders = %+v, want exactly lines %v", got, wantLines)
	}
	for i, o := range got {
		if o.Line != wantLines[i] {
			t.Errorf("offender[%d] on line %d, want %d (%+v)", i, o.Line, wantLines[i], o)
		}
	}
	if !strings.Contains(got[0].Trigger, "tipMark") || !strings.Contains(got[0].TitleOn, "${esc(tip)}") {
		t.Errorf("offender[0] = %+v, want the tipMark call blamed on the titled tile", got[0])
	}
	if !strings.Contains(got[1].TitleOn, `title="hint"`) {
		t.Errorf("offender[1] = %+v, want the inline trigger blamed on <div title=\"hint\">", got[1])
	}
}
