package resumehandle

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The walk and id checks below are the guards that keep capture bounded and
// its output safe to render into a shell command: a deep or huge session tree
// must not stall the PR-opened hook, and an id read from inside a transcript
// must never carry shell metacharacters. The happy-path tests in
// resumehandle_test.go never reach these branches.

func TestNewestResumeSession_IgnoresTranscriptsBelowMaxDepth(t *testing.T) {
	home := homeForCapture(t)
	now := time.Now()
	projects := filepath.Join(home, ".claude", "projects")
	// projects is depth 0; a transcript under four nested directories sits at
	// depth 4 and is still walked, one more level is not.
	within := filepath.Join(projects, "a", "b", "c", "d", "aaaaaaaa-1111-2222-3333-444444444444.jsonl")
	writeResumeTranscript(t, within, now.Add(-time.Hour))
	tooDeep := filepath.Join(projects, "a", "b", "c", "d", "e", "bbbbbbbb-1111-2222-3333-444444444444.jsonl")
	writeResumeTranscript(t, tooDeep, now)

	h, ok := capture("claude", now)
	if !ok {
		t.Fatal("no handle captured for claude")
	}
	if h.SessionID != "aaaaaaaa-1111-2222-3333-444444444444" {
		t.Errorf("session id = %q, want the transcript within the depth bound, not the newer one below it", h.SessionID)
	}
}

