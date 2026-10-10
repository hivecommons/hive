package dashboard

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/review/pipeline"
)

// Review evidence reader (hivecommons/hive#11061): serves the per-PR review
// evidence bundles the review relay and merge path write under
// /data/evidence (pkg/github review_evidence*.go). Read-only, and
// owner/merger only: a bundle names reviewers, models and findings.

// reviewEvidenceRetention is the retention block every evidence response
// carries, mirroring /api/runs/audit's contract: what ages out is reported
// with an explicit expired marker, never as silent absence.
type reviewEvidenceRetention struct {
	Contract          string `json:"contract"`
	ExpiredMarkerKind string `json:"expired_marker_kind"`
}

func newReviewEvidenceRetention() reviewEvidenceRetention {
	return reviewEvidenceRetention{Contract: ghpkg.ReviewEvidenceRetention, ExpiredMarkerKind: "expired"}
}

// reviewEvidenceMissingResponse is the 404 body: Status is "expired" when the
// bundle is known to have existed and is gone, "not_found" when this hive
// never recorded one.
type reviewEvidenceMissingResponse struct {
	OK        bool                    `json:"ok"`
	Error     string                  `json:"error"`
	Status    string                  `json:"status"`
	Expired   bool                    `json:"expired"`
	Repo      string                  `json:"repo"`
	Number    int                     `json:"number"`
	Head      string                  `json:"head,omitempty"`
	Retention reviewEvidenceRetention `json:"retention"`
}

type reviewEvidenceListItem struct {
	ID               string    `json:"id"`
	HeadSHA          string    `json:"head_sha"`
	PreviousBundleID string    `json:"previous_bundle_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	Hash             string    `json:"hash"`
	Signed           bool      `json:"signed"`
	Verdicts         int       `json:"verdicts"`
	Merged           bool      `json:"merged"`
}

type reviewEvidenceListResponse struct {
	Repo      string                   `json:"repo"`
	Number    int                      `json:"number"`
	Latest    string                   `json:"latest,omitempty"`
	Bundles   []reviewEvidenceListItem `json:"bundles"`
	Expired   bool                     `json:"expired"`
	Reason    string                   `json:"reason,omitempty"`
	Retention reviewEvidenceRetention  `json:"retention"`
}

// reviewEvidenceManifestFile is one entry of the zip's manifest.json.
type reviewEvidenceManifestFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

type reviewEvidenceManifest struct {
	BundleID    string                       `json:"bundle_id"`
	Hash        string                       `json:"hash"`
	Signed      bool                         `json:"signed"`
	PublicKey   string                       `json:"public_key,omitempty"`
	GeneratedAt time.Time                    `json:"generated_at"`
	Files       []reviewEvidenceManifestFile `json:"files"`
	Notes       []string                     `json:"notes,omitempty"`
}

// reviewEvidenceTarget parses ?repo=owner/name&number=N.
func reviewEvidenceTarget(r *http.Request) (string, int, error) {
	q := r.URL.Query()
	repo := strings.Trim(strings.TrimSpace(q.Get("repo")), "/")
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", 0, errors.New("repo must be owner/repo")
	}
	number, err := strconv.Atoi(strings.TrimSpace(q.Get("number")))
	if err != nil || number <= 0 {
		return "", 0, errors.New("number must be a positive integer")
	}
	return repo, number, nil
}

// handleReviewEvidence serves GET /api/review/evidence?repo=&number=
// [&head=][&format=json|zip][&download=1]. Without head it returns the
// newest bundle (the latest head Hive reviewed). format=json (default)
// serves the bundle exactly as sealed on disk; format=zip streams the bundle
// with the verdict reports and review-links entry it was built from plus a
// manifest of their SHA-256s.
func (s *Server) handleReviewEvidence(w http.ResponseWriter, r *http.Request) {
	if !requireMergerOrOwnerRole(w, r) {
		return
	}
	repo, number, err := reviewEvidenceTarget(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	format := strings.ToLower(strings.TrimSpace(q.Get("format")))
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "zip" {
		jsonError(w, "format must be json or zip", http.StatusBadRequest)
		return
	}
	head := strings.TrimSpace(q.Get("head"))
	found, err := ghpkg.FindReviewEvidence("", repo, number, head)
	if err != nil {
		jsonError(w, "review evidence unreadable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if found.Ambiguous {
		jsonError(w, found.Reason, http.StatusConflict)
		return
	}
	if found.Entry == nil {
		status := "not_found"
		if found.Expired {
			status = "expired"
		}
		jsonStatusResponse(w, http.StatusNotFound, reviewEvidenceMissingResponse{
			Error: found.Reason, Status: status, Expired: found.Expired,
			Repo: repo, Number: number, Head: head, Retention: newReviewEvidenceRetention(),
		})
		return
	}
	entry := found.Entry
	base := reviewEvidenceFilename(repo, number, entry.Bundle.HeadSHA)
	if format == "zip" {
		s.writeReviewEvidenceZip(w, repo, number, entry, base)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if q.Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.json"`, base))
	}
	_, _ = w.Write(entry.Raw)
}

