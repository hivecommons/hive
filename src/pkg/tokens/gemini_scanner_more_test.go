package tokens

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// geminiFixtureTotal is TotalTokens for one copy of the fixture recording.
const geminiFixtureTotal = 26900 + 12150

func geminiAgentDirHash(agent string) string {
	sum := sha256.Sum256([]byte("/data/agents/" + agent))
	return hex.EncodeToString(sum[:])
}

// scanGeminiWithTimeout fails the test instead of hanging when the scan
// blocks (a FIFO opened without O_NONBLOCK never returns).
func scanGeminiWithTimeout(t *testing.T, home, owner string, cache *geminiScanCache) *AggregateSummary {
	t.Helper()
	type result struct {
		agg *AggregateSummary
		err error
	}
	done := make(chan result, 1)
	go func() {
		agg, err := scanGeminiSessionTree(home, owner, nil, cache)
		done <- result{agg, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.agg
	case <-time.After(10 * time.Second):
		t.Fatal("gemini scan did not return; it blocked on a non-regular file")
		return nil
	}
}

func TestScanGeminiSessionTree_SkipsFIFOAndSymlink(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gemini")
	chats := filepath.Join(home, "tmp", "repo", "chats")
	writeGeminiFixtureAs(t, chats, "session-a.jsonl", "sess-a")

	// A recording outside the tree, linked in: must not be followed.
	outside := writeGeminiFixtureAs(t, t.TempDir(), "session-out.jsonl", "sess-outside")
	if err := os.Symlink(outside, filepath.Join(chats, "session-link.jsonl")); err != nil {
		t.Fatal(err)
	}
	// A FIFO with no writer: opening it blocking would hang the scan.
	if err := syscall.Mkfifo(filepath.Join(chats, "session-fifo.jsonl"), 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	// A symlink to an endless device must not be read either.
	if err := os.Symlink("/dev/zero", filepath.Join(chats, "session-zero.jsonl")); err != nil {
		t.Fatal(err)
	}

	agg := scanGeminiWithTimeout(t, home, "", newGeminiScanCache())
	if agg.TotalTokens != geminiFixtureTotal {
		t.Errorf("TotalTokens = %d, want %d (only the regular file)", agg.TotalTokens, geminiFixtureTotal)
	}
	for _, s := range agg.Sessions {
		if strings.HasPrefix(s.SessionID, "sess-outside") {
			t.Errorf("symlinked recording was read: %+v", s)
		}
	}
}

func TestScanGeminiSessionTree_HostileProjectRootIgnored(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gemini")

	// Oversize marker: ignored even though it starts with an agent work dir.
	big := filepath.Join(home, "tmp", "big")
	writeGeminiFixtureAs(t, filepath.Join(big, "chats"), "session-a.jsonl", "sess-big")
	marker := "/data/agents/scanner/repo\n" + strings.Repeat("x", 2*maxGeminiProjectRootBytes)
	if err := os.WriteFile(filepath.Join(big, ".project_root"), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}

	// Symlinked marker: not followed.
	linked := filepath.Join(home, "tmp", "linked")
	writeGeminiFixtureAs(t, filepath.Join(linked, "chats"), "session-b.jsonl", "sess-linked")
	target := filepath.Join(t.TempDir(), "root")
	if err := os.WriteFile(target, []byte("/data/agents/scanner/repo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(linked, ".project_root")); err != nil {
		t.Fatal(err)
	}

	// FIFO marker: skipped without blocking.
	fifoDir := filepath.Join(home, "tmp", "fifo")
	writeGeminiFixtureAs(t, filepath.Join(fifoDir, "chats"), "session-c.jsonl", "sess-fifo")
	if err := syscall.Mkfifo(filepath.Join(fifoDir, ".project_root"), 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	agg := scanGeminiWithTimeout(t, home, "", nil)
	if got := agg.ByAgent["scanner"]; got != 0 {
		t.Errorf("ByAgent[scanner] = %d, want 0 (no marker may be trusted)", got)
	}
	if got := agg.ByAgent[geminiUnknownAgent]; got != 3*geminiFixtureTotal {
		t.Errorf("ByAgent[gemini] = %d, want %d", got, 3*geminiFixtureTotal)
	}
}

func TestScanGeminiSessionTree_HashToSlugMigrationCountedOnce(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	shared := filepath.Join(home, ".gemini")

	// The CLI copied tmp/<sha256>/ to tmp/<slug>/ and kept the original, so
	// the same session exists twice; the slug copy then kept growing.
	hashDir := filepath.Join(shared, "tmp", geminiAgentDirHash("scanner"))
	writeGeminiFixture(t, filepath.Join(hashDir, "chats"), "session-a.jsonl")
	slugDir := filepath.Join(shared, "tmp", "scanner")
	slugPath := writeGeminiFixture(t, filepath.Join(slugDir, "chats"), "session-a.jsonl")
	if err := os.WriteFile(filepath.Join(slugDir, ".project_root"), []byte("/data/agents/scanner"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(slugPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"g4","timestamp":"2026-11-20T10:01:00.000Z","type":"gemini","content":"ok","model":"gemini-2.5-flash","tokens":{"input":500,"output":20,"cached":0,"thoughts":10,"tool":0,"total":530}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	agg := scanGeminiWithTimeout(t, shared, "", nil)
	if want := int64(geminiFixtureTotal + 530); agg.TotalTokens != want {
		t.Errorf("TotalTokens = %d, want %d (copies merged by sessionId, g4 once)", agg.TotalTokens, want)
	}
	if agg.SessionCount != 2 {
		t.Errorf("SessionCount = %d, want 2 (one session, two models)", agg.SessionCount)
	}
	if got := agg.ByAgent["scanner"]; got != agg.TotalTokens {
		t.Errorf("ByAgent[scanner] = %d, want %d", got, agg.TotalTokens)
	}
}

func TestScanGeminiSessionTree_HashEraDirResolvesAgent(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	shared := filepath.Join(home, ".gemini")
	// builder has a per-agent HOME (no .gemini of its own); its old project
	// dir in the shared tree is named by the sha256 of its work dir and has
	// no .project_root.
	if err := os.MkdirAll(filepath.Join(home, "agents", "builder"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeGeminiFixture(t, filepath.Join(shared, "tmp", geminiAgentDirHash("builder"), "chats"), "session-a.jsonl")

	agg := scanGeminiWithTimeout(t, shared, "", nil)
	if got := agg.ByAgent["builder"]; got != geminiFixtureTotal {
		t.Errorf("ByAgent[builder] = %d, want %d (ByAgent=%v)", got, geminiFixtureTotal, agg.ByAgent)
	}
}

func TestScanGeminiSessionTree_CacheReparsesOnlyChangedFiles(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gemini")
	chats := filepath.Join(home, "tmp", "repo", "chats")
	pathA := writeGeminiFixtureAs(t, chats, "session-a.jsonl", "sess-a")
	pathB := writeGeminiFixtureAs(t, chats, "session-b.jsonl", "sess-b")

	cache := newGeminiScanCache()
	scan := func() *AggregateSummary {
		cache.begin()
		agg := scanGeminiWithTimeout(t, home, "", cache)
		cache.prune()
		return agg
	}

	first := scan()
	if cache.parses != 2 {
		t.Fatalf("parses after first scan = %d, want 2", cache.parses)
	}
	again := scan()
	if cache.parses != 2 {
		t.Errorf("parses after unchanged re-scan = %d, want 2 (cache hit)", cache.parses)
	}
	if again.TotalTokens != first.TotalTokens {
		t.Errorf("cached TotalTokens = %d, want %d", again.TotalTokens, first.TotalTokens)
	}

	f, err := os.OpenFile(pathA, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"g4","timestamp":"2026-11-20T10:01:00.000Z","type":"gemini","content":"ok","model":"gemini-2.5-flash","tokens":{"input":500,"output":20,"cached":0,"thoughts":10,"tool":0,"total":530}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	grown := scan()
	if cache.parses != 3 {
		t.Errorf("parses after one file grew = %d, want 3 (only that file)", cache.parses)
	}
	if grown.TotalTokens != first.TotalTokens+530 {
		t.Errorf("grown TotalTokens = %d, want %d", grown.TotalTokens, first.TotalTokens+530)
	}

	if err := os.Remove(pathB); err != nil {
		t.Fatal(err)
	}
	scan()
	if n := len(cache.entries); n != 1 {
		t.Errorf("cache entries after a file was removed = %d, want 1 (pruned)", n)
	}
	if _, ok := cache.entries[pathB]; ok {
		t.Errorf("removed file still cached")
	}

	// The collector owns one cache across scans.
	c := NewCollector(t.TempDir(), testLogger())
	c.SetPersistPath(filepath.Join(t.TempDir(), "summary.json"))
	c.SetGeminiSessionsDir(home)
	c.scan()
	c.scan()
	if c.geminiCache.parses != 1 {
		t.Errorf("collector parses after two scans = %d, want 1", c.geminiCache.parses)
	}
}

func TestScanGeminiSessionTree_ModelFromNeighbour(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gemini")
	chats := filepath.Join(home, "tmp", "repo", "chats")
	if err := os.MkdirAll(chats, 0o755); err != nil {
		t.Fatal(err)
	}
	// g2 has tokens but no model (a synthetic turn): it takes the nearer
	// neighbour's model, the earlier one at equal distance (g1, pro).
	// h1 has no earlier neighbour, so it takes h2's (flash).
	content := strings.Join([]string{
		`{"sessionId":"s-n","projectHash":"repo","startTime":"2026-11-20T10:00:00.000Z","lastUpdated":"2026-11-20T10:00:00.000Z"}`,
		`{"id":"g1","timestamp":"2026-11-20T10:00:01.000Z","type":"gemini","content":"a","model":"gemini-2.5-pro","tokens":{"input":100,"output":10,"cached":0,"thoughts":0,"tool":0,"total":110}}`,
		`{"id":"g2","timestamp":"2026-11-20T10:00:02.000Z","type":"gemini","content":"b","tokens":{"input":200,"output":20,"cached":0,"thoughts":0,"tool":0,"total":220}}`,
		`{"id":"g3","timestamp":"2026-11-20T10:00:03.000Z","type":"gemini","content":"c","model":"gemini-2.5-flash","tokens":{"input":300,"output":30,"cached":0,"thoughts":0,"tool":0,"total":330}}`,
	}, "\n") + "\n"
	other := strings.Join([]string{
		`{"sessionId":"s-m","projectHash":"repo","startTime":"2026-11-20T10:00:00.000Z","lastUpdated":"2026-11-20T10:00:00.000Z"}`,
		`{"id":"u1","timestamp":"2026-11-20T10:00:00.500Z","type":"user","content":"go"}`,
		`{"id":"h1","timestamp":"2026-11-20T10:00:01.000Z","type":"gemini","content":"a","tokens":{"input":40,"output":4,"cached":0,"thoughts":0,"tool":0,"total":44}}`,
		`{"id":"h2","timestamp":"2026-11-20T10:00:02.000Z","type":"gemini","content":"b","model":"gemini-2.5-flash","tokens":{"input":50,"output":5,"cached":0,"thoughts":0,"tool":0,"total":55}}`,
	}, "\n") + "\n"
	for name, body := range map[string]string{"session-n.jsonl": content, "session-m.jsonl": other} {
		if err := os.WriteFile(filepath.Join(chats, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	agg := scanGeminiWithTimeout(t, home, "", nil)
	if got := agg.ByModel["gemini-2.5-pro"]; got != 110+220 {
		t.Errorf("ByModel[pro] = %d, want %d (g1 + g2)", got, 110+220)
	}
	if got := agg.ByModel["gemini-2.5-flash"]; got != 330+44+55 {
		t.Errorf("ByModel[flash] = %d, want %d (g3 + h1 + h2)", got, 330+44+55)
	}
	if got := agg.ByModel[geminiUnknownModel]; got != 0 {
		t.Errorf("ByModel[%s] = %d, want 0", geminiUnknownModel, got)
	}
}
