package github

import "github.com/hivecommons/hive/pkg/ioscan"

func (c *Client) scanCanaryText(text, source string) (ioscan.CanaryLeak, bool) {
	if c == nil || !c.canariesEnabled || c.canaryRegistry == nil || text == "" {
		return ioscan.CanaryLeak{}, false
	}
	leak, ok := c.canaryRegistry.Scan("", text, source)
	if !ok {
		return ioscan.CanaryLeak{}, false
	}
	if c.canaryLeakFunc != nil {
		c.canaryLeakFunc(leak)
	}
	if c.logger != nil {
		c.logger.Warn("ioscan canary leak detected", "agent", leak.Agent, "source", leak.Source)
	}
	return leak, true
}
