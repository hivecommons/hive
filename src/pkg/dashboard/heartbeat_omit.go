package dashboard

import "github.com/hivecommons/hive/pkg/config"

// heartbeatOmitForDisplay returns the normalized omit classes, never nil, so
// the Hub tab can render "nothing omitted" without a null check.
func heartbeatOmitForDisplay(raw []string) []string {
	classes, err := config.HeartbeatOmitClasses(raw)
	if err != nil || classes == nil {
		return []string{}
	}
	return classes
}
