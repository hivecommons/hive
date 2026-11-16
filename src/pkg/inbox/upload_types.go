package inbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Upload kinds: what a dropped file was detected as. The kind is decided
// from the content (magic bytes plus a parse check); the extension only picks
// between text formats, and neither the extension nor the client MIME type
// is trusted alone.
const (
	UploadKindMarkdown = "markdown"
	UploadKindText     = "text"
	UploadKindJSON     = "json"
	UploadKindJSONL    = "jsonl"
	UploadKindYAML     = "yaml"
	UploadKindCSV      = "csv"
	UploadKindTSV      = "tsv"
	UploadKindHTML     = "html"
	// UploadKindAudio and UploadKindVideo are media uploads (their original
	// binary is deleted after inbox.uploads.media_retention_days).
	UploadKindAudio = "audio"
	UploadKindVideo = "video"
)

// Upload error codes, returned to the dashboard per file.
const (
	UploadErrEmpty       = "empty"
	UploadErrTooLarge    = "too_large"
	UploadErrUnsupported = "unsupported_type"
	UploadErrParse       = "parse_failed"
	UploadErrQuota       = "quota_exceeded"
	UploadErrTooMany     = "too_many_files"
	UploadErrInternal    = "internal"
	// UploadErrLinkRefused: the link breaks the fetch rules (scheme,
	// credentials, a private or metadata address, too many redirects).
	UploadErrLinkRefused = "link_refused"
	// UploadErrFetch: the site could not be reached or answered an error.
	UploadErrFetch = "fetch_failed"
)

// UploadError is a per-file rejection with a message the owner can act on.
type UploadError struct {
	Code    string
	Message string
}

func (e *UploadError) Error() string { return e.Message }

