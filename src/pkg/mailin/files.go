package mailin

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// fileType is an accepted attachment extension: its Inbox kind and the
// sniffed content types that may back it, each mapped to the content type
// the file is stored as.
type fileType struct {
	kind  string
	sniff map[string]string
}

// Sniffed content types beyond the stored ones. An ISO base media file
// whose brand is shared by audio and video recorders is "application/mp4"
// until its extension says which.
const (
	ctMP4Generic = "application/mp4"
	ctAudioMP4   = "audio/mp4"
	ctVideoMP4   = "video/mp4"
	ctQuickTime  = "video/quicktime"
	ctMatroska   = "video/x-matroska"
	ctWebM       = "audio/webm"
	ctMarkdown   = "text/markdown"
	ctPDF        = "application/pdf"
	// ctZip is a ZIP container: an Office Open XML document until
	// verifyContainer has looked inside.
	ctZip  = "application/zip"
	ctDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	ctPPTX = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	ctXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

// Office Open XML containers (hivecommons/hive#18699): every one holds
// ooxmlContentTypes and its kind's main part; ooxmlMacroPart marks a
// macro-enabled document, which is refused whatever its extension says.
const (
	ooxmlContentTypes = "[Content_Types].xml"
	ooxmlMacroPart    = "vbaproject.bin"
)

// ooxmlMainPart is the part that tells a Word, PowerPoint or Excel
// container apart, by stored content type.
var ooxmlMainPart = map[string]string{
	ctDOCX: "word/document.xml",
	ctPPTX: "ppt/presentation.xml",
	ctXLSX: "xl/workbook.xml",
}

// verifyContainer looks inside a file stored as ct. A type that is not a
// container passes; an Office Open XML document must be a readable ZIP
// with its content types and main part, and no macro project.
func verifyContainer(ct string, r io.ReaderAt, size int64) bool {
	mainPart, ok := ooxmlMainPart[ct]
	if !ok {
		return true
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return false
	}
	var haveTypes, haveMain bool
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, "/")
		switch {
		case name == ooxmlContentTypes:
			haveTypes = true
		case name == mainPart:
			haveMain = true
		case strings.ToLower(path.Base(name)) == ooxmlMacroPart:
			return false
		}
	}
	return haveTypes && haveMain
}

func same(cts ...string) map[string]string {
	m := make(map[string]string, len(cts))
	for _, ct := range cts {
		m[ct] = ct
	}
	return m
}

// fileTypes is the mail-in allowlist. The type is decided by content
// sniffing; the extension only has to agree with it.
var fileTypes = map[string]fileType{
	".md":       {wire.AttachMarkdown, same(ctMarkdown)},
	".markdown": {wire.AttachMarkdown, same(ctMarkdown)},
	".m4a":      {wire.AttachAudio, map[string]string{ctAudioMP4: ctAudioMP4, ctMP4Generic: ctAudioMP4}},
	".mp3":      {wire.AttachAudio, same("audio/mpeg")},
	".wav":      {wire.AttachAudio, same("audio/wave")},
	".ogg":      {wire.AttachAudio, same("audio/ogg")},
	// WebM (audio or video) goes to transcription as it is.
	".webm": {wire.AttachAudio, same(ctWebM)},
	".mp4":  {wire.AttachVideo, map[string]string{ctVideoMP4: ctVideoMP4, ctMP4Generic: ctVideoMP4, ctAudioMP4: ctVideoMP4}},
	".mov":  {wire.AttachVideo, map[string]string{ctQuickTime: ctQuickTime, ctMP4Generic: ctQuickTime, ctVideoMP4: ctQuickTime}},
	".mkv":  {wire.AttachVideo, same(ctMatroska)},
	".png":  {wire.AttachImage, same("image/png")},
	".jpg":  {wire.AttachImage, same("image/jpeg")},
	".jpeg": {wire.AttachImage, same("image/jpeg")},
	".gif":  {wire.AttachImage, same("image/gif")},
	".webp": {wire.AttachImage, same("image/webp")},
	// Office documents (#18699): stored and attached, never opened here.
	".pdf":  {wire.AttachDocument, same(ctPDF)},
	".docx": {wire.AttachDocument, map[string]string{ctZip: ctDOCX}},
	".pptx": {wire.AttachDocument, map[string]string{ctZip: ctPPTX}},
	".xlsx": {wire.AttachDocument, map[string]string{ctZip: ctXLSX}},
}

// AcceptedExtensions is the allowlist as the SMTP reply lists it.
const AcceptedExtensions = "md, pdf, docx, pptx, xlsx, m4a, mp3, wav, ogg, webm, mp4, mov, mkv, png, jpg, gif, webp"

// maxNameRunes bounds an attachment file name.
const maxNameRunes = 120