// handleReviewEvidenceList serves GET /api/review/evidence/list?repo=&number=:
// every retained bundle for the PR, oldest first, with the newest as latest.
func (s *Server) handleReviewEvidenceList(w http.ResponseWriter, r *http.Request) {
	if !requireMergerOrOwnerRole(w, r) {
		return
	}
	repo, number, err := reviewEvidenceTarget(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	entries, err := ghpkg.ListReviewEvidence("", repo, number)
	if err != nil {
		jsonError(w, "review evidence unreadable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resp := reviewEvidenceListResponse{Repo: repo, Number: number, Bundles: []reviewEvidenceListItem{}, Retention: newReviewEvidenceRetention()}
	for _, e := range entries {
		b := e.Bundle
		resp.Bundles = append(resp.Bundles, reviewEvidenceListItem{
			ID: b.ID, HeadSHA: b.HeadSHA, PreviousBundleID: b.PreviousBundleID,
			CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt, Hash: b.Hash, Signed: b.Signed,
			Verdicts: len(b.Verdicts), Merged: b.Merge != nil,
		})
	}
	if n := len(resp.Bundles); n > 0 {
		resp.Latest = resp.Bundles[n-1].ID
	} else {
		missing, err := ghpkg.FindReviewEvidence("", repo, number, "")
		if err == nil {
			resp.Expired, resp.Reason = missing.Expired, missing.Reason
		}
	}
	jsonResponse(w, resp)
}

// reviewEvidenceFilename is a download name with no separators or quotes:
// owner-repo-N-<head>.
func reviewEvidenceFilename(repo string, number int, head string) string {
	raw := fmt.Sprintf("%s-%d-%s", repo, number, head)
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, raw)
}

// writeReviewEvidenceZip streams the bundle and the artifacts it references.
// Each artifact is assembled before the first byte is written so the zip is
// never cut off half way by a read failure; an artifact that cannot be read
// is left out and named in the manifest's notes.
func (s *Server) writeReviewEvidenceZip(w http.ResponseWriter, repo string, number int, entry *ghpkg.ReviewEvidenceEntry, base string) {
	type zipFile struct {
		name string
		data []byte
	}
	files := []zipFile{{name: "bundle.json", data: entry.Raw}}
	var notes []string
	head := entry.Bundle.HeadSHA

	if artifact, err := review.LoadArtifact(""); err != nil && !errors.Is(err, os.ErrNotExist) {
		notes = append(notes, "verdicts.json omitted: review verdicts unreadable: "+err.Error())
	} else {
		items := []review.Aggregate{}
		for _, item := range artifact.Items {
			if item.Number == number && pipeline.SameRepo(repo, item.Repo) && (item.HeadSHA == "" || strings.EqualFold(item.HeadSHA, head)) {
				items = append(items, item)
			}
		}
		data, _ := json.MarshalIndent(map[string]any{"generated_at": artifact.GeneratedAt, "items": items}, "", "  ")
		files = append(files, zipFile{name: "verdicts.json", data: data})
	}

	if links, err := ghpkg.LoadReviewLinks(""); err != nil {
		notes = append(notes, "review-links.json omitted: review links unreadable: "+err.Error())
	} else {
		slice := map[string]ghpkg.ReviewLink{}
		if link, ok := lookupReviewLink(links, repo, reviewPipelineBareRepo(repo), number); ok {
			slice[ghpkg.ReviewLinkKey(repo, number)] = link
		}
		data, _ := json.MarshalIndent(map[string]any{"links": slice}, "", "  ")
		files = append(files, zipFile{name: "review-links.json", data: data})
	}

	manifest := reviewEvidenceManifest{
		BundleID: entry.Bundle.ID, Hash: entry.Bundle.Hash, Signed: entry.Bundle.Signed,
		GeneratedAt: time.Now().UTC(), Notes: notes,
	}
	// The public half of the configured signing key, so `hivectl review
	// evidence verify` can check the archive offline. It is the key in force
	// now: after a rotation, verify against the key published at signing time.
	if entry.Bundle.Signed && s != nil && s.deps != nil && s.deps.Config != nil {
		if pub, err := ghpkg.ReviewEvidencePublicKey(s.deps.Config.Evidence.SigningKeyPath()); err == nil {
			manifest.PublicKey = pub
		} else {
			manifest.Notes = append(manifest.Notes, "public_key omitted: signing key unreadable: "+err.Error())
		}
	}
	for _, f := range files {
		sum := sha256.Sum256(f.data)
		manifest.Files = append(manifest.Files, reviewEvidenceManifestFile{Name: f.name, SHA256: hex.EncodeToString(sum[:]), Bytes: len(f.data)})
	}
	manifestData, _ := json.MarshalIndent(manifest, "", "  ")
	files = append(files, zipFile{name: "manifest.json", data: manifestData})

	// Built in memory (bundles are small) so Content-Length is exact and a
	// client never receives a truncated archive. Writes to a bytes.Buffer
	// cannot fail and the names are fixed, so the zip errors are unreachable.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		fw, _ := zw.Create(base + "/" + f.name)
		_, _ = fw.Write(f.data)
	}
	_ = zw.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, base))
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	if _, err := w.Write(buf.Bytes()); err != nil && s != nil && s.logger != nil {
		s.logger.Warn("review evidence archive write failed", "repo", repo, "number", number, "error", err)
	}
}
