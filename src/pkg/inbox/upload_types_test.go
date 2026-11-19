package inbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readUploadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "uploads", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSniffUploadAcceptsTextDocuments(t *testing.T) {
	cases := []struct {
		file, name, want string
	}{
		{"notes.md", "notes.md", UploadKindMarkdown},
		{"data.json", "data.json", UploadKindJSON},
		{"events.jsonl", "events.jsonl", UploadKindJSONL},
		{"events.jsonl", "events.ndjson", UploadKindJSONL},
		{"config.yaml", "config.yaml", UploadKindYAML},
		{"config.yaml", "config.YML", UploadKindYAML},
		{"table.csv", "table.csv", UploadKindCSV},
		{"table.tsv", "table.tsv", UploadKindTSV},
		{"notes.md", "notes.txt", UploadKindText},
		// No extension: decided from content.
		{"data.json", "data", UploadKindJSON},
		{"notes.md", "README", UploadKindText},
	}
	for _, tc := range cases {
		got, err := SniffUpload(tc.name, readUploadFixture(t, tc.file))
		if err != nil || got != tc.want {
			t.Errorf("SniffUpload(%s from %s) = %q, %v; want %q", tc.name, tc.file, got, err, tc.want)
		}
	}
	// A UTF-8 BOM is fine.
	if got, err := SniffUpload("a.txt", append([]byte{0xef, 0xbb, 0xbf}, "hello"...)); err != nil || got != UploadKindText {
		t.Fatalf("BOM text = %q, %v", got, err)
	}
}

func TestSniffUploadRejectsDisguisedAndUnsupported(t *testing.T) {
	pe := append([]byte("MZ\x90\x00\x03\x00\x00\x00"), []byte(strings.Repeat("This program cannot be run in DOS mode. ", 4))...)
	cases := []struct {
		name string
		data []byte
		code string
		msg  string
	}{
		{"report.txt", pe, UploadErrUnsupported, "Windows executable"},
		{"notes.md", []byte("\x7fELF\x02\x01\x01rest"), UploadErrUnsupported, "Linux executable"},
		{"data.json", []byte{0xcf, 0xfa, 0xed, 0xfe, 7, 0, 0, 1}, UploadErrUnsupported, "macOS executable"},
		{"table.csv", []byte("PK\x03\x04\x14\x00rest"), UploadErrUnsupported, "zip archive"},
		{"paper.txt", []byte("%PDF-1.7\n%rest"), UploadErrUnsupported, "a PDF"},
		{"letter.doc", []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1, 0}, UploadErrUnsupported, "save it as .docx or PDF"},
		{"letter.doc", []byte("plain words"), UploadErrUnsupported, "save it as .docx or PDF"},
		{"song.txt", []byte("ID3\x04\x00rest"), UploadErrUnsupported, "audio"},
		{"clip.md", []byte("\x00\x00\x00\x18ftypmp42"), UploadErrUnsupported, "audio or video"},
		{"tool.exe", []byte("echo hi"), UploadErrUnsupported, `".exe"`},
		{"run.sh", []byte("#!/bin/sh\necho hi\n"), UploadErrUnsupported, `".sh"`},
		{"deck.pptx", []byte("text"), UploadErrUnsupported, "PDF"},
		{"utf16.txt", []byte{0xff, 0xfe, 'h', 0, 'i', 0}, UploadErrUnsupported, "UTF-8"},
		{"nul.txt", []byte("abc\x00def"), UploadErrUnsupported, "binary"},
		{"latin1.txt", []byte("caf\xe9"), UploadErrUnsupported, "UTF-8"},
		{"ctrl.txt", []byte(strings.Repeat("\x01\x02\x03a", 50)), UploadErrUnsupported, "binary"},
		{"empty.md", nil, UploadErrEmpty, "empty"},
	}
	for _, tc := range cases {
		kind, err := SniffUpload(tc.name, tc.data)
		if err == nil {
			t.Errorf("SniffUpload(%s) = %q, want %s", tc.name, kind, tc.code)
			continue
		}
		if err.Code != tc.code || !strings.Contains(err.Message, tc.msg) {
			t.Errorf("SniffUpload(%s) = %s %q, want %s containing %q", tc.name, err.Code, err.Message, tc.code, tc.msg)
		}
	}
}

func TestUploadSupportedExtensionsSorted(t *testing.T) {
	exts := UploadSupportedExtensions()
	if len(exts) != len(uploadTextExtensions) {
		t.Fatalf("extensions = %v", exts)
	}
	for i := 1; i < len(exts); i++ {
		if exts[i-1] > exts[i] {
			t.Fatalf("not sorted: %v", exts)
		}
	}
	if !uploadKindIsMedia(UploadKindAudio) || !uploadKindIsMedia(UploadKindVideo) || uploadKindIsMedia(UploadKindJSON) {
		t.Fatal("media kinds")
	}
	if (&UploadError{Message: "m"}).Error() != "m" {
		t.Fatal("Error()")
	}
}
