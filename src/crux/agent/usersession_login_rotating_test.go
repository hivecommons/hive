package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
)

// Every credential in this file is synthetic: each token string carries
// bridgeTestTokenSentinel, so a test can prove that no log line, audit field
// or error ever holds token material.

// codexTestLogin is a synthetic Codex ChatGPT login file.
func codexTestLogin(tag string, lastRefresh time.Time) []byte {
	return []byte(fmt.Sprintf(`{
  "OPENAI_API_KEY": null,
  "auth_mode": "chatgpt",
  "tokens": {
    "id_token": "id-%[1]s-%[2]s",
    "access_token": "acc-%[1]s-%[2]s",
    "refresh_token": "ref-%[1]s-%[2]s",
    "account_id": "account-test"
  },
  "last_refresh": %[3]q
}
`, tag, bridgeTestTokenSentinel, lastRefresh.UTC().Format(time.RFC3339Nano)))
}

// openCodeTestEntry is a synthetic OpenCode "oauth" provider entry.
func openCodeTestEntry(tag string, expires time.Time) string {
	return fmt.Sprintf(`{"type":"oauth","refresh":"ref-%[1]s-%[2]s","access":"acc-%[1]s-%[2]s","expires":%[3]d}`,
		tag, bridgeTestTokenSentinel, expires.UnixMilli())
}

// openCodeTestLogin is a synthetic OpenCode auth.json: one rotating OAuth
// provider and one API-key provider.
func openCodeTestLogin(tag string, expires time.Time) []byte {
	return []byte(fmt.Sprintf(`{"anthropic":%s,"openrouter":{"type":"api","key":"key-%s"}}`,
		openCodeTestEntry(tag, expires), bridgeTestTokenSentinel))
}

// codexRefresh / openCodeRefresh return the refresh token a test file holds.
func codexRefresh(t *testing.T, path string) string {
	t.Helper()
	_, refresh, _, ok := parseCodexLogin(mustRead(t, path))
	if !ok {
		t.Fatalf("%s is not a Codex ChatGPT login", path)
	}
	return refresh
}

func openCodeEntries(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(mustRead(t, path), &top); err != nil {
		t.Fatalf("%s is not an OpenCode auth file: %v", path, err)
	}
	return top
}

func openCodeRefresh(t *testing.T, path, provider string) string {
	t.Helper()
	e, ok := openCodeOAuthEntry(openCodeEntries(t, path)[provider])
	if !ok {
		t.Fatalf("%s has no OAuth entry for %s", path, provider)
	}
	return e.Refresh
}

// rotatingAccessAndRefresh returns the access token and the refresh token of
// the vendor's login in data (OpenCode: the "anthropic" entry) and whether
// the refresh key is present at all. It accepts a refresh-less copy.
func rotatingAccessAndRefresh(t *testing.T, rel string, data []byte) (access, refresh string, hasKey bool) {
	t.Helper()
	switch rel {
	case userSessionCodexLoginRel:
		var f struct {
			Tokens map[string]*string `json:"tokens"`
		}
		if err := json.Unmarshal(data, &f); err != nil || f.Tokens == nil {
			t.Fatalf("not a Codex ChatGPT login: %v", err)
		}
		if a := f.Tokens["access_token"]; a != nil {
			access = *a
		}
		r, ok := f.Tokens["refresh_token"]
		if ok && r != nil {
			refresh = *r
		}
		return access, refresh, ok
	case userSessionOpenCodeLoginRel:
		var top map[string]map[string]any
		if err := json.Unmarshal(data, &top); err != nil || top["anthropic"] == nil {
			t.Fatalf("not an OpenCode login with an anthropic entry: %v", err)
		}
		e := top["anthropic"]
		access, _ = e["access"].(string)
		r, ok := e["refresh"]
		refresh, _ = r.(string)
		return access, refresh, ok
	}
	t.Fatalf("no rotating login at %s", rel)
	return "", "", false
}

