// Package pdfbox is the sandbox side of Inbox PDF uploads: the hive-pdftext
// command that reads one untrusted PDF and prints its text as JSON.
//
// PDF parsers have a long history of bugs, so the hive never parses an
// uploaded PDF in its own process. It runs hive-pdftext the way it runs the
// whisper sandbox: as the low-privilege hive-transcribe uid (through
// su-exec), with an empty environment, under prlimit (address space, CPU
// seconds, file size, open files, no core dumps) and nice, in an empty
// network namespace where the kernel allows it, with the PDF on stdin and a
// wall-clock timeout after which the whole process group is killed.
// hive-pdftext keeps the PDF in a private 0700 work dir that it removes on
// exit, caps pages and characters, and writes only JSON to stdout.
//
// Text is extracted with github.com/ledongthuc/pdf (BSD-3-Clause, pure Go),
// the library the knowledge document parser already uses.
package pdfbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ledongthuc/pdf"
)

// Defaults shared by the sandbox and the hive side.
const (
	// DefaultSandbox is the hive-pdftext path in the hive image.
	DefaultSandbox = "/usr/local/bin/hive-pdftext"
	// DefaultMaxPages caps the pages read.
	DefaultMaxPages = 200
	// DefaultMaxChars caps the characters returned.
	DefaultMaxChars = 1 << 20
	// DefaultMemoryMB caps the sandbox's address space.
	DefaultMemoryMB = 1024
	// DefaultTimeout bounds one extraction.
	DefaultTimeout = 60 * time.Second
	// maxOpenFiles / niceness for the sandbox.
	maxOpenFiles = 32
	niceness     = "10"
	// cpuLimitMargin is added to RLIMIT_CPU on top of the timeout, so the
	// wall-clock timeout fires first in normal operation.
	cpuLimitMargin = 10
	// dirMode / fileMode for the private work dir and the PDF in it.
	dirMode  = 0o700
	fileMode = 0o600
	// umask makes everything the sandbox creates private.
	umask = 0o077
)

// Exit codes of hive-pdftext.
const (
	ExitOK       = 0
	ExitUsage    = 2
	ExitTooLarge = 3
	ExitParse    = 5
	ExitSetup    = 7
)

