package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestCustomersAffiliationSourceChip runs the page's own render helpers for a
// suggestion that came from enrichment: a chip names the public source and
// the dates, Accept and Dismiss are unchanged, an offline guess keeps its
// evidence text and no chip, and every value is escaped.
func TestCustomersAffiliationSourceChip(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH: the affiliation chip helpers were NOT executed by this run")
	}
	html := indexHTML(t)
	script := jsConstBlock(t, html, "CUSTOMERS_AFFILIATION_SOURCES") + "\n" +
		`function escapeHtml(s) { return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); }` + "\n" +
		jsFunc(t, html, "customersAffiliationSourceChip") + "\n" +
		jsFunc(t, html, "customersAffiliationHtml") + `
function must(cond, msg) { if (!cond) throw new Error(msg); }
const gitdm = customersAffiliationHtml({ affiliation_suggestion: { value: 'Globex', rule: 'gitdm', evidence: 'from cncf/gitdm, since 2022-01-01', source: 'gitdm', from: '2022-01-01' } });
must(gitdm.includes('Likely: <strong>Globex</strong>'), 'value missing: ' + gitdm);
must(gitdm.includes('class="kc-pill customers-pill customers-affiliation-source" data-source="gitdm"') && gitdm.includes('>gitdm</span>'), 'no gitdm chip: ' + gitdm);
must(gitdm.includes('since 2022-01-01') && gitdm.includes('From cncf/gitdm, since 2022-01-01'), 'dates missing: ' + gitdm);
must(gitdm.includes('data-action="customersAffiliationAccept" data-arg0="Globex"') && gitdm.includes('data-action="customersAffiliationDismiss" data-arg0="Globex"'), 'Accept or Dismiss missing');
const span = customersAffiliationHtml({ affiliation_suggestion: { value: 'Acme', source: 'gitdm', from: '2019-01-01', until: '2021-12-31' } });
must(span.includes('2019-01-01 to 2021-12-31'), 'a range is not shown: ' + span);
const guess = customersAffiliationHtml({ affiliation_suggestion: { value: 'Acme', rule: 'github_company', evidence: 'from GitHub company field "acme"' } });
must(!guess.includes('customers-affiliation-source') && guess.includes('from GitHub company field &quot;acme&quot;'), 'an offline guess changed: ' + guess);
const evil = customersAffiliationHtml({ affiliation_suggestion: { value: '<b>x</b>', source: '<i>s</i>', from: '<u>' } });
must(!evil.includes('<b>x</b>') && !evil.includes('<i>s</i>') && !evil.includes('<u>'), 'unescaped enrichment value: ' + evil);
must(customersAffiliationSourceChip({ value: 'x' }) === '', 'no source, no chip');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

// TestCustomersPublicContributionsBlock runs the read-only "Public
// contributions" helper: one row per project with an external link that
// opens in a new tab with no referrer, only https links, nothing editable,
// and nothing at all without activity.
func TestCustomersPublicContributionsBlock(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH: the public contributions helper was NOT executed by this run")
	}
	html := indexHTML(t)
	script := `function escapeHtml(s) { return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); }` + "\n" +
		jsFunc(t, html, "customersPublicContributionsHtml") + `
function must(cond, msg) { if (!cond) throw new Error(msg); }
must(customersPublicContributionsHtml({}) === '' && customersPublicContributionsHtml({ public_contributions: [] }) === '', 'no activity must render nothing');
const out = customersPublicContributionsHtml({ public_contributions: [
  { project: 'All CNCF', contributions: 42, period: 'all time', url: 'https://all.devstats.cncf.io/d/9?x=1&y=2', source: 'devstats' },
  { project: '<b>evil</b>', contributions: 1, url: 'javascript:alert(1)', source: 'devstats' },
] });
must(out.includes('Public contributions') && out.includes('All CNCF: 42 contributions'), 'block or row missing: ' + out);
must(out.includes('href="https://all.devstats.cncf.io/d/9?x=1&amp;y=2" target="_blank" rel="noopener noreferrer"') && out.includes('&#8599;'), 'external link missing: ' + out);
must(!out.includes('javascript:') && !out.includes('<b>evil</b>') && out.includes('&lt;b&gt;evil&lt;/b&gt;'), 'unsafe link or unescaped project: ' + out);
must(!out.includes('<input') && !out.includes('data-action='), 'the block must be read only');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
