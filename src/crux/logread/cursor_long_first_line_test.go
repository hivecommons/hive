package logread

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A file is identified by a digest of its first line. The digest was taken
// over the whole line when a page was cut, and over at most 64 KiB when the
// cursor was looked up, so a cursor into a file whose first line is longer
// than 64 KiB always came back cursor_expired and only the newest page of
// that file could be read.
func TestCursorIntoAFileWithALongFirstLine(t *testing.T) {
	for _, n := range []int{60 * 1024, maxHeadBytes - 1, maxHeadBytes, maxHeadBytes + 1, 70 * 1024, 300 * 1024} {
		dir := t.TempDir()
		lines := []string{rec(0, "INFO", strings.Repeat("A", n))}
		lines = append(lines, recs(1, 300)...)
		writePlain(t, dir, HiveLogFilename, lines)
		opts := Options{LogDir: dir}
		page1 := Read(Request{Lines: 100}, opts)
		if page1.LineCount != 100 || !page1.HasMore || page1.NextCursor == "" {
			t.Fatalf("first line of %d bytes: page 1 has %d lines, has_more=%v, cursor=%q", n, page1.LineCount, page1.HasMore, page1.NextCursor)
		}
		page2 := Read(Request{Lines: 100, Cursor: page1.NextCursor}, opts)
		if page2.Status != StatusOK || page2.LineCount != 100 {
			t.Errorf("first line of %d bytes: following next_cursor gave status %s with %d lines (%s), want ok with 100", n, page2.Status, page2.LineCount, page2.Reason)
		}
	}
}

func TestCursorIntoACompressedFileWithALongFirstLine(t *testing.T) {
	dir := t.TempDir()
	lines := []string{rec(0, "INFO", strings.Repeat("A", 70*1024))}
	lines = append(lines, recs(1, 300)...)
	writeGz(t, dir, backupName(0)+".gz", lines)
	writePlain(t, dir, HiveLogFilename, recs(300, 310))
	opts := Options{LogDir: dir}
	page1 := Read(Request{Lines: 100}, opts)
	page2 := Read(Request{Lines: 100, Cursor: page1.NextCursor}, opts)
	if page2.Status != StatusOK || page2.LineCount != 100 {
		t.Fatalf("following next_cursor into the compressed file gave status %s with %d lines (%s), want ok with 100", page2.Status, page2.LineCount, page2.Reason)
	}
}

// The output sources cut a line that never ends every 64 KiB of text, so the
// record on disk, wrapped in JSON, is longer than 64 KiB. When such a record
// opens the file, the same cursor failure hit proxy, litellm and hive-output.
func TestCursorIntoAnOutputSourceThatOpensWithARunawayLine(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: base}
	log := openTestOutput(t, dir, SourceLiteLLM, clock)
	w := log.Stream(StreamStdout)
	_, _ = w.Write([]byte(strings.Repeat("B", 200*1024) + "\n"))
	for i := 0; i < 300; i++ {
		clock.advance(time.Second)
		fmt.Fprintf(w, "ordinary line %04d\n", i)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	opts := Options{OutputDir: dir, LocalLiteLLM: true}
	page1 := Read(Request{Source: SourceLiteLLM, Lines: 100}, opts)
	page2 := Read(Request{Source: SourceLiteLLM, Lines: 100, Cursor: page1.NextCursor}, opts)
	if page2.Status != StatusOK || page2.LineCount != 100 {
		t.Fatalf("following next_cursor gave status %s with %d lines (%s), want ok with 100", page2.Status, page2.LineCount, page2.Reason)
	}
}

// Every page of such a file is reachable, and no line is lost or repeated.
func TestPagingThroughAFileWithALongFirstLineReturnsEveryLineOnce(t *testing.T) {
	dir := t.TempDir()
	lines := []string{rec(0, "INFO", strings.Repeat("A", 70*1024))}
	lines = append(lines, recs(1, 250)...)
	writePlain(t, dir, HiveLogFilename, lines)
	opts := Options{LogDir: dir}
	seen := 0
	cursor := ""
	for page := 0; page < 10; page++ {
		res := Read(Request{Lines: 100, Cursor: cursor}, opts)
		if res.Status != StatusOK {
			t.Fatalf("page %d: status %s (%s)", page, res.Status, res.Reason)
		}
		seen += res.LineCount
		if !res.HasMore {
			break
		}
		cursor = res.NextCursor
	}
	if seen != len(lines) {
		t.Fatalf("paged through %d lines, the file has %d", seen, len(lines))
	}
}

func TestHeadOf(t *testing.T) {
	long := strings.Repeat("x", maxHeadBytes+10)
	for name, tc := range map[string]struct {
		data string
		want int // length of the head, -1 for none
	}{
		"ordinary line":               {"abc\ndef\n", 3},
		"empty first line":            {"\nabc\n", 0},
		"no newline yet":              {"abc", -1},
		"empty":                       {"", -1},
		"line of exactly the cap":     {long[:maxHeadBytes] + "\nrest", maxHeadBytes},
		"line longer than the cap":    {long + "\nrest", maxHeadBytes},
		"cap-sized read, no newline":  {long[:maxHeadBytes], maxHeadBytes},
		"short read, no newline":      {long[:maxHeadBytes-1], -1},
		"longer read, no newline yet": {long, maxHeadBytes},
	} {
		got := headOf([]byte(tc.data))
		switch {
		case tc.want < 0 && got != nil:
			t.Errorf("%s: head has %d bytes, want none", name, len(got))
		case tc.want >= 0 && (got == nil || len(got) != tc.want):
			t.Errorf("%s: head = %d bytes (nil=%v), want %d", name, len(got), got == nil, tc.want)
		}
	}
}
