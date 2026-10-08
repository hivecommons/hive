package dashboard

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// reviewEvidenceTestEnv points the evidence root, verdict artifact and
// review-links ledger at a temp dir and returns it.
func reviewEvidenceTestEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origRoot, origVerdicts, origLegacy, origLinks := ghpkg.ReviewEvidenceRoot, review.ReviewVerdictsPath, review.LegacyReviewVerdictsPath, ghpkg.ReviewLinksPath
	ghpkg.ReviewEvidenceRoot = filepath.Join(dir, "evidence")
	review.ReviewVerdictsPath = filepath.Join(dir, "review-verdicts.json")
	review.LegacyReviewVerdictsPath = filepath.Join(dir, "legacy-review-verdicts.json")
	ghpkg.ReviewLinksPath = filepath.Join(dir, "review-links.json")
	t.Cleanup(func() {
		ghpkg.ReviewEvidenceRoot, review.ReviewVerdictsPath, review.LegacyReviewVerdictsPath, ghpkg.ReviewLinksPath = origRoot, origVerdicts, origLegacy, origLinks
	})
	return dir
}

func seedReviewEvidenceBundle(t *testing.T, head, prev string, created time.Time, merged bool) *evidence.Bundle {
	t.Helper()
	b := &evidence.Bundle{
		SchemaVersion:    evidence.SchemaVersion,
		ID:               evidence.BundleID("acme/widget", 7, head),
		Repo:             "acme/widget",
		Number:           7,
		Author:           evidence.Author{Login: "alice", Kind: evidence.AuthorHuman},
		BaseSHA:          "base000",
		HeadSHA:          head,
		PreviousBundleID: prev,
		CreatedAt:        created,
		UpdatedAt:        created,
	}
	b.Verdicts = []evidence.Verdict{{Perspective: "security", Model: "m", Backend: "b", Verdict: "approve", Confidence: 1, RecordedAt: created}}
	if merged {
		b.Merge = &evidence.MergeEvent{Actor: "hive-app[bot]", Method: "squash", At: created, SHA: "merge1"}
	}
	if err := evidence.Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	path := ghpkg.ReviewEvidencePath("", "acme/widget", 7, head)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.MarshalIndent(b, "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return b
}

func reviewEvidenceRequest(t *testing.T, handler http.HandlerFunc, role, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("X-Hive-Role", role)
	if role == config.RoleOwner {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func TestHandleReviewEvidence(t *testing.T) {
	reviewEvidenceTestEnv(t)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	old := seedReviewEvidenceBundle(t, "aaaaaaa1", "", t0, false)
	latest := seedReviewEvidenceBundle(t, "bbbbbbb2", old.ID, t0.Add(time.Hour), true)
	seedReviewEvidenceBundle(t, "bbbbbbb3", evidence.BundleID("acme/widget", 7, "gone0001"), t0.Add(30*time.Minute), false)
	s := NewServer(0, slog.Default())

	tests := []struct {
		name       string
		role       string
		query      string
		wantCode   int
		wantHead   string
		wantStatus string
		wantError  string
		wantDispo  bool
	}{
		{name: "read-write forbidden", role: config.RoleReadWrite, query: "repo=acme/widget&number=7", wantCode: http.StatusForbidden},
		{name: "unverified owner forbidden", role: "owner-unverified", query: "repo=acme/widget&number=7", wantCode: http.StatusForbidden},
		{name: "merger latest", role: config.RoleMerger, query: "repo=acme/widget&number=7", wantCode: http.StatusOK, wantHead: "bbbbbbb2"},
		{name: "owner by head", role: config.RoleOwner, query: "repo=acme/widget&number=7&head=aaaaaaa1", wantCode: http.StatusOK, wantHead: "aaaaaaa1"},
		{name: "explicit json download", role: config.RoleOwner, query: "repo=acme/widget&number=7&format=JSON&download=1", wantCode: http.StatusOK, wantHead: "bbbbbbb2", wantDispo: true},
		{name: "bad repo", role: config.RoleOwner, query: "repo=widget&number=7", wantCode: http.StatusBadRequest, wantError: "owner/repo"},
		{name: "bad number", role: config.RoleOwner, query: "repo=acme/widget&number=x", wantCode: http.StatusBadRequest, wantError: "positive integer"},
		{name: "bad format", role: config.RoleOwner, query: "repo=acme/widget&number=7&format=tar", wantCode: http.StatusBadRequest, wantError: "json or zip"},
		{name: "ambiguous head", role: config.RoleOwner, query: "repo=acme/widget&number=7&head=bbbbbbb", wantCode: http.StatusConflict, wantError: "matches 2 bundles"},
		{name: "expired head", role: config.RoleOwner, query: "repo=acme/widget&number=7&head=gone0001", wantCode: http.StatusNotFound, wantStatus: "expired"},
		{name: "never recorded", role: config.RoleOwner, query: "repo=acme/widget&number=8", wantCode: http.StatusNotFound, wantStatus: "not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role := tt.role
			req := httptest.NewRequest(http.MethodGet, "/api/review/evidence?"+tt.query, nil)
			if role == "owner-unverified" {
				role = config.RoleOwner
				req.Header.Set("X-Hive-Role", role)
			} else {
				req.Header.Set("X-Hive-Role", role)
				if role == config.RoleOwner {
					req.Header.Set(ownerRoleVerifiedHeader, "true")
				}
			}
			w := httptest.NewRecorder()
			s.handleReviewEvidence(w, req)
			if w.Code != tt.wantCode {
				t.Fatalf("status %d, want %d: %s", w.Code, tt.wantCode, w.Body.String())
			}
			if tt.wantHead != "" {
				var b evidence.Bundle
				if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
					t.Fatal(err)
				}
				if b.HeadSHA != tt.wantHead {
					t.Fatalf("head %s, want %s", b.HeadSHA, tt.wantHead)
				}
				// Served byte for byte as sealed, so it still verifies its hash.
				if h, _ := evidence.Hash(&b); h != b.Hash {
					t.Fatalf("served bundle hash mismatch")
				}
				if b.HeadSHA == "bbbbbbb2" && (b.ID != latest.ID || b.Merge == nil) {
					t.Fatalf("latest bundle = %+v", b)
				}
			}
			if got := w.Header().Get("Content-Disposition"); tt.wantDispo != (got != "") || (tt.wantDispo && !strings.Contains(got, `filename="acme-widget-7-bbbbbbb2.json"`)) {
				t.Fatalf("Content-Disposition = %q", got)
			}
			if tt.wantStatus != "" {
				var body reviewEvidenceMissingResponse
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Status != tt.wantStatus || body.Expired != (tt.wantStatus == "expired") || body.Retention.ExpiredMarkerKind != "expired" || body.Retention.Contract == "" || body.Error == "" {
					t.Fatalf("missing body = %+v", body)
				}
			}
			if tt.wantError != "" && !strings.Contains(w.Body.String(), tt.wantError) {
				t.Fatalf("body %s, want %q", w.Body.String(), tt.wantError)
			}
		})
	}
}

func TestHandleReviewEvidenceZip(t *testing.T) {
	reviewEvidenceTestEnv(t)
	b := seedReviewEvidenceBundle(t, "aaaaaaa1", "", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), true)
	if err := review.WriteArtifact(review.ReviewVerdictsPath, review.Artifact{Items: []review.Aggregate{
		{Repo: "acme/widget", Number: 7, HeadSHA: "aaaaaaa1", Verdict: review.VerdictApprove},
		{Repo: "widget", Number: 7, Verdict: review.VerdictApprove},
		{Repo: "acme/widget", Number: 7, HeadSHA: "older", Verdict: review.VerdictChangesRequested},
		{Repo: "acme/widget", Number: 8, HeadSHA: "aaaaaaa1", Verdict: review.VerdictApprove},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := ghpkg.RecordReviewLink(ghpkg.ReviewLinksPath, "acme/widget", 7, ghpkg.ReviewLink{URL: "https://github.com/acme/widget/pull/7#pullrequestreview-1", HeadSHA: "aaaaaaa1"}); err != nil {
		t.Fatal(err)
	}
	if err := ghpkg.RecordReviewLink(ghpkg.ReviewLinksPath, "acme/widget", 9, ghpkg.ReviewLink{URL: "https://github.com/acme/widget/pull/9#pullrequestreview-2"}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(0, slog.Default())

	w := reviewEvidenceRequest(t, s.handleReviewEvidence, config.RoleOwner, "/api/review/evidence?repo=acme/widget&number=7&format=zip")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("status %d type %q: %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), `filename="acme-widget-7-aaaaaaa1.zip"`) {
		t.Fatalf("Content-Disposition = %q", w.Header().Get("Content-Disposition"))
	}
	files := readEvidenceZip(t, w.Body.Bytes())
	for _, name := range []string{"bundle.json", "verdicts.json", "review-links.json", "manifest.json"} {
		if _, ok := files["acme-widget-7-aaaaaaa1/"+name]; !ok {
			t.Fatalf("zip missing %s: %v", name, evidenceZipNames(files))
		}
	}
	var got evidence.Bundle
	if err := json.Unmarshal(files["acme-widget-7-aaaaaaa1/bundle.json"], &got); err != nil || got.Hash != b.Hash {
		t.Fatalf("bundle in zip = %+v, %v", got, err)
	}
	var verdicts struct {
		Items []review.Aggregate `json:"items"`
	}
	if err := json.Unmarshal(files["acme-widget-7-aaaaaaa1/verdicts.json"], &verdicts); err != nil || len(verdicts.Items) != 2 {
		t.Fatalf("verdicts = %+v, %v (want this PR's head and head-less items only)", verdicts.Items, err)
	}
	var links struct {
		Links map[string]ghpkg.ReviewLink `json:"links"`
	}
	if err := json.Unmarshal(files["acme-widget-7-aaaaaaa1/review-links.json"], &links); err != nil || len(links.Links) != 1 || links.Links["acme/widget#7"].HeadSHA != "aaaaaaa1" {
		t.Fatalf("links = %+v, %v", links, err)
	}
	var manifest reviewEvidenceManifest
	if err := json.Unmarshal(files["acme-widget-7-aaaaaaa1/manifest.json"], &manifest); err != nil || manifest.BundleID != b.ID || len(manifest.Files) != 3 || len(manifest.Notes) != 0 {
		t.Fatalf("manifest = %+v, %v", manifest, err)
	}

	// Unreadable artifacts are left out and named, never fatal.
	if err := os.WriteFile(review.ReviewVerdictsPath, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ghpkg.ReviewLinksPath, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	w = reviewEvidenceRequest(t, s.handleReviewEvidence, config.RoleMerger, "/api/review/evidence?repo=acme/widget&number=7&format=zip")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	files = readEvidenceZip(t, w.Body.Bytes())
	if err := json.Unmarshal(files["acme-widget-7-aaaaaaa1/manifest.json"], &manifest); err != nil || len(manifest.Files) != 1 || len(manifest.Notes) != 2 {
		t.Fatalf("degraded manifest = %+v, %v", manifest, err)
	}
	if _, ok := files["acme-widget-7-aaaaaaa1/verdicts.json"]; ok {
		t.Fatal("unreadable verdicts still zipped")
	}
}

func TestHandleReviewEvidenceUnreadableRoot(t *testing.T) {
	reviewEvidenceTestEnv(t)
	// A file where the PR directory belongs makes the read fail outright.
	prDir := ghpkg.ReviewEvidenceDir("", "acme/widget", 7)
	if err := os.MkdirAll(filepath.Dir(prDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewServer(0, slog.Default())
	for _, h := range []http.HandlerFunc{s.handleReviewEvidence, s.handleReviewEvidenceList} {
		if w := reviewEvidenceRequest(t, h, config.RoleOwner, "/api/review/evidence?repo=acme/widget&number=7"); w.Code != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500: %s", w.Code, w.Body.String())
		}
	}
}

func TestHandleReviewEvidenceList(t *testing.T) {
	reviewEvidenceTestEnv(t)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	first := seedReviewEvidenceBundle(t, "aaaaaaa1", "", t0, false)
	second := seedReviewEvidenceBundle(t, "bbbbbbb2", first.ID, t0.Add(time.Hour), true)
	if err := os.MkdirAll(ghpkg.ReviewEvidenceDir("", "acme/widget", 9), 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewServer(0, slog.Default())

	if w := reviewEvidenceRequest(t, s.handleReviewEvidenceList, config.RoleReadWrite, "/api/review/evidence/list?repo=acme/widget&number=7"); w.Code != http.StatusForbidden {
		t.Fatalf("read-write status %d, want 403", w.Code)
	}
	if w := reviewEvidenceRequest(t, s.handleReviewEvidenceList, config.RoleOwner, "/api/review/evidence/list?repo=acme&number=7"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad repo status %d, want 400", w.Code)
	}

	decode := func(w *httptest.ResponseRecorder) reviewEvidenceListResponse {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var resp reviewEvidenceListResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := decode(reviewEvidenceRequest(t, s.handleReviewEvidenceList, config.RoleMerger, "/api/review/evidence/list?repo=acme/widget&number=7"))
	if len(resp.Bundles) != 2 || resp.Latest != second.ID || resp.Bundles[0].ID != first.ID || resp.Bundles[1].PreviousBundleID != first.ID ||
		!resp.Bundles[1].Merged || resp.Bundles[0].Merged || resp.Bundles[1].Verdicts != 1 || resp.Bundles[1].Hash != second.Hash || resp.Expired {
		t.Fatalf("list = %+v", resp)
	}
	resp = decode(reviewEvidenceRequest(t, s.handleReviewEvidenceList, config.RoleOwner, "/api/review/evidence/list?repo=acme/widget&number=9"))
	if len(resp.Bundles) != 0 || !resp.Expired || resp.Latest != "" || resp.Reason == "" {
		t.Fatalf("expired list = %+v", resp)
	}
	resp = decode(reviewEvidenceRequest(t, s.handleReviewEvidenceList, config.RoleOwner, "/api/review/evidence/list?repo=acme/widget&number=10"))
	if resp.Bundles == nil || len(resp.Bundles) != 0 || resp.Expired || resp.Retention.ExpiredMarkerKind != "expired" {
		t.Fatalf("empty list = %+v", resp)
	}
}

func TestReviewEvidenceRoutesRegistered(t *testing.T) {
	reviewEvidenceTestEnv(t)
	seedReviewEvidenceBundle(t, "aaaaaaa1", "", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), false)
	s := NewServer(0, slog.Default())
	for _, path := range []string{"/api/review/evidence?repo=acme/widget&number=7", "/api/review/evidence/list?repo=acme/widget&number=7"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Hive-Role", config.RoleMerger)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s via mux: status %d: %s", path, w.Code, w.Body.String())
		}
	}
}

func TestReviewEvidenceFilename(t *testing.T) {
	if got := reviewEvidenceFilename("ac me/wid\"get", 7, "a/b"); got != "ac-me-wid-get-7-a-b" {
		t.Fatalf("filename = %q", got)
	}
}

func readEvidenceZip(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		out[f.Name] = b
	}
	return out
}

func evidenceZipNames(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestHandleReviewEvidenceZipCarriesPublicKey(t *testing.T) {
	reviewEvidenceTestEnv(t)
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	b := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, ID: evidence.BundleID("acme/widget", 7, "aaaaaaa1"),
		Repo: "acme/widget", Number: 7, Author: evidence.Author{Login: "alice", Kind: evidence.AuthorHuman},
		BaseSHA: "base000", HeadSHA: "aaaaaaa1", CreatedAt: created, UpdatedAt: created,
	}
	if err := evidence.Sign(b, priv); err != nil {
		t.Fatal(err)
	}
	path := ghpkg.ReviewEvidencePath("", "acme/widget", 7, "aaaaaaa1")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(b)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "signing.key")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(make([]byte, ed25519.SeedSize))), 0o600); err != nil {
		t.Fatal(err)
	}
	badKey := filepath.Join(t.TempDir(), "bad.key")
	if err := os.WriteFile(badKey, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name     string
		keyFile  string
		wantKey  string
		wantNote bool
	}{
		{name: "configured key", keyFile: keyFile, wantKey: hex.EncodeToString(priv.Public().(ed25519.PublicKey))},
		{name: "no key configured", keyFile: ""},
		{name: "unreadable key", keyFile: badKey, wantNote: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := NewServer(0, slog.Default())
			s.deps = &Dependencies{Config: &config.Config{Evidence: config.EvidenceConfig{SigningKeyFile: tt.keyFile}}, Logger: slog.Default()}
			w := reviewEvidenceRequest(t, s.handleReviewEvidence, config.RoleOwner, "/api/review/evidence?repo=acme/widget&number=7&format=zip")
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			var manifest reviewEvidenceManifest
			if err := json.Unmarshal(readEvidenceZip(t, w.Body.Bytes())["acme-widget-7-aaaaaaa1/manifest.json"], &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.PublicKey != tt.wantKey || !manifest.Signed {
				t.Fatalf("manifest key %q signed %v, want %q", manifest.PublicKey, manifest.Signed, tt.wantKey)
			}
			hasNote := false
			for _, n := range manifest.Notes {
				hasNote = hasNote || strings.HasPrefix(n, "public_key omitted")
			}
			if hasNote != tt.wantNote {
				t.Fatalf("notes = %v", manifest.Notes)
			}
		})
	}
}
