package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every credential in this file is synthetic: each token string carries
// bridgeTestTokenSentinel (see usersession_login_rotating_test.go).

// A session's copy of a Codex or OpenCode login keeps every field but the
// refresh token, whose key stays (both CLIs require it) with nothing in it.
func TestSessionRotatingLoginCopy_EmptiesEveryRefreshToken(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		copyFn func([]byte) ([]byte, bool)
		shared []byte
		wantOK bool
		// check inspects the decoded copy.
		check func(t *testing.T, top map[string]any)
	}{
		{
			name: "codex ChatGPT login", copyFn: sessionCodexLogin,
			shared: []byte(strings.Replace(string(codexTestLogin("shared", now)), `"OPENAI_API_KEY": null`, `"OPENAI_API_KEY": null, "future_field": 7`, 1)),
			wantOK: true,
			check: func(t *testing.T, top map[string]any) {
				tokens, _ := top["tokens"].(map[string]any)
				if tokens["refresh_token"] != "" || tokens["access_token"] != testAccess("shared") ||
					tokens["id_token"] != "id-shared-"+bridgeTestTokenSentinel || tokens["account_id"] != "account-test" {
					t.Errorf("tokens = %v", tokens)
				}
				if top["auth_mode"] != "chatgpt" || top["last_refresh"] == nil || top["future_field"] != float64(7) {
					t.Error("the copy dropped a field other than the refresh token")
				}
			},
		},
		{
			name: "codex API-key login", copyFn: sessionCodexLogin,
			shared: []byte(`{"OPENAI_API_KEY":"sk-` + bridgeTestTokenSentinel + `"}`),
			wantOK: true,
			check: func(t *testing.T, top map[string]any) {
				if top["OPENAI_API_KEY"] != "sk-"+bridgeTestTokenSentinel {
					t.Error("an API-key login was changed")
				}
			},
		},
		{name: "codex not JSON", copyFn: sessionCodexLogin, shared: []byte("not a login")},
		{name: "codex tokens not an object", copyFn: sessionCodexLogin, shared: []byte(`{"tokens":"x"}`)},
		{
			name: "opencode oauth and api entries", copyFn: sessionOpenCodeLogin,
			shared: []byte(fmt.Sprintf(`{"anthropic":%s,"openai":%s,"openrouter":{"type":"api","key":"key-fleet"},"odd":[1]}`,
				openCodeTestEntry("a", now), openCodeTestEntry("o", now))),
			wantOK: true,
			check: func(t *testing.T, top map[string]any) {
				for _, p := range []string{"anthropic", "openai"} {
					e, _ := top[p].(map[string]any)
					if e["refresh"] != "" || e["type"] != "oauth" || e["access"] == "" || e["expires"] == nil {
						t.Errorf("%s entry = %v", p, e)
					}
				}
				if e, _ := top["openrouter"].(map[string]any); e["key"] != "key-fleet" {
					t.Error("an API-key entry was changed")
				}
				if top["odd"] == nil {
					t.Error("an entry hive does not understand was dropped")
				}
			},
		},
		{name: "opencode not JSON", copyFn: sessionOpenCodeLogin, shared: []byte("[]")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, ok := c.copyFn(c.shared)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if bytes.Contains(out, []byte(`"ref-`)) {
				t.Error("the copy carries a refresh token")
			}
			var top map[string]any
			if err := json.Unmarshal(out, &top); err != nil {
				t.Fatal(err)
			}
			c.check(t, top)
		})
	}
}

