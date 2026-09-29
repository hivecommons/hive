package agent

import "testing"

func TestLoginExpiryWarningIsNotLoginPrompt(t *testing.T) {
	for _, warning := range []string{
		"⚠ Your login expires in 1 day · run /login to renew",
		"Your login expires in 2 hours · run /login to renew",
		"⚠️ Your login expires soon · run /login to renew",
		"  Your login will expire in 30 minutes · run /login to renew  ",
		"YOUR LOGIN EXPIRES IN 1 DAY · run /login to renew",
	} {
		t.Run(warning, func(t *testing.T) {
			if paneShowsLoginPrompt([]string{"Claude Code v2.1.283", warning, "❯", "Got your test message and I'm ready. What do you need?"}) {
				t.Error("future login expiry must not mark a working agent as needing login")
			}
		})
	}
}

func TestLoginExpiryWarningDoesNotHideLoginPrompt(t *testing.T) {
	const warning = "⚠ Your login expires in 1 day · run /login to renew"
	for _, prompt := range []string{
		"Please run /login",
		"Not logged in",
		"not logged in",
		"Select login method",
		"Your login has expired · run /login to renew",
		"Your login expired · run /login to renew",
		"Please run /login · API Error: 401",
		warning + " · API Error: 401",
	} {
		t.Run(prompt, func(t *testing.T) {
			for _, lines := range [][]string{{prompt}, {warning, prompt}, {prompt, warning}} {
				if !paneShowsLoginPrompt(lines) {
					t.Errorf("genuine login prompt must still be detected: %q", lines)
				}
			}
		})
	}
}
