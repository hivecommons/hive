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

// cardTheme is the card palette: a warm near-black surface with hairline
// rules, warm off-white text, the dashboard's amber for every numeral and
// for "with Hive" bars, a flat warm gray for "without", and green only for
// pass/merged dots. One SVG is written per theme and the README picks one
// with <picture>. The numeral glow is the only decoration and only the dark
// theme has it.
type cardTheme struct {
	Name, BG, Border, Rule, Head, Text, Muted, Brand, Brand2, Neutral, OK string
	Glow                                                                  bool
}

var cardThemes = []cardTheme{
	{Name: "dark", BG: "#0f0d0a", Border: "#2a251f", Rule: "#1c1814", Head: "#fffaf0", Text: "#ece5d8", Muted: "#8b949e", Brand: "#f0b429", Brand2: "#ffd166", Neutral: "#3a342c", OK: "#2da44e", Glow: true},
	{Name: "light", BG: "#fbf8f2", Border: "#ddd6c8", Rule: "#e8e2d6", Head: "#1a1612", Text: "#2b2620", Muted: "#6b6459", Brand: "#9a5f0b", Brand2: "#b8741a", Neutral: "#d9d2c5", OK: "#1a7f37"},
}

// Layout constants. Every card spans the README content column and is
// declared at twice its display size so GitHub downsamples it crisp; the
// coordinates below are display pixels (the viewBox). Character budgets
// assume a monospace glyph about 0.6 em wide.
const (
	cardFont   = "'IBM Plex Mono', 'JetBrains Mono', ui-monospace, SFMono-Regular, Menlo, Consolas, monospace"
	cardW      = 880
	cardScale  = 2
	cardPad    = 40
	cardRadius = 16
	colX       = cardW - 2*cardPad // usable width
	eyebrowPx  = 11
	eyebrowGap = 40 // eyebrow baseline to headline baseline
	headPx     = 32
	headLineH  = 42
	headChars  = 41
	bigPx      = 88
	bigPxTight = 64 // three or more stats on one row
	bodyPx     = 15
	bodyLineH  = 22
	footPx     = 12
	footLineH  = 18
	wideChars  = 88
	halfChars  = 40 // two columns
	tileChars  = 20 // four columns
)

// Card names; the asset file is <AssetBase>-card-<name>-<theme>.svg.
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

// CardFile is the asset filename for a card in a theme.
func (r Report) CardFile(name, theme string) string {
	return fmt.Sprintf("%s-card-%s-%s.svg", r.AssetBase(), name, theme)
}

// Cards renders every card in every theme, keyed by filename. Cards whose
// data is missing or below MinBase are omitted, never drawn as placeholders.
func (r Report) Cards() map[string]string {
	out := map[string]string{}
	for _, th := range cardThemes {
		for _, name := range CardNames() {
			if svg, ok := r.Card(name, th); ok {
				out[r.CardFile(name, th.Name)] = svg
			}
		}
	}
	return out
}

// Card renders one named card; ok is false when its data is not shown.
func (r Report) Card(name string, th cardTheme) (string, bool) {
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
	return "", false
}

