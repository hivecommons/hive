package logread

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// rec renders one hive log record the way slog's JSON handler writes it.
func rec(i int, level, msg string) string {
	return fmt.Sprintf(`{"time":%q,"level":%q,"msg":%q}`, base.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano), level, msg)
}

func recs(from, to int) []string {
	var out []string
	for i := from; i < to; i++ {
		out = append(out, rec(i, "INFO", fmt.Sprintf("line %04d", i)))
	}
	return out
}

func content(lines []string) []byte {
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func writePlain(t *testing.T, dir, name string, lines []string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), content(lines), 0o600); err != nil {
		t.Fatal(err)
	}
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeGz(t *testing.T, dir, name string, lines []string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), gzipBytes(t, content(lines)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func backupName(i int) string {
	return "hive-" + base.Add(time.Duration(i)*time.Hour).Format(backupTimeFormat) + ".log"
}

func texts(res *Result) []string {
	out := make([]string, 0, len(res.Lines))
	for _, l := range res.Lines {
		out = append(out, l.Text)
	}
	return out
}

func mustInstant(t *testing.T, raw string) time.Time {
	t.Helper()
	v, err := ParseInstant("since", raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// readAll pages backwards until the read reports no cursor, returning every
// line oldest first.
func readAll(t *testing.T, req Request, opts Options) ([]string, int) {
	t.Helper()
	var pages [][]string
	for calls := 1; calls < 1000; calls++ {
		res := Read(req, opts)
		if res.Status != StatusOK {
			t.Fatalf("page %d: status %s (%s)", calls, res.Status, res.Reason)
		}
		pages = append(pages, texts(res))
		if res.NextCursor == "" {
			var all []string
			for i := len(pages) - 1; i >= 0; i-- {
				all = append(all, pages[i]...)
			}
			return all, calls
		}
		req.Cursor = res.NextCursor
	}
	t.Fatal("paging did not terminate")
	return nil, 0
}

func TestReadTailDefault(t *testing.T) {
	dir := t.TempDir()
	writePlain(t, dir, HiveLogFilename, recs(0, 150))
	now := base.Add(time.Hour)
	res := Read(Request{}, Options{LogDir: dir, Now: func() time.Time { return now }})
	if res.Status != StatusOK || res.Source != SourceHive {
		t.Fatalf("status=%s source=%s", res.Status, res.Source)
	}
	if res.LinesApplied != DefaultLines || res.LineCount != DefaultLines || len(res.Lines) != DefaultLines {
		t.Fatalf("applied=%d count=%d", res.LinesApplied, res.LineCount)
	}
	first, last := res.Lines[0], res.Lines[len(res.Lines)-1]
	if !strings.Contains(first.Text, "line 0050") || !strings.Contains(last.Text, "line 0149") {
		t.Fatalf("want lines 50..149 oldest first, got %q .. %q", first.Text, last.Text)
	}
	if last.Source != SourceHive || last.Level != "info" || last.Time != base.Add(149*time.Second).Format(time.RFC3339Nano) {
		t.Fatalf("line shape = %+v", last)
	}
	if !res.HasMore || res.NextCursor == "" {
		t.Fatal("older lines remain: want has_more and a cursor")
	}
	if res.HiveNow != now.Format(time.RFC3339Nano) || res.Untrusted != UntrustedMarker {
		t.Fatalf("hive_now=%s untrusted=%q", res.HiveNow, res.Untrusted)
	}
	if !res.Coverage.Complete || res.Coverage.WorkBoundBytes != DefaultWorkBoundBytes {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
}

func TestReadDropsUnfinishedTrailingRecord(t *testing.T) {
	dir := t.TempDir()
	data := append(content(recs(0, 3)), []byte(`{"time":"2026-03-01T12:00:09Z","lev`)...)
	if err := os.WriteFile(filepath.Join(dir, HiveLogFilename), data, 0o600); err != nil {
		t.Fatal(err)
	}
	res := Read(Request{}, Options{LogDir: dir})
	if res.LineCount != 3 || !strings.Contains(res.Lines[2].Text, "line 0002") {
		t.Fatalf("lines = %q", texts(res))
	}
}

func TestSourcesAndUnknownSource(t *testing.T) {
	dir := t.TempDir()
	writePlain(t, dir, HiveLogFilename, recs(0, 2))
	list := Sources(Options{LogDir: dir})
	if list.Default != SourceHive || len(list.Sources) != 1 || !list.Sources[0].Available || !list.Sources[0].Default || list.Untrusted != UntrustedMarker {
		t.Fatalf("sources = %+v", list)
	}
	for _, tc := range []struct {
		name string
		dir  string
	}{
		{"unconfigured", ""},
		{"missing", filepath.Join(dir, "absent")},
		{"empty", t.TempDir()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := Sources(Options{LogDir: tc.dir}).Sources[0]
			if info.Available || info.Reason == "" {
				t.Fatalf("info = %+v", info)
			}
			res := Read(Request{}, Options{LogDir: tc.dir})
			if res.Status != StatusNotAvailable || res.Reason != info.Reason || res.Untrusted != UntrustedMarker {
				t.Fatalf("res = %+v", res)
			}
		})
	}
	res := Read(Request{Source: "kernel"}, Options{LogDir: dir})
	if res.Status != StatusNotAvailable || res.Source != "kernel" || res.Reason == "" || res.Lines == nil || res.Untrusted != UntrustedMarker {
		t.Fatalf("unknown source = %+v", res)
	}
}

func TestReadBounds(t *testing.T) {
	t.Run("clamp", func(t *testing.T) {
		dir := t.TempDir()
		var short []string
		for i := 0; i < 1200; i++ {
			short = append(short, fmt.Sprintf("x%04d", i))
		}
		writePlain(t, dir, HiveLogFilename, short)
		res := Read(Request{Lines: 5000}, Options{LogDir: dir})
		if res.LinesRequested != 5000 || res.LinesApplied != MaxLines || !res.LinesClamped || res.LineCount != MaxLines {
			t.Fatalf("requested=%d applied=%d clamped=%v count=%d", res.LinesRequested, res.LinesApplied, res.LinesClamped, res.LineCount)
		}
	})
	t.Run("byte cap before line count", func(t *testing.T) {
		dir := t.TempDir()
		var lines []string
		for i := 0; i < 200; i++ {
			lines = append(lines, rec(i, "INFO", fmt.Sprintf("%04d %s", i, strings.Repeat("x", 2048))))
		}
		writePlain(t, dir, HiveLogFilename, lines)
		res := Read(Request{Lines: 1000}, Options{LogDir: dir})
		if !res.ByteCapped || !res.HasMore || res.NextCursor == "" || res.LineCount >= 200 || res.LineCount == 0 {
			t.Fatalf("byte_capped=%v has_more=%v count=%d", res.ByteCapped, res.HasMore, res.LineCount)
		}
		b, _ := json.Marshal(res)
		if len(b) > MaxResultBytes {
			t.Fatalf("result is %d bytes, cap %d", len(b), MaxResultBytes)
		}
		all, _ := readAll(t, Request{Lines: 1000}, Options{LogDir: dir})
		if len(all) != 200 {
			t.Fatalf("paging returned %d lines, want 200", len(all))
		}
	})
	t.Run("long line cut", func(t *testing.T) {
		dir := t.TempDir()
		long := rec(0, "INFO", strings.Repeat("é", 10*1024))
		writePlain(t, dir, HiveLogFilename, []string{long, rec(1, "INFO", "short")})
		res := Read(Request{}, Options{LogDir: dir})
		if res.LineCount != 2 || res.CutLines != 1 {
			t.Fatalf("count=%d cut=%d", res.LineCount, res.CutLines)
		}
		l := res.Lines[0]
		if !l.Cut || l.FullBytes != len(long) || len(l.Text) > MaxLineBytes || !strings.HasPrefix(long, l.Text) {
			t.Fatalf("cut=%v full=%d len=%d", l.Cut, l.FullBytes, len(l.Text))
		}
		if res.Lines[1].Cut {
			t.Fatal("short line flagged cut")
		}
	})
}

func TestParseInstant(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
		ok   bool
	}{
		{"", "", true},
		{"2026-03-01T12:00:00Z", "2026-03-01T12:00:00Z", true},
		{"2026-03-01T07:00:00-05:00", "2026-03-01T12:00:00Z", true},
		{"2026-03-01T12:00:00.5+01:00", "2026-03-01T11:00:00.5Z", true},
		{"2026-03-01T12:00:00", "", false},
		{"2026-03-01 12:00:00", "", false},
		{"yesterday", "", false},
	} {
		got, err := ParseInstant("since", tc.raw)
		if (err == nil) != tc.ok {
			t.Fatalf("%q: err=%v", tc.raw, err)
		}
		if err != nil {
			if !strings.Contains(err.Error(), "explicit offset") {
				t.Fatalf("%q: error %q does not explain the offset", tc.raw, err)
			}
			continue
		}
		if tc.want != "" && got.UTC().Format(time.RFC3339Nano) != tc.want {
			t.Fatalf("%q -> %s", tc.raw, got.UTC().Format(time.RFC3339Nano))
		}
	}
}

func TestReadWindow(t *testing.T) {
	dir := t.TempDir()
	lines := append(recs(0, 100), "plain text without a time")
	writePlain(t, dir, HiveLogFilename, lines)
	since := mustInstant(t, "2026-03-01T07:00:10-05:00")
	until := mustInstant(t, "2026-03-01T12:00:19Z")
	res := Read(Request{Since: since, Until: until}, Options{LogDir: dir})
	if res.LineCount != 10 || !strings.Contains(res.Lines[0].Text, "line 0010") || !strings.Contains(res.Lines[9].Text, "line 0019") {
		t.Fatalf("lines = %q", texts(res))
	}
	if res.Window.Since != "2026-03-01T12:00:10Z" || res.Window.Until != "2026-03-01T12:00:19Z" {
		t.Fatalf("window = %+v", res.Window)
	}
	if res.UntimedDropped != 1 || !res.Coverage.Complete || res.HasMore || res.NextCursor != "" {
		t.Fatalf("untimed=%d coverage=%+v has_more=%v", res.UntimedDropped, res.Coverage, res.HasMore)
	}
	if res.WindowBeforeOldestRetained {
		t.Fatal("window starts inside retention")
	}
}

func TestReadLevelAndTextFilters(t *testing.T) {
	dir := t.TempDir()
	writePlain(t, dir, HiveLogFilename, []string{
		rec(0, "DEBUG", "debug detail"),
		rec(1, "INFO", "Spawned Worker"),
		rec(2, "WARN", "worker slow"),
		rec(3, "ERROR", "Worker crashed hard"),
		rec(4, "ERROR+2", "worker crashed: noisy"),
		"panic: unlevelled worker stack",
		rec(5, "WARN", "disk nearly full"),
	})
	for _, tc := range []struct {
		name       string
		req        Request
		want       []string
		unlevelled int
	}{
		{"min warn", Request{MinLevel: "warn"}, []string{"worker slow", "Worker crashed hard", "worker crashed: noisy", "disk nearly full"}, 1},
		{"min error", Request{MinLevel: "error"}, []string{"Worker crashed hard", "worker crashed: noisy"}, 1},
		{"include case-insensitive", Request{Include: "WORKER"}, []string{"Spawned Worker", "worker slow", "Worker crashed hard", "worker crashed: noisy", "panic: unlevelled worker stack"}, 0},
		{"include and exclude", Request{Include: "worker", Exclude: []string{"CRASHED", "", "spawned"}}, []string{"worker slow", "panic: unlevelled worker stack"}, 0},
		{"level and include", Request{MinLevel: "warn", Include: "worker", Exclude: []string{"noisy"}}, []string{"worker slow", "Worker crashed hard"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := Read(tc.req, Options{LogDir: dir})
			got := texts(res)
			if len(got) != len(tc.want) {
				t.Fatalf("got %q", got)
			}
			for i := range got {
				if !strings.Contains(got[i], tc.want[i]) {
					t.Fatalf("line %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
			if res.UnlevelledDropped != tc.unlevelled {
				t.Fatalf("unlevelled_dropped = %d, want %d", res.UnlevelledDropped, tc.unlevelled)
			}
		})
	}
}

func TestParseMinLevel(t *testing.T) {
	for raw, want := range map[string]string{"": "", "DEBUG": "debug", " Info ": "info", "warning": "warn", "error": "error"} {
		got, err := ParseMinLevel(raw)
		if err != nil || got != want {
			t.Fatalf("%q -> %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"fatal", "warn+2", "loud"} {
		if _, err := ParseMinLevel(raw); err == nil {
			t.Fatalf("%q accepted", raw)
		}
	}
}

func TestReadCompressedHistoryAndRetention(t *testing.T) {
	dir := t.TempDir()
	writeGz(t, dir, backupName(0)+".gz", recs(0, 50))
	writePlain(t, dir, backupName(1), recs(50, 100))
	// A rotated file still being compressed exists in both forms; the plain
	// copy is complete and is read once.
	writePlain(t, dir, backupName(2), recs(100, 150))
	writeGz(t, dir, backupName(2)+".gz", recs(100, 120))
	writePlain(t, dir, HiveLogFilename, recs(150, 200))
	writePlain(t, dir, "other.log", []string{"not the hive's log"})
	writePlain(t, dir, "hive-garbage.log", []string{"not a rotated file"})

	res := Read(Request{Lines: 1000}, Options{LogDir: dir})
	if res.LineCount != 200 || !strings.Contains(res.Lines[0].Text, "line 0000") || !strings.Contains(res.Lines[199].Text, "line 0199") {
		t.Fatalf("count=%d", res.LineCount)
	}
	for i, l := range res.Lines {
		if !strings.Contains(l.Text, fmt.Sprintf("line %04d", i)) {
			t.Fatalf("line %d = %q", i, l.Text)
		}
	}
	if res.Coverage.FilesScanned != 4 || !res.Coverage.Complete || res.Coverage.CoveredFrom != base.Format(time.RFC3339Nano) {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
	if res.OldestRetained != base.Format(time.RFC3339Nano) || res.WindowBeforeOldestRetained {
		t.Fatalf("oldest=%s before=%v", res.OldestRetained, res.WindowBeforeOldestRetained)
	}

	early := Read(Request{Since: base.Add(-time.Hour)}, Options{LogDir: dir})
	if !early.WindowBeforeOldestRetained || early.OldestRetained != base.Format(time.RFC3339Nano) {
		t.Fatalf("window before retention not flagged: %+v", early)
	}
}

func TestReadPartialHistoryFiles(t *testing.T) {
	dir := t.TempDir()
	full := gzipBytes(t, content(recs(0, 400)))
	if err := os.WriteFile(filepath.Join(dir, backupName(0)+".gz"), full[:len(full)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backupName(1)+".gz"), []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	cut := content(recs(400, 410))
	if err := os.WriteFile(filepath.Join(dir, backupName(2)), cut[:len(cut)-7], 0o600); err != nil {
		t.Fatal(err)
	}
	writePlain(t, dir, HiveLogFilename, recs(410, 420))
	res := Read(Request{Lines: 1000}, Options{LogDir: dir})
	if res.Status != StatusOK {
		t.Fatalf("status %s", res.Status)
	}
	want := map[string]bool{backupName(0) + ".gz": true, backupName(1) + ".gz": true, backupName(2): true}
	if len(res.PartialFiles) != len(want) {
		t.Fatalf("partial_files = %q", res.PartialFiles)
	}
	for _, name := range res.PartialFiles {
		if !want[name] {
			t.Fatalf("partial_files = %q", res.PartialFiles)
		}
	}
	if res.LineCount < 20 {
		t.Fatalf("readable lines of partial files dropped: %d", res.LineCount)
	}
}

func TestReadWorkBound(t *testing.T) {
	dir := t.TempDir()
	writePlain(t, dir, backupName(0), recs(0, 100))
	writePlain(t, dir, HiveLogFilename, recs(100, 200))
	opts := Options{LogDir: dir, WorkBoundBytes: 1024}

	res := Read(Request{Include: "no such text"}, opts)
	if res.LineCount != 0 || res.Coverage.Complete || res.Coverage.StoppedReason != StoppedWorkBound || res.NextCursor == "" {
		t.Fatalf("incomplete scan must say so: count=%d coverage=%+v cursor=%q", res.LineCount, res.Coverage, res.NextCursor)
	}
	if res.Coverage.WorkBoundBytes != 1024 || res.Coverage.ScannedBytes > 1024 || res.Coverage.CoveredFrom == "" {
		t.Fatalf("coverage = %+v", res.Coverage)
	}

	all, calls := readAll(t, Request{Lines: 1000}, opts)
	if calls < 10 || len(all) != 200 {
		t.Fatalf("calls=%d lines=%d", calls, len(all))
	}
	for i, text := range all {
		if !strings.Contains(text, fmt.Sprintf("line %04d", i)) {
			t.Fatalf("gap or overlap at %d: %q", i, text)
		}
	}
}

func TestReadCompressedFileBeyondWorkBound(t *testing.T) {
	dir := t.TempDir()
	writeGz(t, dir, backupName(0)+".gz", recs(0, 100))
	writePlain(t, dir, HiveLogFilename, recs(100, 105))
	opts := Options{LogDir: dir, WorkBoundBytes: 2048}

	res := Read(Request{Lines: 1000}, opts)
	if res.LineCount != 5 || res.Coverage.Complete || res.Coverage.StoppedReason != StoppedWorkBound || res.NextCursor == "" {
		t.Fatalf("count=%d coverage=%+v", res.LineCount, res.Coverage)
	}
	next := Read(Request{Lines: 1000, Cursor: res.NextCursor}, opts)
	if next.Status != StatusOK || next.LineCount != 0 || next.Coverage.StoppedReason != StoppedFileTooLarge || next.NextCursor != "" {
		t.Fatalf("next = %+v", next)
	}
	if got := WorkBound(0); got != DefaultWorkBoundBytes {
		t.Fatalf("WorkBound(0) = %d", got)
	}
	if got := WorkBound(DefaultWorkBoundBytes * 2); got != DefaultWorkBoundBytes {
		t.Fatalf("a configured bound cannot raise the default: %d", got)
	}
}

func TestReadPagingAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	writeGz(t, dir, backupName(0)+".gz", recs(0, 120))
	writePlain(t, dir, HiveLogFilename, recs(120, 250))
	opts := Options{LogDir: dir}

	page1 := Read(Request{Lines: 100}, opts)
	if page1.LineCount != 100 || page1.NextCursor == "" {
		t.Fatalf("page1 = %d", page1.LineCount)
	}
	// The hive rotates between calls: hive.log is renamed and compressed,
	// and a new hive.log receives newer lines.
	data, err := os.ReadFile(filepath.Join(dir, HiveLogFilename))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backupName(1)+".gz"), gzipBytes(t, data), 0o600); err != nil {
		t.Fatal(err)
	}
	writePlain(t, dir, HiveLogFilename, recs(250, 260))

	page2 := Read(Request{Lines: 100, Cursor: page1.NextCursor}, opts)
	page3 := Read(Request{Lines: 100, Cursor: page2.NextCursor}, opts)
	if page3.NextCursor != "" || page3.HasMore {
		t.Fatalf("page3 cursor=%q has_more=%v", page3.NextCursor, page3.HasMore)
	}
	var all []string
	for _, p := range []*Result{page3, page2, page1} {
		all = append(all, texts(p)...)
	}
	if len(all) != 250 {
		t.Fatalf("got %d lines across pages, want 250", len(all))
	}
	for i, text := range all {
		if !strings.Contains(text, fmt.Sprintf("line %04d", i)) {
			t.Fatalf("gap or overlap at %d: %q", i, text)
		}
	}
}

func TestReadCursorExpired(t *testing.T) {
	dir := t.TempDir()
	writePlain(t, dir, backupName(0), recs(0, 50))
	writePlain(t, dir, HiveLogFilename, recs(50, 60))
	opts := Options{LogDir: dir}
	page1 := Read(Request{Lines: 20}, opts)
	if page1.NextCursor == "" {
		t.Fatal("no cursor")
	}
	if err := os.Remove(filepath.Join(dir, backupName(0))); err != nil {
		t.Fatal(err)
	}
	for name, cur := range map[string]string{
		"history deleted": page1.NextCursor,
		"malformed":       "zz-not-a-cursor",
		"wrong json":      encodeCursor(cursor{Source: SourceHive})[:10],
		"other source":    encodeCursor(cursor{Source: "kernel", Digest: "ab", Offset: 1}),
	} {
		res := Read(Request{Cursor: cur}, opts)
		if res.Status != StatusCursorExpired || res.Reason == "" || res.LineCount != 0 || res.Untrusted != UntrustedMarker {
			t.Fatalf("%s: %+v", name, res)
		}
	}
	// A cursor into hive.log beyond its end (the file was truncated and
	// rewritten with the same first line) is expired too.
	writePlain(t, dir, HiveLogFilename, recs(50, 60))
	res := Read(Request{Lines: 5}, opts)
	writePlain(t, dir, HiveLogFilename, recs(50, 51))
	big := mustDecode(t, res.NextCursor)
	big.Offset = 1 << 20
	if got := Read(Request{Cursor: encodeCursor(big)}, opts); got.Status != StatusCursorExpired {
		t.Fatalf("beyond end: %+v", got)
	}
}

func mustDecode(t *testing.T, s string) cursor {
	t.Helper()
	c, err := decodeCursor(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestReadScrubbing(t *testing.T) {
	const dashToken = "dash-token-0123456789abcdef"
	const passphrase = "correct-horse-battery-staple"
	const fileSecret = "file-secret-value-998877"
	secretDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secretDir, "relay"), []byte(fileSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	basic := base64.StdEncoding.EncodeToString([]byte("hive:" + dashToken))
	values := CollectSecretValues(SecretSources{
		Values:     []string{dashToken, "hive:" + dashToken, "short-value"},
		Environ:    []string{"FOO_PASSPHRASE=" + passphrase, "HOME=/home/hive-user-dir", "SSH_KEY_FILE=/etc/hive/some/long/path"},
		SecretDirs: []string{secretDir},
	})
	opts := Options{Secrets: NewSecretScrubber(values)}

	dir := t.TempDir()
	opts.LogDir = dir
	writePlain(t, dir, HiveLogFilename, []string{
		rec(0, "INFO", "env passphrase "+passphrase),
		rec(1, "INFO", "dashboard token "+dashToken+" in url ?t="+dashToken),
		rec(2, "INFO", "probe header Authorization: Basic "+basic),
		rec(3, "INFO", "embedded creds="+basic+" end"),
		rec(4, "INFO", "relay secret "+fileSecret),
		rec(5, "INFO", "short-value stays"),
		rec(6, "INFO", "already [REDACTED] upstream"),
		"-----BEGIN OPENSSH PRIVATE KEY-----",
		"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW",
		"QyNTUxOQAAACBkZXBsb3kta2V5LWJvZHktbGluZS10d28AAAAAAAAAAAAAAAAAAAAAAAAA",
		"-----END OPENSSH PRIVATE KEY-----",
		rec(7, "INFO", "after the key"),
		"-----BEGIN RSA PRIVATE KEY-----",
		"MIIEpAIBAAKCAQEAunterminatedkeybodyline0000000000000000000000000000000",
	})
	raw := []string{dashToken, passphrase, fileSecret, basic, "b3BlbnNzaC1rZXkt", "QyNTUxOQAAACBkZX", "MIIEpAIBAAKCAQEA"}

	// One page at a time, two lines per page, so the private key is split
	// across page boundaries.
	var all []string
	redactions := map[string]int{}
	req := Request{Lines: 2}
	preExisting := 0
	for {
		res := Read(req, opts)
		all = append(texts(res), all...)
		for _, r := range res.Redactions {
			redactions[r.Kind] += r.Count
		}
		preExisting += res.PreExistingMarkerLines
		if res.NextCursor == "" {
			break
		}
		req.Cursor = res.NextCursor
	}
	joined := strings.Join(all, "\n")
	for _, secret := range raw {
		if strings.Contains(joined, secret) {
			t.Fatalf("raw value %q survived scrubbing:\n%s", secret, joined)
		}
	}
	if !strings.Contains(joined, "short-value stays") || !strings.Contains(joined, "after the key") {
		t.Fatalf("unrelated text was redacted:\n%s", joined)
	}
	if redactions[SecretValueKind] < 6 || redactions[PrivateKeyKind] < 6 || preExisting != 1 {
		t.Fatalf("redactions=%v pre_existing=%d", redactions, preExisting)
	}
	single := Read(Request{Lines: 1000}, opts)
	for _, l := range single.Lines {
		if strings.Contains(l.Text, "already [REDACTED]") != l.PreExistingMarker {
			t.Fatalf("pre-existing flag wrong on %q", l.Text)
		}
	}
	if single.PreExistingMarkerLines != 1 {
		t.Fatalf("pre_existing_marker_lines = %d", single.PreExistingMarkerLines)
	}

	// The text filters see scrubbed text only: a raw secret matches nothing.
	for _, secret := range raw[:4] {
		res := Read(Request{Include: secret}, opts)
		if res.LineCount != 0 {
			t.Fatalf("include %q matched %q", secret, texts(res))
		}
	}
	res := Read(Request{Exclude: []string{dashToken}}, opts)
	if res.LineCount != single.LineCount {
		t.Fatal("exclude matched a raw secret value")
	}
	if res := Read(Request{Include: "<redacted:" + SecretValueKind + ">"}, opts); res.LineCount < 4 {
		t.Fatalf("include of the marker matched %d lines", res.LineCount)
	}
}

func TestSecretScrubber(t *testing.T) {
	const v = "abcdefghijklmnop"
	s := NewSecretScrubber([]string{v, "short", "  "})
	for _, form := range []string{
		v,
		base64.StdEncoding.EncodeToString([]byte("x" + v + "yz")),
		base64.StdEncoding.EncodeToString([]byte("xy" + v)),
		base64.URLEncoding.EncodeToString([]byte("user:" + v)),
	} {
		out, n := s.Replace("before " + form + " after")
		if n == 0 || strings.Contains(out, v) || strings.Contains(out, form) {
			t.Fatalf("%q -> %q (%d)", form, out, n)
		}
	}
	if out, n := s.Replace("short and unrelated"); n != 0 || out != "short and unrelated" {
		t.Fatalf("no-op replaced: %q %d", out, n)
	}
	var nilScrubber *SecretScrubber
	if out, n := nilScrubber.Replace("x"); out != "x" || n != 0 {
		t.Fatal("nil scrubber changed text")
	}
	if out, n := NewSecretScrubber(nil).Replace("x"); out != "x" || n != 0 {
		t.Fatal("empty scrubber changed text")
	}
	url := NewSecretScrubber([]string{"p@ss word/with+chars"})
	if out, n := url.Replace("q=p%40ss+word%2Fwith%2Bchars and p@ss%20word%2Fwith+chars"); n != 2 || strings.Contains(out, "word") {
		t.Fatalf("url forms: %q %d", out, n)
	}
}

func TestSecretEnvNameAndCollection(t *testing.T) {
	for name, want := range map[string]bool{
		"HIVE_TOKEN": true, "FOO_PASSPHRASE": true, "db_password": true, "API_KEY": true, "AWS_SECRET_ACCESS_KEY": true,
		"GH_CREDENTIALS": true, "SSH_KEY_FILE": false, "TOKEN_PATH": false, "SECRET_DIR": false, "HOME": false, "PATH": false,
	} {
		if got := SecretEnvName(name); got != want {
			t.Fatalf("SecretEnvName(%q) = %v", name, got)
		}
	}
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "top"), "top-level-secret-1\n-----BEGIN X-----\nsecond-line-secret\n")
	write(filepath.Join(root, "a", "nested"), "nested-secret-value")
	write(filepath.Join(deep, "too-deep"), "too-deep-secret-value")
	write(filepath.Join(root, "huge"), strings.Repeat("h", maxSecretFileBytes+1))
	got := strings.Join(CollectSecretValues(SecretSources{SecretDirs: []string{root, "", filepath.Join(root, "missing")}, Environ: []string{"NOEQUALS"}}), "|")
	for _, want := range []string{"top-level-secret-1", "second-line-secret", "nested-secret-value"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "too-deep") || strings.Contains(got, "hhhh") || strings.Contains(got, "|-----BEGIN X-----|") {
		t.Fatalf("collected too much: %q", got)
	}
}

func TestKeySpans(t *testing.T) {
	lines := []string{
		"tail of a key from an older page",
		"-----END PRIVATE KEY----- then text",
		"plain",
		"x -----BEGIN EC PRIVATE KEY----- body -----END EC PRIVATE KEY----- y",
		"-----BEGIN PRIVATE KEY-----",
		"open body",
	}
	var out []string
	for i, sp := range keySpans(lines) {
		out = append(out, applySpans(lines[i], sp))
	}
	want := []string{
		privateKeyMarker,
		privateKeyMarker + " then text",
		"plain",
		"x " + privateKeyMarker + " y",
		privateKeyMarker,
		privateKeyMarker,
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, out[i], want[i])
		}
	}
	if startsInsideKey([]string{"none", "-----BEGIN PRIVATE KEY-----"}) || startsInsideKey(nil) {
		t.Fatal("BEGIN first is not inside a key")
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c := cursor{Source: SourceHive, Time: "2026-03-01T12:00:00Z", Digest: "abc", Offset: 42}
	enc := encodeCursor(c)
	if strings.Trim(enc, "0123456789abcdef") != "" {
		t.Fatalf("cursor %q is not lowercase hex", enc)
	}
	got, err := decodeCursor(enc)
	c.V = cursorVersion
	if err != nil || got != c {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for _, bad := range []string{"", "0g", encodeCursor(cursor{Source: SourceHive, Digest: "a", Offset: -2})} {
		if _, err := decodeCursor(bad); err == nil {
			t.Fatalf("%q decoded", bad)
		}
	}
	if _, _, ok := fileIdentity(nil); ok {
		t.Fatal("empty head has an identity")
	}
	if tm, d, ok := fileIdentity([]byte("no time here")); !ok || tm != "" || d == "" {
		t.Fatal("untimed head identity")
	}
}

func TestParseHead(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		ok    bool
		level string
	}{
		{rec(0, "WARN", "x"), true, "warn"},
		{`{"time":"2026-03-01T12:00:00Z"}`, true, ""},
		{`{"time":"2026-03-01T12:00:00Z","level":"INFO`, true, ""},
		{`{"time":"bad","level":"INFO"}`, false, ""},
		{`{"time":"2026-03-01T12:00:00Z`, false, ""},
		{`time=2026-03-01T12:00:00Z level=INFO`, false, ""},
	} {
		when, level := parseHead(tc.raw)
		if !when.IsZero() != tc.ok || level != tc.level {
			t.Fatalf("%q -> %v %q", tc.raw, when, level)
		}
	}
}

func TestClampLines(t *testing.T) {
	for _, tc := range []struct {
		in, want int
		clamped  bool
	}{{0, 100, false}, {-3, 100, false}, {7, 7, false}, {1000, 1000, false}, {1001, 1000, true}} {
		got, clamped := ClampLines(tc.in)
		if got != tc.want || clamped != tc.clamped {
			t.Fatalf("ClampLines(%d) = %d %v", tc.in, got, clamped)
		}
	}
}

func TestScanFallsBackToCompressedTwin(t *testing.T) {
	dir := t.TempDir()
	writeGz(t, dir, backupName(0)+".gz", recs(0, 3))
	res := &Result{Source: SourceHive, LinesApplied: 10}
	s := newScan(Request{}, Options{LogDir: dir}, res)
	f := &logFile{path: filepath.Join(dir, backupName(0)), name: backupName(0)}
	if err := s.scanFile(f, -1); err != nil || len(s.collected) != 3 || !f.gz {
		t.Fatalf("err=%v collected=%d gz=%v", err, len(s.collected), f.gz)
	}
	if err := s.scanFile(&logFile{path: filepath.Join(dir, "absent.log"), name: "absent.log", current: true}, -1); err == nil {
		t.Fatal("missing current file read without error")
	}
}

func TestSplitRegion(t *testing.T) {
	lines, offsets := splitRegion([]byte("tail\nfirst\r\nsecond"), 100)
	if strings.Join(lines, "|") != "first|second" || offsets[0] != 105 || offsets[1] != 112 {
		t.Fatalf("%q %v", lines, offsets)
	}
	if lines, _ := splitRegion([]byte("no newline"), 5); lines != nil {
		t.Fatal("mid-line fragment kept")
	}
}

func TestReadLineLongerThanWorkBound(t *testing.T) {
	dir := t.TempDir()
	writePlain(t, dir, HiveLogFilename, []string{rec(0, "INFO", "first"), rec(1, "INFO", strings.Repeat("y", 4096))})
	res := Read(Request{}, Options{LogDir: dir, WorkBoundBytes: 1024})
	if res.LineCount != 0 || res.Coverage.Complete || res.Coverage.StoppedReason != StoppedLineTooLarge || res.NextCursor != "" {
		t.Fatalf("count=%d coverage=%+v cursor=%q", res.LineCount, res.Coverage, res.NextCursor)
	}
}
