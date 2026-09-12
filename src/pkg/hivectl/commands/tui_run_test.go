package commands

// Coverage for newTUICommand's RunE body (tui.go), which
// TestTUICommandRejectsArguments deliberately stops short of: cobra's args
// gate fires before RunE, so the closure — exportCachedSessionForTUI followed
// by tui.Run() — had no test executing it.
//
// The closure is reached safely, with no TTY, the same way pkg/tui's own
// Run test does it: a dashboard answering 401 makes tui.Run's preflight
// refuse before bubbletea tries to take over the terminal. An exported
// HIVE_DASHBOARD_COOKIE keeps exportCachedSessionForTUI on its documented
// "operator exported something, do nothing" path, so the test never touches a
// real session store.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hivectl"
	tuiclient "github.com/hivecommons/hive/pkg/tui/client"
)

// TestTUICommandRunESurfacesPreflightRefusal executes the registered command
// end-to-end through root.Execute(): the RunE closure must run the export
// hand-off, call tui.Run, and return its 401 refusal to cobra rather than
// swallowing it.
func TestTUICommandRunESurfacesPreflightRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	t.Setenv(tuiclient.BaseURLEnv, server.URL)
	// A pre-exported cookie always wins: the export must leave it alone and
	// the command must proceed to tui.Run with it.
	t.Setenv(hivectl.CookieEnv, "hive_session=operator-exported")

	_, _, root := newTestRoot()
	root.SetArgs([]string{"tui"})

	err := root.Execute()
	if err == nil {
		t.Fatal("`hivectl tui` against a 401 dashboard = nil, want the preflight refusal")
	}
	for _, want := range []string{tuiclient.TokenEnv, tuiclient.CookieEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %s:\n%v", want, err)
		}
	}
	if got := os.Getenv(hivectl.CookieEnv); got != "hive_session=operator-exported" {
		t.Errorf("%s = %q after RunE, want the operator's export untouched", hivectl.CookieEnv, got)
	}
}