func TestNewestResumeSession_StopsAtEntryBudget(t *testing.T) {
	home := homeForCapture(t)
	now := time.Now()
	project := filepath.Join(home, ".claude", "projects", "-data-work-hive")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	// ReadDir returns entries sorted by name: the budget's worth of filler
	// ("0000…") sorts before the real transcript ("zzzz…"), so the walk gives
	// up before it reaches it.
	for i := 0; i < resumeWalkMaxEntries; i++ {
		name := "0" + strconv.Itoa(i) + ".txt"
		if err := os.WriteFile(filepath.Join(project, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeResumeTranscript(t, filepath.Join(project, "zzzzzzzz-1111-2222-3333-444444444444.jsonl"), now)

	if h, ok := capture("claude", now); ok {
		t.Errorf("walk past the entry budget captured %+v", h)
	}
}

func TestConsider_KeepsNewestRegardlessOfWalkOrder(t *testing.T) {
	home := homeForCapture(t)
	now := time.Now()
	projects := filepath.Join(home, ".claude", "projects", "-data-work-hive")
	// Name order puts the newest transcript first, so the older one is
	// considered after it and must not displace it.
	writeResumeTranscript(t, filepath.Join(projects, "11111111-2222-3333-4444-555555555555.jsonl"), now)
	writeResumeTranscript(t, filepath.Join(projects, "99999999-8888-7777-6666-555555555555.jsonl"), now.Add(-time.Hour))

	h, ok := capture("claude", now)
	if !ok {
		t.Fatal("no handle captured for claude")
	}
	if h.SessionID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("session id = %q, want the newest transcript even when walked first", h.SessionID)
	}
}

func TestConsider_SkipsSessionDirsWithoutAReadableTranscript(t *testing.T) {
	home := homeForCapture(t)
	now := time.Now()
	root := filepath.Join(home, ".copilot", "session-state")
	// A session directory with no events.jsonl yet, and one whose
	// events.jsonl is itself a directory: neither dates a conversation.
	if err := os.MkdirAll(filepath.Join(root, "sess-empty-0001"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sess-dir-00002", "events.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if h, ok := capture("copilot", now); ok {
		t.Errorf("session directories without a transcript file produced %+v", h)
	}

	writeResumeTranscript(t, filepath.Join(root, "sess-real-0003", "events.jsonl"), now.Add(-time.Hour))
	h, ok := capture("copilot", now)
	if !ok || h.SessionID != "sess-real-0003" {
		t.Errorf("handle = %+v, ok = %v; want sess-real-0003", h, ok)
	}
}

func TestTrimResumeName_RejectsDotPrefixedNames(t *testing.T) {
	for _, name := range []string{".hidden-transcript.jsonl", ".DS_Store-long-enough.jsonl"} {
		if id := trimResumeName(name, "", ".jsonl"); id != "" {
			t.Errorf("trimResumeName(%q) = %q, want no id", name, id)
		}
	}
	// A dot-prefixed name is also never a claude session on disk.
	home := homeForCapture(t)
	now := time.Now()
	writeResumeTranscript(t, filepath.Join(home, ".claude", "projects", "-p", ".hidden-transcript.jsonl"), now)
	if h, ok := capture("claude", now); ok {
		t.Errorf("dot-prefixed transcript produced %+v", h)
	}
}

func TestCodexRolloutID_RejectsShortAndNonHexNames(t *testing.T) {
	for _, name := range []string{
		"rollout-abcdefghij.jsonl", // long enough to be an id, too few dash-separated fields
		"rollout-2026-09-30T08-05-47-gggggggg-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl", // uuid-shaped but not hex
		"rollout-2026-09-30T08-05-47-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeeZ.jsonl", // non-hex in the last group
	} {
		if id := codexRolloutID(name); id != "" {
			t.Errorf("codexRolloutID(%q) = %q, want no id", name, id)
		}
	}
	if id := codexRolloutID("rollout-2026-09-30T08-05-47-ABCDEF01-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl"); id != "ABCDEF01-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("upper-case hex uuid rejected: id = %q", id)
	}
}

func TestIsSafeResumeID_BoundsLength(t *testing.T) {
	if isSafeResumeID(strings.Repeat("a", resumeIDMinLen-1)) {
		t.Error("id below the minimum length accepted")
	}
	if !isSafeResumeID(strings.Repeat("a", resumeIDMaxLen)) {
		t.Error("id at the maximum length rejected")
	}
	if isSafeResumeID(strings.Repeat("a", resumeIDMaxLen+1)) {
		t.Error("id over the maximum length accepted")
	}
	// The bound holds for ids read off file names too.
	long := "2026-09-30T08-00-00-000Z_" + strings.Repeat("a", resumeIDMaxLen+1) + ".jsonl"
	if id := timestampedSessionID(long); id != "" {
		t.Errorf("over-long id accepted from a session file name: %q", id)
	}
}

func TestGeminiSessionID_RejectsShortFileNames(t *testing.T) {
	dir := t.TempDir()
	// Valid metadata but a file name too short to carry the id's first 8
	// characters cannot be checked against it, so no handle.
	path := filepath.Join(dir, "session-abc.jsonl")
	body := `{"sessionId":"aaaaaaaa-1111-2222-3333-444444444444"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if id := geminiSessionID(path); id != "" {
		t.Errorf("file name too short to carry the id's first 8 characters yielded %q", id)
	}
	// A transcript that is a bare metadata line without a trailing newline
	// is still read (ReadBytes returns io.EOF with the line).
	path = filepath.Join(dir, "session-2026-09-30T08-00-aaaaaaaa.jsonl")
	if err := os.WriteFile(path, []byte(`{"sessionId":"aaaaaaaa-1111-2222-3333-444444444444"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if id := geminiSessionID(path); id != "aaaaaaaa-1111-2222-3333-444444444444" {
		t.Errorf("single-line transcript yielded %q", id)
	}
}

func TestGooseLaunchLogID_RequiresUnderscoreSeparator(t *testing.T) {
	// Fifteen characters like a launch log, but the date/time separator is
	// not "_": not a launch log.
	if id := gooseLaunchLogID("20260930-095526.log"); id != "" {
		t.Errorf("gooseLaunchLogID with '-' separator = %q, want no id", id)
	}
}

func TestGooseSessionID_RejectsUnterminatedID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "20260930_095526.log")
	// The log was cut mid-write: the session.id attribute has no closing
	// quote, so there is no id to read.
	body := `{"spans":[{"session.id":"20260930_1`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if id := gooseSessionID(path); id != "" {
		t.Errorf("unterminated session.id yielded %q", id)
	}
}

func TestOmpProjectDir_TempRootBuckets(t *testing.T) {
	home := homeForCapture(t)
	now := time.Now()
	sessions := filepath.Join(home, ".omp", "agent", "sessions")
	const tmpID = "ffffffff-1111-2222-3333-444444444444"

	// A working directory under the temp root but outside HOME is bucketed
	// "-tmp-<relative>", with the path resolved the way omp's process sees it.
	workInTmp := t.TempDir()
	rel, err := filepath.Rel(canonicalPath(os.TempDir()), canonicalPath(workInTmp))
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Skipf("t.TempDir() %q is not under os.TempDir() %q", workInTmp, os.TempDir())
	}
	encode := strings.NewReplacer("/", "-", `\`, "-", ":", "-")
	bucket := "-tmp-" + encode.Replace(rel)
	writeResumeTranscript(t, filepath.Join(sessions, bucket, "2026-09-30T08-00-00-000Z_"+tmpID+".jsonl"), now)

	h, ok := captureIn("omp", workInTmp, now)
	if !ok || h.SessionID != tmpID {
		t.Fatalf("handle = %+v, ok = %v; want %s from bucket %s", h, ok, tmpID, bucket)
	}

	// A bucket of some other temp directory is never this agent's.
	if ompProjectDir(home, filepath.Join(sessions, "-tmp-other-dir"), workInTmp) {
		t.Error("a foreign -tmp- bucket matched the working directory")
	}
}

func TestOmpProjectDir_RootDirectoriesBucketAsBarePrefix(t *testing.T) {
	home := homeForCapture(t)
	sessions := filepath.Join(home, ".omp", "agent", "sessions")

	// HOME itself is bucketed "-" and the temp root itself "-tmp".
	if !ompProjectDir(home, filepath.Join(sessions, "-"), home) {
		t.Error("HOME as the working directory did not match bucket \"-\"")
	}
	if ompProjectDir(home, filepath.Join(sessions, "-work"), home) {
		t.Error("HOME as the working directory matched a sub-directory bucket")
	}
	if !ompProjectDir("", filepath.Join(sessions, "-tmp"), os.TempDir()) {
		t.Error("the temp root as the working directory did not match bucket \"-tmp\"")
	}
	// Without a HOME to relativise against, a directory outside the temp
	// root falls through to no match.
	if ompProjectDir("", filepath.Join(sessions, "-data-agents-scanner"), "/data/agents/scanner") {
		t.Error("an absolute directory matched a home-relative bucket with no HOME")
	}
}
