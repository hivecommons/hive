package mailin

import (
	"archive/zip"
	"bytes"
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
	mkvData  = []byte("\x1A\x45\xDF\xA3\xA3\x42\x86\x81\x01\x42\x82\x88matroska")
	mp4Data  = []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")
	m4vData  = []byte("\x00\x00\x00\x18ftypM4V \x00\x00\x00\x00M4V isom")
	movData  = []byte("\x00\x00\x00\x14ftypqt  \x00\x00\x00\x00qt  ")
	oldMov   = []byte("\x00\x00\x00\x08wide\x00\x00\x00\x10mdat")
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
		{[]byte("\x00\x00\x00\x20ftypqt  \x00"), ctQuickTime},
		{mp4Data, ctMP4Generic}, {m4vData, ctVideoMP4}, {movData, ctQuickTime}, {oldMov, ctQuickTime}, {mkvData, ctMatroska},
		{[]byte("\x00\x00\x00\x20ftypXXXX\x00"), ""},
		{[]byte("\x00\x00\x00\x20ftyp"), ""},
		{[]byte("\x00\x00\x00\x20blob\x00\x00"), ""},
		// Text that happens to spell an atom name is still Markdown.
		{[]byte("one free day"), ctMarkdown},
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
	// Video, by extension and sniffed container (#18564).
	for name, want := range map[string]struct {
		data []byte
		kind string
		ct   string
	}{
		"clip.mp4":   {mp4Data, wire.AttachVideo, ctVideoMP4},
		"clip2.mp4":  {m4vData, wire.AttachVideo, ctVideoMP4},
		"phone.mov":  {movData, wire.AttachVideo, ctQuickTime},
		"old.mov":    {oldMov, wire.AttachVideo, ctQuickTime},
		"film.mkv":   {mkvData, wire.AttachVideo, ctMatroska},
		"memo2.m4a":  {mp4Data, wire.AttachAudio, ctAudioMP4},
		"voice.webm": {webmData, wire.AttachAudio, ctWebM},
	} {
		a, ok := acceptFile(name, want.data)
		if !ok || a.Kind != want.kind || a.ContentType != want.ct {
			t.Errorf("%s: %v %+v", name, ok, a)
		}
	}
	for name, data := range map[string][]byte{"film.mkv": webmData, "clip.mp4": movData[:8], "song.m4a": m4vData, "movie.mov": mkvData} {
		if _, ok := acceptFile(name, data); ok {
			t.Errorf("%s accepted with the wrong content", name)
		}
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
		"invoice.pdf":   pngData,
		"deck.pptm":     []byte("PK\x03\x04"),
		"old.doc":       []byte("\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1"),
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

// ooxml builds a ZIP container holding the named parts.
func ooxml(t *testing.T, parts ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("<x/>")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var pdfData = []byte("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n1 0 obj\n")

// Office documents (hivecommons/hive#18699).
func pptxData(t *testing.T) []byte {
	return ooxml(t, ooxmlContentTypes, "_rels/.rels", "ppt/presentation.xml", "ppt/slides/slide1.xml")
}

func docxData(t *testing.T) []byte { return ooxml(t, ooxmlContentTypes, "word/document.xml") }

func xlsxData(t *testing.T) []byte { return ooxml(t, ooxmlContentTypes, "xl/workbook.xml") }

// TestAcceptDocuments (hivecommons/hive#18699): PDF, Word, PowerPoint and
// Excel files are accepted by extension and content; a container that is
// another kind of document, a plain archive, a macro-enabled or a damaged
// one is not.
func TestAcceptDocuments(t *testing.T) {
	pptx, docx, xlsx := pptxData(t), docxData(t), xlsxData(t)
	for name, want := range map[string]struct {
		data []byte
		ct   string
	}{
		"report.pdf": {pdfData, ctPDF}, "Deck.PPTX": {pptx, ctPPTX}, "memo.docx": {docx, ctDOCX}, "numbers.xlsx": {xlsx, ctXLSX},
	} {
		a, ok := acceptFile(name, want.data)
		if !ok || a.Kind != wire.AttachDocument || a.ContentType != want.ct || a.Size != len(want.data) {
			t.Errorf("%s: %v %+v", name, ok, a)
		}
	}
	if got := sniff(pdfData); got != ctPDF {
		t.Errorf("pdf sniffed as %q", got)
	}
	if got := sniff(pptx); got != ctZip {
		t.Errorf("pptx sniffed as %q", got)
	}
	macro := ooxml(t, ooxmlContentTypes, "ppt/presentation.xml", "ppt/vbaProject.bin")
	for name, data := range map[string][]byte{
		"deck.pptx":      docx,
		"memo.docx":      xlsx,
		"numbers.xlsx":   pptx,
		"archive.pptx":   ooxml(t, "readme.txt"),
		"notypes.pptx":   ooxml(t, "ppt/presentation.xml"),
		"macro.pptx":     macro,
		"truncated.pptx": pptx[:len(pptx)/2],
		"head.pptx":      []byte("PK\x03\x04"),
		"fake.pdf":       pptx,
		"fake.docx":      pdfData,
		"deck.png":       pptx,
	} {
		if _, ok := acceptFile(name, data); ok {
			t.Errorf("%s accepted", name)
		}
	}
}
