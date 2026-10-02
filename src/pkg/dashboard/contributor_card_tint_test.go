package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestContributorCardTintStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`.contrib-card {`,
		`--contrib-accent`,
		`--contrib-accent-2`,
		`data-contrib-login`,
		`data-contrib-avatar`,
		`class="contributors-grid"`,
		`function contribTintFromPixels(pixels)`,
		`function contribTintFallback(login)`,
		`img.crossOrigin = 'anonymous'`,
		`hive-contrib-tint:`,
		`CONTRIB_TINT_CACHE_TTL_MS = 7 * 24 * 60 * 60 * 1000`,
		`@media (prefers-reduced-motion: reduce)`,
		`tintContributorCards();`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing contributor tint wiring %q", want)
		}
	}
}

func TestContributorTintHelpers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — contributor tint helpers were NOT executed by this run")
	}
	html := indexHTML(t)
	funcs := []string{
		"function hslToHex(h, s, l)",
		"function rgbToHex(r, g, b)",
		"function rgbSaturation(r, g, b)",
		"function contribTintFallback(login)",
		"function contribTintFromPixels(pixels)",
	}
	var b strings.Builder
	for _, fn := range funcs {
		b.WriteString(jsFunctionBody(t, html, fn))
		b.WriteByte('\n')
	}
	b.WriteString(`
function assertHex(v, name) { if (!/^#[0-9a-f]{6}$/.test(v)) throw new Error(name+' not hex: '+v); }
const fallbackA = contribTintFallback('Danathar');
const fallbackB = contribTintFallback('Karibhou');
assertHex(fallbackA.accent, 'fallbackA accent');
assertHex(fallbackA.accent2, 'fallbackA accent2');
if (fallbackA.accent === fallbackB.accent && fallbackA.accent2 === fallbackB.accent2) throw new Error('fallback tints should vary by login');
const px = [];
for (let i = 0; i < 20; i++) px.push(210, 64, 86, 255);
for (let i = 0; i < 8; i++) px.push(40, 148, 220, 255);
for (let i = 0; i < 10; i++) px.push(2, 2, 2, 255);
for (let i = 0; i < 10; i++) px.push(250, 250, 250, 255);
const tint = contribTintFromPixels(px);
if (!tint) throw new Error('expected tint from saturated pixels');
assertHex(tint.accent, 'pixel accent');
assertHex(tint.accent2, 'pixel accent2');
if (tint.accent === '#020202' || tint.accent === '#fafafa') throw new Error('ignored colours selected: '+JSON.stringify(tint));
if (contribTintFromPixels([1,1,1,255,250,250,250,255]) !== null) throw new Error('low-information pixels should not produce a tint');
`)
	cmd := exec.Command(node, "-e", b.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node contributor tint helpers failed: %v\n%s", err, out)
	}
}
