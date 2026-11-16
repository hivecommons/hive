package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/inbox/pdfbox"
)

func TestRunVersionUsageAndExtract(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"-version"}, nil, &out, &errb); code != pdfbox.ExitOK || !strings.HasPrefix(out.String(), "hive-pdftext ") {
		t.Fatalf("-version = %d %q", code, out.String())
	}
	if code := run(context.Background(), []string{"-nope"}, nil, &out, &errb); code != pdfbox.ExitUsage {
		t.Fatalf("bad flag = %d", code)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "pkg", "inbox", "pdfbox", "testdata", "text.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run(context.Background(), []string{"-work-root", t.TempDir()}, bytes.NewReader(data), &out, &errb); code != pdfbox.ExitOK {
		t.Fatalf("extract = %d %s", code, errb.String())
	}
	var res pdfbox.Result
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Pages != 2 || !res.HasText() {
		t.Fatalf("result = %+v, %v", res, err)
	}
	errb.Reset()
	if code := run(context.Background(), []string{"-max-bytes", "10", "-work-root", t.TempDir()}, bytes.NewReader(data), &out, &errb); code != pdfbox.ExitTooLarge || !strings.Contains(errb.String(), "larger") {
		t.Fatalf("too large = %d %q", code, errb.String())
	}
	if code := run(context.Background(), []string{"-work-root", t.TempDir()}, strings.NewReader("%PDF-1.4 garbage"), &out, &errb); code != pdfbox.ExitParse {
		t.Fatalf("garbage = %d", code)
	}
}

func TestRunMainVersion(t *testing.T) {
	if code := runMain([]string{"-version"}); code != pdfbox.ExitOK {
		t.Fatalf("runMain -version = %d", code)
	}
}
