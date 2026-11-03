package dashboard

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// #12520: the Experiment Lab repo allow-list ("Repos experiments may target") pills toggled a class that no CSS
// rule consumed, so a selected repo looked identical to an excluded one. The
// two states must each have a distinct rule: selected is a filled amber chip
// with a leading ✓, excluded is a dimmed outline.
func TestNousRepoTargetPillsHaveDistinctOnOffStyles(t *testing.T) {
	html := indexHTML(t)

	onRule := regexp.MustCompile(`\.nous-config-pill\.is-on, \.nous-config-pill\[aria-pressed="true"\]\s*\{[^}]*\}`).FindString(html)
	if onRule == "" {
		t.Fatal("no .nous-config-pill.is-on / [aria-pressed=\"true\"] rule in index.html")
	}
	for _, want := range []string{"background: var(--amber)", "color: var(--bg)"} {
		if !strings.Contains(onRule, want) {
			t.Errorf("selected pill rule lacks %q so it is not a filled amber chip: %s", want, onRule)
		}
	}
	checkRule := regexp.MustCompile(`\.nous-config-pill\.is-on::before, \.nous-config-pill\[aria-pressed="true"\]::before\s*\{[^}]*\}`).FindString(html)
	if !strings.Contains(checkRule, "content: '✓'") {
		t.Errorf("selected pill has no leading ✓ marker: %q", checkRule)
	}

	offRule := regexp.MustCompile(`\.nous-config-pill\.is-off, \.nous-config-pill\[aria-pressed="false"\]\s*\{[^}]*\}`).FindString(html)
	if offRule == "" {
		t.Fatal("no .nous-config-pill.is-off / [aria-pressed=\"false\"] rule in index.html")
	}
	for _, want := range []string{"background: transparent", "color: var(--muted)", "opacity: 0.7"} {
		if !strings.Contains(offRule, want) {
			t.Errorf("excluded pill rule lacks %q so it is not a dimmed outline: %s", want, offRule)
		}
	}

	if regexp.MustCompile(`\.nous-config-pill\.active\s*\{\s*--pill-c`).MatchString(html) {
		t.Error("the dead .nous-config-pill.active { --pill-c } rule is back; nothing reads --pill-c on this pill")
	}

	// Pills must be real buttons so Enter/Space activate natively.
	render := jsFunctionBody(t, html, "function renderNousConfig(cfg)")
	if !strings.Contains(render, `<button type="button" class="${nousRepoPillClass(isActive)}" aria-pressed="${isActive}" data-repo=`) {
		t.Error("repo target pills are not rendered as <button type=\"button\"> with nousRepoPillClass/aria-pressed")
	}
	if strings.Contains(render, `class="nous-config-pill${isActive ? ' active' : ''}" role="button"`) {
		t.Error("repo target pills still use the div[role=button] + bare .active rendering")
	}
	save := jsFunctionBody(t, html, "async function saveNousConfig()")
	if !strings.Contains(save, "document.querySelectorAll('.nous-config-pill.active')") {
		t.Error("saveNousConfig no longer reads .nous-config-pill.active; the toggle must keep that hook in sync")
	}
}

// Runs the real toggle code under node with a minimal DOM double and checks
// class, aria-pressed, title and the "N of M repos allowed" counter all move.
func TestNousRepoTargetPillToggleUpdatesStateAndCounter(t *testing.T) {
	html := indexHTML(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: repo target pill toggle was not executed")
	}
	script := `const assert = require('node:assert/strict');
` + jsFunctionBody(t, html, "function nousRepoSummary(active, total)") +
		jsFunctionBody(t, html, "function nousRepoPillClass(isOn)") +
		jsFunctionBody(t, html, "function nousRepoPillTitle(repo, isOn)") +
		jsFunctionBody(t, html, "function nousRepoPillSetState(pill, isOn)") +
		jsFunctionBody(t, html, "function toggleNousRepo(pill)") + `
function pill(repo, on) {
  const classes = new Set(nousRepoPillClass(on).split(' '));
  const attrs = { 'aria-pressed': String(on) };
  return {
    dataset: { repo }, title: nousRepoPillTitle(repo, on),
    classList: {
      contains: (c) => classes.has(c),
      toggle: (c, force) => { if (force === undefined) force = !classes.has(c); force ? classes.add(c) : classes.delete(c); return force; },
    },
    setAttribute: (k, v) => { attrs[k] = v; },
    getAttribute: (k) => attrs[k],
  };
}
const pills = [pill('hivecommons/hive', true), pill('hivecommons/docs', true), pill('hivecommons/pluk', false)];
const summary = { textContent: '' };
global.document = {
  getElementById: (id) => id === 'nc-repo-summary' ? summary : null,
  querySelectorAll: (sel) => sel.endsWith('.is-on') ? pills.filter(p => p.classList.contains('is-on')) : pills,
};

// Initial render classes reflect the saved config.
assert.equal(nousRepoPillClass(true), 'nous-config-pill active is-on');
assert.equal(nousRepoPillClass(false), 'nous-config-pill is-off');
assert.equal(pills[0].title, 'hivecommons/hive — Click to exclude');
assert.equal(pills[2].title, 'hivecommons/pluk — Click to include');

// Exclude a selected repo.
toggleNousRepo(pills[0]);
assert.equal(pills[0].classList.contains('is-on'), false);
assert.equal(pills[0].classList.contains('is-off'), true);
assert.equal(pills[0].classList.contains('active'), false, '.active hook follows the toggle for saveNousConfig');
assert.equal(pills[0].getAttribute('aria-pressed'), 'false');
assert.equal(pills[0].title, 'hivecommons/hive — Click to exclude'.replace('exclude', 'include'));
assert.equal(summary.textContent, '1 of 3 repos allowed.');

// Include an excluded repo.
toggleNousRepo(pills[2]);
assert.equal(pills[2].classList.contains('is-on'), true);
assert.equal(pills[2].classList.contains('is-off'), false);
assert.equal(pills[2].classList.contains('active'), true);
assert.equal(pills[2].getAttribute('aria-pressed'), 'true');
assert.equal(pills[2].title, 'hivecommons/pluk — Click to exclude');
assert.equal(summary.textContent, '2 of 3 repos allowed.');

// Toggle everything off: the counter falls back to the empty-state hint.
toggleNousRepo(pills[1]);
toggleNousRepo(pills[2]);
assert.equal(summary.textContent, 'No repos selected — experiments may target any repo.');
assert.equal(nousRepoSummary(1, 1), '1 of 1 repo allowed.');
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node repo target pill toggle failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
