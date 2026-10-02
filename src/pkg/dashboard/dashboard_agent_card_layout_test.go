package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestAgentCardGridStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`.agents { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr));`,
		`@media (max-width: 1399px) { .agents { grid-template-columns: repeat(2, minmax(0, 1fr)); } }`,
		`@media (max-width: 899px) { .agents { grid-template-columns: 1fr; } }`,
		`grid-template-columns: minmax(5.5rem, max-content) minmax(0, 1fr)`,
		`id="agents-reset-layout-btn" data-action="resetAgentCardLayout"`,
		`data-agent-order-grip`,
		`data-agent-resize-col`,
		`data-agent-resize-y`,
		`resize: vertical`,
		`hive-agent-card-layout:`,
		`function agentLayoutApplyOrder(agents)`,
		`function agentCardLayoutSetSize(name, patch)`,
		`function resetAgentCardLayout()`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing agent card layout wiring %q", want)
		}
	}
	if strings.Contains(html, `body.light-mode .agents { grid-template-columns: 1fr; }`) {
		t.Fatal("light mode still forces every agent card into one full-width column")
	}
}

func TestAgentCardLayoutOrderAndFocusedAgentNoPersist(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — agent card layout helpers were NOT executed by this run")
	}
	html := indexHTML(t)
	funcs := []string{
		"function agentsFocusedFirst(agents)",
		"function agentLayoutHiveKey()",
		"function agentLayoutName(name)",
		"function agentLayoutDefaultOrder(agents)",
		"function clampAgentCardSpan(span)",
		"function clampAgentCardHeight(h)",
		"function agentCardLayoutRead()",
		"function agentCardLayoutWrite(layout, defaultOrder)",
		"function agentCardLayoutNormalize(agents)",
		"function agentLayoutApplyOrder(agents)",
	}
	var b strings.Builder
	b.WriteString(`const AGENT_CARD_LAYOUT_KEY_PREFIX = 'hive-agent-card-layout:';
const AGENT_CARD_LAYOUT_VERSION = 1;
const AGENT_CARD_MIN_H = 192;
const AGENT_CARD_MAX_H = 900;
let _ocSelectedAgent = '';
const window = {_lastStatus:{hiveId:'hive-a'}};
function safeJsonParse(s, fallback){ try { return JSON.parse(s); } catch { return fallback; } }
const localStorage = {data:{}, writes:0, setItem(k,v){this.writes++; this.data[k]=String(v)}, getItem(k){return this.data[k] ?? null}, removeItem(k){this.writes++; delete this.data[k]}};
`)
	for _, fn := range funcs {
		b.WriteString(jsFunctionBody(t, html, fn))
		b.WriteByte('\n')
	}
	b.WriteString(`
const agents = [{name:'scanner'}, {name:'builder'}, {name:'docs'}];
localStorage.data['hive-agent-card-layout:hive-a'] = JSON.stringify({v:1, order:['docs','scanner','builder'], sizes:{scanner:{span:3,height:260}}});
let ordered = agentLayoutApplyOrder(agents).map(a => a.name).join(',');
if (ordered !== 'docs,scanner,builder') throw new Error('persisted order not applied: '+ordered);
let before = localStorage.data['hive-agent-card-layout:hive-a'];
let writes = localStorage.writes;
_ocSelectedAgent = 'builder';
let focused = agentsFocusedFirst(agentLayoutApplyOrder(agents)).map(a => a.name).join(',');
if (focused !== 'builder,docs,scanner') throw new Error('focused agent not rendered first: '+focused);
if (localStorage.data['hive-agent-card-layout:hive-a'] !== before) throw new Error('focused render mutated stored order');
if (localStorage.writes !== writes) throw new Error('focused render persisted unexpectedly');
`)
	cmd := exec.Command(node, "-e", b.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node agent card layout helpers failed: %v\n%s", err, out)
	}
}
