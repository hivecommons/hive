package pdfbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixtures are hand-built PDF 1.4 files: text.pdf has two pages of
// WinAnsi Helvetica text and an Info title; scanned.pdf has two pages that
// only paint rectangles (no text layer), like a scan.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func runPDF(t *testing.T, o Options, data []byte) (Result, error) {
	t.Helper()
	if o.WorkRoot == "" {
		o.WorkRoot = t.TempDir()
	}
	var out bytes.Buffer
	err := Run(context.Background(), o, bytes.NewReader(data), &out)
	if err != nil {
		return Result{}, err
	}
	var res Result
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("output is not JSON: %q", out.String())
	}
	return res, nil
}

func TestRunExtractsTextPDF(t *testing.T) {
	res, err := runPDF(t, Options{}, fixture(t, "text.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Pages != 2 || res.Read != 2 || res.Truncated || !res.HasText() || res.Title != "Inbox test PDF" {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Text[0], "Hello from the Inbox") || !strings.Contains(res.Text[1], "Second page text") {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestRunScannedPDFHasNoText(t *testing.T) {
	res, err := runPDF(t, Options{}, fixture(t, "scanned.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Pages != 2 || res.HasText() {
		t.Fatalf("scanned = %+v", res)
	}
}

func TestRunCapsPagesCharsAndBytes(t *testing.T) {
	data := fixture(t, "text.pdf")
	res, err := runPDF(t, Options{MaxPages: 1}, data)
	if err != nil || res.Read != 1 || !res.Truncated || res.Pages != 2 {
		t.Fatalf("page cap = %+v, %v", res, err)
	}
	res, err = runPDF(t, Options{MaxChars: 5}, data)
	if err != nil || !res.Truncated || len(strings.Join(res.Text, "")) > 5 {
		t.Fatalf("char cap = %+v, %v", res, err)
	}
	var pe *Error
	if _, err := runPDF(t, Options{MaxBytes: 100}, data); !errors.As(err, &pe) || pe.Code != ExitTooLarge {
		t.Fatalf("byte cap = %v", err)
	}
	if _, err := runPDF(t, Options{}, nil); !errors.As(err, &pe) || pe.Code != ExitParse {
		t.Fatalf("empty = %v", err)
	}
	for _, bad := range [][]byte{[]byte("%PDF-1.7\nnot really"), bytes.Repeat([]byte{0xff}, 512), data[:len(data)/2]} {
		if _, err := runPDF(t, Options{}, bad); !errors.As(err, &pe) || pe.Code != ExitParse {
			t.Fatalf("malformed = %v", err)
		}
	}
}

func TestRunWorkDirIsPrivateAndRemoved(t *testing.T) {
	root := t.TempDir()
	var seen string
	_, err := runPDF(t, Options{WorkRoot: root, OnWorkDir: func(dir string) {
		seen = dir
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != dirMode {
			t.Errorf("work dir mode = %v, %v", info.Mode().Perm(), err)
		}
	}}, fixture(t, "text.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if seen == "" || !strings.HasPrefix(seen, root) {
		t.Fatalf("work dir = %q", seen)
	}
	if _, err := os.Stat(seen); !os.IsNotExist(err) {
		t.Fatal("the work dir must be removed after the run")
	}
}

func TestPrefixes(t *testing.T) {
	p := strings.Join(LimitPrefix(DefaultMemoryMB, 30*time.Second, 1000), " ")
	for _, want := range []string{"prlimit", "--as=1073741824", "--cpu=40", "--fsize=1001", "--core=0", "-- nice -n 10"} {
		if !strings.Contains(p, want) {
			t.Errorf("limit prefix %q missing %q", p, want)
		}
	}
	if NetIsolationPrefix()[0] != "unshare" {
		t.Fatal("isolation prefix")
	}
	if (&Error{Msg: "m"}).Error() != "m" {
		t.Fatal("Error()")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("broken pipe") }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestRunSetupAndIOFailures(t *testing.T) {
	var pe *Error
	missing := filepath.Join(t.TempDir(), "does", "not", "exist")
	if err := Run(context.Background(), Options{WorkRoot: missing}, bytes.NewReader(fixture(t, "text.pdf")), &bytes.Buffer{}); !errors.As(err, &pe) || pe.Code != ExitSetup {
		t.Fatalf("missing work root = %v", err)
	}
	if err := Run(context.Background(), Options{WorkRoot: t.TempDir()}, failingReader{}, &bytes.Buffer{}); !errors.As(err, &pe) || pe.Code != ExitSetup || !strings.Contains(pe.Msg, "read input") {
		t.Fatalf("stdin error = %v", err)
	}
	if err := Run(context.Background(), Options{WorkRoot: t.TempDir()}, bytes.NewReader(fixture(t, "text.pdf")), failingWriter{}); !errors.As(err, &pe) || pe.Code != ExitSetup {
		t.Fatalf("stdout error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, Options{WorkRoot: t.TempDir()}, bytes.NewReader(fixture(t, "text.pdf")), &bytes.Buffer{}); !errors.As(err, &pe) || pe.Code != ExitParse || !strings.Contains(pe.Msg, "too long") {
		t.Fatalf("cancelled = %v", err)
	}
	if (Result{Text: []string{" ", ""}}).HasText() {
		t.Fatal("whitespace is not text")
	}
}
