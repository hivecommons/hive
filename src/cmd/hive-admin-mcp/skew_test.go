package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// versionHub serves body on /api/version, 200 on /api/status/summary and 404
// elsewhere, recording the User-Agent of every request.
func versionHub(t *testing.T, body string, agents *[]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if agents != nil {
			*agents = append(*agents, r.Header.Get("User-Agent"))
		}
		switch r.URL.Path {
		case "/api/version":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		case "/api/status/summary":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func setStdioShort(t *testing.T, short string) {
	t.Helper()
	orig := gitShort
	t.Cleanup(func() { gitShort = orig })
	gitShort = short
}

func TestSkewNoticeOnlyWhenBothBuildsKnownAndDiffer(t *testing.T) {
	for _, tc := range []struct {
		stdio, hub string
		want       bool
	}{
		{"def5678", "abc1234", true},
		{"abc1234", "abc1234", false},
		{"abc1234", "ABC1234def", false},
		{"unknown", "abc1234", false},
		{"def5678", "unknown", false},
		{"def5678", "", false},
	} {
		notice := skewNotice(tc.stdio, tc.hub, "prod")
		if (notice != "") != tc.want {
			t.Fatalf("skewNotice(%q, %q) = %q, want notice=%v", tc.stdio, tc.hub, notice, tc.want)
		}
		if tc.want && (!strings.Contains(notice, tc.stdio) || !strings.Contains(notice, tc.hub) || !strings.Contains(notice, "src/docs/admin-mcp.md") || strings.Contains(notice, "\n")) {
			t.Fatalf("notice = %q, want one line naming both SHAs and the update steps", notice)
		}
	}
}

func TestSkewNoticeInInitializeAndToolErrors(t *testing.T) {
	for _, tc := range []struct {
		name, stdio, version string
		skew                 bool
	}{
		{"skew", "def5678", `{"short":"abc1234"}`, true},
		{"skew via currentCommit", "def5678", `{"currentCommit":"abc1234"}`, true},
		{"no skew", "abc1234", `{"short":"abc1234"}`, false},
		{"unknown hub sha", "def5678", `{"short":"unknown"}`, false},
		{"missing hub sha", "def5678", `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setStdioShort(t, tc.stdio)
			var agents []string
			hub := versionHub(t, tc.version, &agents)
			r := &roster{hives: []hiveConfig{{Name: "prod", Address: hub.URL, Token: "token"}}, timeout: time.Second}
			r.refreshSkew(context.Background())
			provider := readProvider{roster: r}

			ctx := context.Background()
			ct, st := mcp.NewInMemoryTransports()
			ss, err := newServer(provider).Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()

			instructions := cs.InitializeResult().Instructions
			if tc.skew {
				if !strings.Contains(instructions, tc.stdio) || !strings.Contains(instructions, "abc1234") || !strings.Contains(instructions, "src/docs/admin-mcp.md") {
					t.Fatalf("instructions = %q, want both SHAs and the update pointer", instructions)
				}
			} else if instructions != "" {
				t.Fatalf("instructions = %q, want none", instructions)
			}
			notice := r.skewNotice()
			if tc.skew != (notice != "") || (tc.skew && instructions != notice) {
				t.Fatalf("notice = %q, instructions = %q", notice, instructions)
			}

			_, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "no_such_tool"})
			if err == nil || !strings.Contains(err.Error(), `unknown tool "no_such_tool"`) {
				t.Fatalf("unknown tool err = %v", err)
			}
			if got := strings.Contains(err.Error(), "hive-admin-mcp build"); got != tc.skew {
				t.Fatalf("unknown tool err = %v, want notice=%v", err, tc.skew)
			}

			unsupported := `{"error":"unsupported admin MCP read tool \"no_such_read\""}`
			result, err := provider.handler("no_such_read")(ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: []byte(`{}`)}})
			if err != nil {
				t.Fatal(err)
			}
			text := mustMarshalContentText(t, result.Content[0])
			if !result.IsError {
				t.Fatalf("unsupported result is not an error: %s", text)
			}
			if tc.skew {
				if !strings.Contains(text, `unsupported admin MCP read tool \"no_such_read\"; hive-admin-mcp build `+tc.stdio) {
					t.Fatalf("unsupported err = %s, want the skew notice", text)
				}
			} else if text != unsupported {
				t.Fatalf("unsupported err = %s, want unchanged %s", text, unsupported)
			}

			// /api/status/summary is served, so hive_status must succeed without any notice.
			result, err = provider.handler(adminmcp.ToolHiveStatus)(ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: []byte(`{}`)}})
			if err != nil {
				t.Fatal(err)
			}
			text = mustMarshalContentText(t, result.Content[0])
			if result.IsError || strings.Contains(text, "hive-admin-mcp build") {
				t.Fatalf("hive_status = %s, want success without notice", text)
			}

			// /api/status is not served: a hub 404 carries the notice only under skew.
			result, err = provider.handler(adminmcp.ToolFleetStatus)(ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: []byte(`{}`)}})
			if err != nil {
				t.Fatal(err)
			}
			text = mustMarshalContentText(t, result.Content[0])
			if !strings.Contains(text, "HTTP 404") {
				t.Fatalf("hub 404 err = %s", text)
			}
			if got := strings.Contains(text, "hive-admin-mcp build"); got != tc.skew {
				t.Fatalf("hub 404 err = %s, want notice=%v", text, tc.skew)
			}

			wantUA := "hive-admin-mcp/" + adminmcp.ServerVersion + "+" + tc.stdio
			if len(agents) == 0 {
				t.Fatal("hub saw no requests")
			}
			for _, ua := range agents {
				if ua != wantUA {
					t.Fatalf("User-Agent = %q, want %q", ua, wantUA)
				}
			}
		})
	}
}

