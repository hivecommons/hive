package inbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// EmailType is the Inbox source for mail-in files: Markdown, audio, video,
// image and office document (PDF, Word, PowerPoint, Excel) attachments and
// text bodies mailed to hive@<host> through
// hive-mx (hivecommons/hive#18395, #18564, design
// src/docs/design/email-inbox.md). It is push, not a connector: the
// dashboard's relay-signed POST /api/inbox/email adds items to a spool under
// connector-state/email/items, and the source lists that spool.
//
// Items from a sender on the hive's email allowlist (DMARC aligned) are
// normal items. Every other item is AwaitingApproval: it is listed, but it
// cannot be routed or transcribed and is skipped by auto-routing rules until
// the owner approves it (or declines, which deletes it).
const EmailType = "email"

// Email spool limits.
const (
	// DefaultEmailQuotaBytes bounds the whole spool (a mail-in video can
	// be 25 MB).
	DefaultEmailQuotaBytes = 1 << 30
	// EmailPendingRetention is how long an unapproved item is kept.
	EmailPendingRetention = 14 * 24 * time.Hour
	emailIDHexLen         = 24
	emailMetaFile         = "meta.json"
	emailDataFile         = "data"
	emailDirMode          = 0o700
	emailFileMode         = 0o600
	// emailTitleSep joins the subject and the file name of a multi-file mail.
	emailTitleSep = " · "
)

// Errors the email source returns.
var (
	ErrEmailQuota       = errors.New("the email spool is full")
	ErrAwaitingApproval = errors.New("this item is awaiting the owner's approval")
	ErrEmailNotFound    = errors.New("no such email item")
)

