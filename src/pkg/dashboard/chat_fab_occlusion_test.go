package dashboard

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

type chatCSSRule struct {
	selectors []string
	decls     map[string]string
}

var chatCSSCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)

// chatCSSDecls parses a flat `prop: value; …` rule body into a map.
func chatCSSDecls(body string) map[string]string {
	decls := map[string]string{}
	for _, decl := range strings.Split(chatCSSCommentRE.ReplaceAllString(body, ""), ";") {
		prop, value, ok := strings.Cut(decl, ":")
		if !ok {
			continue
		}
		decls[strings.TrimSpace(prop)] = strings.TrimSpace(value)
	}
	return decls
}

// chatCSSRules scans flat (non-nested) `selector { body }` pairs from a
// stylesheet slice.
func chatCSSRules(css string) []chatCSSRule {
	var rules []chatCSSRule
	for _, chunk := range strings.Split(chatCSSCommentRE.ReplaceAllString(css, ""), "}") {
		selectors, body, ok := strings.Cut(chunk, "{")
		if !ok {
			continue
		}
		rule := chatCSSRule{decls: chatCSSDecls(body)}
		for _, sel := range strings.Split(selectors, ",") {
			rule.selectors = append(rule.selectors, strings.TrimSpace(sel))
		}
		rules = append(rules, rule)
	}
	return rules
}

func (r chatCSSRule) hasSelector(sel string) bool {
	for _, s := range r.selectors {
		if s == sel {
			return true
		}
	}
	return false
}

func TestChatFABIsReadableThroughAtRestAndAccessibleOnFocus(t *testing.T) {
	html := indexHTML(t)
	const why = "the fixed chat button must stay readable-through at rest and fully visible for hover/focus/open states (#7160)"

	rest := chatCSSDecls(indexSliceBetween(t, html, ".hive-chat-fab {", "}"))
	opacity, err := strconv.ParseFloat(rest["opacity"], 64)
	if err != nil || opacity >= 1 {
		t.Errorf(".hive-chat-fab opacity = %q, want a float < 1 — %s", rest["opacity"], why)
	}
	if !strings.Contains(rest["transition"], "opacity") {
		t.Errorf(".hive-chat-fab transition = %q, want opacity to be animated — %s", rest["transition"], why)
	}

	rules := chatCSSRules(indexSliceBetween(t, html, "/* Hive Chat Panel */", ".hive-chat-panel {"))
	solidStates := []string{".hive-chat-fab:hover", ".hive-chat-fab:focus-visible", ".hive-chat-fab.open", ".hive-chat-fab.has-unread"}
	for _, state := range solidStates {
		solid := false
		for _, rule := range rules {
			if rule.hasSelector(state) && rule.decls["opacity"] == "1" {
				solid = true
			}
		}
		if !solid {
			t.Errorf("no rule sets opacity: 1 for %s — %s", state, why)
		}
	}
	focusOutline := false
	for _, rule := range rules {
		if outline, ok := rule.decls["outline"]; ok && rule.hasSelector(".hive-chat-fab:focus-visible") && outline != "none" && outline != "" {
			focusOutline = true
		}
	}
	if !focusOutline {
		t.Errorf("no .hive-chat-fab:focus-visible rule sets a visible outline — keyboard users need a focus ring (#7160)")
	}

	button := indexSliceBetween(t, html, `<button type="button" class="hive-chat-fab"`, ">")
	if !strings.Contains(button, `id="hiveChatFab"`) || !regexp.MustCompile(`aria-label="[^"]+"`).MatchString(button) {
		t.Errorf("chat FAB markup %q must carry id=\"hiveChatFab\" and a non-empty aria-label (#7160)", button)
	}

	handlers := indexSliceBetween(t, html, "chatFab.onclick = () => {", "\n    chatCheatToggle.onclick")
	runNodeScript(t, `
const assert = require('node:assert/strict');
function classList() {
  const set = new Set();
  return {
    add: (c) => { set.add(c); },
    remove: (c) => { set.delete(c); },
    contains: (c) => set.has(c),
    toggle: (c, force) => {
      const on = force === undefined ? !set.has(c) : !!force;
      if (on) set.add(c); else set.delete(c);
      return on;
    },
  };
}
const chatPanel = { classList: classList() };
const chatFab = { classList: classList() };
const chatInput = { focus: () => {} };
const chatMessages = { scrollTop: 0, scrollHeight: 0 };
function chatPollMessages() {}
function refreshChatPresence() {}
const chatClose = {};
`+handlers+`
chatFab.classList.add('has-unread');
assert(!chatFab.classList.contains('open'), 'FAB must start closed (#7160)');
chatFab.onclick();
assert(chatPanel.classList.contains('open') && chatFab.classList.contains('open'), 'opening the panel must mark the FAB open so it goes solid (#7160)');
assert(!chatFab.classList.contains('has-unread'), 'opening the panel must clear the unread marker (#7160)');
chatClose.onclick();
assert(!chatPanel.classList.contains('open') && !chatFab.classList.contains('open'), 'closing the panel must clear open on both (#7160)');
chatFab.onclick();
assert(chatPanel.classList.contains('open') && chatFab.classList.contains('open'), 'second click must reopen (#7160)');
chatFab.onclick();
assert(!chatPanel.classList.contains('open') && !chatFab.classList.contains('open'), 'FAB click must toggle closed symmetrically (#7160)');
`)
}
