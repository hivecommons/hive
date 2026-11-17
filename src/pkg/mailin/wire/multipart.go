package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"os"
	"regexp"
	"strconv"
)

// Large deliveries (hivecommons/hive#18564). A delivery whose attachments
// total more than InlineFilesBytes travels as multipart/form-data: a
// PartDelivery part with the Delivery as JSON (attachment Data left out),
// then one PartFile part per attachment, in order, with its raw bytes. Both
// ends stream it: hive-mx from its spool, the spoke into its staging.

// ErrMultipart is returned for a multipart body that does not match the
// contract.
var ErrMultipart = errors.New("wire: malformed multipart delivery")

// FilesBytes is the total size of a delivery's attachments.
func FilesBytes(d Delivery) int64 {
	if d.Files == nil {
		return 0
	}
	var n int64
	for _, a := range d.Files.Attachments {
		n += int64(a.Size)
	}
	return n
}

// WriteMultipart writes d to mw and closes it. Each attachment's bytes come
// from Data, or from the file at Path when Data is empty.
func WriteMultipart(mw *multipart.Writer, d Delivery) error {
	meta := d
	var atts []Attachment
	if d.Files != nil {
		files := *d.Files
		atts = files.Attachments
		files.Attachments = make([]Attachment, len(atts))
		for i, a := range atts {
			a.Data = nil
			files.Attachments[i] = a
		}
		meta.Files = &files
	}
	pw, err := mw.CreatePart(partHeader(PartDelivery, ContentTypeJSON))
	if err != nil {
		return err
	}
	if err := json.NewEncoder(pw).Encode(meta); err != nil {
		return err
	}
	for _, a := range atts {
		pw, err := mw.CreatePart(partHeader(PartFile, "application/octet-stream"))
		if err != nil {
			return err
		}
		if err := writeAttachment(pw, a); err != nil {
			return err
		}
	}
	return mw.Close()
}

func partHeader(name, contentType string) textproto.MIMEHeader {
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="`+name+`"`)
	h.Set("Content-Type", contentType)
	return h
}

func writeAttachment(w io.Writer, a Attachment) error {
	if len(a.Data) > 0 || a.Path == "" {
		_, err := w.Write(a.Data)
		return err
	}
	f, err := os.Open(a.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// countWriter counts the bytes written through it.
type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// ReadMultipart reads a body written by WriteMultipart. store is handed
// each file part (at most its declared size plus one byte) and returns
// where it kept the bytes; ReadMultipart checks the size and SHA-256
// declared in the delivery part and records the path on the attachment.
// On error the caller removes whatever store wrote.
func ReadMultipart(r io.Reader, boundary string, store func(i int, a Attachment, body io.Reader) (string, error)) (Delivery, error) {
	var d Delivery
	mr := multipart.NewReader(r, boundary)
	p, err := mr.NextPart()
	if err != nil || p.FormName() != PartDelivery {
		return d, fmt.Errorf("%w: no delivery part", ErrMultipart)
	}
	if err := json.NewDecoder(io.LimitReader(p, MaxDeliveryMetaBytes)).Decode(&d); err != nil {
		return d, fmt.Errorf("%w: delivery part: %v", ErrMultipart, err)
	}
	var atts []Attachment
	if d.Files != nil {
		atts = d.Files.Attachments
	}
	for i := range atts {
		p, err := mr.NextPart()
		if err != nil || p.FormName() != PartFile {
			return d, fmt.Errorf("%w: file part %d missing", ErrMultipart, i)
		}
		if atts[i].Size < 0 {
			return d, fmt.Errorf("%w: file part %d has a negative size", ErrMultipart, i)
		}
		h, cnt := sha256.New(), &countWriter{}
		body := io.TeeReader(io.LimitReader(p, int64(atts[i].Size)+1), io.MultiWriter(h, cnt))
		path, err := store(i, atts[i], body)
		if err != nil {
			return d, err
		}
		atts[i].Path, atts[i].Data = path, nil
		if cnt.n != int64(atts[i].Size) || hex.EncodeToString(h.Sum(nil)) != atts[i].SHA256 {
			return d, fmt.Errorf("%w: file part %d does not match its size or digest", ErrMultipart, i)
		}
	}
	if _, err := mr.NextPart(); !errors.Is(err, io.EOF) {
		return d, fmt.Errorf("%w: unexpected trailing part", ErrMultipart)
	}
	return d, nil
}

// chunkDeliveryRE is a chunk's relay delivery id: <delivery id>.<index>.<count>.
var chunkDeliveryRE = regexp.MustCompile(`^([0-9a-f]{32})\.(\d{1,4})\.(\d{1,4})$`)

// ChunkDelivery is the relay delivery id of piece i of n of a delivery.
// The hub signs each piece with it, so the index and count are covered by
// the relay signature and every piece passes the spoke's replay check.
func ChunkDelivery(id string, i, n int) string {
	return id + "." + strconv.Itoa(i) + "." + strconv.Itoa(n)
}

// ParseChunkDelivery splits a chunk's relay delivery id. ok is false unless
// the id is a hive-mx delivery id and 0 <= i < n <= MaxRelayChunks.
func ParseChunkDelivery(s string) (id string, i, n int, ok bool) {
	m := chunkDeliveryRE.FindStringSubmatch(s)
	if m == nil {
		return "", 0, 0, false
	}
	i, _ = strconv.Atoi(m[2])
	n, _ = strconv.Atoi(m[3])
	if n < 1 || n > MaxRelayChunks || i >= n {
		return "", 0, 0, false
	}
	return m[1], i, n, true
}
