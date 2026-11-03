package agent

import (
	"regexp"

	"github.com/hivecommons/hive/crux/agent/panesignal"
)

const (
	ompStatusLineMarker = panesignal.OMPStatusLineMarker
	ompStatusLogoMarker = panesignal.OMPStatusLogoMarker
)

var ompSplashSkipRe = regexp.MustCompile(`(?i)press (?:⏎|enter) to skip`)

func paneShowsOMPMidTurn(pane string) bool {
	return panesignal.ShowsOMPMidTurn(pane)
}

func paneShowsOMPSetupWizard(pane string) bool {
	return panesignal.ShowsOMPSetupWizard(pane)
}
