package tokens

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeOwnerSpec is the su-exec spec the tests pretend the tree owner has.
const fakeOwnerSpec = "2010:1000"

// ownerSeams swaps the privileged-read seams for fakes that read the temp
// tree directly, and counts transcript opens. Cache state is reset around the
// test so cases do not see each other's parses.
type ownerSeams struct {
	opens   int
	listErr error
	specs   []string
}

func installOwnerSeams(t *testing.T) *ownerSeams {
	t.Helper()
	s := &ownerSeams{}
	prevSpec, prevRun, prevOpen := claudeTreeOwnerSpec, ownerRun, ownerOpen
	resetOwnerCache()
	t.Cleanup(func() {
		claudeTreeOwnerSpec, ownerRun, ownerOpen = prevSpec, prevRun, prevOpen
		resetOwnerCache()
	})
	claudeTreeOwnerSpec = func(string) (string, bool, error) { return fakeOwnerSpec, true, nil }
	ownerRun = func(spec string, _ time.Duration, argv ...string) ([]byte, error) {
		s.specs = append(s.specs, spec)
		if s.listErr != nil {
			return nil, s.listErr
		}
		dir := argv[len(argv)-1]
		var out bytes.Buffer
		for _, pattern := range []string{
			filepath.Join(dir, "*", "*.jsonl"),
			filepath.Join(dir, "*", "subagents", "*.jsonl"),
		} {
			matches, _ := filepath.Glob(pattern)
			for _, m := range matches {
				info, err := os.Stat(m)
				if err != nil {
					continue
				}
				fmt.Fprintf(&out, "%d %d %s\n", info.Size(), info.ModTime().Unix(), m)
			}
		}
		// Lines a helper must never be trusted with: malformed, and a path
		// outside the tree.
		out.WriteString("garbage\n")
		fmt.Fprintf(&out, "10 %d /etc/passwd.jsonl\n", time.Now().Unix())
		return out.Bytes(), nil
	}
	ownerOpen = func(spec, path string) (io.ReadCloser, error) {
		s.specs = append(s.specs, spec)
		s.opens++
		return os.Open(path)
	}
	return s
}

func resetOwnerCache() {
	ownerSessionCache.Lock()
	ownerSessionCache.entries = map[string]ownerCacheEntry{}
	ownerSessionCache.Unlock()
}

