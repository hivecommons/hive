package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A hub 404 on a read tool carries the build-skew notice, so a binary and hub
// that are out of step explain themselves. The diagnostic reads lost it: they
// replace the hub's error with one that hides the response body, and the
// notice was only added to the error type they replaced.
func TestDiagRead404CarriesTheSkewNotice(t *testing.T) {
	for _, tc := range []struct {
		name, stdio string
		skew        bool
	}{
		{"skew", "def5678", true},
		{"no skew", "abc1234", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setStdioShort(t, tc.stdio)
			// versionHub answers 404 for every /api/diag route, as a hub built
			// before the diagnostic reads does.
			hub := versionHub(t, `{"short":"abc1234"}`, nil)
			r := &roster{hives: []hiveConfig{{Name: "prod", Address: hub.URL, Token: "token"}}, timeout: time.Second}
			r.refreshSkew(context.Background())
			provider := readProvider{roster: r}
			for tool, args := range map[string]string{
				adminmcp.ToolAgentLogRead:   `{"agent":"scanner"}`,
				adminmcp.ToolRunDetailRead:  `{"run":"org/repo#7"}`,
				adminmcp.ToolHealthDeepRead: `{}`,
			} {
				result, err := provider.handler(tool)(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: []byte(args)}})
				if err != nil {
					t.Fatal(err)
				}
				text := mustMarshalContentText(t, result.Content[0])
				if !result.IsError || !strings.Contains(text, tool+": the hive answered HTTP 404") {
					t.Fatalf("%s = %s, want the 404 refusal", tool, text)
				}
				if got := strings.Contains(text, "hive-admin-mcp build"); got != tc.skew {
					t.Errorf("%s 404 = %s, want skew notice=%v", tool, text, tc.skew)
				}
				// The hub's response body never reaches the error.
				if strings.Contains(text, "not found") {
					t.Errorf("%s 404 carries the hub's response body: %s", tool, text)
				}
			}
		})
	}
}
