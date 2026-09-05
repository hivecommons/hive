package spoke

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

const (
	GitHubAppTokenStatusOK      = "ok"
	GitHubAppTokenStatusStale   = "stale"
	GitHubAppTokenStatusMissing = "missing"
	GitHubAppTokenStatusError   = "error"
	GitHubAppTokenStaleAfter    = 45 * time.Minute

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

func HashDashboardToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
