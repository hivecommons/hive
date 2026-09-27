package dashboard

// v6 guard-invariant conformance for the dashboard chat *surface* (the
// /api/chat handler and its local intents in chat_*.go). The adapter-level
// checks (ioscan on Submit, outbound scrubbing, fail-closed allowlist on the
// spine) live in pkg/dashchat/conformance_v6_test.go; this suite observes the
// real HTTP surface so that a local shortcut around the spine — a `!` command
// answered in pkg/dashboard before the shared allowlist/role floor runs — fails
// here (src/docs/v6-readiness.md §2, hivecommons/hive#8304, #7563, #9136).

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashchat"
)

// Every `!`-prefixed message is a spine command: the dashboard must hand it to
// DashboardChatSubmit (accepted + seq) rather than answer it locally, because a
// local answer skips the spine's allowlist and owner-role gates.
func TestV6ConformanceDashboardChat_BangCommandsAreNeverAnsweredLocally(t *testing.T) {
	for _, query := range []string{
		"!help",
		"!runs spec owner/repo#N",
		"!runs list",
		"!scanner kick please inspect",
		"!status",
	} {
		s, bot := chatTestServer(t)
		rec := doPost(s, "/api/chat", map[string]interface{}{"query": query})
		if rec.Code != http.StatusOK {
			t.Fatalf("v6 conformance (role floor): %q: status = %d body=%s", query, rec.Code, rec.Body.String())
		}
		body := decodeJSON(t, rec)
		if body["status"] == "ok" || body["answer"] != nil {
			t.Fatalf("v6 conformance (role floor / fail closed): %q was answered locally by the dashboard surface "+
				"instead of the shared chat spine: %v", query, body)
		}
		if body["accepted"] != true || body["seq"] == nil {
			t.Fatalf("v6 conformance (role floor): %q was not submitted to the spine: %v", query, body)
		}
		got := bot.Drain(0)
		if len(got) != 1 || got[0].Role != "user" || got[0].Text != query {
			t.Fatalf("v6 conformance (role floor): %q: spine outbox = %+v, want exactly the user echo", query, got)
		}
	}
}

