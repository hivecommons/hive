package hub

import (
	"os"
	"testing"
)

func TestLandingHiveSearchSurvivesSortAndRefresh(t *testing.T) {
	html, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	runNodeAssertions(t, `
const assert = require('node:assert/strict');
const search = {value: 'project'};
const table = {innerHTML: ''};
const elements = {'hive-search': search, 'hive-table-container': table};
const document = {getElementById(id) { return elements[id]; }};
const esc = String;
const badge = () => '';
const acmmBadge = badge, modeBadge = badge, sparkline = badge, contributeButton = badge;
const renderStats = () => {}, renderTicker = () => {};
var _mainSortKey = '', _mainSortAsc = true;
var _allMainHives = [
  {name: 'Project A', agentCount: 10},
  {name: 'Other', agentCount: 5},
  {name: 'Project B', agentCount: 2}
];
const fetch = async () => ({json: async () => ({hives: _allMainHives.concat(
  {name: 'Project C', agentCount: 3}
)})});
const names = () => Array.from(table.innerHTML.matchAll(/<span class="hive-name">([^<]+)/g), match => match[1]);
`+extractHubJSFunction(t, string(html), "renderHives")+
		extractHubJSFunction(t, string(html), "sortMainHives")+
		"async "+extractHubJSFunction(t, string(html), "loadRegistry")+`
(async () => {
  sortMainHives('agentCount');
  assert.deepEqual(names(), ['Project B', 'Project A']);
  sortMainHives('agentCount');
  assert.deepEqual(names(), ['Project A', 'Project B']);
  await loadRegistry();
  assert.deepEqual(names(), ['Project A', 'Project C', 'Project B']);
  search.value = '';
  renderHives();
  assert.deepEqual(names(), ['Project A', 'Other', 'Project C', 'Project B']);
  search.value = 'missing';
  renderHives();
  assert.match(table.innerHTML, /No public hives match your search/);
})();
`)
}