func writeClaudeTranscript(t *testing.T, path, model string, in, out int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"assistant","timestamp":"%s","message":{"model":%q,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`+"\n",
		time.Now().UTC().Format(time.RFC3339), model, in, out)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestScanClaudeSessionTree_UnreadableTreeReadsAsOwner is the #19307 root
// cause: a per-UID tree the hive cannot list must still contribute its
// usage, attributed to the tree's agent and priced from the table.
func TestScanClaudeSessionTree_UnreadableTreeReadsAsOwner(t *testing.T) {
	seams := installOwnerSeams(t)
	tree := t.TempDir()
	mainPath := filepath.Join(tree, "-data-agents-scanner", "s1.jsonl")
	sub := filepath.Join(tree, "-data-agents-scanner", "subagents", "a1.jsonl")
	writeClaudeTranscript(t, mainPath, "claude-sonnet-5-5", 1000, 200)
	writeClaudeTranscript(t, sub, "claude-sonnet-5-5", 10, 5)

	agg, err := ScanClaudeSessionTree(tree, "scanner")
	if err != nil {
		t.Fatalf("ScanClaudeSessionTree: %v", err)
	}
	if agg.SessionCount != 2 {
		t.Fatalf("SessionCount = %d, want 2 (outside-tree and malformed listing lines must be dropped)", agg.SessionCount)
	}
	if got, want := agg.TotalTokens, int64(1215); got != want {
		t.Errorf("TotalTokens = %d, want %d", got, want)
	}
	if agg.ByAgent["scanner"] != agg.TotalTokens {
		t.Errorf("ByAgent = %v, want all tokens on scanner", agg.ByAgent)
	}
	for _, spec := range seams.specs {
		if spec != fakeOwnerSpec {
			t.Errorf("helper ran as %q, want %q", spec, fakeOwnerSpec)
		}
	}
	est := EstimateFromSummary(agg)
	if est.TotalUSD <= 0 {
		t.Errorf("TotalUSD = %v, want > 0 for owner-read claude usage", est.TotalUSD)
	}
	if mc := est.ByAgent["scanner"]; !mc.Priced || mc.USD <= 0 {
		t.Errorf("ByAgent[scanner] = %+v, want an exact non-zero price", mc)
	}
}

// TestScanClaudeSessionTree_OwnerReadCachesUnchangedFiles: only transcripts
// whose size or mtime changed are streamed again, and a vanished transcript
// leaves the total.
func TestScanClaudeSessionTree_OwnerReadCachesUnchangedFiles(t *testing.T) {
	seams := installOwnerSeams(t)
	tree := t.TempDir()
	a := filepath.Join(tree, "-data-agents-reviewer", "a.jsonl")
	b := filepath.Join(tree, "-data-agents-reviewer", "b.jsonl")
	writeClaudeTranscript(t, a, "claude-sonnet-5-5", 100, 10)
	writeClaudeTranscript(t, b, "claude-sonnet-5-5", 100, 10)

	if _, err := ScanClaudeSessionTree(tree, "reviewer"); err != nil {
		t.Fatal(err)
	}
	if seams.opens != 2 {
		t.Fatalf("first scan opens = %d, want 2", seams.opens)
	}

	agg, err := ScanClaudeSessionTree(tree, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if seams.opens != 2 {
		t.Errorf("unchanged rescan opened %d more files, want 0", seams.opens-2)
	}
	if agg.TotalTokens != 220 {
		t.Errorf("cached rescan TotalTokens = %d, want 220", agg.TotalTokens)
	}

	// A running turn appends to a; bump its mtime so the change is visible
	// even on coarse-mtime filesystems.
	writeClaudeTranscript(t, a, "claude-sonnet-5-5", 50, 5)
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(a, later, later); err != nil {
		t.Fatal(err)
	}
	agg, err = ScanClaudeSessionTree(tree, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if seams.opens != 3 {
		t.Errorf("after one append opens = %d, want 3", seams.opens)
	}
	if agg.TotalTokens != 275 {
		t.Errorf("after append TotalTokens = %d, want 275", agg.TotalTokens)
	}

	if err := os.Remove(b); err != nil {
		t.Fatal(err)
	}
	agg, err = ScanClaudeSessionTree(tree, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if agg.SessionCount != 1 || agg.TotalTokens != 165 {
		t.Errorf("after removal = %d sessions / %d tokens, want 1 / 165", agg.SessionCount, agg.TotalTokens)
	}
	ownerSessionCache.Lock()
	_, stale := ownerSessionCache.entries[filepath.Clean(b)]
	ownerSessionCache.Unlock()
	if stale {
		t.Error("removed transcript is still cached")
	}
}

// TestScanClaudeSessionTree_OwnerListFailureIsAnError: a tree that cannot be
// read at all must surface an error, never an empty "no usage" scan.
func TestScanClaudeSessionTree_OwnerListFailureIsAnError(t *testing.T) {
	seams := installOwnerSeams(t)
	seams.listErr = errors.New("su-exec unavailable")
	if _, err := ScanClaudeSessionTree(t.TempDir(), "scanner"); err == nil {
		t.Fatal("ScanClaudeSessionTree returned nil error for an unlistable tree")
	}
}

// TestScanClaudeSessionTree_OwnerSpecRefusalIsAnError: an unreadable tree
// whose owner cannot be used is reported, not scanned as empty.
func TestScanClaudeSessionTree_OwnerSpecRefusalIsAnError(t *testing.T) {
	prev := claudeTreeOwnerSpec
	t.Cleanup(func() { claudeTreeOwnerSpec = prev })
	claudeTreeOwnerSpec = func(string) (string, bool, error) { return "", false, errors.New("owned by uid 0") }
	if _, err := ScanClaudeSessionTree(t.TempDir(), "scanner"); err == nil {
		t.Fatal("ScanClaudeSessionTree returned nil error for a refused owner")
	}
}

func TestClaudeTreeOwnerSpec_ReadableTreeIsReadDirectly(t *testing.T) {
	for _, dir := range []string{t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		spec, ok, err := claudeTreeOwnerSpec(dir)
		if err != nil || ok || spec != "" {
			t.Errorf("claudeTreeOwnerSpec(%s) = %q, %v, %v; want direct read", dir, spec, ok, err)
		}
	}
}

func TestClaudeTreeOwnerSpec_UnreadableTreeUsesOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "projects")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	spec, ok, err := claudeTreeOwnerSpec(dir)
	if err != nil || !ok {
		t.Fatalf("claudeTreeOwnerSpec = %q, %v, %v; want owner read", spec, ok, err)
	}
	if want := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()); spec != want {
		t.Errorf("spec = %q, want %q", spec, want)
	}
}

func TestOwnerSpecForDir_FailsClosed(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "l")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{file, link} {
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if spec, err := ownerSpecForDir(p, info); err == nil {
			t.Errorf("ownerSpecForDir(%s) = %q, want refusal", p, spec)
		}
	}
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := ownerSpecForDir(root, info)
	if os.Geteuid() == 0 {
		if err == nil {
			t.Errorf("root-owned tree spec = %q, want refusal", spec)
		}
		return
	}
	st, _ := info.Sys().(*syscall.Stat_t)
	if want := fmt.Sprintf("%d:%d", st.Uid, st.Gid); err != nil || spec != want {
		t.Errorf("ownerSpecForDir = %q, %v; want %q", spec, err, want)
	}
}

func TestParseOwnerListing(t *testing.T) {
	dir := "/data/home/agents/scanner/.claude/projects"
	out := strings.Join([]string{
		"120 1795107600 " + dir + "/-data-agents-scanner/a b.jsonl",
		"x 1 " + dir + "/-data-agents-scanner/bad.jsonl",
		"1 1 /data/home/agents/reviewer/.claude/projects/x/y.jsonl",
		"1 1 " + dir + "/../../../reviewer/.claude/projects/x/y.jsonl",
		"1 1 " + dir + "/-data-agents-scanner/notes.txt",
		"",
	}, "\n")
	files := parseOwnerListing(dir, []byte(out))
	if len(files) != 1 {
		t.Fatalf("parseOwnerListing = %+v, want only the in-tree transcript", files)
	}
	if f := files[0]; f.size != 120 || f.mtime != 1795107600 || !strings.HasSuffix(f.path, "a b.jsonl") {
		t.Errorf("parsed %+v", f)
	}
}

// TestLookupPrice_BothBackendSpellings pins that the Claude CLI's dashed id
// and Copilot's dotted id resolve to the same exact price row, so the
// backend an agent runs on never changes its estimate.
func TestLookupPrice_BothBackendSpellings(t *testing.T) {
	for _, pair := range [][2]string{
		{"claude-sonnet-5-5", "claude-sonnet-5.5"},
		{"claude-sonnet-4-6", "claude-sonnet-4.6"},
		{"claude-opus-4-8", "anthropic/claude-opus-4.8"},
	} {
		dash, okDash := LookupPrice(pair[0])
		dot, okDot := LookupPrice(pair[1])
		if !okDash || !okDot || dash != dot {
			t.Errorf("LookupPrice(%q)=%v,%v vs LookupPrice(%q)=%v,%v; want the same exact row", pair[0], dash, okDash, pair[1], dot, okDot)
		}
	}
}

// TestCollectorNoteClaudeTreeErr_LogsOncePerError: a persistent tree failure
// is one warning, not one per 30s scan.
func TestCollectorNoteClaudeTreeErr_LogsOncePerError(t *testing.T) {
	var buf bytes.Buffer
	c := NewCollector(t.TempDir(), slog.New(slog.NewTextHandler(&buf, nil)))
	errDenied := fs.ErrPermission
	c.noteClaudeTreeErr("/t", errDenied)
	c.noteClaudeTreeErr("/t", errDenied)
	if n := strings.Count(buf.String(), "claude session scan failed"); n != 1 {
		t.Errorf("repeated error logged %d times, want 1", n)
	}
	c.noteClaudeTreeErr("/t", nil)
	c.noteClaudeTreeErr("/t", nil)
	if n := strings.Count(buf.String(), "claude session scan recovered"); n != 1 {
		t.Errorf("recovery logged %d times, want 1", n)
	}
	c.noteClaudeTreeErr("/t", errDenied)
	if n := strings.Count(buf.String(), "claude session scan failed"); n != 2 {
		t.Errorf("recurrence logged %d failures in total, want 2", n)
	}
}
