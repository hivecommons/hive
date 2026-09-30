package dashboard

import (
	"net/url"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// hubMyHivesPath is the hub page that lists every hive the signed-in user can
// open ("My Hives", served by HubServer.handleDashboard). It is the target of
// the "My hives" entry in the spoke dashboard's user menu (#9696), so a user
// with several hives can jump back to the list and pick another one.
const hubMyHivesPath = "/dashboard"

// myHivesURL returns the absolute URL of the hub's My Hives page for this
// spoke, or "" when the spoke has no hub link. Hub.URL alone is not a hub
// link: config defaults fill it with the public hub even on a standalone
// install, so Hub.Enabled is the real signal (the same pairing
// hiveIDLockedByHub uses). Anything other than an absolute http(s) URL is
// refused so the menu never renders a javascript: or relative href.
func myHivesURL(cfg *config.Config) string {
	if cfg == nil || !cfg.Hub.Enabled {
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.Hub.URL), "/")
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return base + hubMyHivesPath
}
