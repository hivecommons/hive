package outcomebench

import (
	"encoding/json"
	"fmt"
	"html"
	"math"
	"os"
	"regexp"
	"strings"
	"time"
)

// The README cards are the plain-language face of the benchmark: one idea
// per card, one number in big type, one flat sentence, and a footer that
// says in words what was counted. Intervals, n and cohort names stay in the
// generated report; the cards never print them, and a card whose base is
// below MinBase is not written at all.

// Story is one real pull request where the review swarm caught something
// before merge and the author fixed it. Stories are curated input (a saved
// JSON list), not computed, because choosing a readable, non-sensitive
// example is a human call; every field is checkable against the PR.
type Story struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	// ReviewURL is the posted review that carried the changes_requested
	// verdict.
	ReviewURL string `json:"review_url"`
	// Caught is the plain-English description of what the review agent found.
	Caught string `json:"caught"`
	// OpenedAt, CaughtAt, FixApprovedAt and MergedAt come from the evidence
	// bundles: PR opened, first changes_requested verdict, first approve on
	// the following head, merge.
	OpenedAt      time.Time `json:"opened_at"`
	CaughtAt      time.Time `json:"caught_at"`
	FixApprovedAt time.Time `json:"fix_approved_at"`
	MergedAt      time.Time `json:"merged_at"`
}

// URL is the pull request page.
func (s Story) URL() string { return fmt.Sprintf("https://github.com/%s/pull/%d", s.Repo, s.Number) }

// FixMinutes is how long after the catch the fixed head was approved.
// CaughtMinutes is how long after opening the review agent sent it back.
func (s Story) CaughtMinutes() int { return int(math.Round(s.CaughtAt.Sub(s.OpenedAt).Minutes())) }

func (s Story) FixMinutes() int { return int(math.Round(s.FixApprovedAt.Sub(s.CaughtAt).Minutes())) }

// MergeMinutes is open-to-merge for the whole PR.
func (s Story) MergeMinutes() int { return int(math.Round(s.MergedAt.Sub(s.OpenedAt).Minutes())) }

// LoadStories reads a saved JSON list of stories and rejects any whose
// timeline is not in order, so a typo cannot produce a negative minute.
func LoadStories(path string) ([]Story, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Story
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	for i, s := range out {
		if s.Repo == "" || s.Number <= 0 || strings.TrimSpace(s.Caught) == "" {
			return nil, fmt.Errorf("%s: story %d needs repo, number and caught", path, i)
		}
		if !(s.OpenedAt.Before(s.CaughtAt) && s.CaughtAt.Before(s.FixApprovedAt) && s.FixApprovedAt.Before(s.MergedAt)) {
			return nil, fmt.Errorf("%s: story %s#%d timeline is out of order", path, s.Repo, s.Number)
		}
	}
	return out, nil
}

// RedTeam is the result of Hive's own prompt-injection evaluation
// (docs/ioscan-red-team.md): how many attack payloads the scanner caught
// and how many harmless requests it wrongly flagged. It is a lab set, not
// production traffic, and the card says so; MinBase does not apply because
// the base is the size of the eval set, which the footer states.
type RedTeam struct {
	Attacks     int    `json:"attacks"`
	Caught      int    `json:"caught"`
	Benign      int    `json:"benign"`
	FalseAlarms int    `json:"false_alarms"`
	Source      string `json:"source"`
	// RunAt is when the eval set was last run, for the card footer.
	RunAt time.Time `json:"run_at"`
}

// LoadRedTeam reads a saved red-team summary.
func LoadRedTeam(path string) (*RedTeam, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rt RedTeam
	if err := json.Unmarshal(data, &rt); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if rt.Attacks <= 0 || rt.Caught < 0 || rt.Caught > rt.Attacks || rt.Benign < 0 || rt.FalseAlarms > rt.Benign {
		return nil, fmt.Errorf("%s: counts are inconsistent", path)
	}
	return &rt, nil
}

// DefaultReadmeMinBase is the smallest production count a README card may
// be built on. It is deliberately far above MinN: the report may discuss a
// 43-merge bucket with its interval, but a card with one big number and no
// interval must rest on a base a reader would trust without one.
const DefaultReadmeMinBase = 200

// MinBase is the README card threshold in force for this report.
func (r Report) MinBase() int {
	if r.ReadmeMinBase > 0 {
		return r.ReadmeMinBase
	}
	return DefaultReadmeMinBase
}