// A started production bot (dashchat.NewBot + SetAgentNames + Start, as wired in
// cmd/hive) must only reply to `!help` for allowlisted authors. The spine does
// not emit a visible refusal for a denied author (pkg/chat/router.go — it logs
// and drops), so the assertion is on the absence of a reply: with mallory's
// `!help` queued before alice's, the inbox is FIFO and Listen delivers
// synchronously, so mallory's command has been fully processed before alice's
// help reply can exist. Exactly one help reply proves the gate.
func TestV6ConformanceDashboardChat_StartedBotGatesRepliesOnAllowlist(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	bot := dashchat.NewBot(dashchat.Config{AllowedUsers: []string{"alice"}}, logger)
	bot.SetAgentNames([]string{"scanner"})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := bot.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s := NewServer(0, logger)
	s.RegisterAPI(&Dependencies{
		Config:              &config.Config{},
		DashboardChatSubmit: bot.Submit,
		DashboardChatDrain:  chatDrainForTest(bot),
	})

	submit := func(user string) {
		t.Helper()
		body := strings.NewReader(`{"query":"!help"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hive-Role", config.RoleReadWrite)
		req.Header.Set("X-Hive-User", user)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: POST /api/chat = %d body=%s", user, rec.Code, rec.Body.String())
		}
		if body := decodeJSON(t, rec); body["accepted"] != true {
			t.Fatalf("v6 conformance (role floor): %s: `!help` was not handed to the spine: %v", user, body)
		}
	}
	submit("mallory")
	submit("alice")

	const helpMarker = "Hive v2 Discord Bot Commands"
	var msgs []interface{}
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec := doChatGet(s, "/api/chat/messages?since=0")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/chat/messages = %d body=%s", rec.Code, rec.Body.String())
		}
		msgs, _ = decodeJSON(t, rec)["messages"].([]interface{})
		if chatOutboxHas(msgs, "bot", helpMarker) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	var botTexts, userAuthors []string
	for _, raw := range msgs {
		m, _ := raw.(map[string]interface{})
		text, _ := m["text"].(string)
		switch m["role"] {
		case "bot":
			botTexts = append(botTexts, text)
		case "user":
			author, _ := m["author_id"].(string)
			userAuthors = append(userAuthors, author)
		}
	}
	if len(userAuthors) != 2 || userAuthors[0] != "mallory" || userAuthors[1] != "alice" {
		t.Fatalf("v6 conformance (role floor): user echoes = %v, want [mallory alice]", userAuthors)
	}
	if len(botTexts) != 2 || !strings.Contains(botTexts[0], "online") || !strings.Contains(botTexts[1], helpMarker) {
		t.Fatalf("v6 conformance (role floor / fail closed): bot messages = %q, want exactly the online banner "+
			"and ONE spine help reply (denied author mallory must get none; allowlisted alice must get one)", botTexts)
	}
}

func chatOutboxHas(msgs []interface{}, role, marker string) bool {
	for _, raw := range msgs {
		m, _ := raw.(map[string]interface{})
		text, _ := m["text"].(string)
		if m["role"] == role && strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// The surface's local intents may only read dashboard snapshots. They must not
// import guarded subsystems, and they must not match `!` commands: any string
// literal starting with `!` used as a strings.HasPrefix/Contains/EqualFold
// argument or compared in a switch/if is a local shortcut around the spine.
func TestV6ConformanceDashboardChat_SurfaceDoesNotReachStateDirectly(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package directory: %v", err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "chat_") || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		scanned++
		for _, spec := range file.Imports {
			imported := strings.Trim(spec.Path.Value, `"`)
			if why, blocked := dashboardChatSurfaceBlockedImports[imported]; blocked {
				t.Errorf("v6 conformance (Converse / role floor / mode ladder): %s imports %q — %s "+
					"(src/docs/v6-readiness.md §2, hivecommons/hive#8304)", name, imported, why)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			var lit *ast.BasicLit
			switch node := n.(type) {
			case *ast.CallExpr:
				fn, ok := node.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := fn.X.(*ast.Ident); !ok || pkg.Name != "strings" {
					return true
				}
				switch fn.Sel.Name {
				case "HasPrefix", "Contains", "EqualFold":
				default:
					return true
				}
				for _, arg := range node.Args {
					if l, ok := arg.(*ast.BasicLit); ok && chatBangLiteral(l) {
						lit = l
					}
				}
			case *ast.BinaryExpr:
				if node.Op != token.EQL {
					return true
				}
				for _, side := range []ast.Expr{node.X, node.Y} {
					if l, ok := side.(*ast.BasicLit); ok && chatBangLiteral(l) {
						lit = l
					}
				}
			default:
				return true
			}
			if lit != nil {
				t.Errorf("v6 conformance (role floor / fail closed): %s matches %s locally — `!` commands "+
					"belong to the shared chat spine, which enforces the allowlist and owner role "+
					"(src/docs/v6-readiness.md §2, hivecommons/hive#9136)", fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("scanned no chat_*.go surface sources; conformance scan would pass vacuously")
	}
}

func chatBangLiteral(lit *ast.BasicLit) bool {
	return lit.Kind == token.STRING && strings.HasPrefix(strings.Trim(lit.Value, "\"`"), "!")
}

var dashboardChatSurfaceBlockedImports = map[string]string{
	"github.com/hivecommons/hive/pkg/agent":   "direct agent control would bypass Converse and the proxy mode ladder",
	"github.com/hivecommons/hive/pkg/proxy":   "tool calls require the proxy mode ladder/capability check",
	"github.com/hivecommons/hive/pkg/effects": "mutations require an effects claim",
	"github.com/hivecommons/hive/pkg/hub":     "hub control requires the role floor",
	"os/exec":                                 "executing commands from dashboard chat bypasses every guard at once",
}
