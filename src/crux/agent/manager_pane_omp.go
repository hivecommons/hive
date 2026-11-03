package agent

import (
	"regexp"

	"github.com/hivecommons/hive/crux/agent/panesignal"
)

var ompSplashSkipRe = regexp.MustCompile(`(?i)press (?:⏎|enter) to skip`)

func paneShowsOMPMidTurn(pane string) bool {
	return panesignal.ShowsOMPMidTurn(pane)
}