// minCICardCoverage is the share of reviewed merges that must carry a
// recorded check result before the CI card is drawn; below it the figure
// describes a sample the footer cannot explain in one line.
const minCICardCoverage = 0.8

// cardTheme is the card palette: a warm near-black surface with a hairline
// border, warm off-white text, a flat amber for the one figure on each card,
// a flat warm gray for "without", and green only for pass/merged dots. One
// SVG and one 2× PNG are written per theme and the README picks the PNG
// with <picture>. There is no gradient, glow or filter: the PNG is the
// rendering of record and must look the same everywhere.
type cardTheme struct {
	Name, BG, Border, Rule, Head, Text, Muted, Brand, Neutral, OK string
}

var cardThemes = []cardTheme{
	{Name: "dark", BG: "#0f0d0a", Border: "#28231c", Rule: "#211c16", Head: "#fffaf0", Text: "#d9d1c4", Muted: "#8a8174", Brand: "#f0b429", Neutral: "#3a342c", OK: "#3fb950"},
	{Name: "light", BG: "#fbf8f2", Border: "#e2dbcd", Rule: "#ebe5d9", Head: "#1a1612", Text: "#3b342c", Muted: "#7a7165", Brand: "#a3640c", Neutral: "#d6cfc2", OK: "#1a7f37"},
}

// Layout constants. Every card spans the README content column and is
// rendered at twice its display size so GitHub downsamples it crisp; the
// coordinates below are display pixels (the SVG viewBox). The face is IBM
// Plex Mono, whose advance is exactly 0.6 em, so character budgets are
// exact: chars = width / (0.6 × px).
const (
	cardFont   = "'IBM Plex Mono', 'JetBrains Mono', ui-monospace, SFMono-Regular, Menlo, Consolas, monospace"
	cardW      = 880
	cardScale  = 2
	cardPad    = 48
	cardRadius = 20
	colX       = cardW - 2*cardPad // usable width: 784
	eyebrowPx  = 12
	eyebrowTrk = 2.4
	eyebrowGap = 48 // eyebrow baseline to first headline baseline
	headPx     = 28
	headLineH  = 38
	headLines  = 3 // headline block is always this tall, so figures line up across cards
	bigPx      = 112
	bigColW    = 400 // the figure and its label
	bigLabelPx = 16
	sideX      = cardPad + bigColW + 24 // second column of the figure row
	bodyPx     = 16
	bodyLineH  = 24
	footPx     = 13
	footLineH  = 20
	halfGap    = 48
	statCardH  = 500
)

// chars is how many Plex Mono glyphs of size px fit in width display px.
func chars(width, px int) int { return int(float64(width) / (0.6 * float64(px))) }

// fitPx shrinks a figure so it stays inside width.
func fitPx(s string, px, width int) int {
	n := len([]rune(s))
	if w := int(0.6 * float64(px) * float64(n)); w > width {
		return width * 10 / (6 * n)
	}
	return px
}

var (
	headChars = chars(colX, headPx)                // 46
	sideChars = chars(cardW-cardPad-sideX, bodyPx) // 37
	wideChars = chars(colX, footPx)                // 100
	halfChars = chars((colX-halfGap)/2-20, bodyPx) // 36
	tileChars = chars(colX/4-24, bodyPx-1)         // 19
)

// Card names; the asset file is <AssetBase>-card-<name>-<theme>.{svg,png}.
const (
	CardAccuracy        = "accuracy"
	CardChecksGreen     = "checks-green"
	CardStories         = "stories"
	CardPromptInjection = "prompt-injection"
	CardCost            = "cost"
	CardAlso            = "also"
	CardHowItWorks      = "how-it-works"
)

// CardNames lists every card the generator knows, in README order.
func CardNames() []string {
	return []string{CardAccuracy, CardChecksGreen, CardStories, CardPromptInjection, CardCost, CardAlso, CardHowItWorks}
}

// CardFile is the SVG asset filename for a card in a theme.
func (r Report) CardFile(name, theme string) string {
	return fmt.Sprintf("%s-card-%s-%s.svg", r.AssetBase(), name, theme)
}

// CardPNGFile is the rendered PNG the README embeds.
func (r Report) CardPNGFile(name, theme string) string {
	return fmt.Sprintf("%s-card-%s-%s.png", r.AssetBase(), name, theme)
}

