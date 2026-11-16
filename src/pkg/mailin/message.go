package mailin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
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
}

// Default limits.
const (
	DefaultMaxFileBytes     = 2 << 20
	DefaultMaxFilesBytes    = 2 << 20
	DefaultMaxFiles         = 10
	DefaultMaxCalendarBytes = 512 << 10
	// maxParts bounds the MIME parts walked per message.
	maxParts = 100
	// maxDepth bounds multipart nesting.
	maxDepth = 8
)

// DefaultLimits are the limits hive-mx runs with unless overridden.
func DefaultLimits() Limits {
	return Limits{
		MaxFileBytes:     DefaultMaxFileBytes,
		MaxFilesBytes:    DefaultMaxFilesBytes,
		MaxFiles:         DefaultMaxFiles,
		MaxCalendarBytes: DefaultMaxCalendarBytes,
	}
}

// Parsed is what hive-mx keeps of a message.
type Parsed struct {
	Kind      wire.Kind
	Subject   string
	MessageID string
	Invites   []wire.Invite
	Files     *wire.Files
	ProofCode string
}

// ErrNothingToDeliver is returned for a message that is not a proof mail
// and carries neither a calendar invite nor an accepted attachment.
var ErrNothingToDeliver = errors.New("mailin: no calendar invite or supported attachment")

// errTooComplex is returned for a message nested or split beyond the caps.
var errTooComplex = errors.New("mailin: message structure too complex")

// isCalendarType reports a calendar media type.
func isCalendarType(mt string) bool {
	return mt == "text/calendar" || mt == "application/ics"
}

// Parse classifies a message. The priority is proof code in the subject,
// then calendar invite, then mail-in files. The body text is never kept.
func Parse(raw []byte, lim Limits) (Parsed, error) {
	var p Parsed
	ent, err := message.Read(bytes.NewReader(raw))
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

	var files []wire.Attachment
	filesBytes, parts := 0, 0
	walkErr := ent.Walk(func(path []int, e *message.Entity, err error) error {
		parts++
		if parts > maxParts || len(path) > maxDepth {
			return errTooComplex
		}
		if err != nil {
			// Unknown charset or transfer encoding: skip the part.
			return nil
		}
		mt, params, _ := e.Header.ContentType()
		mt = strings.ToLower(mt)
		if strings.HasPrefix(mt, "multipart/") {
			return nil
		}
		if isCalendarType(mt) {
			data, rerr := readCapped(e.Body, lim.MaxCalendarBytes)
			if rerr != nil {
				return nil
			}
			if inv, cerr := ParseCalendar(data); cerr == nil {
				p.Invites = append(p.Invites, inv...)
			}
			return nil
		}
		name := attachmentName(e.Header, params)
		if name == "" || len(files) >= lim.MaxFiles {
			return nil
		}
		data, rerr := readCapped(e.Body, lim.MaxFileBytes)
		if rerr != nil || filesBytes+len(data) > lim.MaxFilesBytes {
			return nil
		}
		if a, ok := acceptFile(name, data); ok {
			sum := sha256.Sum256(data)
			a.SHA256 = hex.EncodeToString(sum[:])
			files = append(files, a)
			filesBytes += len(data)
		}
		return nil
	})
	if walkErr != nil {
		return p, walkErr
	}
	p.Invites = dedupeInvites(p.Invites)
	switch {
	case len(p.Invites) > 0:
		p.Kind = wire.KindInvite
	case len(files) > 0:
		p.Kind = wire.KindFiles
		p.Files = &wire.Files{Title: p.Subject, Attachments: files}
	default:
		return p, ErrNothingToDeliver
	}
	return p, nil
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