// EmailMeta is one spooled attachment.
type EmailMeta struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Filename    string    `json:"filename"`
	Kind        string    `json:"kind"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	From        string    `json:"from"`
	Aligned     bool      `json:"aligned"`
	Approved    bool      `json:"approved"`
	ApprovedBy  string    `json:"approved_by,omitempty"`
	MessageID   string    `json:"message_id,omitempty"`
	DeliveryID  string    `json:"delivery_id,omitempty"`
	ReceivedAt  time.Time `json:"received_at"`
	// Note is the message's text body, kept with each of its files.
	Note string `json:"note,omitempty"`
}

// EmailSource is the email spool as an Inbox source. Safe for concurrent use.
type EmailSource struct {
	Dir        string
	QuotaBytes int64
	Now        func() time.Time

	mu sync.Mutex
	// logMu guards the recent-mail log (email_log.go).
	logMu sync.Mutex
}

// Type implements Source.
func (e *EmailSource) Type() string { return EmailType }

// Builtin implements BuiltinSource: listed without being configured.
func (e *EmailSource) Builtin() bool { return true }

func (e *EmailSource) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

func (e *EmailSource) quota() int64 {
	if e.QuotaBytes > 0 {
		return e.QuotaBytes
	}
	return DefaultEmailQuotaBytes
}

func (e *EmailSource) itemDir(id string) string { return filepath.Join(e.Dir, id) }

func validEmailID(id string) bool {
	if len(id) != emailIDHexLen {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// emailItemID is stable per message and file, so a retried delivery of the
// same mail lands on the same item.
func emailItemID(d wire.Delivery, a wire.Attachment) string {
	key := d.MessageID
	if key == "" {
		key = d.ID
	}
	sum := sha256.Sum256([]byte(key + "\x00" + d.Recipient + "\x00" + a.SHA256 + "\x00" + a.Name))
	return hex.EncodeToString(sum[:])[:emailIDHexLen]
}

func (e *EmailSource) readMeta(id string) (EmailMeta, error) {
	var m EmailMeta
	b, err := os.ReadFile(filepath.Join(e.itemDir(id), emailMetaFile))
	if errors.Is(err, os.ErrNotExist) {
		return m, ErrEmailNotFound
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	return m, nil
}

func (e *EmailSource) writeMeta(m EmailMeta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(e.itemDir(m.ID), emailMetaFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, emailFileMode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// metas lists the spool, newest first.
func (e *EmailSource) metas() ([]EmailMeta, error) {
	entries, err := os.ReadDir(e.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []EmailMeta
	for _, de := range entries {
		if !de.IsDir() || !validEmailID(de.Name()) {
			continue
		}
		m, err := e.readMeta(de.Name())
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReceivedAt.After(out[j].ReceivedAt) })
	return out, nil
}

func (e *EmailSource) usageLocked() int64 {
	ms, _ := e.metas()
	var n int64
	for _, m := range ms {
		n += m.Size
	}
	return n
}

// pruneLocked deletes unapproved items past EmailPendingRetention.
func (e *EmailSource) pruneLocked(now time.Time) {
	ms, _ := e.metas()
	for _, m := range ms {
		if !m.Approved && now.Sub(m.ReceivedAt) > EmailPendingRetention {
			_ = os.RemoveAll(e.itemDir(m.ID))
		}
	}
}

func emailTitle(subject string, a wire.Attachment, many bool) string {
	subject = strings.TrimSpace(subject)
	switch {
	case subject == "":
		return a.Name
	case many:
		return truncateRunes(subject+emailTitleSep+a.Name, wire.MaxTitleRunes)
	}
	return subject
}

// attachmentBytes is an attachment's size, held in memory or staged.
func attachmentBytes(a wire.Attachment) int64 {
	if a.Path != "" && len(a.Data) == 0 {
		return int64(a.Size)
	}
	return int64(len(a.Data))
}

// storeAttachment writes an attachment's bytes as dir's data file: a
// staged file is moved (the staging is under the spool), bytes in memory
// are written.
func storeAttachment(dir string, a wire.Attachment) error {
	dst := filepath.Join(dir, emailDataFile)
	if len(a.Data) > 0 || a.Path == "" {
		return os.WriteFile(dst, a.Data, emailFileMode)
	}
	if err := os.Rename(a.Path, dst); err == nil {
		return os.Chmod(dst, emailFileMode)
	}
	src, err := os.Open(a.Path)
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, emailFileMode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// Add spools one delivery's attachments. trusted marks a sender on the
// allowlist with DMARC alignment; everyone else's items await approval.
// It returns the new or already present items. An attachment is either
// in memory (Data) or staged on disk (Path, its digest verified on
// receipt); the message's text body is kept as each item's note.
func (e *EmailSource) Add(d wire.Delivery, trusted bool) ([]Item, error) {
	if d.Files == nil || len(d.Files.Attachments) == 0 {
		return nil, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.pruneLocked(now)
	if err := os.MkdirAll(e.Dir, emailDirMode); err != nil {
		return nil, err
	}
	var incoming int64
	for _, a := range d.Files.Attachments {
		incoming += attachmentBytes(a)
	}
	if e.usageLocked()+incoming > e.quota() {
		return nil, ErrEmailQuota
	}
	note := clipNoteText(d.Files.Note)
	many := len(d.Files.Attachments) > 1
	var out []Item
	for _, a := range d.Files.Attachments {
		size := attachmentBytes(a)
		if size == 0 {
			continue
		}
		if len(a.Data) > 0 {
			sum := sha256.Sum256(a.Data)
			a.SHA256 = hex.EncodeToString(sum[:])
		}
		id := emailItemID(d, a)
		if m, err := e.readMeta(id); err == nil {
			out = append(out, e.item(m))
			continue
		}
		m := EmailMeta{
			ID: id, Title: emailTitle(d.Files.Title, a, many), Filename: CleanUploadName(a.Name),
			Kind: a.Kind, ContentType: a.ContentType, Size: size, SHA256: a.SHA256,
			From: d.Auth.From, Aligned: d.Auth.Aligned, Approved: trusted,
			MessageID: d.MessageID, DeliveryID: d.ID, ReceivedAt: now, Note: note,
		}
		if trusted {
			m.ApprovedBy = "allowlist"
		}
		dir := e.itemDir(id)
		if err := os.MkdirAll(dir, emailDirMode); err != nil {
			return out, err
		}
		if err := storeAttachment(dir, a); err != nil {
			_ = os.RemoveAll(dir)
			return out, err
		}
		if err := e.writeMeta(m); err != nil {
			_ = os.RemoveAll(dir)
			return out, err
		}
		out = append(out, e.item(m))
	}
	return out, nil
}

// clipNoteText bounds a note to wire.MaxNoteBytes on a rune boundary.
func clipNoteText(s string) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, ""))
	if len(s) <= wire.MaxNoteBytes {
		return s
	}
	s = s[:wire.MaxNoteBytes]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// emailVideoAudioMIME is the type a mail-in video's container is handed
// to the transcriber as (the providers and the whisper sandbox read the
// audio track of an MP4 or QuickTime file), "" when it cannot be
// transcribed here and is stored only.
func emailVideoAudioMIME(m EmailMeta) string {
	switch m.ContentType {
	case "video/mp4", "video/quicktime":
		return "audio/mp4"
	}
	return ""
}

// emailHasAudio reports an item the transcriber can read.
func emailHasAudio(m EmailMeta) bool {
	return m.Kind == wire.AttachAudio || (m.Kind == wire.AttachVideo && emailVideoAudioMIME(m) != "")
}

// Approve lets an awaiting item be routed and transcribed.
func (e *EmailSource) Approve(id, by string) (Item, error) {
	if !validEmailID(id) {
		return Item{}, ErrEmailNotFound
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	m, err := e.readMeta(id)
	if err != nil {
		return Item{}, err
	}
	if !m.Approved {
		m.Approved, m.ApprovedBy = true, by
		if err := e.writeMeta(m); err != nil {
			return Item{}, err
		}
	}
	return e.item(m), nil
}

// Decline deletes an item and its file.
func (e *EmailSource) Decline(id string) error {
	if !validEmailID(id) {
		return ErrEmailNotFound
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.readMeta(id); err != nil {
		return err
	}
	return os.RemoveAll(e.itemDir(id))
}

// Open returns an item's file and its metadata.
func (e *EmailSource) Open(id string) (io.ReadCloser, EmailMeta, error) {
	if !validEmailID(id) {
		return nil, EmailMeta{}, ErrEmailNotFound
	}
	m, err := e.readMeta(id)
	if err != nil {
		return nil, m, err
	}
	f, err := os.Open(filepath.Join(e.itemDir(id), emailDataFile))
	if err != nil {
		return nil, m, err
	}
	return f, m, nil
}

func (e *EmailSource) item(m EmailMeta) Item {
	it := Item{
		Source: EmailType, Connector: EmailType, ID: m.ID, Title: firstNonEmpty(m.Title, m.Filename, m.ID),
		RecordedAt: m.ReceivedAt, Tags: []string{EmailType, m.Kind}, Origin: "email · " + m.From,
		AddedBy: m.From, UploadKind: m.Kind, AwaitingApproval: !m.Approved, Sender: m.From,
	}
	if emailHasAudio(m) {
		// Audio (and a video's audio track) has no text until it is
		// transcribed.
		it.Pending = true
	}
	switch {
	case m.Kind == wire.AttachMarkdown:
		if b, err := os.ReadFile(filepath.Join(e.itemDir(m.ID), emailDataFile)); err == nil {
			it.Excerpt = truncateRunes(strings.TrimSpace(string(b)), SummaryRunes)
		}
	case m.Note != "":
		it.Excerpt = truncateRunes(m.Note, SummaryRunes)
	}
	return it
}

// List implements Source.
func (e *EmailSource) List(context.Context) ([]Item, error) {
	ms, err := e.metas()
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(ms))
	for _, m := range ms {
		out = append(out, e.item(m))
	}
	return out, nil
}

// EmailProvenance is where an email item came from.
func EmailProvenance(m EmailMeta) string {
	return "email://" + m.ID + "/" + m.Filename
}

// Fetch implements Source. Markdown is returned as the item text (wrapped as
// untrusted data); an image, document, audio or video file is described, its bytes
// served by the dashboard; audio text arrives through transcription. The
// message's text body follows, wrapped as untrusted data as well.
func (e *EmailSource) Fetch(_ context.Context, it Item) (Content, error) {
	if !validEmailID(it.ID) {
		return Content{}, ErrEmailNotFound
	}
	m, err := e.readMeta(it.ID)
	if err != nil {
		return Content{}, err
	}
	c := Content{Title: firstNonEmpty(m.Title, m.Filename), Provenance: EmailProvenance(m), TranscriptLabel: "Email attachment"}
	header := fmt.Sprintf("- **From:** %s (%s)\n- **File:** %s (%s, %s)\n", m.From, alignedLabel(m.Aligned), m.Filename, m.ContentType, humanBytes(m.Size))
	switch m.Kind {
	case wire.AttachMarkdown:
		b, err := os.ReadFile(filepath.Join(e.itemDir(m.ID), emailDataFile))
		if err != nil {
			return Content{}, err
		}
		text := string(bytes.ToValidUTF8(b, nil))
		c.Markdown = header + "\n" + WrapUntrusted(EmailType, m.ID, strings.TrimSpace(text))
	case wire.AttachAudio:
		c.Pending = true
		c.Markdown = header + "\nAn audio file; its text appears here once it is transcribed."
	case wire.AttachVideo:
		if emailHasAudio(m) {
			c.Pending = true
			c.Markdown = header + "\nA video, stored as an attachment of this item; the text of its audio track appears here once it is transcribed."
		} else {
			c.Markdown = header + "\nA video, stored as an attachment of this item. Its audio cannot be transcribed on this hive (the transcriber does not read " + m.ContentType + "): download it to watch it."
		}
	case wire.AttachDocument:
		c.Markdown = header + "\nA document, stored as an attachment of this item. Its text is not extracted: open the file to read it."
	default:
		c.Markdown = header + "\nAn image, stored as an attachment of this item."
	}
	if m.Note != "" {
		c.Markdown += "\n\n**Message:**\n\n" + WrapUntrusted(EmailType, m.ID+":note", m.Note)
	}
	return c, nil
}

func alignedLabel(aligned bool) string {
	if aligned {
		return "DMARC aligned"
	}
	return "not authenticated"
}

// OpenAudio implements AudioOpener for audio items: the transcriber reads
// the spooled file directly.
func (e *EmailSource) OpenAudio(_ context.Context, it Item) (LocalAudio, error) {
	if !validEmailID(it.ID) {
		return LocalAudio{}, ErrEmailNotFound
	}
	m, err := e.readMeta(it.ID)
	if err != nil {
		return LocalAudio{}, err
	}
	if !emailHasAudio(m) {
		return LocalAudio{}, ErrNoAudio
	}
	f, err := os.Open(filepath.Join(e.itemDir(it.ID), emailDataFile))
	if err != nil {
		return LocalAudio{}, err
	}
	mime := emailVideoAudioMIME(m)
	if mime == "" {
		mime = audioMIMEByExt[strings.ToLower(filepath.Ext(m.Filename))]
	}
	if mime == "" {
		mime = m.ContentType
	}
	return LocalAudio{File: f, Name: m.Filename, MIME: mime, Size: m.Size, ModTime: m.ReceivedAt}, nil
}
