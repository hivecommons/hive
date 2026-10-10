package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestACMMNavbarPillOpensDialogAndNoPreviewButtons(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the ACMM dialog navbar behaviour was NOT executed by this run")
	}

	html := indexHTML(t)
	topbar := dashboardTopbarHTML(t)
	badge := `<button type="button" id="acmm-badge" class="nav-chip nav-chip--level nav-chip--gold" data-action="openACMMDialog" aria-haspopup="dialog" aria-controls="acmm-overlay"`
	if !strings.Contains(topbar, badge) {
		t.Fatalf("navbar ACMM pill is not wired as a dialog button: missing %q", badge)
	}
	if strings.Contains(html, `id="acmm-sidebar-badge"`) {
		t.Fatal("sidebar ACMM badge still exists")
	}

	script := "const assert = require('node:assert/strict');\n" +
		"const elements = new Map();\n" +
		"function el(id){ const o = { id, style:{}, innerHTML:'', textContent:'', classList:{ hidden:true, remove(c){ if(c === 'hidden') this.hidden = false; }, add(c){ if(c === 'hidden') this.hidden = true; }, contains(c){ return c === 'hidden' ? this.hidden : false; } } }; elements.set(id, o); return o; }\n" +
		"['acmm-overlay','acmm-apply-error','acmm-dialog-current','acmm-packs-list'].forEach(el);\n" +
		"global.document = { getElementById: id => elements.get(id) || null };\n" +
		"global.window = { _lastStatus: { acmmLevel: 1 } };\n" +
		"global.fetch = async (url) => { assert.equal(url, '/api/packs'); return { json: async () => [{ level: 2, name: 'Advisory', description: 'observe only', agentCount: 1, governor: { modes: 'SURGE', mergePolicy: 'manual' }, agents: [{ name: 'guide', displayName: 'guide', description: 'docs', emoji: '🧭', color: '#8e44ad' }] }] }; };\n" +
		"let _acmmOpen = false; let _acmmPacks = null;\n" +
		"function hiveRememberModalOpen() {}\n" +
		jsFunc(t, html, "esc") + "\n" +
		"const ACMM_LEVEL_COLORS = ['var(--status-neutral)', 'var(--acmm-level-1)', 'var(--acmm-level-2)', 'var(--acmm-level-3)', 'var(--acmm-level-4)', 'var(--acmm-level-5)', 'var(--acmm-level-6)'];\n" +
		"const acmmLevelColor = level => ACMM_LEVEL_COLORS[level] || 'var(--status-info)';\n" +
		jsFunc(t, html, "acmmLevelLabel") + "\n" +
		jsFunc(t, html, "renderACMMPacks") + "\n" +
		jsFunc(t, html, "openACMMDialog") + "\n" +
		"(async () => { await openACMMDialog(); assert.equal(elements.get('acmm-overlay').classList.contains('hidden'), false); const rendered = elements.get('acmm-packs-list').innerHTML; assert.match(rendered, /Apply/); assert.equal(/>Preview<|setACMMLevel|Preview this level/.test(rendered), false); })().catch(err => { console.error(err && err.stack || err); process.exit(1); });\n"

	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node ACMM dialog behaviour check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
