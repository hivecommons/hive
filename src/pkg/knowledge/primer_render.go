package knowledge

import (
	"fmt"
	"strings"
)

// Primer render caps. MaxFacts bounds how many facts a kick may receive; these
// bound how many bytes. The pattern is the code map's (CodeMapCaps): entries
// are dropped whole at a boundary, never clipped mid-body, and the render says
// so. Override with knowledge.primer.max_bytes.
const (
	// DefaultPrimerMaxBytes is the rendered-prompt cap when
	// PrimerConfig.MaxBytes is zero. 4 KiB is the "Top 5 safe wins" figure from
	// the 2026-10 token-usage review (#12124): enough for a dozen typical facts,
	// small next to the issue/PR lists it shares the kick with.
	DefaultPrimerMaxBytes = 4096
	// primerMinBytes is the floor a configured cap is raised to, so a typo
	// cannot produce a primer that is nothing but its own marker.
	primerMinBytes = 512
	// primerFooterReserve keeps room for the final "omitted" marker so the
	// render never exceeds the cap.
	primerFooterReserve = 96

	primerHeader      = "# Relevant Knowledge\n\n"
	primerHintsHeader = "## Available Documentation\n\n"
)

// PrimerTruncatedMarker prefixes the line that reports facts dropped by the
// byte cap. It is the same marker the code map uses, so one grep finds every
// bounded primer in a kick.
const PrimerTruncatedMarker = CodeMapTruncatedMarker

// PrimerOmitted reports what a Render left out.
type PrimerOmitted struct {
	Facts int
	Hints int
	Bytes int
}

// Any reports whether anything was dropped.
func (o PrimerOmitted) Any() bool { return o.Facts > 0 || o.Hints > 0 }

func primerTruncLine(o PrimerOmitted) string {
	var what []string
	if o.Facts > 0 {
		what = append(what, fmt.Sprintf("%d facts", o.Facts))
	}
	if o.Hints > 0 {
		what = append(what, fmt.Sprintf("%d hints", o.Hints))
	}
	return fmt.Sprintf("\n_%s %s / %d bytes omitted (size cap)._\n", PrimerTruncatedMarker, strings.Join(what, ", "), o.Bytes)
}

func primerMaxBytes(n int) int {
	if n <= 0 {
		n = DefaultPrimerMaxBytes
	}
	if n < primerMinBytes {
		n = primerMinBytes
	}
	return n
}

// Render is FormatForPrompt with the truncation report. Facts are visited in
// the order the primer ranked them (type priority, then confidence), grouped
// under a heading per type, and each is kept only if its whole rendered entry
// fits the remaining budget; one that does not fit is dropped whole and later,
// smaller facts are still considered, so a single long fact cannot suppress
// everything ranked after it. Hints follow the same rule. When anything was
// dropped the output ends with a one-line marker naming the counts, so a
// reader of the kick knows the primer was bounded rather than exhaustive.
func (pk *PrimedKnowledge) Render() (string, PrimerOmitted) {
	var omitted PrimerOmitted
	if len(pk.Facts) == 0 {
		return "", omitted
	}

	remaining := primerMaxBytes(pk.MaxBytes) - len(primerHeader) - primerFooterReserve

	typeOrder := []string{}
	typeSections := map[string][]Fact{}
	for _, f := range pk.Facts {
		key := string(f.Type)
		if _, exists := typeSections[key]; !exists {
			typeOrder = append(typeOrder, key)
		}
		typeSections[key] = append(typeSections[key], f)
	}

	var b strings.Builder
	b.WriteString(primerHeader)
	for _, typ := range typeOrder {
		heading := "## " + typ + "\n\n"
		headingWritten := false
		for _, f := range typeSections[typ] {
			entry := renderPrimerFact(f)
			need := len(entry)
			if !headingWritten {
				need += len(heading)
			}
			if need > remaining {
				omitted.Facts++
				omitted.Bytes += len(entry)
				continue
			}
			if !headingWritten {
				b.WriteString(heading)
				headingWritten = true
			}
			b.WriteString(entry)
			remaining -= need
		}
	}

	if len(pk.Hints) > 0 {
		headingWritten := false
		for _, hint := range pk.Hints {
			line := "- " + hint + "\n"
			need := len(line)
			if !headingWritten {
				need += len(primerHintsHeader) + 1
			}
			if need > remaining {
				omitted.Hints++
				omitted.Bytes += len(line)
				continue
			}
			if !headingWritten {
				b.WriteString(primerHintsHeader)
				headingWritten = true
			}
			b.WriteString(line)
			remaining -= need
		}
		if headingWritten {
			b.WriteString("\n")
		}
	}

	if omitted.Any() {
		b.WriteString(primerTruncLine(omitted))
	}
	return b.String(), omitted
}

// renderPrimerFact renders one fact as a list entry: title, optional
// confidence and source, then the body indented beneath it.
func renderPrimerFact(f Fact) string {
	var b strings.Builder
	b.WriteString("- **" + f.Title + "**")
	if f.ConfidenceScored && f.Confidence < 1.0 {
		b.WriteString(" (confidence: " + formatConfidence(f.Confidence) + ")")
	}
	if docURL := factDocURL(f); docURL != "" {
		b.WriteString(" (source: " + docURL + ")")
	}
	b.WriteString("\n  " + f.Body + "\n\n")
	return b.String()
}
