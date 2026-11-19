package inbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inbox/pdfbox"
	"github.com/hivecommons/hive/pkg/transcribe"
)

// fakePDFSandbox writes a shell script standing in for hive-pdftext.
func fakePDFSandbox(t *testing.T, body string) *PDFRuntime {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hive-pdftext")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &PDFRuntime{Sandbox: path, RunAs: transcribe.WhisperRunAsSelf, Timeout: 5 * time.Second}
}

const pdfMagic = "%PDF-1.4\n"

func TestPDFRuntimeRunsTheSandbox(t *testing.T) {
	rt := fakePDFSandbox(t, `cat >/dev/null; echo '{"pages":3,"read":2,"text":["Hello page one",""],"title":"Deck","truncated":true}'`)
	res, uerr := rt.Run(context.Background(), []byte(pdfMagic))
	if uerr != nil || res.Pages != 3 || res.Title != "Deck" {
		t.Fatalf("run = %+v, %v", res, uerr)
	}
	ex, uerr := pdfMarkdown(res)
	if uerr != nil || !strings.Contains(ex.Markdown, "## Page 1\n\nHello page one") || strings.Contains(ex.Markdown, "## Page 2") || !strings.Contains(ex.Markdown, "Read 2 of 3 pages") || ex.Summary != "PDF, 3 pages" || ex.Title != "Deck" {
		t.Fatalf("markdown = %+v, %v", ex, uerr)
	}
	// The sandbox gets the configured caps and the PDF on stdin.
	argvFile := filepath.Join(t.TempDir(), "argv")
	rt = fakePDFSandbox(t, `echo "$@" > `+argvFile+`; wc -c | tr -d ' ' >> `+argvFile+`; echo '{"pages":1,"read":1,"text":["x"]}'`)
	rt.MaxPages, rt.MaxChars = 7, 99
	if _, uerr := rt.Run(context.Background(), []byte(pdfMagic+"body")); uerr != nil {
		t.Fatal(uerr)
	}
	got, _ := os.ReadFile(argvFile)
	if !strings.Contains(string(got), "-max-bytes 13 -max-pages 7 -max-chars 99") || !strings.Contains(string(got), "\n13") {
		t.Fatalf("argv/stdin = %q", got)
	}
}

func TestPDFRuntimeScannedAndFailures(t *testing.T) {
	rt := fakePDFSandbox(t, `cat >/dev/null; echo '{"pages":2,"read":2,"text":["",""]}'`)
	res, uerr := rt.Run(context.Background(), []byte(pdfMagic))
	if uerr != nil {
		t.Fatal(uerr)
	}
	if _, uerr := pdfMarkdown(res); uerr == nil || uerr.Message != pdfScannedMessage {
		t.Fatalf("scanned = %v", uerr)
	}
	for body, code := range map[string]string{
		`cat >/dev/null; echo "hive-pdftext: the PDF could not be read" >&2; exit 5`: UploadErrParse,
		`cat >/dev/null; exit 3`:                    UploadErrTooLarge,
		`cat >/dev/null; echo 'not json'`:           UploadErrParse,
		`cat >/dev/null; head -c 3000000 /dev/zero`: UploadErrParse,
	} {
		rt := fakePDFSandbox(t, body)
		rt.MaxChars = 10
		if _, uerr := rt.Run(context.Background(), []byte(pdfMagic)); uerr == nil || uerr.Code != code {
			t.Errorf("%s = %v, want %s", body, uerr, code)
		}
	}
	missing := &PDFRuntime{Sandbox: filepath.Join(t.TempDir(), "nope"), RunAs: transcribe.WhisperRunAsSelf}
	if _, uerr := missing.Run(context.Background(), []byte(pdfMagic)); uerr == nil || uerr.Code != UploadErrUnsupported || !strings.Contains(uerr.Message, "not installed") {
		t.Fatalf("missing sandbox = %v", uerr)
	}
	nouser := fakePDFSandbox(t, "true")
	nouser.RunAs = "hive-no-such-user-xyz"
	if ok, why := nouser.Available(); ok || !strings.Contains(why, "does not exist") {
		t.Fatalf("no user = %v %q", ok, why)
	}
}

