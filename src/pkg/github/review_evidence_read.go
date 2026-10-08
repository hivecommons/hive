package github

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/evidence"
)

// ReviewEvidenceRetention describes how long bundles are kept, for the API's
// retention block. Bundles live on the durable data dir and nothing prunes
// them automatically; what an operator (or a volume wipe) removes is reported
// as expired, never as silent absence (hivecommons/hive#11061).
const ReviewEvidenceRetention = "review evidence bundles under /data/evidence are kept until an operator removes them; a bundle this hive can prove existed (its PR directory remains, or a newer bundle links to it) is reported expired"

// ReviewEvidenceEntry is one bundle as stored: the parsed bundle plus the
// exact bytes on disk, so a download serves what was sealed byte for byte.
type ReviewEvidenceEntry struct {
	Bundle *evidence.Bundle
	Raw    []byte
	Path   string
}

// ReviewEvidenceLookup is the answer to "which bundle is this?". Entry is nil
// when no bundle matched; Expired then says whether one is known to have
// existed and been removed, Ambiguous that a head prefix matched more than
// one bundle, and Reason explains either way.
type ReviewEvidenceLookup struct {
	Entry     *ReviewEvidenceEntry
	Expired   bool
	Ambiguous bool
	Reason    string
}

// ReviewEvidenceDir is the directory holding every bundle for repo#number
// under root (ReviewEvidenceRoot when empty).
func ReviewEvidenceDir(root, repo string, number int) string {
	return filepath.Dir(ReviewEvidencePath(root, repo, number, "head"))
}

// ListReviewEvidence returns every readable bundle for repo#number, oldest
// first. A PR with no directory has no bundles and is not an error. Files
// that do not parse, or that belong to a different PR, are skipped: the list
// is what the reader can stand behind.
func ListReviewEvidence(root, repo string, number int) ([]ReviewEvidenceEntry, error) {
	dir := ReviewEvidenceDir(root, repo, number)
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	var out []ReviewEvidenceEntry
	for _, e := range dirEntries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		b, err := LoadReviewEvidence(path)
		if err != nil || b.Number != number || !strings.EqualFold(b.Repo, strings.TrimSpace(repo)) {
			continue
		}
		out = append(out, ReviewEvidenceEntry{Bundle: b, Raw: raw, Path: path})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Bundle, out[j].Bundle
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	return out, nil
}

// FindReviewEvidence selects the bundle for repo#number. An empty head picks
// the newest bundle (the latest head Hive reviewed); otherwise head matches a
// bundle's head SHA exactly or as a unique prefix of at least 7 characters.
func FindReviewEvidence(root, repo string, number int, head string) (ReviewEvidenceLookup, error) {
	entries, err := ListReviewEvidence(root, repo, number)
	if err != nil {
		return ReviewEvidenceLookup{}, err
	}
	head = strings.ToLower(strings.TrimSpace(head))
	if head == "" {
		if len(entries) > 0 {
			return ReviewEvidenceLookup{Entry: &entries[len(entries)-1]}, nil
		}
		return reviewEvidenceMissing(root, repo, number, "", entries), nil
	}
	var matches []int
	for i, e := range entries {
		h := strings.ToLower(e.Bundle.HeadSHA)
		if h == head {
			return ReviewEvidenceLookup{Entry: &entries[i]}, nil
		}
		if len(head) >= 7 && strings.HasPrefix(h, head) {
			matches = append(matches, i)
		}
	}
	switch len(matches) {
	case 1:
		return ReviewEvidenceLookup{Entry: &entries[matches[0]]}, nil
	case 0:
		return reviewEvidenceMissing(root, repo, number, head, entries), nil
	}
	return ReviewEvidenceLookup{Ambiguous: true, Reason: fmt.Sprintf("head %q matches %d bundles; give more of the SHA", head, len(matches))}, nil
}

// reviewEvidenceMissing decides between "expired" and "never recorded". A
// bundle is known to have existed when a retained bundle names it as its
// previous head, or when the PR's directory survives with no bundles left in
// it (the writer only creates it to hold one).
func reviewEvidenceMissing(root, repo string, number int, head string, entries []ReviewEvidenceEntry) ReviewEvidenceLookup {
	if head != "" {
		for _, e := range entries {
			prev := strings.ToLower(e.Bundle.PreviousBundleID)
			if _, prevHead, ok := strings.Cut(prev, "@"); ok && (prevHead == head || (len(head) >= 7 && strings.HasPrefix(prevHead, head))) {
				return ReviewEvidenceLookup{Expired: true, Reason: fmt.Sprintf("bundle for head %s was linked from %s but is no longer retained", head, e.Bundle.ID)}
			}
		}
	}
	if len(entries) == 0 {
		if info, err := os.Stat(ReviewEvidenceDir(root, repo, number)); err == nil && info.IsDir() {
			return ReviewEvidenceLookup{Expired: true, Reason: "bundles for this PR were recorded but are no longer retained"}
		}
		return ReviewEvidenceLookup{Reason: "no review evidence was recorded for this PR"}
	}
	return ReviewEvidenceLookup{Reason: fmt.Sprintf("no review evidence was recorded for head %s", head)}
}

// ReviewEvidencePublicKey is the hex Ed25519 public key for the signing key
// at path, or "" when no key is configured (the writer then seals bundles
// unsigned). It lets a download carry the key a verifier needs without ever
// exposing the private half.
func ReviewEvidencePublicKey(path string) (string, error) {
	key, err := evidence.LoadSigningKey(path)
	if err != nil || len(key) == 0 {
		return "", err
	}
	return hex.EncodeToString(key.Public().(ed25519.PublicKey)), nil
}
