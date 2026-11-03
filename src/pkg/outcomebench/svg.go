package outcomebench

import (
	"fmt"
	"html"
	"math"
	"strings"
)

// Chart palette: Hive's brand amber for the mediated series and a neutral
// slate for the unmediated one, both legible on GitHub's light and dark
// backgrounds. Text uses currentColor-free mid greys for the same reason.
const (
	colourMediated   = "#f4c75f"
	colourUnmediated = "#8a97a8"
	colourAxis       = "#7d8a9b"
	colourText       = "#8a97a8"
	colourGrid       = "#7d8a9b"
)

const (
	svgWidth   = 760
	svgHeight  = 300
	svgPadL    = 56
	svgPadR    = 24
	svgPadT    = 36
	svgPadB    = 56
	svgFont    = "ui-sans-serif, system-ui, sans-serif"
	svgFontPx  = 12
	barGap     = 10
	groupGap   = 36
	markerSize = 3.5
)

type barMetric struct {
	label  string
	med, u Rate
}

// minCIComparableCoverage is the share of merged PRs that must have a
// captured CI status before a cohort's CI-failure rate is drawn as a bar
// rather than a placeholder: below it the figure is a sample of reviewed
// heads, not the cohort.
const minCIComparableCoverage = 0.5

func (r Report) barMetrics() []barMetric {
	ci := func(c Cohort) Rate {
		if !c.CICoverage.Reported || c.CICoverage.Rate < minCIComparableCoverage {
			return Rate{N: c.CIFailureAtHead.N}
		}
		return c.CIFailureAtHead
	}
	return []barMetric{
		{"Revert rate", r.Mediated.RevertRate, r.Unmediated.RevertRate},
		{"Change-failure rate", r.Mediated.ChangeFailure, r.Unmediated.ChangeFailure},
		{"CI failure at head", ci(r.Mediated), ci(r.Unmediated)},
		{"Changes requested pre-merge", r.Mediated.ChangesRequested, Rate{}},
	}
}

// niceHours is niceMax for an hours axis.
func niceHours(v float64) float64 {
	for _, m := range []float64{0.5, 1, 2, 4, 8, 24, 48, 168} {
		if v <= m {
			return m
		}
	}
	return math.Ceil(v/168) * 168
}

// niceMax picks the smallest axis maximum from a short list of round values
// so the quarter gridlines land on readable percentages.
func niceMax(v float64) float64 {
	for _, m := range []float64{0.05, 0.1, 0.2, 0.4, 0.6, 0.8, 1} {
		if v <= m {
			return m
		}
	}
	return 1
}

func svgHeader(title string) string {
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="%s" font-family="%s" font-size="%d">
<title>%s</title>
<text x="%d" y="20" fill="%s" font-size="14" font-weight="600">%s</text>
`, svgWidth, svgHeight, svgWidth, svgHeight, html.EscapeString(title), svgFont, svgFontPx, html.EscapeString(title), svgPadL, colourText, html.EscapeString(title))
}

func legend(b *strings.Builder, x, y int) {
	fmt.Fprintf(b, `<rect x="%d" y="%d" width="12" height="12" fill="%s"/><text x="%d" y="%d" fill="%s">mediated</text>`+"\n", x, y, colourMediated, x+16, y+10, colourText)
	fmt.Fprintf(b, `<rect x="%d" y="%d" width="12" height="12" fill="%s"/><text x="%d" y="%d" fill="%s">unmediated</text>`+"\n", x+90, y, colourUnmediated, x+106, y+10, colourText)
}

// BarChartSVG draws mediated vs unmediated per rate metric with Wilson
// whiskers. Unreported bars are drawn as a hatched placeholder labelled n.
func (r Report) BarChartSVG() string {
	metrics := r.barMetrics()
	var b strings.Builder
	b.WriteString(svgHeader("Mediated vs unmediated — " + r.Month() + " (95% Wilson intervals)"))
	legend(&b, svgWidth-svgPadR-200, 8)
	plotW := svgWidth - svgPadL - svgPadR
	plotH := svgHeight - svgPadT - svgPadB
	maxV := 0.05
	for _, m := range metrics {
		for _, rt := range []Rate{m.med, m.u} {
			if rt.Reported && rt.Hi > maxV {
				maxV = rt.Hi
			}
		}
	}
	maxV = niceMax(maxV)
	y := func(v float64) float64 { return float64(svgPadT+plotH) - v/maxV*float64(plotH) }
	for i := 0; i <= 4; i++ {
		v := maxV * float64(i) / 4
		fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="%s" stroke-opacity="0.3"/><text x="%d" y="%.1f" fill="%s" text-anchor="end">%.0f%%</text>`+"\n",
			svgPadL, y(v), svgWidth-svgPadR, y(v), colourGrid, svgPadL-6, y(v)+4, colourText, 100*v)
	}
	groupW := (plotW - groupGap*(len(metrics)-1)) / len(metrics)
	barW := (groupW - barGap) / 2
	for gi, m := range metrics {
		gx := svgPadL + gi*(groupW+groupGap)
		drawBar := func(x int, rt Rate, fill string, skip bool) {
			if skip {
				return
			}
			if !rt.Reported {
				fmt.Fprintf(&b, `<rect x="%d" y="%.1f" width="%d" height="%d" fill="none" stroke="%s" stroke-dasharray="3 3"/><text x="%d" y="%.1f" fill="%s" text-anchor="middle" font-size="10">n=%d</text>`+"\n",
					x, y(0)-18, barW, 18, fill, x+barW/2, y(0)-6, colourText, rt.N)
				return
			}
			top := y(rt.Rate)
			fmt.Fprintf(&b, `<rect x="%d" y="%.1f" width="%d" height="%.1f" fill="%s" rx="2"/>`+"\n", x, top, barW, y(0)-top, fill)
			cx := x + barW/2
			fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="%s" stroke-width="1.5"/><line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="%s" stroke-width="1.5"/><line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="%s" stroke-width="1.5"/>`+"\n",
				cx, y(rt.Lo), cx, y(rt.Hi), colourAxis, cx-5, y(rt.Lo), cx+5, y(rt.Lo), colourAxis, cx-5, y(rt.Hi), cx+5, y(rt.Hi), colourAxis)
			fmt.Fprintf(&b, `<text x="%d" y="%.1f" fill="%s" text-anchor="middle" font-size="10">%.1f%% (n=%d)</text>`+"\n", cx, y(rt.Hi)-4, colourText, 100*rt.Rate, rt.N)
		}
		drawBar(gx, m.med, colourMediated, false)
		drawBar(gx+barW+barGap, m.u, colourUnmediated, m.label == "Changes requested pre-merge")
		fmt.Fprintf(&b, `<text x="%d" y="%d" fill="%s" text-anchor="middle">%s</text>`+"\n", gx+groupW/2, svgPadT+plotH+18, colourText, html.EscapeString(m.label))
	}
	fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="%s"/>`+"\n", svgPadL, y(0), svgWidth-svgPadR, y(0), colourAxis)
	fmt.Fprintf(&b, `<text x="%d" y="%d" fill="%s" font-size="10">Hatched = n &lt; %d or CI not captured for most of the cohort: not reported. Change-failure = revert or follow-up fix within %d days.</text>`+"\n", svgPadL, svgHeight-10, colourText, MinN, int(Maturation.Hours()/24))
	b.WriteString("</svg>\n")
	return b.String()
}

