package dashboard

import (
	"fmt"

	"github.com/hivecommons/hive/pkg/config"
)

// A dedupe checklist the hive resolves itself (hivecommons/hive#15754): the
// close-on-merge backfill ticks each `- [ ] #N` item once #N is closed or
// merged and closes the checklist when every item is resolved. Its row says
// the hive is on it — "Advisory · hive is working the items (k/N resolved)"
// — instead of asking a person to read and close it; "Close now" stays as
// the optional human lead.

const (
	// stageTrackerAdvisoryWorkingFormat is the chip and caption, given the
	// resolved and total item counts.
	stageTrackerAdvisoryWorkingFormat = "Advisory · hive is working the items (%d/%d resolved)"
	// StageTrackerAdvisoryCloseNowLead is the optional human action.
	StageTrackerAdvisoryCloseNowLead = "Close now"
	// stageTrackerAdvisoryWorkingWhy is the StageActor sentence.
	stageTrackerAdvisoryWorkingWhy = "hive ticks each item once it is closed or merged and closes the advisory when all are resolved; nothing waits on a person"
)

// stageTrackerAdvisoryWorking marks a dedupe checklist with task-list items
// as worked by the hive when the auto-close knob is on. Any other advisory
// is left as it is.
func stageTrackerAdvisoryWorking(adv *StageTrackerAdvisory, autoclose bool) {
	if adv == nil || !autoclose || adv.Source == stageTrackerAdvisorySourceVerdict || adv.Items <= 0 {
		return
	}
	adv.Working = true
	adv.Chip = fmt.Sprintf(stageTrackerAdvisoryWorkingFormat, adv.Resolved, adv.Items)
	adv.Caption = adv.Chip
	adv.Lead = StageTrackerAdvisoryCloseNowLead
}

// stageTrackerAdvisoryAutoclose reads the dedupe knob; unset (or no
// config) means on.
func stageTrackerAdvisoryAutoclose(cfg *config.Config) bool {
	return cfg == nil || cfg.Dedupe.AdvisoryAutocloseEnabled()
}