// Cards renders every card in every theme as SVG, keyed by filename. Cards
// whose data is missing or below MinBase are omitted, never drawn as
// placeholders.
func (r Report) Cards() map[string]string {
	out := map[string]string{}
	for _, th := range cardThemes {
		for _, name := range CardNames() {
			if c, ok := r.card(name, th); ok {
				out[r.CardFile(name, th.Name)] = c.SVG()
			}
		}
	}
	return out
}

// CardPNGs rasterises every card in every theme at cardScale, keyed by
// filename. Each PNG carries the SHA-256 of its SVG twin in a tEXt chunk so
// the drift test can tell a stale PNG from a fresh one without comparing
// pixels, which differ by platform.
func (r Report) CardPNGs() (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, th := range cardThemes {
		for _, name := range CardNames() {
			c, ok := r.card(name, th)
			if !ok {
				continue
			}
			png, err := c.PNG()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", r.CardPNGFile(name, th.Name), err)
			}
			out[r.CardPNGFile(name, th.Name)] = png
		}
	}
	return out, nil
}

// Card renders one named card as SVG; ok is false when its data is not shown.
func (r Report) Card(name string, th cardTheme) (string, bool) {
	c, ok := r.card(name, th)
	if !ok {
		return "", false
	}
	return c.SVG(), true
}

func (r Report) card(name string, th cardTheme) (*card, bool) {
	switch name {
	case CardAccuracy:
		return r.accuracyCard(th)
	case CardChecksGreen:
		return r.checksGreenCard(th)
	case CardStories:
		return r.storiesCard(th)
	case CardPromptInjection:
		return r.promptInjectionCard(th)
	case CardCost:
		return r.costCard(th)
	case CardAlso:
		return r.alsoCard(th)
	case CardHowItWorks:
		return howItWorksCard(th), true
	}
	return nil, false
}

// HasCard reports whether a card has data to show.
func (r Report) HasCard(name string) bool {
	_, ok := r.card(name, cardThemes[0])
	return ok
}

// windowLabel is "Oct 2026" for a monthly report, or the generation month.
func (r Report) windowLabel() string {
	t := r.Since
	if t.IsZero() {
		t = r.GeneratedAt
	}
	return monthLabel(t)
}

// monthLabel is "(oct '26)": lowercase month, apostrophe year, in
// parentheses. No ISO dates appear in the README or its cards.
func monthLabel(t time.Time) string { return "(" + shortMonth(t) + " '" + t.UTC().Format("06") + ")" }

// rangeLabel is "(aug–oct '26)", or monthLabel when both fall in one month.
func rangeLabel(a, b time.Time) string {
	if a.UTC().Year() == b.UTC().Year() && a.UTC().Month() == b.UTC().Month() {
		return monthLabel(b)
	}
	if a.UTC().Year() == b.UTC().Year() {
		return "(" + shortMonth(a) + "–" + shortMonth(b) + " '" + b.UTC().Format("06") + ")"
	}
	return "(" + shortMonth(a) + " '" + a.UTC().Format("06") + "–" + shortMonth(b) + " '" + b.UTC().Format("06") + ")"
}

func shortMonth(t time.Time) string { return strings.ToLower(t.UTC().Format("Jan")) }

// commas formats 11571 as 11,571.
func commas(n int) string {
	if n < 0 {
		return "-" + commas(-n)
	}
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// oneIn turns a rate into "1 in N" (N rounded), the way people say it.
func oneIn(rate float64) string {
	if rate <= 0 {
		return "0"
	}
	return fmt.Sprintf("1 in %d", int(math.Round(1/rate)))
}

// share renders a rate above one half as "k in d" with the smallest
// denominator within a few points of the true value, and otherwise as
// oneIn.
func share(rate float64) string {
	if rate < 0.5 {
		return oneIn(rate)
	}
	for d := 2; d <= 5; d++ {
		k := int(math.Round(rate * float64(d)))
		if math.Abs(float64(k)/float64(d)-rate) < 0.04 && k < d {
			return fmt.Sprintf("%d in %d", k, d)
		}
	}
	return fmt.Sprintf("%d in 10", int(math.Round(rate*10)))
}

// shareWords is share() in words for a headline: "two out of three".
func shareWords(rate float64) string {
	words := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}
	var k, d int
	if _, err := fmt.Sscanf(share(rate), "%d in %d", &k, &d); err != nil || k > 10 || d > 10 {
		return share(rate)
	}
	return words[k] + " out of " + words[d]
}

