package hub

import "time"

// heartbeatUpgradePolicy builds the descriptive HeartbeatUpgradePolicy for one
// spoke (#7262). It is a pure projection of the state the heartbeat handler
// already resolved for its UpgradeTo decision — the SaaS record, the spoke's
// own auto_upgrade flag, the kill switch and the reachable target — so the
// spoke's dashboard cannot disagree with the hub card about who upgrades the
// hive, on what cadence, or how far behind it is.
//
// Returns nil when the hub has no upgrade opinion about the hive: no SaaS
// record and the spoke does not self-upgrade. That keeps the wire format
// unchanged for bare spokes and lets the spoke fall back to its local view.
func (s *HubServer) heartbeatUpgradePolicy(payload *HeartbeatPayload, saasHive *SaaSHive, spokeManaged, paused bool, branch, armedTarget string) *HeartbeatUpgradePolicy {
	if saasHive == nil && !spokeManaged {
		return nil
	}
	hubManaged := saasHive != nil && saasHive.AutoUpgrade
	trackedChannel := ""
	schedule := ""
	if saasHive != nil {
		trackedChannel = saasHive.TrackedChannel
		if hubManaged {
			schedule = normalizeAutoUpgradeMode(saasHive.AutoUpgradeMode)
		}
	}
	if spokeManaged && schedule == "" {
		// A self-upgrading spoke acts on the first UpgradeTo it sees.
		schedule = AutoUpgradeModeInstant
	}
	reach := s.reachableUpgradeTarget(branch, payload.ImageRef, trackedChannel)
	policy := &HeartbeatUpgradePolicy{
		HubManaged:     hubManaged,
		SpokeManaged:   spokeManaged,
		Schedule:       schedule,
		Paused:         paused,
		Branch:         branch,
		Channel:        reach.Channel,
		TargetSHA:      reach.SHA,
		TargetResolved: reach.Resolved,
		ArmedTarget:    armedTarget,
	}
	if reach.Channel == ReleaseChannelStable && reach.Resolved {
		policy.NextUpdateAt = stableNextPromotionAt(peekChannelTargets())
	}
	if schedule == AutoUpgradeModeDaily || schedule == AutoUpgradeModeWeekly {
		policy.ScheduleHour = autoUpgradeDailyHour
		policy.ScheduleTimezone = autoUpgradeTimezone
		if policy.NextUpdateAt == "" && reach.Channel != ReleaseChannelStable {
			policy.NextUpdateAt = nextScheduledAutoUpgradeAt(schedule, saasHive.AutoUpgradeLastFired, time.Now())
		}
	}
	if schedule == AutoUpgradeModeWeekly {
		policy.ScheduleWeekday = autoUpgradeWeeklyDay.String()
	}
	return policy
}

func nextScheduledAutoUpgradeAt(mode, lastFiredDate string, now time.Time) string {
	norm := normalizeAutoUpgradeMode(mode)
	if norm != AutoUpgradeModeDaily && norm != AutoUpgradeModeWeekly {
		return ""
	}
	loc, err := autoUpgradeLocation()
	if err != nil {
		return ""
	}
	local := now.In(loc)
	next := scheduledWindowForLocalDay(local)
	if norm == AutoUpgradeModeWeekly {
		next = scheduledWeeklyWindow(local)
		if firedThisISOWeek(lastFiredDate, local, loc) {
			next = next.AddDate(0, 0, 7)
		}
		return next.UTC().Format(time.RFC3339)
	}
	if lastFiredDate == local.Format(autoUpgradeDateFormat) {
		next = next.AddDate(0, 0, 1)
	}
	return next.UTC().Format(time.RFC3339)
}

func scheduledWindowForLocalDay(local time.Time) time.Time {
	return time.Date(local.Year(), local.Month(), local.Day(), autoUpgradeDailyHour, 0, 0, 0, local.Location())
}

func scheduledWeeklyWindow(local time.Time) time.Time {
	dayISO := int(local.Weekday())
	if dayISO == 0 {
		dayISO = 7
	}
	openDay := int(autoUpgradeWeeklyDay)
	delta := openDay - dayISO
	windowDay := local.AddDate(0, 0, delta)
	return scheduledWindowForLocalDay(windowDay)
}

func firedThisISOWeek(lastFiredDate string, local time.Time, loc *time.Location) bool {
	if lastFiredDate == "" {
		return false
	}
	fired, err := time.ParseInLocation(autoUpgradeDateFormat, lastFiredDate, loc)
	if err != nil {
		return false
	}
	fy, fw := fired.ISOWeek()
	ny, nw := local.ISOWeek()
	return fy == ny && fw == nw
}
