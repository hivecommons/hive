package mailin

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// Minimal file heads for each accepted type.
var (
	pngData  = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01")
	jpegData = []byte("\xFF\xD8\xFF\xE0\x00\x10JFIF\x00")
	gifData  = []byte("GIF89a\x01\x00\x01\x00")
	webpData = []byte("RIFF\x1a\x00\x00\x00WEBPVP8 \x0e\x00\x00\x00")
	m4aData  = []byte("\x00\x00\x00\x20ftypM4A \x00\x00\x00\x00M4A isom")
	mp3Data  = []byte("ID3\x04\x00\x00\x00\x00\x00\x00")
	mp3Sync  = []byte{0xFF, 0xFB, 0x90, 0x64, 0x00}
	wavData  = []byte("RIFF\x24\x00\x00\x00WAVEfmt \x10\x00\x00\x00")
	oggData  = []byte("OggS\x00\x02\x00\x00\x00\x00")
	webmData = []byte("\x1A\x45\xDF\xA3\x9F\x42\x86\x81\x01")
	mdData   = []byte("# Notes\n\n- one\n- two\r\n\tindented\f\n")
)

func TestSniff(t *testing.T) {
	cases := []struct {
		data []byte
		want string
	}{
		{pngData, "image/png"}, {jpegData, "image/jpeg"}, {gifData, "image/gif"}, {webpData, "image/webp"},
		{m4aData, "audio/mp4"}, {mp3Data, "audio/mpeg"}, {mp3Sync, "audio/mpeg"}, {wavData, "audio/wave"},
		{oggData, "audio/ogg"}, {webmData, "audio/webm"}, {mdData, "text/markdown"},
		{[]byte("\x00\x00\x00\x20ftypqt  \x00"), ""},
		{[]byte("MZ\x90\x00\x03\x00\x00\x00"), ""},
		{[]byte("\x7fELF\x02\x01\x01"), ""},
		{[]byte{0xC3, 0x28}, ""},
		{append([]byte(strings.Repeat("a", sniffLen+10)), 0x01), ""},
	}
	for i, c := range cases {
		if got := sniff(c.data); got != c.want {
			t.Errorf("case %d: %q, want %q", i, got, c.want)
		}
	}
}

func TestAcceptFile(t *testing.T) {
	ok := map[string][]byte{
		"notes.md": mdData, "README.markdown": mdData, "memo.m4a": m4aData, "call.mp3": mp3Data,
		"rec.wav": wavData, "voice.ogg": oggData, "clip.webm": webmData, "shot.png": pngData,
		"photo.JPG": jpegData, "photo.jpeg": jpegData, "anim.gif": gifData, "pic.webp": webpData,
	}
	for name, data := range ok {
		a, accepted := acceptFile(name, data)
		if !accepted || a.Name != name || a.Size != len(data) {
			t.Errorf("%s rejected: %+v", name, a)
		}
	}
	if a, _ := acceptFile("memo.m4a", m4aData); a.Kind != wire.AttachAudio || a.ContentType != "audio/mp4" {
		t.Errorf("kind %+v", a)
	}
	bad := map[string][]byte{
		"evil.exe":      []byte("MZ\x90\x00"),
		"fake.png":      jpegData,
		"script.md":     []byte("MZ\x90\x00\x01"),
		"empty.md":      nil,
		"noext":         mdData,
		"archive.zip":   []byte("PK\x03\x04"),
		"image.svg":     []byte("<svg></svg>"),
		"song.mp3.exe":  mp3Data,
		"invoice.pdf":   []byte("%PDF-1.7"),
		"voice.m4a.png": m4aData,
	}
	for name, data := range bad {
		if _, accepted := acceptFile(name, data); accepted {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd.md":            "passwd.md",
		`C:\Users\me\notes.md`:           "notes.md",
		"we\x00ird\nname.md":             "weirdname.md",
		"..":                             "",
		"/":                              "",
		strings.Repeat("ä", 300) + ".md": strings.Repeat("ä", maxNameRunes-3) + ".md",
		"x." + strings.Repeat("e", 200):  "",
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