// minutes rounds hours to whole minutes.
func minutes(hours float64) int { return int(math.Round(60 * hours)) }

// wrap splits s into lines of at most width characters on word boundaries.
func wrap(s string, width int) []string {
	var lines []string
	line := ""
	for _, w := range strings.Fields(s) {
		switch {
		case line == "":
			line = w
		case len(line)+1+len(w) > width:
			lines = append(lines, line)
			line = w
		default:
			line += " " + w
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

func esc(s string) string { return html.EscapeString(s) }

// textStyle is what varies between runs of text besides size and colour.
type textStyle struct {
	Weight int     // 400, 500 or 700
	Track  float64 // letter-spacing in display px
	End    bool    // right-aligned at x
}

var (
	plain = textStyle{Weight: 400}
	bold  = textStyle{Weight: 700}
	label = textStyle{Weight: 500, Track: eyebrowTrk}
)

// op is one drawing instruction. The card records ops once and renders
// them twice: as SVG elements and as pixels (raster.go), so the two assets
// cannot disagree.
type op struct {
	kind         byte // 't' text, 'r' rect, 'l' line, 'c' circle, 'a' link rect
	x, y, w, h   int
	rx           int
	px           int
	fill, stroke string
	style        textStyle
	s            string // text, or link href
}

// card is the shared builder: it collects ops, then closes at a height.
type card struct {
	th    cardTheme
	title string
	ops   []op
	h     int
}

func (c *card) text(x, y, px int, fill string, st textStyle, s string) {
	c.ops = append(c.ops, op{kind: 't', x: x, y: y, px: px, fill: fill, style: st, s: s})
}

// lines writes wrapped lines and returns the baseline after the last one.
func (c *card) lines(x, y, px, lineH int, fill string, st textStyle, lines []string) int {
	for i, l := range lines {
		c.text(x, y+i*lineH, px, fill, st, l)
	}
	return y + len(lines)*lineH
}

// eyebrow is the tracked uppercase line at the top: section, base, window.
func (c *card) eyebrow(y int, s string) {
	c.text(cardPad, y, eyebrowPx, c.th.Muted, label, upperKeepDates(s))
}

var reDateLabel = regexp.MustCompile(`\([A-Z]{3}(?: '\d\d)?(?:–[A-Z]{3})? '\d\d\)`)

// upperKeepDates uppercases an eyebrow but leaves the "(oct '26)" date
// labels lowercase, as the README style requires.
func upperKeepDates(s string) string {
	return reDateLabel.ReplaceAllStringFunc(strings.ToUpper(s), strings.ToLower)
}

// headline is the finding in one plain sentence, bold, the biggest text on
// the card. It returns the baseline after the fixed headline block so the
// row below starts at the same height on every card.
func (c *card) headline(y int, s string) int {
	c.lines(cardPad, y, headPx, headLineH, c.th.Head, bold, wrap(s, headChars))
	return y + headLines*headLineH
}

// figure draws the one big flat-amber number of a card.
func (c *card) figure(x, y, px int, s string) {
	c.text(x, y, px, c.th.Brand, bold, s)
}

func (c *card) rule(x1, x2, y int) {
	c.ops = append(c.ops, op{kind: 'l', x: x1, y: y, w: x2 - x1, stroke: c.th.Rule})
}

func (c *card) dot(cx, cy, r int, fill string) {
	c.ops = append(c.ops, op{kind: 'c', x: cx, y: cy, rx: r, fill: fill})
}

func (c *card) link(href string, x, y, w, h int) {
	c.ops = append(c.ops, op{kind: 'a', x: x, y: y, w: w, h: h, s: href})
}

// foot writes the muted footnote under a hairline; y is the hairline.
// Returns the baseline after the last line.
func (c *card) foot(y int, s string) int {
	c.rule(cardPad, cardW-cardPad, y)
	return c.lines(cardPad, y+footLineH+8, footPx, footLineH, c.th.Muted, plain, wrap(s, wideChars))
}

// close fixes the card height; the bottom padding is cardPad.
func (c *card) close(lastBaseline int) *card {
	c.h = lastBaseline - footLineH + cardPad
	return c
}

// closeAt fixes the card at an explicit height.
func (c *card) closeAt(h int) *card {
	c.h = h
	return c
}

// SVG renders the ops. The SVG is the vector twin of the PNG: same ops,
// same coordinates, and a font stack that starts with the embedded face.
func (c *card) SVG() string {
	var out strings.Builder
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="%s" font-family="%s">
<title>%s</title>
<rect x="0.5" y="0.5" width="%d" height="%d" rx="%d" fill="%s" stroke="%s"/>
`, cardW*cardScale, c.h*cardScale, cardW, c.h, esc(c.title), cardFont, esc(c.title), cardW-1, c.h-1, cardRadius, c.th.BG, c.th.Border)
	for _, o := range c.ops {
		switch o.kind {
		case 't':
			attrs := ""
			if o.style.Weight != 400 {
				attrs += fmt.Sprintf(` font-weight="%d"`, o.style.Weight)
			}
			if o.style.Track != 0 {
				attrs += fmt.Sprintf(` letter-spacing="%.1f"`, o.style.Track)
			}
			if o.style.End {
				attrs += ` text-anchor="end"`
			}
			fmt.Fprintf(&out, `<text x="%d" y="%d" fill="%s" font-size="%d"%s>%s</text>`+"\n", o.x, o.y, o.fill, o.px, attrs, esc(o.s))
		case 'l':
			fmt.Fprintf(&out, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="%s"/>`+"\n", o.x, o.y, o.x+o.w, o.y, o.stroke)
		case 'c':
			fmt.Fprintf(&out, `<circle cx="%d" cy="%d" r="%d" fill="%s"/>`+"\n", o.x, o.y, o.rx, o.fill)
		case 'a':
			fmt.Fprintf(&out, `<a href="%s"><rect x="%d" y="%d" width="%d" height="%d" fill="transparent"/></a>`+"\n", esc(o.s), o.x, o.y, o.w, o.h)
		}
	}
	out.WriteString("</svg>\n")
	return out.String()
}