// The CLI in a session cannot write a refresh that diverges from the shared
// login, whatever its copy goes through. Each case sets up the session's file
// after a start and runs one manager sweep; the shared file must change only
// by an adoption of a strictly newer full login, and the session must end on
// a refresh-less copy unless the file is a login of the session's own.
func TestSessionRotatingLogin_SessionCannotWriteADivergingRefresh(t *testing.T) {
	type setup struct {
		// session is what the session's file holds before the sweep, nil to
		// keep the copy the start made; sessionMod is its modification time.
		session    func(v rotatingTestVendor, now time.Time) []byte
		sessionMod time.Duration
		// agent, when set, is an agent refresh of the shared file before the
		// sweep, written at agentMod.
		agent    bool
		agentMod time.Duration
	}
	cases := []struct {
		name string
		setup
		wantAdopted    bool
		wantSharedTag  string // the login the shared file must end on
		wantSessionTag string // the access token of the session's refresh-less copy; "" = left alone
	}{
		{
			name:          "the CLI rewrites its refresh-less copy (a failed refresh)",
			setup:         setup{session: refreshlessRewrite, sessionMod: time.Minute},
			wantSharedTag: "shared",
		},
		{
			name:           "an agent refreshed the shared login",
			setup:          setup{agent: true, agentMod: time.Hour},
			wantSharedTag:  "agent",
			wantSessionTag: "agent",
		},
		{
			name: "a full copy from an earlier release",
			setup: setup{session: func(v rotatingTestVendor, now time.Time) []byte {
				return v.login("shared", now.Add(-2*time.Hour))
			}, sessionMod: time.Minute},
			wantSharedTag:  "shared",
			wantSessionTag: "shared",
		},
		{
			name: "a full copy refreshed in the session",
			setup: setup{session: func(v rotatingTestVendor, now time.Time) []byte {
				return v.login("rotated", now.Add(8*time.Hour))
			}, sessionMod: time.Minute},
			wantAdopted:    true,
			wantSharedTag:  "rotated",
			wantSessionTag: "rotated",
		},
		{
			name: "a full copy refreshed in the session, but an agent refreshed later",
			setup: setup{session: func(v rotatingTestVendor, now time.Time) []byte {
				return v.login("rotated", now.Add(8*time.Hour))
			}, sessionMod: time.Minute, agent: true, agentMod: time.Hour},
			wantSharedTag: "agent",
		},
	}
	for vendor, v := range rotatingTestVendors() {
		for _, c := range cases {
			t.Run(vendor+"/"+c.name, func(t *testing.T) {
				f := newSessionLoginFixture(t)
				now := time.Now()
				target := filepath.Join(sharedAgentHome, v.rel)
				writeLoginAt(t, target, v.login("shared", now.Add(-2*time.Hour)), now.Add(-time.Hour))
				f.start(t)
				path := filepath.Join(userSessionHome(testSessionID), v.rel)
				assertRotatingRefreshless(t, v.rel, path, testAccess("shared"))
				var before []byte
				if c.session != nil {
					before = c.session(v, now)
					writeLoginAt(t, path, before, now.Add(c.sessionMod))
				}
				if c.agent {
					writeLoginAt(t, target, v.login("agent", now.Add(16*time.Hour)), now.Add(c.agentMod))
				}

				f.m.syncUserSessionClaudeLogins(sessionLoginTriggerSignIn)

				if got := len(f.adoptionsOf(vendor)); (got == 1) != c.wantAdopted || got > 1 {
					t.Errorf("%d adoptions, want adopted=%v", got, c.wantAdopted)
				}
				access, _, _ := rotatingAccessAndRefresh(t, v.rel, mustRead(t, target))
				if access != testAccess(c.wantSharedTag) {
					t.Errorf("the shared login is not the %q login", c.wantSharedTag)
				}
				if c.wantSessionTag != "" {
					assertRotatingRefreshless(t, v.rel, path, testAccess(c.wantSessionTag))
				} else if before != nil && !bytes.Equal(mustRead(t, path), before) {
					t.Error("a session file hive must leave alone was rewritten")
				}
				f.assertNoTokenMaterial(t)
			})
		}
	}
}

// refreshlessRewrite is the session's copy as a CLI might rewrite it after a
// refresh it could not make: still refresh-less, with later freshness fields.
func refreshlessRewrite(v rotatingTestVendor, now time.Time) []byte {
	l, _ := rotatingSessionLoginFor(v.rel)
	out, _ := l.sessionCopy(v.login("shared", now.Add(24*time.Hour)))
	return out
}

