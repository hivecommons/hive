package dashboard

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
)

// Over the HTTP endpoint a "." or ".." name reached the dashboard router,
// which answered with a redirect to another route. The redirect is not
// followed in-process, so the caller got "Hive API returned HTTP 307" with a
// link to the other route. The name is now refused before the router sees it.
func TestDashboardAdminMCPRefusesDotNamesBeforeRouting(t *testing.T) {
	routed := 0
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		routed++
		_, _ = w.Write([]byte(`{}`))
	})
	p := dashboardAdminMCPProvider{server: s, role: "owner", ownerVerified: true}
	for tool, args := range map[string]map[string]any{
		adminmcp.ToolAgentNudgeStatus: {"agent": ".."},
		adminmcp.ToolSettingsRead:     {"section": "agent", "agent": ".."},
		adminmcp.ToolAgentLogRead:     {"agent": "."},
		adminmcp.ToolAgentKicksList:   {"agent": ".."},
		adminmcp.ToolRunLogRead:       {"run": ".."},
	} {
		_, err := p.Read(context.Background(), tool, args)
		if !errors.Is(err, adminmcp.ErrRequestPath) {
			t.Errorf("%s %v = %v, want ErrRequestPath", tool, args, err)
		}
	}
	if routed != 0 {
		t.Fatalf("%d refused read(s) reached the dashboard router", routed)
	}
}