// assertRotatingRefreshless fails unless the session's copy at path is a
// refresh-less copy holding wantAccess: the refresh key kept (both CLIs
// require it) with nothing in it, and no refresh token anywhere in the file.
func assertRotatingRefreshless(t *testing.T, rel, path, wantAccess string) {
	t.Helper()
	raw := mustRead(t, path)
	access, refresh, hasKey := rotatingAccessAndRefresh(t, rel, raw)
	l, _ := rotatingSessionLoginFor(rel)
	if refresh != "" || !hasKey || len(l.refreshTokens(raw)) != 0 || bytes.Contains(raw, []byte(`"ref-`)) {
		t.Error("the session's copy carries a refresh token (or lost the required key): the session could rotate the fleet's login")
	}
	if access != wantAccess {
		t.Error("the session's copy does not hold the expected access token")
	}
	if st, err := os.Lstat(path); err != nil || st.Mode().Perm() != 0o400 {
		t.Errorf("the session's copy is not read-only: %v", err)
	}
}

func testAccess(tag string) string { return "acc-" + tag + "-" + bridgeTestTokenSentinel }

// rotatingTestVendor drives one vendor through the shared scenarios.
type rotatingTestVendor struct {
	rel string
	// login builds a synthetic login whose freshness field is fresh.
	login func(tag string, fresh time.Time) []byte
	// refresh reads the refresh token of the vendor's login at path.
	refresh func(t *testing.T, path string) string
}

func rotatingTestVendors() map[string]rotatingTestVendor {
	return map[string]rotatingTestVendor{
		"codex": {rel: userSessionCodexLoginRel, login: codexTestLogin, refresh: codexRefresh},
		"opencode": {rel: userSessionOpenCodeLoginRel, login: openCodeTestLogin, refresh: func(t *testing.T, path string) string {
			return openCodeRefresh(t, path, "anthropic")
		}},
	}
}

func (f *sessionLoginFixture) adoptionsOf(vendor string) []recordedAudit {
	f.sink.mu.Lock()
	defer f.sink.mu.Unlock()
	var out []recordedAudit
	for _, e := range f.sink.entries {
		if e.Action == AuditUserSessionLoginAdopted && e.Fields["vendor"] == vendor {
			out = append(out, e)
		}
	}
	return out
}

func TestAdoptCodexLogin(t *testing.T) {
	now := time.Now()
	old, recent := now.Add(-time.Hour), now
	shared := codexTestLogin("shared", now.Add(-2*time.Hour))
	rotated := codexTestLogin("rotated", now)
	apiKey := []byte(`{"OPENAI_API_KEY":"sk-` + bridgeTestTokenSentinel + `"}`)
	cases := []struct {
		name               string
		sess, shared       []byte
		sessMod, sharedMod time.Time
		want               bool
	}{
		{"refreshed in the session", rotated, shared, recent, old, true},
		{"written before the shared file", rotated, shared, old, recent, false},
		{"same refresh token (an unchanged copy)", shared, shared, recent, old, false},
		{"earlier last refresh", codexTestLogin("older", now.Add(-3*time.Hour)), shared, recent, old, false},
		{"no last refresh in the session", []byte(`{"tokens":{"access_token":"a","refresh_token":"r2"}}`), shared, recent, old, false},
		{"no access token", []byte(`{"tokens":{"access_token":"","refresh_token":"r2"},"last_refresh":"` + now.UTC().Format(time.RFC3339) + `"}`), shared, recent, old, false},
		{"session holds an API key only", apiKey, shared, recent, old, false},
		{"shared is an API-key login", rotated, apiKey, recent, old, false},
		{"session is not JSON", []byte("not a login"), shared, recent, old, false},
		{"shared login without last refresh", rotated, []byte(`{"tokens":{"access_token":"a","refresh_token":"r"}}`), recent, old, true},
	}
	for _, c := range cases {
		got := adoptCodexLogin(c.sess, c.shared, c.sessMod, c.sharedMod)
		if (got != nil) != c.want {
			t.Errorf("%s: adopted = %v, want %v", c.name, got != nil, c.want)
		}
	}

	// An adoption takes the session's ChatGPT login and keeps everything
	// else the shared file holds.
	sharedWithKey := []byte(strings.Replace(string(shared), `"OPENAI_API_KEY": null`, `"OPENAI_API_KEY": "sk-fleet", "future_field": 7`, 1))
	out := adoptCodexLogin(rotated, sharedWithKey, recent, old)
	_, refresh, last, ok := parseCodexLogin(out)
	if !ok || refresh != "ref-rotated-"+bridgeTestTokenSentinel || !last.Equal(now.UTC()) {
		t.Errorf("the adopted file does not hold the session's login")
	}
	var top map[string]any
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	if top["OPENAI_API_KEY"] != "sk-fleet" || top["future_field"] != float64(7) {
		t.Errorf("the adoption dropped the shared file's other fields: %v", top["OPENAI_API_KEY"])
	}
}

