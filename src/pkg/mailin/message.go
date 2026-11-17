package mailin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"strings"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // decode common charsets in subjects and text parts
	"github.com/emersion/go-message/mail"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// Limits bound what one message may carry.
type Limits struct {
	// MaxFileBytes caps one decoded attachment.
	MaxFileBytes int
	// MaxFilesBytes caps all accepted attachments of one message.
	MaxFilesBytes int
	// MaxFiles caps the accepted attachments of one message.
	MaxFiles int
	// MaxCalendarBytes caps one calendar part.
	MaxCalendarBytes int
	// MaxMarkdownBytes caps one Markdown attachment, which is checked in
	// memory (it has no magic number).
	MaxMarkdownBytes int
	// MaxBodyBytes caps the text body kept as a note; a longer body is cut.
	MaxBodyBytes int
}

// Default limits (hivecommons/hive#18564): a 25 MB attachment fits in the
// 35 MB SIZE limit after base64.
const (
	DefaultMaxFileBytes     = 25 << 20
	DefaultMaxFilesBytes    = 30 << 20
	DefaultMaxFiles         = 10
	DefaultMaxCalendarBytes = 512 << 10
	DefaultMaxMarkdownBytes = 2 << 20
	DefaultMaxBodyBytes     = wire.MaxNoteBytes
	// maxParts bounds the MIME parts walked per message.
	maxParts = 100
	// maxDepth bounds multipart nesting.
	maxDepth = 8
	// spoolFilePattern names an attachment spooled by ParseReader.
	spoolFilePattern = "hive-mx-file-*"
)

// DefaultLimits are the limits hive-mx runs with unless overridden.
func DefaultLimits() Limits {
	return Limits{
		MaxFileBytes:     DefaultMaxFileBytes,
		MaxFilesBytes:    DefaultMaxFilesBytes,
		MaxFiles:         DefaultMaxFiles,
		MaxCalendarBytes: DefaultMaxCalendarBytes,
		MaxMarkdownBytes: DefaultMaxMarkdownBytes,
		MaxBodyBytes:     DefaultMaxBodyBytes,
	}
}

// withDefaults fills the limits a caller left at zero.
func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = d.MaxFileBytes
	}
	if l.MaxFilesBytes <= 0 {
		l.MaxFilesBytes = d.MaxFilesBytes
	}
	if l.MaxFiles <= 0 {
		l.MaxFiles = d.MaxFiles
	}
	if l.MaxCalendarBytes <= 0 {
		l.MaxCalendarBytes = d.MaxCalendarBytes
	}
	if l.MaxMarkdownBytes <= 0 {
		l.MaxMarkdownBytes = d.MaxMarkdownBytes
	}
	if l.MaxBodyBytes <= 0 {
		l.MaxBodyBytes = d.MaxBodyBytes
	}
	return l
}

// Parsed is what hive-mx keeps of a message.
type Parsed struct {
	Kind      wire.Kind
	Subject   string
	MessageID string
	Invites   []wire.Invite
	Files     *wire.Files
	ProofCode string
	// Attachments is the number of named attachments the message carried,
	// accepted or not; Skipped lists the ones left out, also when the
	// message is refused (ErrNothingToDeliver).
	Attachments int
	Skipped     []wire.SkippedFile
}

// Cleanup removes the attachments ParseReader spooled to disk.
func (p Parsed) Cleanup() {
	if p.Files == nil {
		return
	}
	for _, a := range p.Files.Attachments {
		if a.Path != "" {
			_ = os.Remove(a.Path)
		}
	}
}

// ErrNothingToDeliver is returned for a message that is not a proof mail
// and carries neither a calendar invite, an accepted attachment nor a text
// body.
var ErrNothingToDeliver = errors.New("mailin: no calendar invite, supported attachment or text body")

// errTooComplex is returned for a message nested or split beyond the caps.
var errTooComplex = errors.New("mailin: message structure too complex")