// hero is the one figure of a stat card: the number, the words under it,
// and the sentence beside it that gives the rest of the picture.
type hero struct{ Big, Under, Side string }

// statCard is the shared anatomy: eyebrow, headline, one figure with its
// base under it and context beside it, footnote. Every stat card is
// statCardH tall so the README reads as a set.
func statCard(th cardTheme, title, eye, head string, h hero, foot string) *card {
	c := &card{th: th, title: title}
	y := cardPad + eyebrowPx
	c.eyebrow(y, eye)
	y = c.headline(y+eyebrowGap, head)
	base := y + bigPx - 8
	c.figure(cardPad, base, fitPx(h.Big, bigPx, bigColW), h.Big)
	c.lines(cardPad, base+36, bigLabelPx, bodyLineH, th.Text, plain, wrap(h.Under, chars(bigColW, bigLabelPx)))
	c.lines(sideX, base-bigPx/2+8, bodyPx, bodyLineH, th.Muted, plain, wrap(h.Side, sideChars))
	c.foot(statCardH-cardPad-2*footLineH-8, foot)
	return c.closeAt(statCardH)
}

func (r Report) accuracyCard(th cardTheme) (*card, bool) {
	f := r.Followup
	fa, ta := f.FixedAndApproved, f.TimeToApproval
	if !fa.Reported || fa.K <= 0 || fa.N < r.MinBase() {
		return nil, false
	}
	words := strings.ToUpper(shareWords(fa.Rate)[:1]) + shareWords(fa.Rate)[1:]
	usually := ""
	if ta.Reported && ta.N >= r.MinBase() {
		usually = fmt.Sprintf(", usually within %d minutes", minutes(ta.Value))
	}
	title := fmt.Sprintf("%s pull requests the review agent sent back came back fixed and approved%s.", words, usually)
	head := fmt.Sprintf("The review agent sent %s pull requests back. %s came back fixed and approved%s.", commas(fa.N), words, usually)
	side := fmt.Sprintf("%s of %s came back fixed and approved%s.", commas(fa.K), commas(fa.N), usually)
	if f.MergedSameHead > 0 {
		side += fmt.Sprintf(" Another %s were fine on a second look and merged as they were.", commas(f.MergedSameHead))
	}
	return statCard(th, title,
		fmt.Sprintf("Accuracy · %s pull requests sent back · %s", commas(fa.N), r.windowLabel()),
		head,
		hero{share(fa.Rate), "came back fixed and approved", side},
		fmt.Sprintf("%s pull requests sent back %s, counted from Hive's own review records. Live repos, not a lab.", commas(fa.N), r.windowLabel()),
	), true
}