func TestAdoptOpenCodeLogin(t *testing.T) {
	now := time.Now()
	old, recent := now.Add(-time.Hour), now
	shared := []byte(fmt.Sprintf(`{"anthropic":%s,"github-copilot":%s,"openai":%s,"openrouter":{"type":"api","key":"key-fleet"}}`,
		openCodeTestEntry("a-shared", now.Add(time.Minute)),
		openCodeTestEntry("g-shared", now.Add(time.Hour)),
		openCodeTestEntry("o-shared", now.Add(time.Hour))))
	sess := []byte(fmt.Sprintf(`{"anthropic":%s,"github-copilot":%s,"openai":%s,"openrouter":{"type":"api","key":"key-session"},"google":%s}`,
		openCodeTestEntry("a-rotated", now.Add(8*time.Hour)),   // newer: adopted
		openCodeTestEntry("g-older", now.Add(time.Minute)),     // older: kept
		openCodeTestEntry("o-shared", now.Add(2*time.Hour)),    // same refresh token: kept
		openCodeTestEntry("google-own", now.Add(8*time.Hour)))) // not in the shared file: not added

	out := adoptOpenCodeLogin(sess, shared, recent, old)
	if out == nil {
		t.Fatal("a newer provider entry was not adopted")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	refresh := func(p string) string {
		e, _ := openCodeOAuthEntry(top[p])
		return e.Refresh
	}
	if refresh("anthropic") != "ref-a-rotated-"+bridgeTestTokenSentinel {
		t.Error("the refreshed entry was not adopted")
	}
	if refresh("github-copilot") != "ref-g-shared-"+bridgeTestTokenSentinel || refresh("openai") != "ref-o-shared-"+bridgeTestTokenSentinel {
		t.Error("an entry that is not newer was replaced")
	}
	if !bytes.Contains(top["openrouter"], []byte("key-fleet")) {
		t.Error("the shared API-key entry was replaced")
	}
	if _, ok := top["google"]; ok {
		t.Error("an entry the shared file did not hold was added")
	}

	if adoptOpenCodeLogin(sess, shared, old, recent) != nil {
		t.Error("a session file written before the shared one was adopted")
	}
	if adoptOpenCodeLogin(shared, shared, recent, old) != nil {
		t.Error("an unchanged copy was adopted")
	}
	if adoptOpenCodeLogin([]byte("not json"), shared, recent, old) != nil {
		t.Error("a file that is not JSON was adopted")
	}
}

// The failure the issue describes, for each rotating vendor: the CLI in a
// session refreshed its copy, which rotated the refresh token. The newer
// login must reach the shared file, once, with an audit entry and the old
// shared file kept beside it.
func TestSessionRefresh_CodexAndOpenCodeNeverLeaveTheSharedLoginStale(t *testing.T) {
	for vendor, v := range rotatingTestVendors() {
		t.Run(vendor, func(t *testing.T) {
			f := newSessionLoginFixture(t)
			now := time.Now()
			target := filepath.Join(sharedAgentHome, v.rel)
			writeLoginAt(t, target, v.login("shared", now.Add(-2*time.Hour)), now.Add(-time.Hour))
			f.start(t)
			path := filepath.Join(userSessionHome(testSessionID), v.rel)
			assertRotatingRefreshless(t, v.rel, path, testAccess("shared"))
			if len(f.adoptionsOf(vendor)) != 0 {
				t.Fatal("a plain start adopted a login")
			}

			writeLoginAt(t, path, v.login("rotated", now.Add(8*time.Hour)), now)
			f.m.syncUserSessionClaudeLogins(sessionLoginTriggerWatchdog)

			if got := v.refresh(t, target); got != "ref-rotated-"+bridgeTestTokenSentinel {
				t.Error("the shared login does not hold the session's refreshed login")
			}
			if n := len(staleFilesBeside(t, target)); n != 1 {
				t.Errorf("replaced shared login preserved %d times, want 1", n)
			}
			recs := f.adoptionsOf(vendor)
			if len(recs) != 1 {
				t.Fatalf("%d adoptions audited, want 1", len(recs))
			}
			rec := recs[0]
			if rec.Fields["session"] != testSessionID || rec.Fields["trigger"] != sessionLoginTriggerWatchdog ||
				rec.Fields["login"] != v.rel || rec.Fields["outcome"] != "success" {
				t.Errorf("audit = %+v", rec)
			}

			// Converged: further checks, forced or not, change nothing.
			before := mustRead(t, target)
			for i := 0; i < 3; i++ {
				f.m.syncUserSessionClaudeLogins(sessionLoginTriggerSignIn)
				f.r.copyLoginFiles(testSessionID, testSessionUID)
			}
			if !bytes.Equal(mustRead(t, target), before) || len(f.adoptionsOf(vendor)) != 1 {
				t.Error("the login did not settle after one adoption")
			}
			// The session keeps using the login it refreshed, now as a
			// refresh-less copy of the shared file.
			assertRotatingRefreshless(t, v.rel, path, testAccess("rotated"))
			f.assertNoTokenMaterial(t)
		})
	}
}

// The shared login stays the single owner of the refresh token (#18740): an
// adoption moves the session's login INTO the shared file and puts the
// session back on a refresh-less copy, so after it exactly one file the hive
// manages holds the refreshed token, and a later agent refresh of the shared
// file reaches the session and is not undone by it.
func TestSessionRefresh_AdoptionPutsTheSessionBackOnARefreshlessCopy(t *testing.T) {
	for vendor, v := range rotatingTestVendors() {
		t.Run(vendor, func(t *testing.T) {
			f := newSessionLoginFixture(t)
			now := time.Now()
			target := filepath.Join(sharedAgentHome, v.rel)
			writeLoginAt(t, target, v.login("shared", now.Add(-2*time.Hour)), now.Add(-time.Hour))
			f.start(t)
			path := filepath.Join(userSessionHome(testSessionID), v.rel)
			writeLoginAt(t, path, v.login("rotated", now.Add(8*time.Hour)), now)

			f.m.syncUserSessionClaudeLogins(sessionLoginTriggerWatchdog)
			if len(f.adoptionsOf(vendor)) != 1 {
				t.Fatal("the session's refreshed login was not adopted")
			}
			assertRotatingRefreshless(t, v.rel, path, testAccess("rotated"))

			// An agent refreshes the shared login afterwards: the session
			// must follow it and must not take it back.
			agent := v.login("agent", now.Add(16*time.Hour))
			writeLoginAt(t, target, agent, now.Add(time.Hour))
			f.m.syncUserSessionClaudeLogins(sessionLoginTriggerSignIn)
			f.r.copyLoginFiles(testSessionID, testSessionUID)
			if !bytes.Equal(mustRead(t, target), agent) || len(f.adoptionsOf(vendor)) != 1 {
				t.Error("the session's older copy displaced the agents' newer shared login")
			}
			assertRotatingRefreshless(t, v.rel, path, testAccess("agent"))
			f.assertNoTokenMaterial(t)
		})
	}
}

// A session start or resume adopts first, so the shared file the copy is
// then compared with is already the newer login.
func TestUserSessionStart_AdoptsANewerCodexLogin(t *testing.T) {
	f := newSessionLoginFixture(t)
	now := time.Now()
	target := filepath.Join(sharedAgentHome, userSessionCodexLoginRel)
	writeLoginAt(t, target, codexTestLogin("shared", now.Add(-2*time.Hour)), now.Add(-time.Hour))
	f.start(t)
	path := filepath.Join(userSessionHome(testSessionID), userSessionCodexLoginRel)
	writeLoginAt(t, path, codexTestLogin("rotated", now), now)

	f.r.copyLoginFiles(testSessionID, testSessionUID)
	if codexRefresh(t, target) != "ref-rotated-"+bridgeTestTokenSentinel {
		t.Error("a resume did not adopt the session's newer login")
	}
	recs := f.adoptionsOf("codex")
	if len(recs) != 1 || recs[0].Fields["trigger"] != sessionLoginTriggerStart {
		t.Errorf("audit = %+v", recs)
	}
	f.assertNoTokenMaterial(t)
}

// An agent refreshed the shared login after the session's copy was made: the
// shared file is newer and is never replaced by the session's older copy.
func TestSessionLogin_OlderSessionCodexLoginIsNeverAdopted(t *testing.T) {
	f := newSessionLoginFixture(t)
	now := time.Now()
	target := filepath.Join(sharedAgentHome, userSessionCodexLoginRel)
	writeLoginAt(t, target, codexTestLogin("first", now.Add(-3*time.Hour)), now.Add(-2*time.Hour))
	f.start(t)
	agentRefresh := codexTestLogin("agent", now.Add(-time.Minute))
	writeLoginAt(t, target, agentRefresh, now)
	f.m.syncUserSessionClaudeLogins(sessionLoginTriggerSignIn)
	if !bytes.Equal(mustRead(t, target), agentRefresh) || len(f.adoptionsOf("codex")) != 0 {
		t.Error("an older session login replaced the agents' newer one")
	}

	// The session's CLI rewrites its file later with an OLDER refresh time
	// (it saved something else): written after, but not newer.
	path := filepath.Join(userSessionHome(testSessionID), userSessionCodexLoginRel)
	writeLoginAt(t, path, codexTestLogin("stale", now.Add(-2*time.Hour)), now.Add(time.Minute))
	f.m.syncUserSessionClaudeLogins(sessionLoginTriggerSignIn)
	if !bytes.Equal(mustRead(t, target), agentRefresh) || len(f.adoptionsOf("codex")) != 0 {
		t.Error("a session login with an older last refresh was adopted")
	}
}

// Deleting a session must never delete the only valid login, for these
// vendors too.
func TestSessionRemove_NeverDestroysTheOnlyValidCodexOrOpenCodeLogin(t *testing.T) {
	for vendor, v := range rotatingTestVendors() {
		t.Run(vendor+"/adopted before removal", func(t *testing.T) {
			f := newSessionLoginFixture(t)
			now := time.Now()
			target := filepath.Join(sharedAgentHome, v.rel)
			writeLoginAt(t, target, v.login("shared", now.Add(-2*time.Hour)), now.Add(-time.Hour))
			f.start(t)
			writeLoginAt(t, filepath.Join(userSessionHome(testSessionID), v.rel), v.login("rotated", now.Add(8*time.Hour)), now)

			if err := f.r.Remove(testSessionID); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(userSessionSandboxDir(testSessionID)); err == nil {
				t.Error("the sandbox survived Remove")
			}
			if v.refresh(t, target) != "ref-rotated-"+bridgeTestTokenSentinel {
				t.Error("the session's login was deleted with the session instead of being adopted")
			}
			recs := f.adoptionsOf(vendor)
			if len(recs) != 1 || recs[0].Fields["trigger"] != sessionLoginTriggerRemove {
				t.Errorf("audit = %+v", recs)
			}
			f.assertNoTokenMaterial(t)
		})
		t.Run(vendor+"/kept when it cannot be adopted", func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("root passes the shared-directory ownership check this test relies on")
			}
			f := newSessionLoginFixture(t)
			now := time.Now()
			target := filepath.Join(sharedAgentHome, v.rel)
			writeLoginAt(t, target, v.login("shared", now.Add(-2*time.Hour)), now.Add(-time.Hour))
			f.start(t)
			path := filepath.Join(userSessionHome(testSessionID), v.rel)
			rotated := v.login("rotated", now.Add(8*time.Hour))
			writeLoginAt(t, path, rotated, now)
			// A world-writable shared directory: sharedfile refuses to write
			// there, so the adoption fails.
			dir := filepath.Dir(target)
			if err := os.Chmod(dir, 0o777); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

			err := f.r.Remove(testSessionID)
			if err == nil || !errors.Is(err, crux.ErrUserSessionHoldsLogin) {
				t.Fatalf("Remove = %v, want a refusal that keeps the session", err)
			}
			if !bytes.Equal(mustRead(t, path), rotated) {
				t.Error("the session's login is gone although it was never adopted")
			}
			if strings.Contains(err.Error(), bridgeTestTokenSentinel) {
				t.Error("token material in the error")
			}
			f.assertNoTokenMaterial(t)
		})
		t.Run(vendor+"/an unchanged copy is just removed", func(t *testing.T) {
			f := newSessionLoginFixture(t)
			now := time.Now()
			target := filepath.Join(sharedAgentHome, v.rel)
			shared := v.login("shared", now.Add(-2*time.Hour))
			writeLoginAt(t, target, shared, now.Add(-time.Hour))
			f.start(t)
			if err := f.r.Remove(testSessionID); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(mustRead(t, target), shared) || f.adoptions() != 0 {
				t.Error("removing an ordinary session touched the shared login")
			}
		})
	}
}