// isCalendarType reports a calendar media type.
func isCalendarType(mt string) bool {
	return mt == "text/calendar" || mt == "application/ics"
}

// Parse classifies a message held in memory; see ParseReader.
func Parse(raw []byte, lim Limits) (Parsed, error) {
	return ParseReader(bytes.NewReader(raw), lim, "")
}

// bodyText collects a message's text body: the first text/plain part and
// the first text/html part that are not attachments.
type bodyText struct {
	plain, html []byte
	havePlain   bool
	haveHTML    bool
}

// note is the body as Markdown: text/plain when it has any text, else the
// sanitized HTML.
func (b bodyText) note() string {
	if t := cleanText(b.plain); t != "" {
		return t
	}
	return htmlToMarkdown(b.html)
}

// ParseReader classifies a message. The priority is proof code in the
// subject, then calendar invite, then mail-in files and the text body. A
// message's text body becomes the note of its files, or, with no accepted
// attachment, a Markdown file of its own (wire.NoteFileName).
//
// With spoolDir set, audio, video and image attachments are streamed to
// files there (Attachment.Path) instead of being held in memory; the
// caller removes them with Parsed.Cleanup, also on error.
func ParseReader(r io.Reader, lim Limits, spoolDir string) (Parsed, error) {
	lim = lim.withDefaults()
	var p Parsed
	ent, err := message.Read(r)
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		return p, fmt.Errorf("mailin: read message: %w", err)
	}
	mh := mail.Header{Header: ent.Header}
	subject, serr := mh.Subject()
	if serr != nil {
		subject = mh.Get("Subject")
	}
	p.Subject = clipRunes(strings.Join(strings.Fields(subject), " "), wire.MaxTitleRunes)
	if id, err := mh.MessageID(); err == nil {
		p.MessageID = clipBytes(id, maxFieldLen)
	}
	if code := wire.FindProofCode(subject); code != "" {
		p.Kind, p.ProofCode = wire.KindProof, code
		return p, nil
	}

	w := walker{lim: lim, spoolDir: spoolDir}
	walkErr := ent.Walk(func(path []int, e *message.Entity, err error) error {
		w.parts++
		if w.parts > maxParts || len(path) > maxDepth {
			return errTooComplex
		}
		if err != nil {
			// Unknown charset or transfer encoding: skip the part.
			return nil
		}
		return w.part(e)
	})
	p.Files = &wire.Files{Title: p.Subject, Attachments: w.files, Skipped: w.skipped}
	p.Attachments, p.Skipped = w.named, w.skipped
	if walkErr != nil {
		return p, walkErr
	}
	p.Invites = dedupeInvites(w.invites)
	note := w.body.note()
	switch {
	case len(p.Invites) > 0:
		p.Cleanup()
		p.Kind, p.Files = wire.KindInvite, nil
	case len(w.files) > 0:
		p.Kind = wire.KindFiles
		p.Files.Note = note
	case note != "":
		data := []byte(note)
		sum := sha256.Sum256(data)
		p.Kind = wire.KindFiles
		p.Files.Attachments = []wire.Attachment{{
			Name: wire.NoteFileName, Kind: wire.AttachMarkdown, ContentType: ctMarkdown,
			Size: len(data), SHA256: hex.EncodeToString(sum[:]), Data: data,
		}}
	default:
		p.Files = nil
		return p, ErrNothingToDeliver
	}
	return p, nil
}

// walker holds what the MIME walk has kept so far.
type walker struct {
	lim        Limits
	spoolDir   string
	parts      int
	invites    []wire.Invite
	files      []wire.Attachment
	filesBytes int
	body       bodyText
	// named counts the named attachments seen; skipped lists the refused.
	named   int
	skipped []wire.SkippedFile
}

// skip records an attachment left out of the delivery: its extension, never
// its name.
func (w *walker) skip(name, reason string) {
	if len(w.skipped) < wire.MaxSkipped {
		w.skipped = append(w.skipped, wire.SkippedFile{Ext: extLabel(name), Reason: reason})
	}
}

