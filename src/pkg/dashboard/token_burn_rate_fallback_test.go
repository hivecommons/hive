package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// A quiet hive leaves too few token-total changes in the sparkline history for
// computeHourlyBurnRates() to bucket, so it returns null. The Tokens card's
// headline tok/hr must fall back to the window average in that state: when it
// dereferenced the null instead, render() threw before renderRepos() and every
// later section stopped repainting until a full page reload.
func TestTokenBurnHeadlineFallsBackWhenHistoryIsFlat(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: token burn-rate fallback was not executed")
	}
	html := indexHTML(t)
	script := `const assert = require('node:assert/strict');
let historyData = [];
` + jsFunc(t, html, "computeHourlyBurnRates") + `
` + jsFunc(t, html, "currentTokenBurnPerHour") + `
// Too little history to bucket.
assert.equal(computeHourlyBurnRates(), null);
assert.equal(currentTokenBurnPerHour(computeHourlyBurnRates(), 48000, 24), 2000);
// Plenty of samples, but tokens never moved (idle hive).
const t0 = Date.UTC(2026, 9, 10, 8, 0, 0);
historyData = Array.from({ length: 60 }, (_, i) => ({ t: t0 + i * 60000, tokenTotal: 500000 }));
assert.equal(computeHourlyBurnRates(), null);
assert.equal(currentTokenBurnPerHour(computeHourlyBurnRates(), 500000, 0), Math.round(500000 / 24));
// Live burn: the newest bucket wins over the window average.
historyData = Array.from({ length: 60 }, (_, i) => ({ t: t0 + i * 60000, tokenTotal: 1000 * (i + 1) }));
const rates = computeHourlyBurnRates();
assert.ok(rates && rates.length >= 3);
assert.equal(currentTokenBurnPerHour(rates, 1, 24), rates[rates.length - 1].rate);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("token burn-rate fallback failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