// A shared file that changed after it was judged (an agent's CLI wrote its
// own refresh) is never overwritten by the adoption.
func TestReplaceSharedLogin_RefusesASharedFileThatChanged(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "auth.json")
	now := time.Now()
	writeLoginAt(t, target, codexTestLogin("judged", now), now.Add(-time.Hour))
	judged, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	agent := codexTestLogin("agent", now.Add(time.Minute))
	writeLoginAt(t, target, agent, now)

	_, err = replaceSharedLogin(target, codexTestLogin("session", now), judged, now)
	if !errors.Is(err, errSharedLoginChanged) {
		t.Fatalf("err = %v, want errSharedLoginChanged", err)
	}
	if !bytes.Equal(mustRead(t, target), agent) || len(staleFilesBeside(t, target)) != 0 {
		t.Error("a shared login that changed after the judgement was touched")
	}

	// Unchanged since the judgement: replaced, with the mode kept.
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	judged, _ = os.Lstat(target)
	stale, err := replaceSharedLogin(target, codexTestLogin("session", now), judged, now)
	if err != nil || stale == "" {
		t.Fatalf("replace = %q, %v", stale, err)
	}
	if st, _ := os.Lstat(target); st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want the shared file's 0640 kept", st.Mode().Perm())
	}
	if !bytes.Equal(mustRead(t, stale), agent) {
		t.Error("the replaced shared login was not preserved")
	}
}

