// Package findingidentity computes deterministic offline identities for
// finding records.
package findingidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	KeyVersion = "finding-identity/v1"

	MetaKey           = "finding_key"
	MetaSubjectDigest = "subject_digest"
	MetaPredicate     = "predicate"
	MetaLocation      = "location"
)

var (
	lineAnchorSuffix = regexp.MustCompile(`(?i)#L\d+(?:-L\d+)?$`)
	lineColonSuffix  = regexp.MustCompile(`:\d+(?::\d+)?$`)
)

// Record is the load-bearing subset of a finding used for deduplication.
// Evidence references are intentionally excluded: they explain why a finding
// was made, but do not change which subject/predicate/location it describes.
type Record struct {
	SubjectDigest string
	Predicate     string
	Location      string
}

// Key returns the stable identity for a complete finding record, or "" when
// the record is not identifiable and must fall back to a legacy heuristic.
func Key(r Record) string {
	subject := normalizeToken(r.SubjectDigest)
	predicate := normalizeToken(r.Predicate)
	if subject == "" || predicate == "" {
		return ""
	}
	location := NormalizeLocation(r.Location)
	if location == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{KeyVersion, subject, predicate, location}, "\x00")))
	return KeyVersion + ":" + hex.EncodeToString(sum[:])[:24]
}

// NormalizeLocation removes volatile line/column coordinates while preserving
// the path or logical scope. Findings on the same subject and predicate should
// not split only because two agents cited different line numbers.
func NormalizeLocation(location string) string {
	loc := strings.TrimSpace(location)
	if loc == "" {
		return ""
	}
	loc = filepath.ToSlash(loc)
	loc = lineAnchorSuffix.ReplaceAllString(loc, "")
	loc = lineColonSuffix.ReplaceAllString(loc, "")
	return strings.Join(strings.Fields(loc), " ")
}

// KeyFromFields computes a key from common metadata spellings. A precomputed
// finding key is trusted only when present; otherwise all identity fields must
// be available.
func KeyFromFields(fields map[string]string) string {
	if len(fields) == 0 {
		return ""
	}
	if key := strings.TrimSpace(first(fields, MetaKey, "finding_identity", "finding_identity_key")); key != "" {
		return key
	}
	return Key(Record{
		SubjectDigest: first(fields, MetaSubjectDigest, "finding_subject_digest", "audit_subject_digest", "content_hash", "audit_content_hash"),
		Predicate:     first(fields, MetaPredicate, "finding_predicate", "audit_predicate"),
		Location:      first(fields, MetaLocation, "finding_location", "normalized_location", "path", "file"),
	})
}

func normalizeToken(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(s)), " "))
}

func first(fields map[string]string, names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(fields[name]); v != "" {
			return v
		}
	}
	return ""
}
