package github

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Test constants — no magic numbers in the assertions below.
const (
	// testRSABits is the key size for the throwaway App signing key. 2048 is
	// the minimum GitHub accepts and keeps test key generation fast.
	testRSABits = 2048
	// testTokenTTL is how far in the future the fake mint endpoint claims the
	// token expires; any positive duration works.
	testTokenTTL = time.Hour
	// preCreatedMode mirrors what entrypoint.sh applies to a pre-created
	// per-agent cache file: owner (dev) rw, owning agent's private group r.
	preCreatedMode os.FileMode = 0o640
	// otherAgentMode is a deliberately world-readable mode used to prove the
	// isolation assertion is checking OUR file's mode, not a global umask.
	otherAgentMode os.FileMode = 0o644
)

// newFakeAppAuth returns an AppAuth wired to a fake mint endpoint that always
// returns the given token, plus a buffer capturing everything it logs.
func newFakeAppAuth(t *testing.T, token string) (*AppAuth, *bytes.Buffer, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      token,
			"expires_at": time.Now().Add(testTokenTTL).Format(time.RFC3339),
		})
	}))
	key, err := rsa.GenerateKey(rand.Reader, testRSABits)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	var logBuf bytes.Buffer
	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		apiURL:         server.URL,
	}
	return auth, &logBuf, server.Close
}

// useTempCacheDir redirects the package-level agent token cache dir at a temp
// dir for the duration of the test.
func useTempCacheDir(t *testing.T) string {
	t.Helper()
	orig := agentTokenCacheDir
	dir := t.TempDir()
	agentTokenCacheDir = dir
	t.Cleanup(func() { agentTokenCacheDir = orig })
	return dir
}

// TestWriteAgentToken_WritesInPlaceWithoutChown is the core regression test
// for the fleet-wide delivery bug: WriteAgentToken must write the token into
// the file entrypoint.sh pre-created, PRESERVING that file's mode, and must
// never fail (or delete the token) because it cannot chown.
func TestWriteAgentToken_WritesInPlaceWithoutChown(t *testing.T) {
	const wantToken = "ghs-scoped-advisor"
	auth, logBuf, closeFn := newFakeAppAuth(t, wantToken)
	defer closeFn()
	dir := useTempCacheDir(t)

	// Simulate entrypoint.sh pre-creating the file as dev:hive-guide 0640.
	path := AgentTokenCachePath("guide")
	if err := os.WriteFile(path, nil, preCreatedMode); err != nil {
		t.Fatalf("pre-creating cache file: %v", err)
	}
	if err := os.Chmod(path, preCreatedMode); err != nil {
		t.Fatalf("chmod pre-created file: %v", err)
	}

	// A non-zero agent UID is the case that used to trigger the fatal chown.
	if err := auth.WriteAgentToken(context.Background(), "guide", "advisor", 2001); err != nil {
		t.Fatalf("WriteAgentToken returned error (must not chown): %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("token file missing after write — the old code deleted it: %v", err)
	}
	if string(got) != wantToken {
		t.Errorf("token = %q, want %q", got, wantToken)
	}

	// The pre-created inode (and therefore its ownership) must survive: a
	// write-temp-then-rename would reset the mode to the default.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != preCreatedMode {
		t.Errorf("mode = %o, want %o (rename would have clobbered the entrypoint ownership)", info.Mode().Perm(), preCreatedMode)
	}

	// No leftover temp file — the old implementation used a .tmp sidecar.
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("stray .tmp file left behind")
	}

	// The token value must never be logged.
	if strings.Contains(logBuf.String(), wantToken) {
		t.Error("token value leaked into logs")
	}
	// Pre-created path must NOT emit the degraded warning.
	if strings.Contains(logBuf.String(), "not pre-created") {
		t.Errorf("unexpected degraded warning on the healthy path: %s", logBuf.String())
	}
	// Fix the directory in the assertion so a path change is caught.
	if !strings.HasPrefix(path, dir+"/") {
		t.Errorf("cache path %q not under cache dir %q", path, dir)
	}
}