// safeSessionPriv is a goroutine-safe stand-in for su-exec that only knows the
// login copy script, writing the copy as the script does: a temporary renamed
// over the destination, mode 0400.
func safeSessionPriv(_ context.Context, _ string, stdin io.Reader, argv ...string) ([]byte, error) {
	if len(argv) < 5 || argv[0] != "sh" || argv[2] != userSessionCopyLoginScript {
		return nil, fmt.Errorf("unexpected helper %q", argv)
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return nil, err
	}
	dest := argv[4]
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".hive-new-*")
	if err != nil {
		return nil, err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmp.Name(), 0o400); err != nil {
		return nil, err
	}
	return nil, os.Rename(tmp.Name(), dest)
}

// Agents refresh the shared login (the single owner) while the hive's sweep
// and session starts check the session's copy concurrently. The shared file
// must only ever hold what an agent wrote, never be replaced by the hive, and
// the session must end on a refresh-less copy of the last agent refresh.
func TestSessionRotatingLogin_ConcurrentAgentRefreshStaysTheSingleSource(t *testing.T) {
	const (
		agentRefreshes = 20
		checkers       = 4
	)
	for vendor, v := range rotatingTestVendors() {
		t.Run(vendor, func(t *testing.T) {
			f := newSessionLoginFixture(t)
			now := time.Now()
			target := filepath.Join(sharedAgentHome, v.rel)
			writeLoginAt(t, target, v.login("shared", now.Add(-2*time.Hour)), now.Add(-time.Hour))
			f.start(t)
			path := filepath.Join(userSessionHome(testSessionID), v.rel)
			useSessionLoginPriv(t, safeSessionPriv)
			ops := sessionLoginOps{priv: safeSessionPriv, statUID: func(string) (int, error) { return testSessionUID, nil }, now: time.Now}
			l, _ := rotatingSessionLoginFor(v.rel)

			agentWrites := map[string]bool{}
			for i := 1; i <= agentRefreshes; i++ {
				agentWrites[string(v.login(fmt.Sprintf("agent%d", i), now.Add(time.Duration(i)*time.Hour)))] = true
			}
			var wg sync.WaitGroup
			done := make(chan struct{})
			errs := make(chan error, agentRefreshes+checkers)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer close(done)
				for i := 1; i <= agentRefreshes; i++ {
					// An agent's CLI writes the shared file the way the CLIs
					// do: a temporary renamed over it, a later mtime each time.
					data := v.login(fmt.Sprintf("agent%d", i), now.Add(time.Duration(i)*time.Hour))
					mod := now.Add(time.Duration(i) * time.Hour)
					tmp := target + ".agent-tmp"
					if err := os.WriteFile(tmp, data, 0o600); err != nil {
						errs <- err
						return
					}
					if err := os.Chtimes(tmp, mod, mod); err != nil {
						errs <- err
						return
					}
					if err := os.Rename(tmp, target); err != nil {
						errs <- err
						return
					}
				}
			}()
			for i := 0; i < checkers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					for {
						select {
						case <-done:
							return
						default:
						}
						if i%2 == 0 {
							f.m.syncUserSessionClaudeLogins(sessionLoginTriggerSignIn)
						} else {
							_, _ = ops.checkRotatingLogin(testSessionID, l, sessionLoginModeStart)
						}
						// The shared file only ever holds an agent's write.
						if data, err := os.ReadFile(target); err == nil && !agentWrites[string(data)] && !bytes.Equal(data, v.login("shared", now.Add(-2*time.Hour))) {
							errs <- fmt.Errorf("the shared login holds something no agent wrote")
							return
						}
					}
				}(i)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
			f.m.syncUserSessionClaudeLogins(sessionLoginTriggerSignIn)

			last := v.login(fmt.Sprintf("agent%d", agentRefreshes), now.Add(agentRefreshes*time.Hour))
			if !bytes.Equal(mustRead(t, target), last) {
				t.Error("the shared login is not the last agent refresh")
			}
			if f.adoptions() != 0 || len(staleFilesBeside(t, target)) != 0 {
				t.Error("the hive replaced the shared login although no session held a newer one")
			}
			assertRotatingRefreshless(t, v.rel, path, testAccess(fmt.Sprintf("agent%d", agentRefreshes)))
			f.assertNoTokenMaterial(t)
		})
	}
}