func (r Report) checksGreenCard(th cardTheme) (*card, bool) {
	c := r.Mediated
	if !r.SplitSupported() || !c.CIFailureAtHead.Reported || c.CIFailureAtHead.N < r.MinBase() || !c.CICoverage.Reported || c.CICoverage.Rate < minCICardCoverage {
		return nil, false
	}
	pass := int(math.Round(100 * (1 - c.CIFailureAtHead.Rate)))
	return statCard(th,
		fmt.Sprintf("%d in 100 reviewed pull requests merged with every check green.", pass),
		fmt.Sprintf("Accuracy · %s reviewed merges · %s", commas(c.CIFailureAtHead.N), r.windowLabel()),
		fmt.Sprintf("%d in 100 reviewed pull requests merged with every check green.", pass),
		hero{fmt.Sprintf("%d in 100", pass), "merged with every check green", fmt.Sprintf("%s reviewed pull requests merged with a recorded check result.", commas(c.CIFailureAtHead.N))},
		fmt.Sprintf("%s reviewed merges with a recorded check result %s.", commas(c.CIFailureAtHead.N), r.windowLabel()),
	), true
}

func (r Report) promptInjectionCard(th cardTheme) (*card, bool) {
	rt := r.RedTeam
	if rt == nil || rt.Attacks <= 0 {
		return nil, false
	}
	pct := int(math.Round(100 * float64(rt.Caught) / float64(rt.Attacks)))
	fa := fmt.Sprintf("One harmless text in %d got flagged by mistake.", rt.Benign)
	if rt.FalseAlarms != 1 {
		fa = fmt.Sprintf("%d harmless texts in %d got flagged by mistake.", rt.FalseAlarms, rt.Benign)
	}
	return statCard(th,
		fmt.Sprintf("Hive caught %d of %d planted texts that try to redirect an agent before an agent read a word. %s", rt.Caught, rt.Attacks, fa),
		fmt.Sprintf("Security · Hive's own test set · %s", monthLabel(rt.RunAt)),
		fmt.Sprintf("We planted %d texts that try to redirect an agent in issues and files. Hive caught %d before an agent read a word.", rt.Attacks, rt.Caught),
		hero{fmt.Sprintf("%d%%", pct), fmt.Sprintf("of %d planted texts caught", rt.Attacks), fmt.Sprintf("%d of %d were caught before an agent read a word. %s", rt.Caught, rt.Attacks, fa)},
		fmt.Sprintf("Hive's own test set of %d texts %s, not production traffic. Agents run as a GitHub App, not on your token.", rt.Attacks+rt.Benign, monthLabel(rt.RunAt)),
	), true
}

func (r Report) costCard(th cardTheme) (*card, bool) {
	per, ok := r.Spend.PerMergedPR()
	if !ok || r.Spend.MergedPRs < r.MinBase() {
		return nil, false
	}
	fig := fmt.Sprintf("$%.0f", per)
	if per < 1 {
		fig = fmt.Sprintf("%d¢", int(math.Round(per*100)))
	}
	window := rangeLabel(r.Spend.WindowStart, r.Spend.WindowEnd)
	total := "$" + commas(int(math.Round(r.Spend.USD)))
	return statCard(th,
		fmt.Sprintf("Agents spent about %s to merge %s pull requests, call it %s a merge.", total, commas(r.Spend.MergedPRs), fig),
		fmt.Sprintf("Cost · %s merged pull requests · %s", commas(r.Spend.MergedPRs), window),
		fmt.Sprintf("Agents spent about %s to merge %s pull requests. Call it %s a merge.", total, commas(r.Spend.MergedPRs), fig),
		hero{fig, "a merge, our estimate", fmt.Sprintf("%s of agent spend across %s merged pull requests %s. Each job's cost sits on the dashboard, like CI minutes.", total, commas(r.Spend.MergedPRs), window)},
		fmt.Sprintf("Our estimate from provider list prices %s. Some models are unpriced and count as zero.", window),
	), true
}

