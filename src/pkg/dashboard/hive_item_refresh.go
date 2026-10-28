package dashboard

import "strings"

// RefreshHiveItem invalidates the tracker cache and queues the same targeted
// re-read as an inbound label webhook, without depending on GitHub delivering
// that webhook back to the hive. Reads share its debounce, configuration and
// ApplyWebhookItems path into the governor's cached snapshot.
func (s *Server) RefreshHiveItem(repo string, number int, isPR bool) {
	if s == nil || strings.TrimSpace(repo) == "" || number <= 0 {
		return
	}
	s.invalidateStageTrackerForWebhook()
	if !s.webhookTrackerRefreshEnabled() {
		return
	}
	s.trackerRefresher().enqueue([]trackerRefreshKey{{repo: repo, number: number, pr: isPR}}, hiveWriteDeliveryPrefix+"item-refresh")
}
