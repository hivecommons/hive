package outcomebench

import (
	"encoding/json"
	"fmt"
	"html"
	"math"
	"os"
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
	// Caught is the plain-English description of what the reviewer found.
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

// cardTheme mirrors the dashboard's design tokens (src/pkg/dashboard/static/
// tokens.css): surface-2 panels, amber brand, muted slate text. One SVG is
// written per theme and the README picks one with <picture>.
type cardTheme struct {
	Name, BG, Border, Text, Muted, Brand, Neutral, OK string
}

var cardThemes = []cardTheme{
	{Name: "dark", BG: "#121922", Border: "#263545", Text: "#f6f8fb", Muted: "#a8b3c2", Brand: "#f4c75f", Neutral: "#56657a", OK: "#74df9a"},
	{Name: "light", BG: "#ffffff", Border: "#d9dee5", Text: "#1f2937", Muted: "#4b5563", Brand: "#9a5f0b", Neutral: "#9aa5b4", OK: "#16a34a"},
}

const (
	cardFont    = "Inter, ui-sans-serif, system-ui, -apple-system, 'Segoe UI', sans-serif"
	cardW       = 280
	cardH       = 210
	cardWideW   = 880
	cardPad     = 20
	cardRadius  = 12
	eyebrowPx   = 10
	claimPx     = 14
	footerPx    = 10
	bigPx       = 52
	claimLineH  = 19
	claimChars  = 32 // characters per claim line at claimPx in a 280px card
	footChars   = 46 // characters per footer line at footerPx in a 280px card
	wideChars   = 58 // characters per line in a two-column wide card
	tileChars   = 28 // characters per line in a four-tile strip
	ladderChars = 70
)

// Card names; the asset file is <AssetBase>-card-<name>-<theme>.svg.
const (
	CardSentBack        = "sent-back"
	CardChecksGreen     = "checks-green"
	CardStories         = "stories"
	CardPromptInjection = "prompt-injection"
	CardCost            = "cost"
	CardAlso            = "also"
	CardHowItWorks      = "how-it-works"
)

// CardNames lists every card the generator knows, in README order.
func CardNames() []string {
	return []string{CardSentBack, CardChecksGreen, CardStories, CardPromptInjection, CardCost, CardAlso, CardHowItWorks}
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
	case CardSentBack:
		return r.sentBackCard(th)
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

// monthLabel is "Aug 2026".
func monthLabel(t time.Time) string { return t.UTC().Format("Jan 2006") }

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

func cardOpen(b *strings.Builder, th cardTheme, w, h int, title string) {
	fmt.Fprintf(b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="%s" font-family="%s">
<title>%s</title>
<rect x="0.5" y="0.5" width="%d" height="%d" rx="%d" fill="%s" stroke="%s"/>
`, w, h, w, h, esc(title), cardFont, esc(title), w-1, h-1, cardRadius, th.BG, th.Border)
}

func eyebrow(b *strings.Builder, th cardTheme, x, y int, s string) {
	fmt.Fprintf(b, `<text x="%d" y="%d" fill="%s" font-size="%d" font-weight="600" letter-spacing="1.2">%s</text>`+"\n", x, y, th.Muted, eyebrowPx, esc(strings.ToUpper(s)))
}

func textLines(b *strings.Builder, th cardTheme, x, y, px, lineH int, fill string, lines []string) int {
	for i, l := range lines {
		fmt.Fprintf(b, `<text x="%d" y="%d" fill="%s" font-size="%d">%s</text>`+"\n", x, y+i*lineH, fill, px, esc(l))
	}
	return y + len(lines)*lineH
}

// footer writes the base statement bottom-aligned, up to two lines.
func footer(b *strings.Builder, th cardTheme, x, bottom, chars int, s string) {
	lines := wrap(s, chars)
	y := bottom - (len(lines)-1)*(footerPx+3)
	textLines(b, th, x, y, footerPx, footerPx+3, th.Muted, lines)
}

// numberCard is the shared layout: eyebrow, big number, sentence, base.
func numberCard(th cardTheme, title, eye, big, claim, base string) string {
	claimLines := wrap(claim, claimChars)
	baseLines := wrap(base, footChars)
	// Height grows with the text so nothing overlaps; cardH is the minimum.
	h := 126 + len(claimLines)*claimLineH + 14 + len(baseLines)*(footerPx+3) + cardPad - 8
	if h < cardH {
		h = cardH
	}
	var b strings.Builder
	cardOpen(&b, th, cardW, h, title)
	eyebrow(&b, th, cardPad, 32, eye)
	fmt.Fprintf(&b, `<text x="%d" y="%d" fill="%s" font-size="%d" font-weight="700">%s</text>`+"\n", cardPad, 96, th.Brand, bigPx, esc(big))
	textLines(&b, th, cardPad, 126, claimPx, claimLineH, th.Text, claimLines)
	footer(&b, th, cardPad, h-cardPad+4, footChars, base)
	b.WriteString("</svg>\n")
	return b.String()
}

func (r Report) sentBackCard(th cardTheme) (string, bool) {
	m := r.Mediated.ChangesRequested
	if !r.SplitSupported() || !m.Reported || m.Rate <= 0 || m.N < r.MinBase() {
		return "", false
	}
	return numberCard(th,
		fmt.Sprintf("%s reviewed pull requests was sent back for fixes before it merged", oneIn(m.Rate)),
		"Accuracy · sent back before merge",
		oneIn(m.Rate),
		"reviewed pull requests was sent back for fixes before it merged.",
		fmt.Sprintf("from %s merged pull requests Hive reviewed, %s", commas(m.N), r.windowLabel()),
	), true
}

func (r Report) checksGreenCard(th cardTheme) (string, bool) {
	c := r.Mediated
	if !r.SplitSupported() || !c.CIFailureAtHead.Reported || c.CIFailureAtHead.N < r.MinBase() || !c.CICoverage.Reported || c.CICoverage.Rate < minCICardCoverage {
		return "", false
	}
	pass := int(math.Round(100 * (1 - c.CIFailureAtHead.Rate)))
	return numberCard(th,
		fmt.Sprintf("%d in 100 reviewed pull requests merged with every check passing", pass),
		"Accuracy · checks at merge",
		fmt.Sprintf("%d in 100", pass),
		"reviewed pull requests merged with every check passing.",
		fmt.Sprintf("from %s reviewed merges with a recorded check result, %s", commas(c.CIFailureAtHead.N), r.windowLabel()),
	), true
}

func (r Report) promptInjectionCard(th cardTheme) (string, bool) {
	rt := r.RedTeam
	if rt == nil || rt.Attacks <= 0 {
		return "", false
	}
	pct := int(math.Round(100 * float64(rt.Caught) / float64(rt.Attacks)))
	return numberCard(th,
		fmt.Sprintf("%d%% of prompt-injection attempts were caught before reaching an agent, with %d false alarm in %d harmless requests", pct, rt.FalseAlarms, rt.Benign),
		"Security · prompt injection",
		fmt.Sprintf("%d%%", pct),
		fmt.Sprintf("of prompt-injection attempts were caught; %d false alarm in %d harmless requests.", rt.FalseAlarms, rt.Benign),
		fmt.Sprintf("Hive's own test set: %d attacks, %d harmless texts, %s. Not production traffic.", rt.Attacks, rt.Benign, monthLabel(rt.RunAt)),
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
	return numberCard(th,
		big+" of agent spend per merged pull request",
		"Cost · per merged pull request",
		big,
		"of agent spend per merged pull request, estimated from provider pricing.",
		fmt.Sprintf("from %s merged pull requests, %s to %s", commas(r.Spend.MergedPRs), monthLabel(r.Spend.WindowStart), monthLabel(r.Spend.WindowEnd)),
	), true
}

// storiesCard shows up to two real PRs side by side: what was caught, how
// long the fix took, when it merged.
func (r Report) storiesCard(th cardTheme) (string, bool) {
	if len(r.Stories) == 0 {
		return "", false
	}
	stories := r.Stories
	if len(stories) > 2 {
		stories = stories[:2]
	}
	colW := (cardWideW - 3*cardPad) / 2
	// Lay out each column first so the card height follows the longest one.
	type col struct {
		s     Story
		lines []string
	}
	cols := make([]col, 0, len(stories))
	maxLines := 0
	for _, s := range stories {
		l := wrap(s.Caught, wideChars)
		if len(l) > maxLines {
			maxLines = len(l)
		}
		cols = append(cols, col{s, l})
	}
	const stepGap = 30
	h := 84 + 16 + maxLines*(claimLineH-2) + 2*stepGap + 10 + cardPad + 28
	var b strings.Builder
	cardOpen(&b, th, cardWideW, h, "Two real pull requests where the reviewer caught a problem before merge")
	eyebrow(&b, th, cardPad, 32, "Accuracy · two real pull requests")
	for i, c := range cols {
		s := c.s
		x := cardPad + i*(colW+cardPad)
		fmt.Fprintf(&b, `<a href="%s"><text x="%d" y="%d" fill="%s" font-size="%d" font-weight="700">%s #%d</text></a>`+"\n", esc(s.URL()), x, 62, th.Brand, claimPx+1, esc(s.Repo), s.Number)
		y := 84
		step := func(label string, lines []string, fill string) {
			fmt.Fprintf(&b, `<circle cx="%d" cy="%d" r="4" fill="%s"/>`+"\n", x+4, y-4, fill)
			fmt.Fprintf(&b, `<text x="%d" y="%d" fill="%s" font-size="%d" font-weight="600">%s</text>`+"\n", x+16, y, th.Muted, footerPx, esc(strings.ToUpper(label)))
			y = textLines(&b, th, x+16, y+16, claimPx-1, claimLineH-2, th.Text, lines) + 10
		}
		step("Caught", c.lines, th.Brand)
		y = 84 + 16 + maxLines*(claimLineH-2) + 10
		step("Fixed", []string{fmt.Sprintf("The author fixed it; the new version was approved %d minutes later.", s.FixMinutes())}, th.OK)
		step("Merged", []string{fmt.Sprintf("Merged %d minutes after the pull request was opened.", s.MergeMinutes())}, th.OK)
	}
	footer(&b, th, cardPad, h-cardPad+4, 120, "Times from the review records Hive keeps. Click a title to open the pull request.")
	b.WriteString("</svg>\n")
	return b.String(), true
}

// tile is one cell of the "also" strip.
type tile struct{ Head, Big, Text string }

// alsoTiles are the four secondary properties. Tiles with a number take it
// from the report; the others are plain statements of how Hive works.
func (r Report) alsoTiles() []tile {
	tiles := []tile{
		{"Consistency", commas(r.Verdicts), "review verdicts, each against the same checklist; one house style file for every task."},
		{"Readability", "", "Every verdict is written as what this is, the problem, the fix, and what happens if it is skipped."},
		{"Efficiency", "", "Agents run only when there is work; capacity is shared fairly across repositories."},
	}
	m, u := r.Mediated.TimeToMerge, r.Unmediated.TimeToMerge
	if r.SplitSupported() && m.Reported && u.Reported && m.N >= r.MinBase() && u.N >= r.MinBase() {
		d := minutes(m.Value) - minutes(u.Value)
		text := fmt.Sprintf("minutes from open to merge for a typical reviewed pull request; review adds about %d.", d)
		if d <= 0 {
			text = "minutes from open to merge for a typical reviewed pull request."
		}
		tiles = append(tiles, tile{"Speed", fmt.Sprint(minutes(m.Value)), text})
	} else {
		tiles = append(tiles, tile{"Speed", "", "Time to merge is reported once enough reviewed pull requests are on record."})
	}
	return tiles
}

func (r Report) alsoCard(th cardTheme) (string, bool) {
	if r.Verdicts < r.MinBase() {
		return "", false
	}
	const h = 190
	tiles := r.alsoTiles()
	var b strings.Builder
	cardOpen(&b, th, cardWideW, h, "Also: consistency, readability, efficiency, speed")
	eyebrow(&b, th, cardPad, 32, "Also · "+r.windowLabel())
	tileW := (cardWideW - 2*cardPad) / len(tiles)
	for i, t := range tiles {
		x := cardPad + i*tileW
		if i > 0 {
			fmt.Fprintf(&b, `<line x1="%d" y1="48" x2="%d" y2="%d" stroke="%s"/>`+"\n", x-10, x-10, h-cardPad-8, th.Border)
		}
		fmt.Fprintf(&b, `<text x="%d" y="%d" fill="%s" font-size="%d" font-weight="700">%s</text>`+"\n", x, 62, th.Text, claimPx, esc(t.Head))
		y := 84
		if t.Big != "" {
			fmt.Fprintf(&b, `<text x="%d" y="%d" fill="%s" font-size="30" font-weight="700">%s</text>`+"\n", x, 94, th.Brand, esc(t.Big))
			y = 114
		}
		textLines(&b, th, x, y, footerPx+2, 15, th.Muted, wrap(t.Text, tileChars))
	}
	b.WriteString("</svg>\n")
	return b.String(), true
}

// rung is one step of the Hive loop for the how-it-works card.
type rung struct{ Step, What, Outcome string }

// hiveLoop is the loop every task goes through, in order.
var hiveLoop = []rung{
	{"Scan", "Hive reads your open issues, pull requests and failing checks.", "Work comes from your backlog"},
	{"Claim", "One agent claims one task, so nothing is done twice.", "No duplicate work"},
	{"Work", "A fresh, short-lived agent does the job and then stops.", "Nothing runs for hours unattended"},
	{"Verify", "CI runs, a separate reviewer checks the change, and planted instructions are screened out.", "Problems are found before merge"},
	{"Merge", "Trust level decides: hold for a human, or merge once every check passes.", "Humans keep the decisions"},
	{"Credit", "The person who opened the issue is credited on the merge.", "The asker is credited"},
}

func howItWorksCard(th cardTheme) string {
	const baseRowH, lineH = 44, 14
	rows := make([][]string, len(hiveLoop))
	h := 72
	for i, r := range hiveLoop {
		rows[i] = wrap(r.What, ladderChars)
		h += baseRowH + (len(rows[i])-1)*lineH
	}
	var b strings.Builder
	cardOpen(&b, th, cardWideW, h, "How Hive works: scan, claim, work, verify, merge, credit")
	eyebrow(&b, th, cardPad, 32, "How it works · every task")
	y := 64
	for i, r := range hiveLoop {
		rowH := baseRowH + (len(rows[i])-1)*lineH
		fmt.Fprintf(&b, `<circle cx="%d" cy="%d" r="12" fill="%s"/><text x="%d" y="%d" fill="%s" font-size="12" font-weight="700" text-anchor="middle">%d</text>`+"\n", cardPad+12, y-4, th.Brand, cardPad+12, y, th.BG, i+1)
		fmt.Fprintf(&b, `<text x="%d" y="%d" fill="%s" font-size="%d" font-weight="700">%s</text>`+"\n", cardPad+36, y, th.Text, claimPx+1, esc(r.Step))
		textLines(&b, th, cardPad+36, y+17, footerPx+2, lineH, th.Muted, rows[i])
		fmt.Fprintf(&b, `<text x="%d" y="%d" fill="%s" font-size="%d" font-weight="600" text-anchor="end">%s</text>`+"\n", cardWideW-cardPad, y, th.OK, footerPx+2, esc(r.Outcome))
		if i < len(hiveLoop)-1 {
			fmt.Fprintf(&b, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="%s"/>`+"\n", cardPad+12, y+10, cardPad+12, y+rowH-18, th.Border)
		}
		y += rowH
	}
	b.WriteString("</svg>\n")
	return b.String()
}