// storiesCard is the before/after scenario: the left column is the familiar
// path without Hive, in plain words and without invented numbers; the right
// column is one real pull request with its recorded timings. A second real
// pull request, if present, is summarised in the footnote.
func (r Report) storiesCard(th cardTheme) (*card, bool) {
	if len(r.Stories) == 0 {
		return nil, false
	}
	s := r.Stories[0]
	c := &card{th: th, title: fmt.Sprintf("%s #%d was sent back once and merged %d minutes after opening.", s.Repo, s.Number, s.MergeMinutes())}
	y := cardPad + eyebrowPx
	c.eyebrow(y, fmt.Sprintf("Before / after · one real pull request · %s", r.windowLabel()))
	y = c.headline(y+eyebrowGap, fmt.Sprintf("A coding agent opened a pull request. The review agent sent it back once. It merged, green, %d minutes after opening.", s.MergeMinutes()))
	colW := (colX - halfGap) / 2
	top := y + 20
	col := func(i int, title, titleFill string, steps [][2]string, dots []string) int {
		x := cardPad + i*(colW+halfGap)
		c.text(x, top, bodyPx, titleFill, bold, title)
		yy := top + 44
		for j, st := range steps {
			c.dot(x+4, yy-4, 4, dots[j])
			c.text(x+20, yy, eyebrowPx, th.Muted, label, strings.ToUpper(st[0]))
			yy = c.lines(x+20, yy+bodyLineH, bodyPx, bodyLineH, th.Text, plain, wrap(st[1], halfChars)) + 16
		}
		return yy
	}
	left := col(0, "Without Hive", th.Muted, [][2]string{
		{"Issue", "A failing check or an open issue waits for someone to pick it up."},
		{"Pull request", "Someone writes the fix when they have time."},
		{"Review", "Someone else reads it when they have time."},
		{"Merge", "Thursday, maybe."},
	}, []string{th.Neutral, th.Neutral, th.Neutral, th.Neutral})
	right := col(1, fmt.Sprintf("With Hive · %s #%d", s.Repo, s.Number), th.Brand, [][2]string{
		{"Pull request", "A coding agent opened it."},
		{fmt.Sprintf("Sent back at %d min", s.CaughtMinutes()), s.Caught},
		{fmt.Sprintf("Fixed at %d min", s.CaughtMinutes()+s.FixMinutes()), "The coding agent pushed a fix and the review agent approved it."},
		{fmt.Sprintf("Merged at %d min", s.MergeMinutes()), "CI was green and a human merged it."},
	}, []string{th.Brand, th.Brand, th.OK, th.OK})
	bottom := left
	if right > bottom {
		bottom = right
	}
	c.link(s.URL(), cardPad+colW+halfGap, top-20, colW, right-top+20)
	foot := "Times from Hive's review records. The right column links to the pull request."
	if len(r.Stories) > 1 {
		t := r.Stories[1]
		foot = fmt.Sprintf("%s #%d went the same way, sent back at %d min, fixed at %d, merged at %d. Times from Hive's review records.", t.Repo, t.Number, t.CaughtMinutes(), t.CaughtMinutes()+t.FixMinutes(), t.MergeMinutes())
	}
	return c.close(c.foot(bottom+8, foot)), true
}

// tile is one column of the "also" strip.
type tile struct{ Head, Big, Text string }

// alsoTiles are the four secondary properties, in the order people ask for
// them after accuracy, security and cost. Tiles with a number take it from
// the report; the others are plain statements of how Hive works.
func (r Report) alsoTiles() []tile {
	tiles := []tile{
		{"Consistent", commas(r.Verdicts), "reviews, all against the same checklist"},
		{"Readable", "1 line", "per finding, in your style guide's voice"},
	}
	if per, ok := r.Spend.PerMergedPR(); ok && r.Spend.MergedPRs >= r.MinBase() {
		fig := fmt.Sprintf("$%.0f", per)
		if per < 1 {
			fig = fmt.Sprintf("%d¢", int(math.Round(per*100)))
		}
		tiles = append(tiles, tile{"Efficient", fig, "a merge, our estimate. Jobs stop when done"})
	} else {
		tiles = append(tiles, tile{"Efficient", "On demand", "jobs start when there is work and stop when done"})
	}
	if with, without, ok := r.speedBars(); ok {
		d := with - without
		if d > 0 {
			tiles = append(tiles, tile{"Fast", fmt.Sprintf("+%d min", d), fmt.Sprintf("a typical merge takes %d minutes, %d without review", with, without)})
		} else {
			tiles = append(tiles, tile{"Fast", fmt.Sprintf("%d min", with), "typical open to merge with review"})
		}
	} else {
		tiles = append(tiles, tile{"Fast", "—", "time to merge shows once enough pull requests are on record"})
	}
	return tiles
}

