package dashboard

import (
	"html"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// sidebarEntry is one static sidebar link: what it shows and where it goes.
type sidebarEntry struct {
	label  string // "<emoji> <text>"
	target string
}

type sidebarGroup struct {
	id, label string
	body      string
	entries   []sidebarEntry
}

var (
	sidebarGroupStartRE = regexp.MustCompile(`<div class="oc-nav-group"([^>]*)>`)
	sidebarGroupLabelRE = regexp.MustCompile(`<div class="oc-nav-label">([^<]+)</div>`)
	sidebarLinkRE       = regexp.MustCompile(`<a class="oc-nav-item[^"]*"([^>]*)>\s*<span class="oc-nav-emoji">([^<]+)</span><span class="oc-nav-text">([^<]+)</span>`)
	helpMenuItemRE      = regexp.MustCompile(`<(?:a|button)[^>]*class="avatar-menu-item"[^>]*role="menuitem"([^>]*)>([^<]+)</(?:a|button)>`)
	attrRE              = regexp.MustCompile(`([\w-]+)="([^"]*)"`)
)

// navTarget names where a link goes, so a sidebar entry and a menu item that
// open the same thing compare equal however they spell it.
func navTarget(attrs string) string {
	a := map[string]string{}
	for _, m := range attrRE.FindAllStringSubmatch(attrs, -1) {
		a[m[1]] = m[2]
	}
	switch {
	case a["href"] != "":
		return "href:" + a["href"]
	case a["data-section"] != "":
		return "section:" + a["data-section"]
	case a["data-action"] == "ocNavigate":
		return "section:" + a["data-arg0"]
	default:
		return "action:" + a["data-action"] + ":" + a["data-arg0"]
	}
}

func parseSidebarGroups(t *testing.T, page string) []sidebarGroup {
	t.Helper()
	start := strings.Index(page, `<nav id="oc-sidebar"`)
	if start < 0 {
		t.Fatal("sidebar nav not found")
	}
	nav := page[start : start+strings.Index(page[start:], "</nav>")]
	starts := sidebarGroupStartRE.FindAllStringSubmatchIndex(nav, -1)
	var groups []sidebarGroup
	for i, m := range starts {
		end := len(nav)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		g := sidebarGroup{body: nav[m[0]:end]}
		if id := regexp.MustCompile(`id="([^"]+)"`).FindStringSubmatch(nav[m[2]:m[3]]); id != nil {
			g.id = id[1]
		}
		if l := sidebarGroupLabelRE.FindStringSubmatch(g.body); l != nil {
			g.label = strings.TrimSpace(l[1])
		}
		for _, l := range sidebarLinkRE.FindAllStringSubmatch(g.body, -1) {
			g.entries = append(g.entries, sidebarEntry{label: strings.TrimSpace(l[2]) + " " + html.UnescapeString(strings.TrimSpace(l[3])), target: navTarget(l[1])})
		}
		groups = append(groups, g)
	}
	return groups
}

func sidebarGroupByLabel(t *testing.T, groups []sidebarGroup, label string) sidebarGroup {
	t.Helper()
	for _, g := range groups {
		if g.label == label {
			return g
		}
	}
	t.Fatalf("sidebar has no %q group", label)
	return sidebarGroup{}
}

// TestSidebarGroupOrderAndMembership pins the sidebar layout: Admin is split
// into Pull requests, Usage and Troubleshooting, the agent pages sit in the
// Agents group, and Help is followed by Community.
func TestSidebarGroupOrderAndMembership(t *testing.T) {
	groups := parseSidebarGroups(t, indexHTML(t))
	var order []string
	for _, g := range groups {
		if g.label != "Filters" {
			order = append(order, g.label)
		}
	}
	wantOrder := []string{"Dashboard", "Collaborate", "Agents", "Resources", "Intelligence", "Pull requests", "Usage", "Troubleshooting", "Help", "Community"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("sidebar group order = %v, want %v", order, wantOrder)
	}

	want := map[string][]sidebarEntry{
		"Agents": {
			{"🎛 Control panel", "section:agents-section"},
			{"📡 Activity", "section:agent-activity-section"},
		},
		"Pull requests": {
			{"🔎 Review", "section:review-queue-section"},
			{"🔀 PR timeline", "section:lifecycle-section"},
		},
		"Usage": {
			{"🪙 Tokens", "section:token-panel"},
			{"💵 Cost", "section:cost-panel"},
		},
		"Troubleshooting": {
			{"🩺 Diagnostics", "section:debug-section"},
			{"📜 Audit log", "section:audit-section"},
		},
		"Help": {
			{"🚀 Getting started", "action:openWelcomeDialog:"},
			{"❓ FAQ", "section:faq-section"},
			{"📖 Docs", "href:https://docs.hivecommons.dev"},
			{"🔌 API reference", "href:/api/docs"},
			{"🐛 Report an issue", "action:openFeedbackModal:bug"},
		},
		"Community": {
			{"💬 Discord", "href:https://hivecommons.dev/discord"},
			{"📺 Live on Twitch", "href:https://www.twitch.tv/hivecommons"},
		},
	}
	for label, entries := range want {
		if got := sidebarGroupByLabel(t, groups, label).entries; !reflect.DeepEqual(got, entries) {
			t.Errorf("%s group entries = %v, want %v", label, got, entries)
		}
	}
	for _, g := range groups {
		if g.label == "Admin" {
			t.Error("the Admin group was split into Pull requests, Usage and Troubleshooting; it must not come back")
		}
	}
}

// TestSidebarAgentPagesOutsideCollapsibleTree pins that Control panel and
// Activity live in #oc-agent-nav-group above the collapsible agent list, so
// collapsing "All agents (n)" keeps them visible, and that no other group
// still links to them.
func TestSidebarAgentPagesOutsideCollapsibleTree(t *testing.T) {
	page := indexHTML(t)
	groups := parseSidebarGroups(t, page)
	var agents sidebarGroup
	for _, g := range groups {
		if g.id == "oc-agent-nav-group" {
			agents = g
		}
	}
	if agents.label != "Agents" {
		t.Fatalf("#oc-agent-nav-group heading = %q, want a plain Agents .oc-nav-label", agents.label)
	}
	body := agents.body
	treeStart := strings.Index(body, `<div class="oc-tree-children" id="oc-agent-tree-body">`)
	toggle := strings.Index(body, `<div class="oc-tree-toggle" data-action="ocToggleAgentTree"`)
	if treeStart < 0 || toggle < 0 || toggle > treeStart {
		t.Fatal("the All agents toggle must sit directly above #oc-agent-tree-body")
	}
	for _, section := range []string{"agents-section", "agent-activity-section"} {
		link := strings.Index(body, `data-section="`+section+`"`)
		if link < 0 {
			t.Fatalf("%s link is not in #oc-agent-nav-group", section)
		}
		if link > toggle {
			t.Errorf("%s link must come before the All agents toggle, outside .oc-tree-children", section)
		}
		for _, g := range groups {
			if g.id != "oc-agent-nav-group" && strings.Contains(g.body, `data-section="`+section+`"`) {
				t.Errorf("%s is still linked from the %q group", section, g.label)
			}
		}
	}
	if !strings.Contains(body, `All agents <span id="oc-agent-tree-count" class="oc-tree-count">(0)</span>`) {
		t.Error(`the toggle must read "All agents (n)" with a live count span`)
	}
	if !strings.Contains(jsFunc(t, page, "ocUpdateSidebarAgents"), `count.textContent = '(' + agents.length + ')';`) {
		t.Error("ocUpdateSidebarAgents must keep the All agents count in step with the rendered agents")
	}
	if !strings.Contains(page, `localStorage.setItem('hive-agent-tree-collapsed'`) {
		t.Error("the agent tree must keep its hive-agent-tree-collapsed saved state key")
	}
}

// TestTopbarHelpMenuMirrorsSidebarHelp pins the top bar "? Help" menu: an
// accessible menu button that reuses the .avatar-menu dropdown, whose items
// carry the same labels and targets as the sidebar Help group, and that keeps
// the Getting started checklist (what the old Guide chip opened) reachable.
func TestTopbarHelpMenuMirrorsSidebarHelp(t *testing.T) {
	page := indexHTML(t)
	topbar := dashboardTopbarHTML(t)
	btn := regexp.MustCompile(`<button[^>]*id="welcome-topbar-btn"[^>]*>`).FindString(topbar)
	for _, want := range []string{`type="button"`, `data-action="toggleHelpMenu"`, `aria-haspopup="menu"`, `aria-expanded="false"`, `aria-controls="oc-help-menu"`, `aria-label="Help menu"`} {
		if !strings.Contains(btn, want) {
			t.Errorf("Help menu trigger missing %s: %s", want, btn)
		}
	}
	if strings.Contains(btn, `display:none`) {
		t.Error("the Help menu trigger must be visible to every viewer; only its Getting started item is write-gated")
	}
	if !strings.Contains(topbar, `<div id="oc-help-menu" class="avatar-menu" role="menu" aria-label="Help" hidden>`) {
		t.Fatal("Help menu must reuse the .avatar-menu dropdown with role=menu")
	}
	menuStart := strings.Index(topbar, `id="oc-help-menu"`)
	menu := topbar[menuStart : menuStart+strings.Index(topbar[menuStart:], "</div>")]

	help := map[string]string{}
	for _, e := range sidebarGroupByLabel(t, parseSidebarGroups(t, page), "Help").entries {
		help[e.label] = e.target
	}
	var got []string
	for _, m := range helpMenuItemRE.FindAllStringSubmatch(menu, -1) {
		label := html.UnescapeString(strings.TrimSpace(m[2]))
		got = append(got, label)
		target, ok := help[label]
		if !ok {
			t.Errorf("Help menu item %q has no sidebar Help entry with the same label", label)
			continue
		}
		if mt := navTarget(m[1]); mt != target {
			t.Errorf("Help menu item %q opens %q, sidebar Help opens %q", label, mt, target)
		}
	}
	want := []string{"🚀 Getting started", "📖 Docs", "❓ FAQ", "🐛 Report an issue"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Help menu items = %v, want %v", got, want)
	}
	if !strings.Contains(menu, `id="welcome-help-menu-item" style="display:none" data-action="openWelcomeDialog"`) {
		t.Error("Getting started must open the welcome checklist and start hidden until write access is known")
	}
	for _, want := range []string{
		"const helpItem = document.getElementById('welcome-help-menu-item');",
		"function toggleHelpMenu()",
		"function closeHelpMenu(returnFocus)",
		"add(document.getElementById('oc-help-menu'), function() { if (typeof closeHelpMenu === 'function') closeHelpMenu(true); }, 30000);",
		"#oc-help-menu, #oc-gh-user-menu {\n        position: fixed;",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("Help menu wiring missing %q", want)
		}
	}
}
