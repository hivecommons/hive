package dashboard

import (
	"strings"
	"testing"
)

// TestLoginPageSurfacesFailures locks in the fix for the login page silently
// spinning "Waiting for authorization…" forever after a denied login. The
// device-flow poll loop must treat a denial (status "error") AND any non-ok
// HTTP body carrying an error as TERMINAL — otherwise it re-polls indefinitely
// and the user never learns the login failed.
func TestLoginPageSurfacesFailures(t *testing.T) {
	required := []string{
		"d.status==='error'",                                    // terminal on explicit error status
		"if(!r.ok||d.error){showError(",                         // terminal on any other error-bearing shape
		"if(d.status==='pending'){setTimeout(check,ms);return}", // pending re-polls, does not fall through
	}
	for _, sub := range required {
		if !strings.Contains(loginPage, sub) {
			t.Errorf("login page must contain %q so failed logins are surfaced, not silently re-polled", sub)
		}
	}
}
