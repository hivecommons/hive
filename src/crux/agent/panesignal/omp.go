package panesignal

import (
	"regexp"
	"strings"
)

const (
	OMPStatusLineMarker     = "▶─"
	OMPStatusLogoMarker     = "π >"
	ompSetupWizardTailLines = 15
)

var (
	ompActivityRowRe           = regexp.MustCompile(`(?m)^\s*⎋\s+\S`)
	ompBrailleSpinnerElapsedRe = regexp.MustCompile(`[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]\s+\d+(?:\.\d+)?[smh]\b`)
	ompSetupWizardRe           = regexp.MustCompile(`Setup step \d+ of \d+|Set up your providers|Sign in to your providers|Select provider to login|Signing in to \S|Browser login:|Paste the authorization code \(or full redirect URL\)`)
)

func ShowsOMPMidTurn(pane string) bool {
	return ompActivityRowRe.MatchString(pane) || ompBrailleSpinnerElapsedRe.MatchString(pane)
}

func ShowsOMPSetupWizard(pane string) bool {
	lines := strings.Split(pane, "\n")
	if len(lines) > ompSetupWizardTailLines {
		lines = lines[len(lines)-ompSetupWizardTailLines:]
	}
	return ompSetupWizardRe.MatchString(strings.Join(lines, "\n"))
}
