package wire

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func attachment(name string, data []byte) Attachment {
	sum := sha256.Sum256(data)
	return Attachment{Name: name, Kind: AttachVideo, ContentType: "video/mp4", Size: len(data), SHA256: hex.EncodeToString(sum[:]), Data: data}
}

func encode(t *testing.T, d Delivery) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := WriteMultipart(mw, d); err != nil {
		t.Fatal(err)
	}
	return mw.Boundary(), buf.Bytes()
}

// keep is a ReadMultipart store that keeps the bytes in memory.
func keep(files *[][]byte) func(int, Attachment, io.Reader) (string, error) {
	return func(i int, _ Attachment, body io.Reader) (string, error) {
		b, err := io.ReadAll(body)
		*files = append(*files, b)
		return "kept-" + string(rune('0'+i)), err
	}
}

func TestMultipartRoundTrip(t *testing.T) {
	dir := t.TempDir()
	onDisk := []byte("video bytes on disk")
	path := filepath.Join(dir, "v")
	if err := os.WriteFile(path, onDisk, 0o600); err != nil {
		t.Fatal(err)
	}
	disk := attachment("clip.mp4", onDisk)
	disk.Data, disk.Path = nil, path
	d := Delivery{ID: "id1", Kind: KindFiles, Recipient: "hive@acme", Files: &Files{Title: "t", Note: "hi",
		Attachments: []Attachment{attachment("a.mp4", []byte("in memory")), disk}}}
	boundary, body := encode(t, d)
	if bytes.Contains(body, []byte(`"data"`)) {
		t.Fatal("attachment bytes leaked into the delivery part")
	}
	var files [][]byte
	got, err := ReadMultipart(bytes.NewReader(body), boundary, keep(&files))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "id1" || got.Files.Note != "hi" || len(files) != 2 || string(files[0]) != "in memory" || !bytes.Equal(files[1], onDisk) {
		t.Fatalf("round trip %+v %q", got, files)
	}
	if got.Files.Attachments[1].Path != "kept-1" || got.Files.Attachments[1].Data != nil {
		t.Fatalf("path not recorded %+v", got.Files.Attachments[1])
	}
	// The caller's delivery is unchanged.
	if string(d.Files.Attachments[0].Data) != "in memory" {
		t.Fatal("WriteMultipart modified its input")
	}
	if FilesBytes(d) != int64(len("in memory")+len(onDisk)) || FilesBytes(Delivery{}) != 0 {
		t.Fatal("FilesBytes")
	}

	// No files at all.
	boundary, body = encode(t, Delivery{ID: "p", Kind: KindProof})
	if got, err := ReadMultipart(bytes.NewReader(body), boundary, keep(&files)); err != nil || got.ID != "p" {
		t.Fatalf("no files: %v %+v", err, got)
	}
	// A missing file on disk fails the writer.
	gone := disk
	gone.Path = filepath.Join(dir, "gone")
	mw := multipart.NewWriter(io.Discard)
	if err := WriteMultipart(mw, Delivery{Files: &Files{Attachments: []Attachment{gone}}}); err == nil {
		t.Fatal("missing file written")
	}
}

func TestReadMultipartRejects(t *testing.T) {
	good := Delivery{ID: "id", Kind: KindFiles, Files: &Files{Attachments: []Attachment{attachment("a.mp4", []byte("abc"))}}}
	tampered := good
	tampered.Files = &Files{Attachments: []Attachment{attachment("a.mp4", []byte("abc"))}}
	tampered.Files.Attachments[0].SHA256 = strings.Repeat("0", 64)
	short := good
	short.Files = &Files{Attachments: []Attachment{attachment("a.mp4", []byte("abc"))}}
	short.Files.Attachments[0].Size = 2
	negative := good
	negative.Files = &Files{Attachments: []Attachment{attachment("a.mp4", []byte("abc"))}}
	negative.Files.Attachments[0].Size = -1

	var files [][]byte
	for name, d := range map[string]Delivery{"digest": tampered, "size": short, "negative": negative} {
		boundary, body := encode(t, d)
		if _, err := ReadMultipart(bytes.NewReader(body), boundary, keep(&files)); !errors.Is(err, ErrMultipart) {
			t.Errorf("%s: %v", name, err)
		}
	}

	raw := func(parts ...[2]string) (string, []byte) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for _, p := range parts {
			w, _ := mw.CreatePart(partHeader(p[0], "application/octet-stream"))
			_, _ = io.WriteString(w, p[1])
		}
		_ = mw.Close()
		return mw.Boundary(), buf.Bytes()
	}
	meta := `{"id":"id","kind":"files","files":{"title":"","attachments":[{"name":"a","size":3,"sha256":"` + attachment("", []byte("abc")).SHA256 + `"}]}}`
	cases := map[string][][2]string{
		"no parts":       nil,
		"wrong first":    {{PartFile, "abc"}},
		"bad json":       {{PartDelivery, "{"}},
		"missing file":   {{PartDelivery, meta}},
		"wrong name":     {{PartDelivery, meta}, {PartDelivery, "abc"}},
		"trailing part":  {{PartDelivery, meta}, {PartFile, "abc"}, {PartFile, "x"}},
		"oversized file": {{PartDelivery, meta}, {PartFile, "abcd"}},
	}
	for name, parts := range cases {
		boundary, body := raw(parts...)
		if _, err := ReadMultipart(bytes.NewReader(body), boundary, keep(&files)); !errors.Is(err, ErrMultipart) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A store error is returned as is.
	boom := errors.New("disk full")
	boundary, body := raw([2]string{PartDelivery, meta}, [2]string{PartFile, "abc"})
	if _, err := ReadMultipart(bytes.NewReader(body), boundary, func(int, Attachment, io.Reader) (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Fatalf("store error: %v", err)
	}
}

func TestChunkDelivery(t *testing.T) {
	id := strings.Repeat("ab", 16)
	if got := ChunkDelivery(id, 2, 5); got != id+".2.5" {
		t.Fatalf("chunk id %q", got)
	}
	if gid, i, n, ok := ParseChunkDelivery(id + ".2.5"); !ok || gid != id || i != 2 || n != 5 {
		t.Fatalf("parse %q %d %d %v", gid, i, n, ok)
	}
	for _, bad := range []string{"", id, id + ".5.5", id + ".0.0", id + ".1.9999", "XYZ" + id[3:] + ".0.1", id + ".-1.2", id + ".0.1.2"} {
		if _, _, _, ok := ParseChunkDelivery(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}