// TrendSVG draws median time-to-merge per month per cohort as lines, with
// the monthly revert rate annotated where reported. Months below MinN are
// gaps, not zeros.
func (r Report) TrendSVG() string {
	var b strings.Builder
	b.WriteString(svgHeader("Median time-to-merge by month (hours) — revert rate annotated"))
	legend(&b, svgWidth-svgPadR-200, 8)
	plotW := svgWidth - svgPadL - svgPadR
	plotH := svgHeight - svgPadT - svgPadB
	maxV := 0.5
	for _, pts := range r.Trend {
		for _, p := range pts {
			if p.TimeToMerge.Reported && p.TimeToMerge.Value > maxV {
				maxV = p.TimeToMerge.Value
			}
		}
	}
	maxV = niceHours(maxV)
	n := len(r.Months)
	x := func(i int) float64 {
		if n <= 1 {
			return float64(svgPadL + plotW/2)
		}
		return float64(svgPadL) + float64(i)*float64(plotW)/float64(n-1)
	}
	y := func(v float64) float64 { return float64(svgPadT+plotH) - v/maxV*float64(plotH) }
	for i := 0; i <= 5; i++ {
		v := maxV * float64(i) / 5
		fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="%s" stroke-opacity="0.3"/><text x="%d" y="%.1f" fill="%s" text-anchor="end">%.2gh</text>`+"\n",
			svgPadL, y(v), svgWidth-svgPadR, y(v), colourGrid, svgPadL-6, y(v)+4, colourText, v)
	}
	for i, m := range r.Months {
		fmt.Fprintf(&b, `<text x="%.1f" y="%d" fill="%s" text-anchor="middle">%s</text>`+"\n", x(i), svgPadT+plotH+18, colourText, m)
	}
	for _, name := range []string{CohortUnmediated, CohortMediated} {
		colour := colourMediated
		if name == CohortUnmediated {
			colour = colourUnmediated
		}
		var path []string
		for i, p := range r.Trend[name] {
			if !p.TimeToMerge.Reported {
				path = nil
				continue
			}
			cmd := "L"
			if len(path) == 0 {
				cmd = "M"
			}
			path = append(path, fmt.Sprintf("%s%.1f %.1f", cmd, x(i), y(p.TimeToMerge.Value)))
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="%.1f" fill="%s"/>`+"\n", x(i), y(p.TimeToMerge.Value), markerSize, colour)
			label := fmt.Sprintf("%.2fh", p.TimeToMerge.Value)
			if p.RevertRate.Reported {
				label += fmt.Sprintf(" · rev %.1f%%", 100*p.RevertRate.Rate)
			}
			anchor := "middle"
			switch {
			case i == 0 && n > 1:
				anchor = "start"
			case i == n-1 && n > 1:
				anchor = "end"
			}
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="%s" text-anchor="%s" font-size="10">%s</text>`+"\n", x(i), y(p.TimeToMerge.Value)-8, colourText, anchor, label)
			if len(path) > 1 {
				fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="%s" stroke-width="2"/>`+"\n", strings.Join(path, " "), colour)
				path = path[len(path)-1:]
				path[0] = "M" + path[0][1:]
			}
		}
	}
	fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="%s"/>`+"\n", svgPadL, y(0), svgWidth-svgPadR, y(0), colourAxis)
	fmt.Fprintf(&b, `<text x="%d" y="%d" fill="%s" font-size="10">Months with n &lt; %d are gaps. Mediated series starts at the first mediated merge.</text>`+"\n", svgPadL, svgHeight-10, colourText, MinN)
	b.WriteString("</svg>\n")
	return b.String()
}