// sniffLen is how much of a file content sniffing looks at.
const sniffLen = 512

// ISO base media "ftyp" brands: audio only, video only, and the generic
// ones both kinds of recorder write.
var (
	m4aBrands   = []string{"M4A ", "M4B "}
	videoBrands = []string{"M4V ", "M4VH", "M4VP", "avc1", "iso4", "iso5", "iso6", "3gp4", "3gp5", "3gp6", "3g2a", "MSNV", "mp71"}
	mp4Brands   = []string{"mp42", "isom", "mp41", "iso2", "dash"}
	qtBrands    = []string{"qt  "}
	// qtAtoms start a QuickTime movie written without an ftyp box.
	qtAtoms = []string{"moov", "mdat", "wide", "free", "skip", "pnot"}
)

func hasString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// sniffISO names an ISO base media / QuickTime head, "" for none.
func sniffISO(head []byte) string {
	if len(head) < 8 {
		return ""
	}
	box := string(head[4:8])
	if box != "ftyp" {
		// A box size starts with a zero byte below 16 MiB; text never does.
		if head[0] == 0 && hasString(qtAtoms, box) {
			return ctQuickTime
		}
		return ""
	}
	if len(head) < 12 {
		return ""
	}
	switch brand := string(head[8:12]); {
	case hasString(m4aBrands, brand):
		return ctAudioMP4
	case hasString(videoBrands, brand):
		return ctVideoMP4
	case hasString(qtBrands, brand):
		return ctQuickTime
	case hasString(mp4Brands, brand):
		return ctMP4Generic
	}
	return ""
}

// sniffHead names the content type of a file's first bytes among the
// binary types mail-in accepts, "" for anything else. A ZIP head is only
// ctZip here; verifyContainer decides whether it is an office document.
func sniffHead(head []byte) string {
	if len(head) > sniffLen {
		head = head[:sniffLen]
	}
	if ct := sniffISO(head); ct != "" {
		return ct
	}
	switch {
	case bytes.HasPrefix(head, []byte("ID3")),
		len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0 && head[1]&0x06 != 0:
		// ID3 tag or an MPEG audio frame sync with a valid layer.
		return "audio/mpeg"
	case len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		return "audio/wave"
	case bytes.HasPrefix(head, []byte("OggS\x00")):
		return "audio/ogg"
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return ctPDF
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		return ctZip
	case bytes.HasPrefix(head, []byte("\x1A\x45\xDF\xA3")):
		// EBML: the DocType says Matroska or WebM.
		if bytes.Contains(head, []byte("matroska")) {
			return ctMatroska
		}
		return ctWebM
	}
	switch ct := http.DetectContentType(head); ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return ct
	}
	return ""
}

// sniff names the content type of b among the types mail-in accepts, ""
// for anything else.
func sniff(b []byte) string {
	if ct := sniffHead(b); ct != "" {
		return ct
	}
	if isMarkdownText(b) {
		return ctMarkdown
	}
	return ""
}

// isMarkdownText accepts valid UTF-8 text with no NUL or other binary
// control bytes; Markdown has no magic number.
func isMarkdownText(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, c := range b {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' && c != '\f' {
			return false
		}
	}
	return true
}

// classify checks a file name against the allowlist. It returns the
// accepted type, the cleaned name and whether the extension is allowed.
func classify(name string) (fileType, string, bool) {
	name = safeName(name)
	ft, ok := fileTypes[strings.ToLower(path.Ext(name))]
	return ft, name, ok
}

// match returns the stored content type when the sniffed type ct backs ft.
func (ft fileType) match(ct string) (string, bool) {
	stored, ok := ft.sniff[ct]
	return stored, ok
}

// acceptFile checks one attachment held in memory against the allowlist:
// the extension must be allowed and the sniffed type must match it.
func acceptFile(name string, data []byte) (wire.Attachment, bool) {
	ft, name, ok := classify(name)
	if !ok || len(data) == 0 {
		return wire.Attachment{}, false
	}
	ct, ok := ft.match(sniff(data))
	if !ok || !verifyContainer(ct, bytes.NewReader(data), int64(len(data))) {
		return wire.Attachment{}, false
	}
	return wire.Attachment{Name: name, Kind: ft.kind, ContentType: ct, Size: len(data), Data: data}, true
}

// safeName keeps the base name of an attachment, without path separators
// or control characters, bounded in length.
func safeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' {
			return -1
		}
		return r
	}, name)
	if name == "." || name == ".." {
		return ""
	}
	if utf8.RuneCountInString(name) > maxNameRunes {
		ext := path.Ext(name)
		r := []rune(strings.TrimSuffix(name, ext))
		keep := maxNameRunes - utf8.RuneCountInString(ext)
		if keep < 1 {
			return ""
		}
		name = string(r[:keep]) + ext
	}
	return name
}
