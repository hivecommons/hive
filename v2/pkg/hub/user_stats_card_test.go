package hub

import (
	"strings"
	"testing"
)

// TestUserStatsCardWiredIntoNameCell asserts the admin Users name cell shows the
// engagement/lifecycle hover card and no longer NAVIGATES on click. The markup
// lives in the inline dashboardHTML JS (invisible to the Go compiler), so pin the
// properties a browser would otherwise reveal as a regression.
func TestUserStatsCardWiredIntoNameCell(t *testing.T) {
	must := []struct {
		name    string
		snippet string
	}{
		{"card function exists", "function renderUserStatsCard(u, hasPendingReq)"},
		{"name cell renders the card", "renderUserStatsCard(u, hasPendingReq)"},
		{"verdict classifier exists", "function userVerdict(u, act, hives)"},
		{"assign relocated into the card as a button", `'<button onclick="openAssignForUser(\'' + escAttr(u.github_username) + '\');return false" '`},
	}
	for _, tc := range must {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(dashboardHTML, tc.snippet) {
				t.Errorf("dashboardHTML missing %q", tc.snippet)
			}
		})
	}

	// The name is no longer an anchor that opens the assign flow on click. The old
	// markup was a `<a href="#" onclick="openAssignForUser(...`. Assert that
	// specific navigating anchor is gone (openAssignForUser now lives only on a
	// <button> inside the card).
	if strings.Contains(dashboardHTML, `<a href="#" onclick="openAssignForUser`) {
		t.Error("the user NAME must not be a click-to-assign anchor anymore; assign moved into the hover card")
	}
}

// TestUserStatsCardNoTitleOnWrapper enforces the single-hover-panel invariant for
// the new name-cell panel: the hive-access-wrap that carries the user-stats pop
// must NOT also carry a native title= (a browser tooltip stacked on the panel is
// the exact overlap bug the invariant prevents). We scan the name-cell wrapper
// block specifically.
func TestUserStatsCardNoTitleOnWrapper(t *testing.T) {
	// The name-cell wrapper opens with this exact prefix (see renderUsers nameCell).
	marker := `var nameCell = avatar +`
	idx := strings.Index(dashboardHTML, marker)
	if idx < 0 {
		t.Fatal("could not find the nameCell builder — test anchor drifted")
	}
	// Scan the wrapper opening line for a title= — it must not have one.
	seg := dashboardHTML[idx : idx+400]
	wrapOpen := `<span class="hive-access-wrap"`
	w := strings.Index(seg, wrapOpen)
	if w < 0 {
		t.Fatal("nameCell no longer uses hive-access-wrap")
	}
	// The wrapper's own opening tag ends at the first '>'. Assert no title= inside it.
	openTag := seg[w:]
	if end := strings.Index(openTag, ">"); end >= 0 {
		openTag = openTag[:end]
	}
	if strings.Contains(openTag, "title=") {
		t.Errorf("the name-cell hive-access-wrap must carry no title= (single-hover-panel invariant); got: %s", openTag)
	}
}

// TestLoginCountNotIncrementedByEnsure is the negative guarantee behind the login
// counter: ensureSaaSUser must NEVER touch LoginCount (it has many callers — my
// hives poll, admin provisioning — that are not logins). The increment lives only
// in handleOAuthCallback.
func TestLoginCountNotIncrementedByEnsure(t *testing.T) {
	useTempUserDir(t)
	// First call creates the record.
	u := ensureSaaSUser("counter-user")
	if u == nil {
		t.Fatal("ensureSaaSUser returned nil")
	}
	if u.LoginCount != 0 {
		t.Fatalf("a freshly ensured user must have LoginCount 0, got %d", u.LoginCount)
	}
	// Subsequent ensures (the my-hives poll path) must not bump it.
	for i := 0; i < 5; i++ {
		ensureSaaSUser("counter-user")
	}
	if got := loadSaaSUser("counter-user").LoginCount; got != 0 {
		t.Errorf("ensureSaaSUser must never increment LoginCount, got %d after 6 calls", got)
	}
}
