package hub

import (
	"os"
	"strings"
)

// hubReleaseTrackingDisableEnv turns off the hub's release tracking: the
// branch-tip SHA poll against api.github.com, image-branch discovery against
// ghcr.io and the GitHub branch list, and the release-channel resolver against
// ghcr.io (#19726). It is for hubs whose operator manages versions outside the
// hub and denies egress to GitHub and GHCR. Same style as
// HIVE_HUB_MARKETPLACE_DISABLED: "1", "true" or "yes" turn it on, anything
// else (including unset) leaves tracking on.
//
// With tracking off the dashboard renders its existing "unknown" state for the
// latest-image and channel rows, no "behind" badge shows, and hub and spoke
// auto-upgrade have no target to roll to. The SHA poller goroutine still runs
// so the reconciliation lanes it drives keep working; only its GitHub and GHCR
// reads are skipped.
const hubReleaseTrackingDisableEnv = "HIVE_HUB_RELEASE_TRACKING_DISABLED"

// releaseTrackingDisabled reports HIVE_HUB_RELEASE_TRACKING_DISABLED. Read on
// every call (it is a cheap env lookup) so tests can flip it with t.Setenv.
func releaseTrackingDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(hubReleaseTrackingDisableEnv)))
	return v == "1" || v == "true" || v == "yes"
}
