package betty

import "regexp"

var (
	// figureRe is a specific quantity: a number with thousands separators,
	// three or more digits, or a number with a magnitude word.
	figureRe = regexp.MustCompile(`(?i)\b\d{1,3}(?:,\d{3})+\b|\b\d{3,}\b|\b\d+(?:\.\d+)?\s*(?:thousand|million|billion|k)\b`)
	// sourceRe is a link or a site name: something a listener could check.
	sourceRe = regexp.MustCompile(`(?i)https?://|\bwww\.|\b[a-z0-9-]+\.(?:com|org|net|io|dev|gov|edu|co|info)\b`)
)

// hasUnsourcedFigure reports whether a research summary states a figure
// without naming a source for it. Betty must say she is not sure rather
// than repeat such a figure.
func hasUnsourcedFigure(summary string) bool {
	return figureRe.MatchString(summary) && !sourceRe.MatchString(summary)
}
