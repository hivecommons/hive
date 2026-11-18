package config

import (
	"os"
	"strings"
)

// ProxyDashFilterEnv switches the GitHub egress proxy's outbound dash filter.
// The filter is on by default; set this to "off" (or "false", "0") to forward
// agent write bodies unchanged.
const ProxyDashFilterEnv = "HIVE_PROXY_DASH_FILTER"

// ProxyDashFilter reports whether the proxy rewrites em-dashes in the text
// fields of agent-originated GitHub JSON writes. Default on.
func ProxyDashFilter() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ProxyDashFilterEnv))) {
	case "off", "false", "0", "no":
		return false
	}
	return true
}
