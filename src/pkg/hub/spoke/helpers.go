package spoke

import (
	"strconv"
	"strings"
)

const (
	agentStatePaused   = "paused"
	routeBaseDashboard = "hive-dashboard"
	maxPayloadBytes    = 1 << 20
)

func itoa(n int) string { return strconv.Itoa(n) }

func imageTagOf(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon <= slash {
		return ""
	}
	return ref[colon+1:]
}
