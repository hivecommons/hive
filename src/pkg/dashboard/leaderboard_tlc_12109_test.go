package dashboard

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLeaderboardTabRendersTLCMarkup pins the #12109 leaderboard fixes that
// live in the landing page template: the battle log links public refs, the
// Hive of the Week card has an empty state instead of an unconditional
// player, team leagues only render non-empty groups, and the standing strip
// explains the contributor-relay scope.
func TestLeaderboardTabRendersTLCMarkup(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "%!") {
		t.Fatalf("landing page has a format error (%%! present)")
	}
	for _, want := range []string{
		`.battle-log-line.hive-row-alt{`,
		`i%2===1?' hive-row-alt':''`,
		`.hotw-empty{`,
		`if(d.video_url){`,
		`No weekly render yet.`,
		`.filter(function(g){return g[1].length;})`,
		`not on the board yet. Standings count tasks shipped through the contributor relay`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("landing page missing %q", want)
		}
	}
	for _, gone := range []string{
		`/assets/hive-of-the-week/latest.mp4`,
		`ship a task to enter the standings`,
	} {
		if strings.Contains(body, gone) {
			t.Errorf("landing page still contains %q", gone)
		}
	}
}