// TestWriteAgentToken_DoesNotReplaceTheInode is the guard against
// reintroducing a write-temp-then-rename. Verified in the real hive image: a
// rename replaces the pre-created inode, the new one is created with the
// writer's primary group (node, gid 1000) instead of the agent's private
// group, and a DIFFERENT agent could then read the token. The inode number
// must therefore be unchanged across a write.
func TestWriteAgentToken_DoesNotReplaceTheInode(t *testing.T) {
	auth, _, closeFn := newFakeAppAuth(t, "ghs-inode-check")
	defer closeFn()
	useTempCacheDir(t)

	path := AgentTokenCachePath("stable")
	if err := os.WriteFile(path, nil, preCreatedMode); err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	if err := auth.WriteAgentToken(context.Background(), "stable", "advisor", 2005); err != nil {
		t.Fatalf("WriteAgentToken: %v", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Error("cache file inode was replaced — a rename would drop the per-agent group and leak the token to every agent in group node")
	}
}

// TestWriteAgentToken_AdvisoryTierIsDelivered proves an ADVISORY agent — whose
// mode fails CanPush() and is therefore deliberately kept GITHUB_TOKEN-less —
// still receives its advisor-tier scoped token on disk. That file is the
// advisory agent's only write path for posting ACMM L2 findings as comments.
func TestWriteAgentToken_AdvisoryTierIsDelivered(t *testing.T) {
	const wantToken = "ghs-advisor-comment-token"
	auth, _, closeFn := newFakeAppAuth(t, wantToken)
	defer closeFn()
	useTempCacheDir(t)

	path := AgentTokenCachePath("scanner")
	if err := os.WriteFile(path, nil, preCreatedMode); err != nil {
		t.Fatalf("pre-creating cache file: %v", err)
	}

	if err := auth.WriteAgentToken(context.Background(), "scanner", "advisor", 2002); err != nil {
		t.Fatalf("advisory token delivery failed: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != wantToken {
		t.Fatalf("advisor token not delivered: content=%q err=%v", got, err)
	}
}

// TestWriteAgentToken_AgentsDoNotShareAFile asserts agent A's token never
// lands in agent B's cache file, and that each agent's file keeps its own
// mode. Cross-agent readability is prevented by per-agent GROUP ownership set
// in entrypoint.sh, which cannot be asserted portably in a unit test without
// root; what IS asserted here is that the code writes strictly per-agent paths
// and never widens or reuses another agent's file.
func TestWriteAgentToken_AgentsDoNotShareAFile(t *testing.T) {
	const tokenA = "ghs-token-for-a"
	authA, _, closeA := newFakeAppAuth(t, tokenA)
	defer closeA()
	useTempCacheDir(t)

	pathA := AgentTokenCachePath("alpha")
	pathB := AgentTokenCachePath("bravo")
	if pathA == pathB {
		t.Fatal("distinct agents resolved to the same cache path")
	}
	if err := os.WriteFile(pathA, nil, preCreatedMode); err != nil {
		t.Fatalf("pre-create A: %v", err)
	}
	// Give B a deliberately different mode so we can prove A's write does not
	// touch it in any way.
	if err := os.WriteFile(pathB, []byte("bravo-existing"), otherAgentMode); err != nil {
		t.Fatalf("pre-create B: %v", err)
	}

	if err := authA.WriteAgentToken(context.Background(), "alpha", "trusted", 2003); err != nil {
		t.Fatalf("WriteAgentToken(alpha): %v", err)
	}

	bContents, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatalf("read B: %v", err)
	}
	if string(bContents) != "bravo-existing" {
		t.Errorf("agent alpha's write modified agent bravo's cache: %q", bContents)
	}
	if strings.Contains(string(bContents), tokenA) {
		t.Error("agent alpha's token leaked into agent bravo's cache file")
	}
	infoB, err := os.Stat(pathB)
	if err != nil {
		t.Fatalf("stat B: %v", err)
	}
	if infoB.Mode().Perm() != otherAgentMode {
		t.Errorf("bravo mode changed to %o", infoB.Mode().Perm())
	}
}

// TestWriteAgentToken_MissingPreCreatedFileWarnsAccurately asserts the
// degraded path produces a LOUD, accurate message. The old message claimed the
// agent "will use shared cache" while having just DELETED the token; the
// replacement must name the real consequence (privilege escalation to the
// shared full token, or total failure) and must not claim things are fine.
func TestWriteAgentToken_MissingPreCreatedFileWarnsAccurately(t *testing.T) {
	const wantToken = "ghs-no-precreate"
	auth, logBuf, closeFn := newFakeAppAuth(t, wantToken)
	defer closeFn()
	useTempCacheDir(t)

	// Deliberately do NOT pre-create the file.
	if err := auth.WriteAgentToken(context.Background(), "orphan", "newcomer", 2004); err != nil {
		t.Fatalf("write should still succeed (best effort), got: %v", err)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "not pre-created") {
		t.Errorf("degraded path did not warn; logs: %s", logs)
	}
	if !strings.Contains(logs, "CANNOT read") {
		t.Errorf("warning does not state the consequence; logs: %s", logs)
	}
	if strings.Contains(logs, wantToken) {
		t.Error("token value leaked into logs")
	}

	// Whatever we did create must stay owner-only, never group-readable —
	// group "node" contains every agent.
	info, err := os.Stat(AgentTokenCachePath("orphan"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("degraded-path file mode %o is readable beyond the owner", info.Mode().Perm())
	}
}

// TestWriteAgentToken_PublishesTrustedBotIdentityFile is the delivery half of
// the #4044 fix: staff agents authenticate with App installation tokens, for
// which `gh api user` structurally 403s, so gh-wrapper.sh's author gate can
// only learn "who am I" from a file this process writes out-of-band of the
// agent. Every mint must publish the bot login next to the token caches,
// world-readable (it is public metadata) in a directory agents cannot write.
func TestWriteAgentToken_PublishesTrustedBotIdentityFile(t *testing.T) {
	const wantLogin = "test-app[bot]"
	auth, _, closeFn := newFakeAppAuth(t, "ghs-identity")
	defer closeFn()
	useTempCacheDir(t)
	auth.SetBotLogin(wantLogin)

	if err := auth.WriteAgentToken(context.Background(), "guide", "advisor", 2001); err != nil {
		t.Fatalf("WriteAgentToken: %v", err)
	}

	data, err := os.ReadFile(BotLoginFilePath())
	if err != nil {
		t.Fatalf("trusted bot-identity file was not published: %v", err)
	}
	if string(data) != wantLogin {
		t.Errorf("bot-identity file content = %q, want %q", string(data), wantLogin)
	}

	info, err := os.Stat(BotLoginFilePath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != botLoginFilePerms {
		t.Errorf("bot-identity file mode = %o, want %o (agents must be able to READ it; the dir denies writes)", info.Mode().Perm(), botLoginFilePerms)
	}

	// A rotated slug (config refresh reinits the client) must replace the file.
	const rotatedLogin = "rotated-app[bot]"
	auth.SetBotLogin(rotatedLogin)
	if err := auth.WriteAgentToken(context.Background(), "guide", "advisor", 2001); err != nil {
		t.Fatalf("WriteAgentToken after rotation: %v", err)
	}
	data, err = os.ReadFile(BotLoginFilePath())
	if err != nil {
		t.Fatalf("re-reading bot-identity file: %v", err)
	}
	if string(data) != rotatedLogin {
		t.Errorf("bot-identity file after rotation = %q, want %q", string(data), rotatedLogin)
	}
}

// TestWriteAgentToken_NoBotLoginPublishesNothing pins the App-less/unusable-App
// posture: with no bot login recorded (cfg.GitHub.BotLogin() returns "" when
// HasUsableApp() is false), no identity file may appear — the wrapper's author
// gate must stay fail-closed rather than trusting an empty identity — and
// token delivery itself must be unaffected.
func TestWriteAgentToken_NoBotLoginPublishesNothing(t *testing.T) {
	auth, _, closeFn := newFakeAppAuth(t, "ghs-no-identity")
	defer closeFn()
	useTempCacheDir(t)

	if err := auth.WriteAgentToken(context.Background(), "guide", "advisor", 2001); err != nil {
		t.Fatalf("WriteAgentToken: %v", err)
	}
	if _, err := os.Stat(BotLoginFilePath()); !os.IsNotExist(err) {
		t.Errorf("bot-identity file must not exist without a bot login (stat err = %v)", err)
	}
}