// speedBars is the one two-sided comparison with a base above MinBase on
// both sides: typical minutes from open to merge, with and without review.
// ok is false when either side is missing, and the card says so instead.
func (r Report) speedBars() (with, without int, ok bool) {
	m, u := r.Mediated.TimeToMerge, r.Unmediated.TimeToMerge
	if !r.SplitSupported() || !m.Reported || !u.Reported || m.N < r.MinBase() || u.N < r.MinBase() {
		return 0, 0, false
	}
	return minutes(m.Value), minutes(u.Value), true
}

func (r Report) alsoCard(th cardTheme) (*card, bool) {
	if r.Verdicts < r.MinBase() {
		return nil, false
	}
	c := &card{th: th, title: "Also consistent, readable, efficient and fast."}
	y := cardPad + eyebrowPx
	c.eyebrow(y, "And the rest · "+r.windowLabel())
	head := "Every pull request gets the same one-line review, in your style guide's voice."
	if with, without, ok := r.speedBars(); ok && with > without {
		head = fmt.Sprintf("Every pull request gets the same one-line review, in your style guide's voice. It adds about %d minutes to a merge.", with-without)
	}
	// The tiles start right under the headline, however many lines it took.
	headWrapped := wrap(head, headChars)
	c.lines(cardPad, y+eyebrowGap, headPx, headLineH, th.Head, bold, headWrapped)
	top := y + eyebrowGap + len(headWrapped)*headLineH + 16
	tiles := r.alsoTiles()
	tileW := colX / len(tiles)
	bottom := top
	for i, t := range tiles {
		x := cardPad + i*tileW
		c.text(x, top, eyebrowPx, th.Muted, label, strings.ToUpper(t.Head))
		c.figure(x, top+56, 40, t.Big)
		yy := c.lines(x, top+92, bodyPx-1, bodyLineH-2, th.Text, plain, wrap(t.Text, tileChars))
		if yy > bottom {
			bottom = yy
		}
	}
	return c.closeAt(bottom - (bodyLineH - 2) + cardPad), true
}

// rung is one step of the Hive loop for the how-it-works card.
type rung struct{ Step, What, Outcome string }

// hiveLoop is the loop every task goes through, in order.
var hiveLoop = []rung{
	{"Scan", "Reads open issues, pull requests, failing checks.", "like a webhook trigger"},
	{"Claim", "One agent takes one task.", "one job per task"},
	{"Work", "A short-lived agent does it, then stops.", "ephemeral runner"},
	{"Verify", "AI review plus your CI must pass.", "both are required checks"},
	{"Merge", "Held for a human, or merged on green.", "PRs only, then on green"},
	{"Credit", "The issue opener is named on the merge.", "co-author trailer"},
}

func howItWorksCard(th cardTheme) *card {
	c := &card{th: th, title: "How Hive works, from scan to credit."}
	y := cardPad + eyebrowPx
	c.eyebrow(y, "How it works · every task")
	c.lines(cardPad, y+eyebrowGap, headPx, headLineH, th.Head, bold, wrap("A coding agent does the work. A review agent and your CI gate the merge.", headChars))
	y = y + eyebrowGap + 2*headLineH + 8
	const stepX, textX = cardPad + 56, cardPad + 56 + 104
	for i, r := range hiveLoop {
		what := wrap(r.What, chars(cardW-cardPad-textX-220, bodyPx))
		rowH := 24 + len(what)*bodyLineH + 16
		base := y + 30
		c.figure(cardPad, base, 22, fmt.Sprintf("%02d", i+1))
		c.text(stepX, base, bodyPx, th.Head, bold, r.Step)
		c.lines(textX, base, bodyPx, bodyLineH, th.Text, plain, what)
		c.text(cardW-cardPad, base, footPx, th.Muted, textStyle{Weight: 400, End: true}, r.Outcome)
		y += rowH
		if i < len(hiveLoop)-1 {
			c.rule(cardPad, cardW-cardPad, y)
		}
	}
	return c.closeAt(y + cardPad - 8)
}