// extLabel is a file name's extension as it is logged and reported: lower
// case, letters and digits only, bounded, "" when there is none.
func extLabel(name string) string {
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(safeName(name))), ".")
	var b strings.Builder
	for _, r := range ext {
		if b.Len() >= wire.MaxExtRunes {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (w *walker) part(e *message.Entity) error {
	mt, params, _ := e.Header.ContentType()
	mt = strings.ToLower(mt)
	if strings.HasPrefix(mt, "multipart/") {
		return nil
	}
	if isCalendarType(mt) {
		data, rerr := readCapped(e.Body, w.lim.MaxCalendarBytes)
		if rerr != nil {
			return nil
		}
		if inv, cerr := ParseCalendar(data); cerr == nil {
			w.invites = append(w.invites, inv...)
		}
		return nil
	}
	name := attachmentName(e.Header, params)
	if name == "" {
		w.bodyPart(e, mt)
		return nil
	}
	w.named++
	if len(w.files) >= w.lim.MaxFiles {
		w.skip(name, wire.SkipCount)
		return nil
	}
	a, why, err := w.attachment(name, e.Body)
	if err != nil {
		return err
	}
	if why != "" {
		w.skip(name, why)
		return nil
	}
	w.files = append(w.files, a)
	w.filesBytes += a.Size
	return nil
}

// bodyPart keeps the first inline text/plain and text/html parts.
func (w *walker) bodyPart(e *message.Entity, mt string) {
	if disp, _, err := e.Header.ContentDisposition(); err == nil && strings.EqualFold(disp, "attachment") {
		return
	}
	switch {
	case mt == "text/plain" && !w.body.havePlain:
		w.body.plain, w.body.havePlain = readCut(e.Body, w.lim.MaxBodyBytes), true
	case mt == "text/html" && !w.body.haveHTML:
		// HTML carries markup around the text: read more of it, the note
		// is cut to MaxBodyBytes after sanitizing.
		w.body.html, w.body.haveHTML = readCut(e.Body, w.lim.MaxBodyBytes*htmlBodyFactor), true
	}
}

// htmlBodyFactor is how much more raw HTML than note text is read.
const htmlBodyFactor = 4

// attachment reads one named part. A refused part is skipped: why is the
// wire.Skip* reason, "" for an accepted one. err is only an I/O failure of
// the spool.
func (w *walker) attachment(name string, body io.Reader) (a wire.Attachment, why string, err error) {
	ft, name, allowed := classify(name)
	if !allowed {
		return a, wire.SkipType, nil
	}
	room := min(w.lim.MaxFileBytes, w.lim.MaxFilesBytes-w.filesBytes)
	if room <= 0 {
		return a, wire.SkipSize, nil
	}
	if ft.kind == wire.AttachMarkdown || w.spoolDir == "" {
		limit := room
		if ft.kind == wire.AttachMarkdown {
			limit = min(room, w.lim.MaxMarkdownBytes)
		}
		data, rerr := readCapped(body, limit)
		switch {
		case errors.Is(rerr, errPartTooLarge):
			return a, wire.SkipSize, nil
		case rerr != nil:
			return a, wire.SkipContent, nil
		}
		a, ok := acceptFile(name, data)
		if !ok {
			return wire.Attachment{}, wire.SkipContent, nil
		}
		sum := sha256.Sum256(data)
		a.SHA256 = hex.EncodeToString(sum[:])
		return a, "", nil
	}
	head := make([]byte, sniffLen)
	n, rerr := io.ReadFull(body, head)
	if rerr != nil && !errors.Is(rerr, io.ErrUnexpectedEOF) {
		return a, wire.SkipContent, nil
	}
	head = head[:n]
	ct, ok := ft.match(sniffHead(head))
	if !ok {
		return a, wire.SkipContent, nil
	}
	path, size, sum, err := spoolPart(w.spoolDir, head, body, room)
	switch {
	case errors.Is(err, errPartTooLarge):
		return a, wire.SkipSize, nil
	case errors.Is(err, errPartUnreadable):
		return a, wire.SkipContent, nil
	case err != nil:
		return a, "", err
	}
	if !verifySpooled(ct, path, int64(size)) {
		_ = os.Remove(path)
		return a, wire.SkipContent, nil
	}
	return wire.Attachment{Name: name, Kind: ft.kind, ContentType: ct, Size: size, SHA256: sum, Path: path}, "", nil
}

// verifySpooled runs verifyContainer on a spooled attachment.
func verifySpooled(ct, path string, size int64) bool {
	if _, container := ooxmlMainPart[ct]; !container {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return verifyContainer(ct, f, size)
}

// spoolPart writes head and the rest of body to a new file in dir, failing
// with errPartTooLarge (and removing the file) past limit bytes.
func spoolPart(dir string, head []byte, body io.Reader, limit int) (path string, size int, sum string, err error) {
	f, err := os.CreateTemp(dir, spoolFilePattern)
	if err != nil {
		return "", 0, "", err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(f.Name())
			path = ""
		}
	}()
	h := sha256.New()
	out := io.MultiWriter(f, h)
	if _, err = out.Write(head); err != nil {
		return "", 0, "", err
	}
	rest := int64(limit - len(head))
	if rest < 0 {
		return "", 0, "", errPartTooLarge
	}
	copied, err := io.Copy(out, io.LimitReader(body, rest+1))
	if err != nil {
		// A truncated or undecodable part is skipped, not a spool failure.
		return "", 0, "", errPartUnreadable
	}
	if copied > rest {
		return "", 0, "", errPartTooLarge
	}
	return f.Name(), len(head) + int(copied), hex.EncodeToString(h.Sum(nil)), nil
}

// dedupeInvites keeps one invite per UID and RECURRENCE-ID, the highest
// SEQUENCE: Outlook sends the same calendar inline and as an .ics part.
func dedupeInvites(in []wire.Invite) []wire.Invite {
	if len(in) < 2 {
		return in
	}
	key := func(i wire.Invite) string {
		if i.RecurrenceID == nil {
			return i.UID
		}
		return i.UID + "\x00" + i.RecurrenceID.UTC().String()
	}
	idx := map[string]int{}
	out := make([]wire.Invite, 0, len(in))
	for _, inv := range in {
		k := key(inv)
		if j, ok := idx[k]; ok {
			if inv.Sequence > out[j].Sequence {
				out[j] = inv
			}
			continue
		}
		idx[k] = len(out)
		out = append(out, inv)
	}
	return out
}

// errPartTooLarge is returned by readCapped for a part over its cap.
var errPartTooLarge = errors.New("mailin: part too large")

// errPartUnreadable is returned by spoolPart for a part that cannot be
// decoded to its end.
var errPartUnreadable = errors.New("mailin: part unreadable")

// readCut reads at most limit bytes of r, dropping the rest.
func readCut(r io.Reader, limit int) []byte {
	data, _ := io.ReadAll(io.LimitReader(r, int64(limit)))
	return data
}

// readCapped reads r fully, failing when it holds more than limit bytes.
func readCapped(r io.Reader, limit int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errPartTooLarge
	}
	return data, nil
}

// attachmentName is the file name a part declares in Content-Disposition
// or, failing that, the Content-Type name parameter.
func attachmentName(h message.Header, ctParams map[string]string) string {
	if _, dp, err := h.ContentDisposition(); err == nil {
		if n := dp["filename"]; n != "" {
			return decodeWord(n)
		}
	}
	return decodeWord(ctParams["name"])
}

var wordDecoder = mime.WordDecoder{CharsetReader: message.CharsetReader}

// decodeWord decodes an RFC 2047 encoded file name, as some clients send.
func decodeWord(s string) string {
	if d, err := wordDecoder.DecodeHeader(s); err == nil {
		return d
	}
	return s
}
