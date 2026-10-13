package hub

// Reasons the My Hives Version cell carries when a hive reports no version.
// They are the operator-facing text, chosen so the cell says what is MISSING
// and where to look rather than rendering a bare "—":
//
//   - no heartbeat at all → the spoke has never checked in (or its registry
//     entry aged out after staleRemoveAge); on a push-only cluster the hub
//     cannot go and look, so the text names that limit explicitly.
//   - a heartbeat but no version → a spoke build too old to report one.
const (
	versionMissingNoHeartbeat         = "no heartbeat yet"
	versionMissingNoHeartbeatPushOnly = "push-only cluster · awaiting spoke heartbeat"
	versionMissingNotReported         = "heartbeat carries no version"
)

// versionMissingReason explains an empty Version/GitHash on a fleet row.
// lastHeartbeat is the registry entry's RFC3339 timestamp ("" when the hub has
// never heard from the hive); pullOnly says whether the hive's cluster is one
// the hub cannot reach. The dashboard appends "last seen …" itself from
// lastHeartbeat, so the reason stays a stable string.
func versionMissingReason(lastHeartbeat string, pullOnly bool) string {
	if lastHeartbeat == "" {
		if pullOnly {
			return versionMissingNoHeartbeatPushOnly
		}
		return versionMissingNoHeartbeat
	}
	return versionMissingNotReported
}

// clusterIsPullOnly reports whether clusterID names a configured push-only
// cluster (ClusterConfig.PullOnly). Unknown clusters are not push-only.
func (s *HubServer) clusterIsPullOnly(clusterID string) bool {
	if s == nil || clusterID == "" {
		return false
	}
	return s.clusters[clusterID].PullOnly
}