func uploadErr(code, format string, args ...any) *UploadError {
	return &UploadError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// uploadTextExtensions maps the accepted text-document extensions to the
// kind they ask for. A file without an extension is detected from content.
var uploadTextExtensions = map[string]string{
	".md":       UploadKindMarkdown,
	".markdown": UploadKindMarkdown,
	".txt":      UploadKindText,
	".text":     UploadKindText,
	".log":      UploadKindText,
	".json":     UploadKindJSON,
	".jsonl":    UploadKindJSONL,
	".ndjson":   UploadKindJSONL,
	".yaml":     UploadKindYAML,
	".yml":      UploadKindYAML,
	".csv":      UploadKindCSV,
	".tsv":      UploadKindTSV,
	".tab":      UploadKindTSV,
	".html":     UploadKindHTML,
	".htm":      UploadKindHTML,
	".docx":     UploadKindDOCX,
	".pdf":      UploadKindPDF,
}

// uploadUnsupportedExtensions names formats people commonly drop that this
// hive does not read, with the message the owner sees.
var uploadUnsupportedExtensions = map[string]string{
	".doc":  "legacy Word (.doc) files are not supported: save it as .docx or PDF and try again",
	".rtf":  "RTF files are not supported: save it as .docx, PDF or plain text and try again",
	".odt":  "OpenDocument (.odt) files are not supported: save it as .docx, PDF or plain text and try again",
	".pptx": "PowerPoint files are not supported: export the slides as PDF and try again",
	".ppt":  "PowerPoint files are not supported: export the slides as PDF and try again",
	".xlsx": "Excel files are not supported: export the sheet as CSV and try again",
	".xls":  "Excel files are not supported: export the sheet as CSV and try again",
}

// UploadSupportedExtensions lists the accepted extensions, sorted, for the
// dashboard's file picker and error messages.
func UploadSupportedExtensions() []string {
	out := make([]string, 0, len(uploadTextExtensions))
	for ext := range uploadTextExtensions {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}

func uploadSupportedList() string {
	return "md, txt, json, jsonl, yaml, csv, tsv, html, docx, pdf"
}

// uploadMagic is a binary signature that is never a text document.
type uploadMagic struct {
	prefix []byte
	// offset is where prefix starts (ISO BMFF "ftyp" sits at 4).
	offset int
	label  string
}

// uploadBinaryMagic lists the signatures checked before any text parse, so a
// renamed executable, archive or media file is refused by its content.
var uploadBinaryMagic = []uploadMagic{
	{prefix: []byte("MZ"), label: "a Windows executable"},
	{prefix: []byte("\x7fELF"), label: "a Linux executable"},
	{prefix: []byte{0xfe, 0xed, 0xfa, 0xce}, label: "a macOS executable"},
	{prefix: []byte{0xfe, 0xed, 0xfa, 0xcf}, label: "a macOS executable"},
	{prefix: []byte{0xce, 0xfa, 0xed, 0xfe}, label: "a macOS executable"},
	{prefix: []byte{0xcf, 0xfa, 0xed, 0xfe}, label: "a macOS executable"},
	{prefix: []byte{0xca, 0xfe, 0xba, 0xbe}, label: "a macOS or Java executable"},
	{prefix: []byte("\x00asm"), label: "a WebAssembly module"},
	{prefix: []byte("PK\x03\x04"), label: "a zip archive (docx, xlsx, odt and similar)"},
	{prefix: []byte("PK\x05\x06"), label: "a zip archive"},
	{prefix: []byte{0x1f, 0x8b}, label: "a gzip archive"},
	{prefix: []byte("7z\xbc\xaf\x27\x1c"), label: "a 7-Zip archive"},
	{prefix: []byte("Rar!"), label: "a RAR archive"},
	{prefix: []byte("%PDF-"), label: "a PDF"},
	{prefix: []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}, label: "a legacy Office document (.doc, .xls, .ppt): save it as .docx or PDF and try again"},
	{prefix: []byte("{\\rtf"), label: "an RTF document"},
	{prefix: []byte("ID3"), label: "an audio file"},
	{prefix: []byte("OggS"), label: "an audio file"},
	{prefix: []byte("fLaC"), label: "an audio file"},
	{prefix: []byte("RIFF"), label: "an audio or video file"},
	{prefix: []byte{0x1a, 0x45, 0xdf, 0xa3}, label: "a video file"},
	{prefix: []byte("ftyp"), offset: 4, label: "an audio or video file"},
	{prefix: []byte{0x89, 'P', 'N', 'G'}, label: "an image"},
	{prefix: []byte{0xff, 0xd8, 0xff}, label: "an image"},
	{prefix: []byte("GIF8"), label: "an image"},
}

// utf8BOM is stripped from text documents; UTF-16 BOMs are refused.
var (
	utf8BOM    = []byte{0xef, 0xbb, 0xbf}
	utf16LEBOM = []byte{0xff, 0xfe}
	utf16BEBOM = []byte{0xfe, 0xff}
)

// maxControlPerMille bounds the share of control characters (other than tab,
// newline, carriage return and form feed) a text document may contain.
const maxControlPerMille = 10

// perMille converts a ratio to parts per thousand.
const perMille = 1000

// SniffUpload decides the kind of a dropped file from its content. name is
// the client file name; only its extension is used, and only to choose among
// text formats the content must then parse as.
func SniffUpload(name string, data []byte) (string, *UploadError) {
	if len(data) == 0 {
		return "", uploadErr(UploadErrEmpty, "the file is empty")
	}
	// A .docx is a zip: it skips the archive refusal and must then open as a
	// Word document (guarded against zip bombs and XXE) in extraction.
	if strings.EqualFold(filepath.Ext(name), ".docx") && docxLooksLikeZip(data) {
		return UploadKindDOCX, nil
	}
	// A PDF is read only when it says so by content and is not named as
	// something else; it is then parsed in the hive-pdftext sandbox.
	if pext := strings.ToLower(filepath.Ext(name)); bytes.HasPrefix(data, []byte("%PDF-")) && (pext == ".pdf" || pext == "") {
		return UploadKindPDF, nil
	}
	for _, m := range uploadBinaryMagic {
		if len(data) >= m.offset+len(m.prefix) && bytes.Equal(data[m.offset:m.offset+len(m.prefix)], m.prefix) {
			return "", uploadErr(UploadErrUnsupported, "unsupported file type: this is %s; supported: %s", m.label, uploadSupportedList())
		}
	}
	if bytes.HasPrefix(data, utf16LEBOM) || bytes.HasPrefix(data, utf16BEBOM) {
		return "", uploadErr(UploadErrUnsupported, "UTF-16 text is not supported: save the file as UTF-8 and try again")
	}
	ext := strings.ToLower(filepath.Ext(name))
	if msg, ok := uploadUnsupportedExtensions[ext]; ok {
		return "", uploadErr(UploadErrUnsupported, "%s", msg)
	}
	hint, known := uploadTextExtensions[ext]
	if ext != "" && !known {
		return "", uploadErr(UploadErrUnsupported, "unsupported file type %q; supported: %s", ext, uploadSupportedList())
	}
	if err := checkUploadText(data); err != nil {
		return "", err
	}
	if hint != "" {
		return hint, nil
	}
	body := bytes.TrimPrefix(data, utf8BOM)
	if json.Valid(body) {
		return UploadKindJSON, nil
	}
	return UploadKindText, nil
}

// checkUploadText refuses content that is not UTF-8 text: invalid UTF-8,
// NUL bytes, or more than a sliver of control characters.
func checkUploadText(data []byte) *UploadError {
	body := bytes.TrimPrefix(data, utf8BOM)
	if bytes.IndexByte(body, 0) >= 0 {
		return uploadErr(UploadErrUnsupported, "unsupported file type: the file is binary, not text; supported: %s", uploadSupportedList())
	}
	if !utf8.Valid(body) {
		return uploadErr(UploadErrUnsupported, "the file is not UTF-8 text: save it as UTF-8 and try again")
	}
	// A second opinion from the standard sniffer: it reports text/* or JSON
	// for text and something else for binary formats not listed above.
	if ct := http.DetectContentType(body); !strings.HasPrefix(ct, "text/") && ct != "application/json" && ct != "application/octet-stream" {
		return uploadErr(UploadErrUnsupported, "unsupported file type: the content looks like %s; supported: %s", ct, uploadSupportedList())
	}
	controls, runes := 0, 0
	for _, r := range string(body) {
		runes++
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' && r != '\f' {
			controls++
		}
	}
	if runes > 0 && controls*perMille/runes > maxControlPerMille {
		return uploadErr(UploadErrUnsupported, "unsupported file type: the file is binary, not text; supported: %s", uploadSupportedList())
	}
	return nil
}

// uploadKindIsMedia reports kinds whose original binary falls under the media
// retention.
func uploadKindIsMedia(kind string) bool {
	return kind == UploadKindAudio || kind == UploadKindVideo
}
