package logread

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// cursor marks where the next, older page ends: the content offset (exclusive)
// in one history file. The file is identified by its first line (its time and
// a digest), not by name, so a cursor into hive.log keeps working after the
// file is rotated (renamed, then compressed) between two calls. Offset -1
// means "from the end of that file".
type cursor struct {
	V      int    `json:"v"`
	Source string `json:"s"`
	Time   string `json:"t,omitempty"`
	Digest string `json:"h"`
	Offset int64  `json:"o"`
}

const cursorVersion = 1

var errBadCursor = errors.New("malformed cursor")

// fileIdentity derives the cursor identity of a file from its first line.
func fileIdentity(head []byte) (t, digest string, ok bool) {
	if len(head) == 0 {
		return "", "", false
	}
	sum := sha256.Sum256(head)
	when, _ := parseHead(string(head))
	if !when.IsZero() {
		t = when.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	}
	return t, hex.EncodeToString(sum[:12]), true
}

// encodeCursor renders a cursor as lowercase hex, which no credential shape
// the outbound scrubbers recognise can appear in.
func encodeCursor(c cursor) string {
	c.V = cursorVersion
	b, _ := json.Marshal(c)
	return hex.EncodeToString(b)
}

func decodeCursor(s string) (cursor, error) {
	raw, err := hex.DecodeString(s)
	if err != nil {
		return cursor{}, errBadCursor
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil || c.V != cursorVersion || c.Digest == "" || c.Offset < -1 {
		return cursor{}, errBadCursor
	}
	return c, nil
}