// Several sessions holding newer logins, judged concurrently by the sweep
// and by per-session checks: the shared file ends on the latest one, whole.
func TestSessionLogin_ConcurrentCodexChecksConvergeOnOneLogin(t *testing.T) {
	const sessions = 6
	withSharedAgentHome(t)
	t.Setenv(userSessionSandboxDirEnv, filepath.Join(t.TempDir(), "sandbox"))
	t.Setenv(userSessionUIDBaseEnv, "")
	t.Setenv(userSessionUIDSlotsEnv, "")
	t.Setenv(userSessionGIDEnv, "")
	priv := func(context.Context, string, io.Reader, ...string) ([]byte, error) {
		return nil, errors.New("unexpected helper")
	}
	useSessionLoginPriv(t, priv)
	m := NewManager(map[string]config.AgentConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)), ProjectContext{})
	m.SetAuditSink(&fakeAuditSink{})

	now := time.Now()
	target := filepath.Join(sharedAgentHome, userSessionCodexLoginRel)
	writeLoginAt(t, target, codexTestLogin("shared", now.Add(-time.Hour)), now.Add(-time.Hour))
	ids := make([]string, sessions)
	for i := range ids {
		ids[i] = fmt.Sprintf("%012x", i+1)
		at := now.Add(time.Duration(i+1) * time.Hour)
		writeLoginAt(t, filepath.Join(userSessionHome(ids[i]), userSessionCodexLoginRel), codexTestLogin(fmt.Sprintf("s%d", i), at), at)
	}
	codex, _ := rotatingSessionLoginFor(userSessionCodexLoginRel)
	ops := sessionLoginOps{priv: priv, statUID: func(string) (int, error) { return testSessionUID, nil }, now: time.Now}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(2)
		go func(id string) {
			defer wg.Done()
			_, _ = ops.checkRotatingLogin(id, codex, sessionLoginModeWatch)
		}(id)
		go func() {
			defer wg.Done()
			m.syncUserSessionClaudeLogins(sessionLoginTriggerSignIn)
		}()
	}
	wg.Wait()
	m.syncUserSessionClaudeLogins(sessionLoginTriggerSignIn)

	if got := codexRefresh(t, target); got != fmt.Sprintf("ref-s%d-%s", sessions-1, bridgeTestTokenSentinel) {
		t.Error("the shared login is not the latest session login")
	}
}
