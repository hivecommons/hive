package dashboard

import (
	"strings"
	"testing"
)

// TestActivityMonitorPhoneTextFloor guards the 12px phone text floor for the
// Activity monitor (#17164): the phone media tier must remap --fs-2xs and
// --fs-xs to --fs-sm inside #activity-monitor-section, plus --ham-odo-digit,
// which :root resolves from --fs-xs before the section override can apply.
func TestActivityMonitorPhoneTextFloor(t *testing.T) {
	html := indexHTML(t)

	const tier = "@media (max-width: 640px), ((pointer: coarse) and (max-width: 820px)) {"
	const rule = "#activity-monitor-section { --fs-2xs: var(--fs-sm); --fs-xs: var(--fs-sm); --ham-odo-digit: var(--fs-sm); }"

	found := false
	for rest := html; ; {
		i := strings.Index(rest, tier)
		if i < 0 {
			break
		}
		rest = rest[i+len(tier):]
		end := strings.Index(rest, "\n    }\n")
		if end < 0 {
			break
		}
		if strings.Contains(rest[:end], rule) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("phone media tier is missing %q — Activity monitor text must stay >= 12px on phones (#17164)", rule)
	}
	if !strings.Contains(html, "--fs-sm: .75rem;") {
		t.Errorf("--fs-sm must remain .75rem (12px), the phone text floor (#17164)")
	}
}