// Result is what hive-pdftext prints: the page count, how many pages were
// read, each read page's text, the document title, and whether a cap cut it.
type Result struct {
	Pages     int      `json:"pages"`
	Read      int      `json:"read"`
	Text      []string `json:"text"`
	Title     string   `json:"title,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

// HasText reports whether any read page has text (a scanned PDF has none).
func (r Result) HasText() bool {
	for _, t := range r.Text {
		if strings.TrimSpace(t) != "" {
			return true
		}
	}
	return false
}

// Options configure one sandbox run.
type Options struct {
	// MaxBytes caps the PDF read from stdin (0 disables).
	MaxBytes int64
	// MaxPages and MaxChars cap the output (0 uses the defaults).
	MaxPages int
	MaxChars int
	// WorkRoot is where the private work dir is created ("" = os.TempDir).
	WorkRoot string
	// OnWorkDir, when set, is called with the work dir once it exists (tests).
	OnWorkDir func(string)
}

// Error is a sandbox failure with its exit code.
type Error struct {
	Code int
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func fail(code int, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Run reads one PDF from stdin into a private work dir, extracts its text
// within the caps and writes the Result as JSON to stdout. The work dir is
// removed whatever happens.
func Run(ctx context.Context, o Options, stdin io.Reader, stdout io.Writer) error {
	if o.MaxPages <= 0 {
		o.MaxPages = DefaultMaxPages
	}
	if o.MaxChars <= 0 {
		o.MaxChars = DefaultMaxChars
	}
	syscall.Umask(umask)
	work, err := os.MkdirTemp(o.WorkRoot, "hive-pdftext-")
	if err != nil {
		return fail(ExitSetup, "work dir: %v", err)
	}
	defer os.RemoveAll(work)
	if err := os.Chmod(work, dirMode); err != nil {
		return fail(ExitSetup, "work dir mode: %v", err)
	}
	if o.OnWorkDir != nil {
		o.OnWorkDir(work)
	}
	input := filepath.Join(work, "input.pdf")
	size, err := copyCapped(input, stdin, o.MaxBytes)
	if err != nil {
		return err
	}
	f, err := os.Open(input)
	if err != nil {
		return fail(ExitSetup, "open input: %v", err)
	}
	defer f.Close()
	res, err := extract(ctx, f, size, o)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(res); err != nil {
		return fail(ExitSetup, "write result: %v", err)
	}
	return nil
}

// copyCapped writes stdin to path (0600), refusing more than limit bytes.
func copyCapped(path string, r io.Reader, limit int64) (int64, error) {
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return 0, fail(ExitSetup, "input file: %v", err)
	}
	defer out.Close()
	src := r
	if limit > 0 {
		src = io.LimitReader(r, limit+1)
	}
	n, err := io.Copy(out, src)
	if err != nil {
		return 0, fail(ExitSetup, "read input: %v", err)
	}
	if limit > 0 && n > limit {
		return 0, fail(ExitTooLarge, "the PDF is larger than %d bytes", limit)
	}
	if n == 0 {
		return 0, fail(ExitParse, "the PDF is empty")
	}
	return n, nil
}

// extract reads the pages; a panic inside the library is a parse failure.
func extract(ctx context.Context, f io.ReaderAt, size int64, o Options) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fail(ExitParse, "the PDF could not be read")
		}
	}()
	reader, rerr := pdf.NewReader(f, size)
	if rerr != nil {
		if errors.Is(rerr, pdf.ErrInvalidPassword) {
			return Result{}, fail(ExitParse, "the PDF is encrypted")
		}
		return Result{}, fail(ExitParse, "the PDF could not be read: %v", rerr)
	}
	res.Pages = reader.NumPage()
	res.Title = strings.Join(strings.Fields(reader.Trailer().Key("Info").Key("Title").Text()), " ")
	chars := 0
	for i := 1; i <= res.Pages; i++ {
		if ctx.Err() != nil {
			return Result{}, fail(ExitParse, "reading the PDF took too long")
		}
		if i > o.MaxPages {
			res.Truncated = true
			break
		}
		text := pageText(reader.Page(i))
		if chars+len(text) > o.MaxChars {
			text = strings.ToValidUTF8(text[:max(0, o.MaxChars-chars)], "")
			res.Truncated = true
		}
		chars += len(text)
		res.Text = append(res.Text, text)
		res.Read++
		if res.Truncated {
			break
		}
	}
	return res, nil
}

// pageText is one page's plain text with whitespace normalised per line; a
// page the library cannot read has none.
func pageText(p pdf.Page) (out string) {
	defer func() {
		if recover() != nil {
			out = ""
		}
	}()
	if p.V.IsNull() {
		return ""
	}
	text, err := p.GetPlainText(nil)
	if err != nil {
		return ""
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		lines = append(lines, strings.Join(strings.Fields(strings.ToValidUTF8(l, "")), " "))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// LimitPrefix is the prlimit + nice argv for one sandbox run.
func LimitPrefix(memoryMB int, timeout time.Duration, maxBytes int64) []string {
	return []string{
		"prlimit",
		"--as=" + strconv.FormatInt(int64(memoryMB)<<20, 10),
		"--cpu=" + strconv.Itoa(int(timeout.Seconds())+cpuLimitMargin),
		"--fsize=" + strconv.FormatInt(maxBytes+1, 10),
		"--nofile=" + strconv.Itoa(maxOpenFiles),
		"--core=0",
		"--", "nice", "-n", niceness,
	}
}

// NetIsolationPrefix runs a command in a fresh user and network namespace
// (only a down loopback), the same isolation the whisper sandbox uses.
func NetIsolationPrefix() []string {
	return []string{"unshare", "--user", "--map-current-user", "--net", "--"}
}
