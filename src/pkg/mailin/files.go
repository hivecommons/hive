package mailin

import (
	"bytes"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// fileType is an accepted attachment extension: its Inbox kind and the
// sniffed content types that may back it.
type fileType struct {
	kind  string
	sniff []string
}

// fileTypes is the mail-in allowlist. The type is decided by content
// sniffing; the extension only has to agree with it.
var fileTypes = map[string]fileType{
	".md":       {wire.AttachMarkdown, []string{"text/markdown"}},
	".markdown": {wire.AttachMarkdown, []string{"text/markdown"}},
	".m4a":      {wire.AttachAudio, []string{"audio/mp4"}},
	".mp3":      {wire.AttachAudio, []string{"audio/mpeg"}},
	".wav":      {wire.AttachAudio, []string{"audio/wave"}},
	".ogg":      {wire.AttachAudio, []string{"audio/ogg"}},
	".webm":     {wire.AttachAudio, []string{"audio/webm"}},
	".png":      {wire.AttachImage, []string{"image/png"}},
	".jpg":      {wire.AttachImage, []string{"image/jpeg"}},
	".jpeg":     {wire.AttachImage, []string{"image/jpeg"}},
	".gif":      {wire.AttachImage, []string{"image/gif"}},
	".webp":     {wire.AttachImage, []string{"image/webp"}},
}

// maxNameRunes bounds an attachment file name.
const maxNameRunes = 120

// sniffLen is how much of a file content sniffing looks at.
const sniffLen = 512

// mp4 "ftyp" brands that carry audio (M4A and the generic ISO brands
// phone recorders use).
var m4aBrands = []string{"M4A ", "M4B ", "mp42", "isom", "mp41", "iso2", "dash"}

// sniff names the content type of b among the types mail-in accepts, ""
// for anything else.
func sniff(b []byte) string {
	head := b
	if len(head) > sniffLen {
		head = head[:sniffLen]
	}
	switch {
	case len(head) >= 12 && string(head[4:8]) == "ftyp":
		brand := string(head[8:12])
		for _, m := range m4aBrands {
			if brand == m {
				return "audio/mp4"
			}
		}
		return ""
	case bytes.HasPrefix(head, []byte("ID3")),
		len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0 && head[1]&0x06 != 0:
		// ID3 tag or an MPEG audio frame sync with a valid layer.
		return "audio/mpeg"
	case len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		return "audio/wave"
	case bytes.HasPrefix(head, []byte("OggS\x00")):
		return "audio/ogg"
	case bytes.HasPrefix(head, []byte("\x1A\x45\xDF\xA3")):
		return "audio/webm"
	}
	switch ct := http.DetectContentType(head); ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return ct
	}
	if isMarkdownText(b) {
		return "text/markdown"
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

// acceptFile checks one attachment against the allowlist: the extension
// must be allowed and the sniffed type must match it.
func acceptFile(name string, data []byte) (wire.Attachment, bool) {
	name = safeName(name)
	ft, ok := fileTypes[strings.ToLower(path.Ext(name))]
	if !ok || len(data) == 0 {
		return wire.Attachment{}, false
	}
	got := sniff(data)
	for _, want := range ft.sniff {
		if got == want {
			return wire.Attachment{Name: name, Kind: ft.kind, ContentType: got, Size: len(data), Data: data}, true
		}
	}
	return wire.Attachment{}, false
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