// TestPDFRuntimeKillsOnTimeout pins the wall-clock limit: a sandbox that
// hangs (and a child it started) is killed, and the upload fails fast.
func TestPDFRuntimeKillsOnTimeout(t *testing.T) {
	rt := fakePDFSandbox(t, `sleep 30 & sleep 30`)
	rt.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, uerr := rt.Run(context.Background(), []byte(pdfMagic))
	if uerr == nil || !strings.Contains(uerr.Message, "took longer") {
		t.Fatalf("timeout = %v", uerr)
	}
	if elapsed := time.Since(start); elapsed > pdfWaitDelay+5*time.Second {
		t.Fatalf("the sandbox was not killed in time: %s", elapsed)
	}
}

func TestPDFRuntimeArgvAndDefaults(t *testing.T) {
	env := map[string]string{EnvPDFSandbox: "/x/hive-pdftext", EnvPDFRunAs: "pdfuser", EnvPDFMemoryMB: "256"}
	rt := DefaultPDFRuntime(func(k string) string { return env[k] })
	if rt.Sandbox != "/x/hive-pdftext" || rt.RunAs != "pdfuser" || rt.MemoryMB != 256 || !rt.Limits || !rt.NoNetwork || rt.Timeout != pdfbox.DefaultTimeout {
		t.Fatalf("runtime = %+v", rt)
	}
	rt = DefaultPDFRuntime(func(string) string { return "" })
	if rt.Sandbox != pdfbox.DefaultSandbox || rt.RunAs != transcribe.DefaultWhisperRunAs || rt.MemoryMB != pdfbox.DefaultMemoryMB {
		t.Fatalf("defaults = %+v", rt)
	}
	rt.SuExec, rt.NoNetwork = "/sbin/su-exec", false
	argv, err := rt.argv(context.Background(), 1234)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.HasPrefix(joined, "/sbin/su-exec hive-transcribe prlimit --as=1073741824 ") || !strings.Contains(joined, "--fsize=1235") || !strings.HasSuffix(joined, pdfbox.DefaultSandbox+" -max-bytes 1234 -max-pages 200 -max-chars 1048576") {
		t.Fatalf("argv = %q", joined)
	}
	tb := &tailBuffer{n: 4}
	_, _ = tb.Write([]byte("abcdef"))
	if string(tb.b) != "cdef" || lastLine("a\nb\n") != "b" {
		t.Fatal("tail")
	}
}

func TestUploadSourceReadsPDFInTheSandbox(t *testing.T) {
	e := newUploadEnv(t, config.InboxConfig{})
	e.src.PDF = fakePDFSandbox(t, `cat >/dev/null; echo '{"pages":1,"read":1,"text":["Quarterly numbers"],"title":"Q3 report"}'`)
	it, err := e.svc.AddUpload(context.Background(), UploadInput{Filename: "q3.pdf", Data: []byte(pdfMagic + "rest"), AddedBy: "ana"})
	if err != nil || it.UploadKind != UploadKindPDF || it.Title != "Q3 report" || it.Summary != "PDF, 1 page" {
		t.Fatalf("pdf item = %+v, %v", it, err)
	}
	e.src.PDF = fakePDFSandbox(t, `cat >/dev/null; echo '{"pages":1,"read":1,"text":[""]}'`)
	if _, err := e.svc.AddUpload(context.Background(), UploadInput{Filename: "scan.pdf", Data: []byte(pdfMagic)}); err == nil || !strings.Contains(err.Error(), "OCR not supported yet") {
		t.Fatalf("scanned upload = %v", err)
	}
	if k, uerr := SniffUpload("noext", []byte(pdfMagic)); uerr != nil || k != UploadKindPDF {
		t.Fatalf("pdf without extension = %q, %v", k, uerr)
	}
}