// HasCard reports whether a card has data to show.
func (r Report) HasCard(name string) bool {
	_, ok := r.Card(name, cardThemes[0])
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

// shareWords is share() in words for a headline: "two in three".
func shareWords(rate float64) string {
	words := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}
	var k, d int
	if _, err := fmt.Sscanf(share(rate), "%d in %d", &k, &d); err != nil || k > 10 || d > 10 {
		return share(rate)
	}
	return words[k] + " in " + words[d]
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

// card is the shared builder: it collects the body, then wraps it in the
// surface once the height is known.
type card struct {
	th    cardTheme
	title string
	b     strings.Builder
}

func (c *card) text(x, y, px int, fill, extra, s string) {
	fmt.Fprintf(&c.b, `<text x="%d" y="%d" fill="%s" font-size="%d"%s>%s</text>`+"\n", x, y, fill, px, extra, esc(s))
}

// lines writes wrapped lines and returns the baseline after the last one.
func (c *card) lines(x, y, px, lineH int, fill string, lines []string) int {
	for i, l := range lines {
		c.text(x, y+i*lineH, px, fill, "", l)
	}
	return y + len(lines)*lineH
}

// eyebrow is the tracked uppercase line at the top: section, base, window.
func (c *card) eyebrow(y int, s string) {
	c.text(cardPad, y, eyebrowPx, c.th.Muted, ` font-weight="500" letter-spacing="2.2"`, upperKeepDates(s))
}

var reDateLabel = regexp.MustCompile(`\([A-Z]{3}(?: '\d\d)?(?:–[A-Z]{3})? '\d\d\)`)

// upperKeepDates uppercases an eyebrow but leaves the "(oct '26)" date
// labels lowercase, as the README style requires.
func upperKeepDates(s string) string {
	return reDateLabel.ReplaceAllStringFunc(strings.ToUpper(s), strings.ToLower)
}

// headline is the finding in one plain sentence; the biggest text after
// the numerals. Returns the baseline after it.
func (c *card) headline(y int, s string) int {
	return c.lines(cardPad, y, headPx, headLineH, c.th.Head, wrap(s, headChars))
}

// numeral draws one big amber figure; in the dark theme a blurred copy
// underneath gives it a soft glow.
func (c *card) numeral(x, y, px int, s string) {
	if c.th.Glow {
		fmt.Fprintf(&c.b, `<text x="%d" y="%d" fill="%s" font-size="%d" font-weight="700" filter="url(#glow)" opacity="0.35">%s</text>`+"\n", x, y, c.th.Brand, px, esc(s))
	}
	c.text(x, y, px, "url(#amber)", ` font-weight="700"`, s)
}

func (c *card) rule(x1, x2, y int) {
	fmt.Fprintf(&c.b, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="%s"/>`+"\n", x1, y, x2, y, c.th.Rule)
}

// foot writes the muted footnote above a hairline; y is the hairline.
func (c *card) foot(y int, s string) int {
	c.rule(cardPad, cardW-cardPad, y)
	return c.lines(cardPad, y+footLineH+4, footPx, footLineH, c.th.Muted, wrap(s, wideChars))
}

// String closes the card at height h.
func (c *card) String(h int) string {
	var out strings.Builder
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="%s" font-family="%s">
<title>%s</title>
<defs>
<linearGradient id="amber" x1="0" y1="0" x2="1" y2="0"><stop offset="0" stop-color="%s"/><stop offset="1" stop-color="%s"/></linearGradient>
<filter id="glow" x="-20%%" y="-40%%" width="140%%" height="180%%"><feGaussianBlur stdDeviation="5"/></filter>
</defs>
<rect x="0.5" y="0.5" width="%d" height="%d" rx="%d" fill="%s" stroke="%s"/>
`, cardW*cardScale, h*cardScale, cardW, h, esc(c.title), cardFont, esc(c.title), c.th.Brand, c.th.Brand2, cardW-1, h-1, cardRadius, c.th.BG, c.th.Border)
	out.WriteString(c.b.String())
	out.WriteString("</svg>\n")
	return out.String()
}

// stat is one numeral with the words under it.
type stat struct{ Big, Under string }

// statCard is the shared anatomy: eyebrow, headline, a row of numerals
// each with its base under it, footnote.
func statCard(th cardTheme, title, eye, head string, stats []stat, foot string) string {
	c := &card{th: th, title: title}
	y := cardPad + eyebrowPx
	c.eyebrow(y, eye)
	y = c.headline(y+eyebrowGap, head)
	px := bigPx
	if len(stats) > 2 {
		px = bigPxTight
	}
	colW := colX / len(stats)
	underChars := (colW - 28) / 9 // 15px mono ≈ 9px per glyph, with a gutter
	base := y - headLineH + 36 + px
	maxUnder := 0
	for i, st := range stats {
		x := cardPad + i*colW
		c.numeral(x, base, px, st.Big)
		under := wrap(st.Under, underChars)
		c.lines(x, base+30, bodyPx, bodyLineH, th.Text, under)
		if len(under) > maxUnder {
			maxUnder = len(under)
		}
	}
	y = base + 30 + maxUnder*bodyLineH + 12
	if foot != "" {
		y = c.foot(y, foot) - footLineH
	}
	return c.String(y + cardPad - 4)
}

func (r Report) accuracyCard(th cardTheme) (string, bool) {
	f := r.Followup
	fa, ta := f.FixedAndApproved, f.TimeToApproval
	if !fa.Reported || fa.K <= 0 || fa.N < r.MinBase() {
		return "", false
	}
	words := strings.ToUpper(shareWords(fa.Rate)[:1]) + shareWords(fa.Rate)[1:]
	stats := []stat{{share(fa.Rate), "came back fixed"}}
	title := fmt.Sprintf("%s pull requests the review agent sent back came back fixed and were approved", share(fa.Rate))
	head := fmt.Sprintf("The review agent sent %s pull requests back. %s came back fixed.", commas(fa.N), words)
	if ta.Reported && ta.N >= r.MinBase() {
		stats = append(stats, stat{fmt.Sprintf("%d min", minutes(ta.Value)), "typical time to fix"})
		title += fmt.Sprintf("; %d minutes typical from sent back to approved", minutes(ta.Value))
		head = fmt.Sprintf("The review agent sent %s pull requests back. %s came back fixed, typically in %d minutes.", commas(fa.N), words, minutes(ta.Value))
	}
	foot := fmt.Sprintf("%s sent back %s", commas(fa.N), r.windowLabel())
	if f.MergedSameHead > 0 {
		foot += fmt.Sprintf("; %s merged as-is after a second review", commas(f.MergedSameHead))
	}
	return statCard(th, title,
		fmt.Sprintf("Accuracy · %s pull requests sent back · %s", commas(fa.N), r.windowLabel()),
		head, stats, foot+".",
	), true
}

func (r Report) checksGreenCard(th cardTheme) (string, bool) {
	c := r.Mediated
	if !r.SplitSupported() || !c.CIFailureAtHead.Reported || c.CIFailureAtHead.N < r.MinBase() || !c.CICoverage.Reported || c.CICoverage.Rate < minCICardCoverage {
		return "", false
	}
	pass := int(math.Round(100 * (1 - c.CIFailureAtHead.Rate)))
	return statCard(th,
		fmt.Sprintf("%d in 100 reviewed pull requests merged with every check passing", pass),
		fmt.Sprintf("Accuracy · %s reviewed merges · %s", commas(c.CIFailureAtHead.N), r.windowLabel()),
		fmt.Sprintf("%d in 100 reviewed pull requests merged with every check green.", pass),
		[]stat{{fmt.Sprintf("%d in 100", pass), "merged with every check green"}},
		fmt.Sprintf("%s reviewed merges with a recorded check result %s.", commas(c.CIFailureAtHead.N), r.windowLabel()),
	), true
}

func (r Report) promptInjectionCard(th cardTheme) (string, bool) {
	rt := r.RedTeam
	if rt == nil || rt.Attacks <= 0 {
		return "", false
	}
	pct := int(math.Round(100 * float64(rt.Caught) / float64(rt.Attacks)))
	fa := fmt.Sprintf("%d in %d", rt.FalseAlarms, rt.Benign)
	if rt.FalseAlarms == 0 {
		fa = "0"
	}
	return statCard(th,
		fmt.Sprintf("%d%% of planted text that tries to redirect an agent (prompt injection) was screened out before an agent read it, with %d false alarm in %d harmless texts", pct, rt.FalseAlarms, rt.Benign),
		fmt.Sprintf("Security · Hive's own test set · %s", monthLabel(rt.RunAt)),
		fmt.Sprintf("We hid %d texts that try to redirect an agent (prompt injection) in issues and files. Hive screened out %d before an agent read them.", rt.Attacks, rt.Caught),
		[]stat{
			{fmt.Sprintf("%d%%", pct), "screened out before an agent read it"},
			{fa, "harmless texts wrongly flagged"},
		},
		fmt.Sprintf("Hive's own test set of %d texts %s; not production traffic.", rt.Attacks+rt.Benign, monthLabel(rt.RunAt)),
	), true
}

func (r Report) costCard(th cardTheme) (string, bool) {
	per, ok := r.Spend.PerMergedPR()
	if !ok || r.Spend.MergedPRs < r.MinBase() {
		return "", false
	}
	big := fmt.Sprintf("about $%.0f", per)
	if per < 1 {
		big = fmt.Sprintf("about %d¢", int(math.Round(per*100)))
	}
	window := rangeLabel(r.Spend.WindowStart, r.Spend.WindowEnd)
	return statCard(th,
		big+" of agent spend per merged pull request",
		fmt.Sprintf("Cost · %s merged pull requests · %s", commas(r.Spend.MergedPRs), window),
		fmt.Sprintf("Agents spent about $%s to merge %s pull requests, %s each. It is tracked like CI minutes.", commas(int(math.Round(r.Spend.USD))), commas(r.Spend.MergedPRs), big),
		[]stat{
			{strings.TrimPrefix(big, "about "), "per merged pull request"},
			{"$" + commas(int(math.Round(r.Spend.USD))), "total agent spend"},
		},
		fmt.Sprintf("Estimate from provider list prices %s; some models unpriced.", window),
	), true
}

// storiesCard is the before/after scenario: the left column is the familiar
// path without Hive, in plain words and without invented numbers; the right
// column is one real pull request with its recorded timings. A second real
// pull request, if present, is summarised in the footnote.
func (r Report) storiesCard(th cardTheme) (string, bool) {
	if len(r.Stories) == 0 {
		return "", false
	}
	s := r.Stories[0]
	c := &card{th: th, title: fmt.Sprintf("Before and after: %s #%d was sent back once and merged %d minutes after opening", s.Repo, s.Number, s.MergeMinutes())}
	y := cardPad + eyebrowPx
	c.eyebrow(y, fmt.Sprintf("Before / after · one real pull request · %s", r.windowLabel()))
	y = c.headline(y+eyebrowGap, fmt.Sprintf("A coding agent opened a pull request. The review agent sent it back once. It merged, green, %d minutes after opening.", s.MergeMinutes())) + 4
	colW := (colX - cardPad) / 2
	top := y + 8
	col := func(i int, label, labelFill string, steps [][2]string, dot []string) int {
		x := cardPad + i*(colW+cardPad)
		c.text(x, top, bodyPx+2, labelFill, ` font-weight="700"`, label)
		yy := top + 36
		for j, st := range steps {
			fmt.Fprintf(&c.b, `<circle cx="%d" cy="%d" r="4" fill="%s"/>`+"\n", x+4, yy-4, dot[j])
			c.text(x+18, yy, eyebrowPx, th.Muted, ` font-weight="500" letter-spacing="2.2"`, strings.ToUpper(st[0]))
			yy = c.lines(x+18, yy+bodyLineH, bodyPx, bodyLineH, th.Text, wrap(st[1], halfChars)) + 12
		}
		return yy
	}
	left := col(0, "Without Hive", th.Muted, [][2]string{
		{"Issue", "A failing check or an open issue waits for someone to pick it up."},
		{"Pull request", "Someone writes the fix when they have time."},
		{"Review", "Someone else reads it when they have time."},
		{"Merge", "Thursday, maybe."},
	}, []string{th.Neutral, th.Neutral, th.Neutral, th.Neutral})
	right := col(1, fmt.Sprintf("With Hive: %s #%d", s.Repo, s.Number), th.Brand, [][2]string{
		{"Pull request", "A coding agent opened it."},
		{fmt.Sprintf("Sent back at %d min", s.CaughtMinutes()), s.Caught},
		{fmt.Sprintf("Fixed at %d min", s.CaughtMinutes()+s.FixMinutes()), "The coding agent pushed a fix; the review agent approved it."},
		{fmt.Sprintf("Merged at %d min", s.MergeMinutes()), "CI green, a human merged it."},
	}, []string{th.Brand, th.Brand, th.OK, th.OK})
	bottom := left
	if right > bottom {
		bottom = right
	}
	x := cardPad + colW + cardPad
	fmt.Fprintf(&c.b, `<a href="%s"><rect x="%d" y="%d" width="%d" height="%d" fill="transparent"/></a>`+"\n", esc(s.URL()), x, top-20, colW, right-top+20)
	foot := "Times from Hive's review records; the right column links to the pull request."
	if len(r.Stories) > 1 {
		t := r.Stories[1]
		foot = fmt.Sprintf("Same shape: %s #%d, sent back at %d min, fixed at %d, merged at %d. Times from Hive's review records.", t.Repo, t.Number, t.CaughtMinutes(), t.CaughtMinutes()+t.FixMinutes(), t.MergeMinutes())
	}
	y = c.foot(bottom, foot) - footLineH
	return c.String(y + cardPad - 4), true
}

// tile is one column of the "also" strip.
type tile struct{ Head, Big, Text string }

// alsoTiles are the four secondary properties. Tiles with a number take it
// from the report; the others are plain statements of how Hive works.
func (r Report) alsoTiles() []tile {
	return []tile{
		{"Consistent", commas(r.Verdicts), "reviews, one checklist, one shared style guide"},
		{"Readable", "4 parts", "to every review: what, problem, fix, if skipped"},
		{"Efficient", "On demand", "jobs start on work and stop when done"},
	}
}

// speedBars is the one two-bar comparison with a base above MinBase on
// both sides: typical minutes from open to merge, with and without review.
// ok is false when either side is missing, and the card says so instead.
func (r Report) speedBars() (with, without int, ok bool) {
	m, u := r.Mediated.TimeToMerge, r.Unmediated.TimeToMerge
	if !r.SplitSupported() || !m.Reported || !u.Reported || m.N < r.MinBase() || u.N < r.MinBase() {
		return 0, 0, false
	}
	return minutes(m.Value), minutes(u.Value), true
}

func (r Report) alsoCard(th cardTheme) (string, bool) {
	if r.Verdicts < r.MinBase() {
		return "", false
	}
	c := &card{th: th, title: "Also: consistent, readable, efficient, fast"}
	y := cardPad + eyebrowPx
	c.eyebrow(y, "Also · "+r.windowLabel())
	head := "Every pull request gets the same AI review, written for people."
	if with, without, ok := r.speedBars(); ok && with > without {
		head = fmt.Sprintf("Every pull request gets the same AI review. It adds about %d minutes.", with-without)
	}
	y = c.headline(y+eyebrowGap, head) + 8
	tiles := r.alsoTiles()
	cols := len(tiles) + 1
	tileW := colX / cols
	top := y
	bottom := top
	for i, t := range tiles {
		x := cardPad + i*tileW
		c.text(x, top, bodyPx, th.Muted, ` font-weight="500" letter-spacing="2.2"`, strings.ToUpper(t.Head))
		c.numeral(x, top+52, 30, t.Big)
		yy := c.lines(x, top+80, bodyPx-1, bodyLineH-2, th.Text, wrap(t.Text, tileChars))
		if yy > bottom {
			bottom = yy
		}
	}
	// Fourth column: the speed comparison as two thin bars.
	x := cardPad + 3*tileW
	c.text(x, top, bodyPx, th.Muted, ` font-weight="500" letter-spacing="2.2"`, "FAST")
	if with, without, ok := r.speedBars(); ok {
		barW := tileW - 76 // room for the value at the bar end
		longest := with
		if without > longest {
			longest = without
		}
		bar := func(yy int, label string, val int, fill string) {
			w := barW * val / longest
			c.text(x, yy, footPx, th.Muted, "", label)
			fmt.Fprintf(&c.b, `<rect x="%d" y="%d" width="%d" height="8" rx="4" fill="%s"/>`+"\n", x, yy+9, w, fill)
			c.text(x+w+8, yy+17, footPx+1, th.Head, ` font-weight="700"`, fmt.Sprintf("%d min", val))
		}
		bar(top+28, "merged without review", without, th.Neutral)
		bar(top+68, "reviewed by Hive", with, "url(#amber)")
		d := with - without
		note := "typical minutes, open to merge"
		if d > 0 {
			note = fmt.Sprintf("review adds about %d minutes", d)
		}
		yy := c.lines(x, top+112, bodyPx-1, bodyLineH-2, th.Text, wrap(note, tileChars))
		if yy > bottom {
			bottom = yy
		}
	} else {
		yy := c.lines(x, top+28, bodyPx-1, bodyLineH-2, th.Text, wrap("Time to merge is reported once enough pull requests are on record.", tileChars))
		if yy > bottom {
			bottom = yy
		}
	}
	return c.String(bottom + cardPad - 8), true
}

// rung is one step of the Hive loop for the how-it-works card.
type rung struct{ Step, What, Outcome string }

// hiveLoop is the loop every task goes through, in order.
var hiveLoop = []rung{
	{"Scan", "Reads open issues, pull requests, failing checks.", "like a webhook trigger"},
	{"Claim", "One agent takes one task.", "one job per task"},
	{"Work", "A short-lived agent does it, then stops.", "ephemeral runner"},
	{"Verify", "AI review plus your CI must pass.", "both are required checks"},
	{"Merge", "Held for a human, or merged on green.", "PRs only → merge on green"},
	{"Credit", "The issue opener is named on the merge.", "co-author trailer"},
}

func howItWorksCard(th cardTheme) string {
	c := &card{th: th, title: "How Hive works: scan, claim, work, verify, merge, credit"}
	y := cardPad + eyebrowPx
	c.eyebrow(y, "How it works · every task")
	y = c.headline(y+eyebrowGap, "A coding agent does the work. A review agent and your CI gate the merge.")
	const stepX, textX = cardPad + 48, cardPad + 48 + 96
	y += 6
	for i, r := range hiveLoop {
		what := wrap(r.What, 42)
		rowH := 20 + len(what)*bodyLineH + 14
		base := y + 26
		c.numeral(cardPad, base, 24, fmt.Sprintf("%02d", i+1))
		c.text(stepX, base, bodyPx, th.Head, ` font-weight="700"`, r.Step)
		c.lines(textX, base, bodyPx, bodyLineH, th.Text, what)
		c.text(cardW-cardPad, base, footPx+1, th.Muted, ` text-anchor="end"`, r.Outcome)
		y += rowH
		if i < len(hiveLoop)-1 {
			c.rule(cardPad, cardW-cardPad, y)
		}
	}
	return c.String(y + cardPad - 10)
}
