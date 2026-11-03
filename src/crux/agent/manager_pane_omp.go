package agent

import "github.com/hivecommons/hive/crux/agent/panesignal"

func paneShowsOMPMidTurn(pane string) bool {
	return panesignal.ShowsOMPMidTurn(pane)
}
