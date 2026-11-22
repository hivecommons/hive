// Package auditchain makes an append-only JSON-lines log tamper-evident with a
// hash chain (hivecommons/hive#19439).
//
// Every chained row carries three fields next to its own content:
//
//   - seq: 1 for the first row of a chain, then the previous row's seq + 1.
//   - prev_hash: the previous row's row_hash; absent on the first row.
//   - row_hash: lowercase hex SHA-256 over the RFC 8785 (JCS) canonical form
//     of the row itself with row_hash removed — so it covers seq, prev_hash
//     and every other field, including ones the writer's Go struct does not
//     know about.
//
// Editing a row breaks its row_hash; dropping, inserting or reordering rows
// breaks the next row's prev_hash and seq. What a chain alone cannot show is
// the removal of its newest rows, or a writer rebuilding the whole chain from
// scratch: for that the latest Link is reported off the machine (the hub
// heartbeat), where a later Link with a lower seq is visible.
//
// The hash is computed over the parsed row, not over its bytes, so a writer
// may emit fields in any order and with any (valid) JSON escaping.
package auditchain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Chain field names. They are part of the on-disk contract.
const (
	FieldSeq      = "seq"
	FieldPrevHash = "prev_hash"
	FieldRowHash  = "row_hash"
)

// Link is the head of a chain: the seq and row_hash of its newest row. The
// zero Link means "no chain yet"; the next row is then a chain's first row.
type Link struct {
	Seq  int64  `json:"seq"`
	Hash string `json:"hash"`
}

// IsZero reports whether l names no row.
func (l Link) IsZero() bool { return l.Seq == 0 && l.Hash == "" }

// Next returns the seq and prev_hash the row after l must carry.
func (l Link) Next() (seq int64, prevHash string) { return l.Seq + 1, l.Hash }

// decodeObject parses one JSON object, keeping numbers exact.
func decodeObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("not a JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON object")
	}
	return obj, nil
}

// RowHash returns the row_hash of one serialized row: SHA-256 over the JCS
// form of the object with any row_hash field removed.
func RowHash(row []byte) (string, error) {
	obj, err := decodeObject(row)
	if err != nil {
		return "", err
	}
	return hashObject(obj)
}

func hashObject(obj map[string]any) (string, error) {
	delete(obj, FieldRowHash)
	canon, err := appendCanonical(nil, obj)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// Seal chains one serialized JSON object onto prev: it sets seq and prev_hash
// (dropping any it already had), computes row_hash, and returns the row in
// canonical form plus the new head. It is for logs that have no Go struct of
// their own to carry the fields; a struct-backed writer sets Seq/PrevHash
// itself, marshals, and calls RowHash.
func Seal(object []byte, prev Link) ([]byte, Link, error) {
	obj, err := decodeObject(object)
	if err != nil {
		return nil, Link{}, err
	}
	seq, prevHash := prev.Next()
	obj[FieldSeq] = json.Number(strconv.FormatInt(seq, 10))
	delete(obj, FieldPrevHash)
	if prevHash != "" {
		obj[FieldPrevHash] = prevHash
	}
	h, err := hashObject(obj)
	if err != nil {
		return nil, Link{}, err
	}
	obj[FieldRowHash] = h
	row, err := appendCanonical(nil, obj)
	if err != nil {
		return nil, Link{}, err
	}
	return row, Link{Seq: seq, Hash: h}, nil
}

// Canonicalize returns the RFC 8785 (JCS) form of one JSON value.
func Canonicalize(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return appendCanonical(nil, v)
}

func appendCanonical(buf []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(buf, "null"...), nil
	case bool:
		if x {
			return append(buf, "true"...), nil
		}
		return append(buf, "false"...), nil
	case string:
		return appendCanonicalString(buf, x), nil
	case json.Number:
		s, err := canonicalNumber(x)
		if err != nil {
			return nil, err
		}
		return append(buf, s...), nil
	case []any:
		buf = append(buf, '[')
		for i, e := range x {
			if i > 0 {
				buf = append(buf, ',')
			}
			var err error
			if buf, err = appendCanonical(buf, e); err != nil {
				return nil, err
			}
		}
		return append(buf, ']'), nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// JCS orders members by their names' UTF-16 code units.
		sort.Slice(keys, func(i, j int) bool { return lessUTF16(keys[i], keys[j]) })
		buf = append(buf, '{')
		for i, k := range keys {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = appendCanonicalString(buf, k)
			buf = append(buf, ':')
			var err error
			if buf, err = appendCanonical(buf, x[k]); err != nil {
				return nil, err
			}
		}
		return append(buf, '}'), nil
	default:
		return nil, fmt.Errorf("auditchain: unsupported JSON value %T", v)
	}
}

func lessUTF16(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// appendCanonicalString writes s the way JCS (ECMAScript JSON.stringify)
// does: only '"', '\\' and control characters are escaped, using the short
// forms where they exist and lowercase \u00xx otherwise.
func appendCanonicalString(buf []byte, s string) []byte {
	const hexDigits = "0123456789abcdef"
	buf = append(buf, '"')
	for _, r := range s {
		switch r {
		case '"':
			buf = append(buf, '\\', '"')
		case '\\':
			buf = append(buf, '\\', '\\')
		case '\b':
			buf = append(buf, '\\', 'b')
		case '\f':
			buf = append(buf, '\\', 'f')
		case '\n':
			buf = append(buf, '\\', 'n')
		case '\r':
			buf = append(buf, '\\', 'r')
		case '\t':
			buf = append(buf, '\\', 't')
		default:
			if r < 0x20 {
				buf = append(buf, '\\', 'u', '0', '0', hexDigits[r>>4], hexDigits[r&0xf])
				continue
			}
			buf = utf8.AppendRune(buf, r)
		}
	}
	return append(buf, '"')
}

// canonicalNumber formats a JSON number as ECMAScript's Number.toString does,
// which is what JCS prescribes.
func canonicalNumber(n json.Number) (string, error) {
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil {
		return "", fmt.Errorf("auditchain: number %q: %w", n, err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("auditchain: number %q is not finite", n)
	}
	if f == 0 {
		return "0", nil
	}
	if abs := math.Abs(f); abs < 1e-6 || abs >= 1e21 {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		mant, exp, _ := strings.Cut(s, "e")
		sign, digits := exp[:1], strings.TrimLeft(exp[1:], "0")
		return mant + "e" + sign + digits, nil
	}
	return strconv.FormatFloat(f, 'f', -1, 64), nil
}