func TestSkewNoticeLeavesOtherErrorsUnchanged(t *testing.T) {
	setStdioShort(t, "def5678")
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"short":"abc1234"}`))
			return
		}
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	}))
	defer hub.Close()
	r := &roster{hives: []hiveConfig{{Name: "prod", Address: hub.URL, Token: "token"}}, timeout: time.Second}
	r.refreshSkew(context.Background())
	if r.skewNotice() == "" {
		t.Fatal("expected skew")
	}
	result, err := (readProvider{roster: r}).handler(adminmcp.ToolHiveStatus)(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: []byte(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if text := mustMarshalContentText(t, result.Content[0]); strings.Contains(text, "hive-admin-mcp build") {
		t.Fatalf("HTTP 500 err = %s, want no skew notice", text)
	}
}

func TestSelectHiveRefreshesSkewNotice(t *testing.T) {
	setStdioShort(t, "abc1234")
	same := versionHub(t, `{"short":"abc1234"}`, nil)
	ahead := versionHub(t, `{"short":"0643a10"}`, nil)
	r := &roster{hives: []hiveConfig{{Name: "same", Address: same.URL, Token: "token"}, {Name: "ahead", Address: ahead.URL, Token: "token"}}, timeout: time.Second}
	r.refreshSkew(context.Background())
	if notice := r.skewNotice(); notice != "" {
		t.Fatalf("notice at start = %q, want none", notice)
	}
	if _, err := r.selectHive(context.Background(), "ahead"); err != nil {
		t.Fatal(err)
	}
	if notice := r.skewNotice(); !strings.Contains(notice, "abc1234") || !strings.Contains(notice, "0643a10") || !strings.Contains(notice, `"ahead"`) {
		t.Fatalf("notice after select_hive = %q", notice)
	}
	if _, err := r.selectHive(context.Background(), "same"); err != nil {
		t.Fatal(err)
	}
	if notice := r.skewNotice(); notice != "" {
		t.Fatalf("notice after switching back = %q, want none", notice)
	}
}

func TestRefreshSkewUnreachableHubLeavesNoNotice(t *testing.T) {
	setStdioShort(t, "def5678")
	r := &roster{hives: []hiveConfig{{Name: "down", Address: "http://127.0.0.1:1", Token: "token"}}, timeout: time.Second, notice: "stale"}
	r.refreshSkew(context.Background())
	if notice := r.skewNotice(); notice != "" {
		t.Fatalf("notice = %q, want none", notice)
	}
}
