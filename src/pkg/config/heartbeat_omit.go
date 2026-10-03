package config

import (
	"fmt"
	"strings"
)

// Heartbeat omit classes accepted by hub.heartbeat_omit.
const (
	HeartbeatOmitRepos         = "repos"
	HeartbeatOmitUsers         = "users"
	HeartbeatOmitTaskTitles    = "task_titles"
	HeartbeatOmitDashboardURLs = "dashboard_urls"
)

// HeartbeatOmitAll is the full set of valid classes, in documentation order.
var HeartbeatOmitAll = []string{
	HeartbeatOmitRepos, HeartbeatOmitUsers, HeartbeatOmitTaskTitles, HeartbeatOmitDashboardURLs,
}

// HeartbeatOmitClasses normalizes and validates hub.heartbeat_omit, returning
// the de-duplicated classes in documentation order. Unknown classes are an
// error so a typo never silently leaves an identifier flowing to the hub.
func HeartbeatOmitClasses(raw []string) ([]string, error) {
	set := map[string]bool{}
	for _, r := range raw {
		c := strings.ToLower(strings.TrimSpace(r))
		if c == "" {
			continue
		}
		valid := false
		for _, v := range HeartbeatOmitAll {
			if c == v {
				valid = true
			}
		}
		if !valid {
			return nil, fmt.Errorf("hub.heartbeat_omit: unknown class %q (valid: %s)", r, strings.Join(HeartbeatOmitAll, ", "))
		}
		set[c] = true
	}
	var out []string
	for _, v := range HeartbeatOmitAll {
		if set[v] {
			out = append(out, v)
		}
	}
	return out, nil
}
