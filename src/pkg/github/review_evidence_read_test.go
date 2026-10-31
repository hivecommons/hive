package github

// Tests for the evidence reader (review_evidence_read.go) and the merge
// pointer (review_evidence_merge.go): latest/head/prefix selection, the
// expired-vs-never-recorded distinction, and one marker comment per PR no
// matter how many times a merge path reports the merge.

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/evidence"
)

func writeTestEvidence(t *testing.T, root, head, prev string, created time.Time) *evidence.Bundle {
	t.Helper()
	b := &evidence.Bundle{
		SchemaVersion:    evidence.SchemaVersion,
		ID:               evidence.BundleID("o/r", 7, head),
		Repo:             "o/r",
		Number:           7,
		Author:           evidence.Author{Login: "alice", Kind: evidence.AuthorHuman},
		BaseSHA:          "base000",
		HeadSHA:          head,
		PreviousBundleID: prev,
		CreatedAt:        created,
		UpdatedAt:        created,
		Verdicts: []evidence.Verdict{{
			Perspective: "security", Model: "m", Backend: "b", Verdict: "approve", Confidence: 1, RecordedAt: created,
		}},
	}
	if err := evidence.Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeReviewEvidence(ReviewEvidencePath(root, "o/r", 7, head), b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFindReviewEvidence(t *testing.T) {
	root := t.TempDir()
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	writeTestEvidence(t, root, "aaaaaaa111", "", t0)
	writeTestEvidence(t, root, "bbbbbbb222", evidence.BundleID("o/r", 7, "aaaaaaa111"), t0.Add(time.Hour))
	writeTestEvidence(t, root, "bbbbbbb333", evidence.BundleID("o/r", 7, "gone0000999"), t0.Add(2*time.Hour))
	dir := ReviewEvidenceDir(root, "o/r", 7)
	// Noise the reader must skip: a temp file, a sub-directory, garbage and a
	// bundle for a different PR that landed in this directory.
	for name, data := range map[string]string{"x.json.tmp": "{", "bad.json": "{nope"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	other, _ := json.Marshal(evidence.Bundle{Repo: "o/r", Number: 8, HeadSHA: "zzz"})
	if err := os.WriteFile(filepath.Join(dir, "other.json"), other, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ReviewEvidenceDir(root, "o/r", 9), 0o755); err != nil {
		t.Fatal(err)
	}

	list, err := ListReviewEvidence(root, "o/r", 7)
	if err != nil || len(list) != 3 || list[0].Bundle.HeadSHA != "aaaaaaa111" || list[2].Bundle.HeadSHA != "bbbbbbb333" {
		t.Fatalf("list = %d entries, err %v", len(list), err)
	}
	if len(list[0].Raw) == 0 || list[0].Path == "" {
		t.Fatalf("entry missing raw bytes or path: %+v", list[0])
	}

	tests := []struct {
		name        string
		number      int
		head        string
		wantHead    string
		wantExpired bool
		wantReason  string
	}{
		{name: "latest", number: 7, wantHead: "bbbbbbb333"},
		{name: "exact head", number: 7, head: "aaaaaaa111", wantHead: "aaaaaaa111"},
		{name: "case-insensitive", number: 7, head: "AAAAAAA111", wantHead: "aaaaaaa111"},
		{name: "unique prefix", number: 7, head: "aaaaaaa", wantHead: "aaaaaaa111"},
		{name: "ambiguous prefix", number: 7, head: "bbbbbbb", wantReason: "matches 2 bundles"},
		{name: "short prefix is not a match", number: 7, head: "aaa", wantReason: "no review evidence was recorded for head aaa"},
		{name: "linked but gone is expired", number: 7, head: "gone0000999", wantExpired: true, wantReason: "no longer retained"},
		{name: "linked prefix is expired", number: 7, head: "gone000", wantExpired: true},
		{name: "unknown head", number: 7, head: "ccccccc444", wantReason: "no review evidence was recorded for head"},
		{name: "empty dir is expired", number: 9, wantExpired: true, wantReason: "no longer retained"},
		{name: "never recorded", number: 10, wantReason: "no review evidence was recorded for this PR"},
		{name: "never recorded head", number: 10, head: "abc", wantReason: "no review evidence was recorded for this PR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FindReviewEvidence(root, "o/r", tt.number, tt.head)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantHead != "" {
				if got.Entry == nil || got.Entry.Bundle.HeadSHA != tt.wantHead {
					t.Fatalf("got %+v, want head %s", got, tt.wantHead)
				}
				return
			}
			if got.Entry != nil || got.Expired != tt.wantExpired || got.Ambiguous != (tt.name == "ambiguous prefix") || !strings.Contains(got.Reason, tt.wantReason) {
				t.Fatalf("got %+v, want expired=%v reason~%q", got, tt.wantExpired, tt.wantReason)
			}
		})
	}
}

func TestListReviewEvidenceUnreadableDir(t *testing.T) {
	root := t.TempDir()
	dir := ReviewEvidenceDir(root, "o/r", 7)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file where the PR directory should be makes ReadDir fail with
	// something other than not-exist.
	if err := os.WriteFile(dir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ListReviewEvidence(root, "o/r", 7); err == nil {
		t.Fatal("want error for unreadable PR directory")
	}
	if _, err := FindReviewEvidence(root, "o/r", 7, ""); err == nil {
		t.Fatal("want error from FindReviewEvidence")
	}
}

func TestReviewEvidenceVerifierRejectsTamperingAndDowngrade(t *testing.T) {
	root := t.TempDir()
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	b := writeTestEvidence(t, root, "aaaaaaa111", "", created)
	path := ReviewEvidencePath(root, "o/r", 7, b.HeadSHA)

	seed := make([]byte, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	if err := evidence.Sign(b, priv); err != nil {
		t.Fatal(err)
	}
	if err := writeReviewEvidence(path, b); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "evidence.key")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(seed)), 0o600); err != nil {
		t.Fatal(err)
	}
	verifier, err := NewReviewEvidenceVerifier(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadVerifiedReviewEvidence(path, verifier); err != nil {
		t.Fatalf("valid signed bundle rejected: %v", err)
	}

	b.Author.Login = "mallory"
	if err := writeReviewEvidence(path, b); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadVerifiedReviewEvidence(path, verifier); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("tampered bundle error = %v, want hash mismatch", err)
	}

	if err := evidence.Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeReviewEvidence(path, b); err != nil {
		t.Fatal(err)
	}
	if _, err := FindVerifiedReviewEvidence(root, "o/r", 7, b.HeadSHA, verifier); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("signature downgrade error = %v, want not signed", err)
	}
	if _, err := LoadVerifiedReviewEvidence(path, ReviewEvidenceVerifier{}); err != nil {
		t.Fatalf("valid unsigned bundle rejected in unsigned mode: %v", err)
	}

	if err := evidence.Sign(b, priv); err != nil {
		t.Fatal(err)
	}
	if err := writeReviewEvidence(path, b); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadVerifiedReviewEvidence(path, ReviewEvidenceVerifier{}); err == nil || !strings.Contains(err.Error(), "without the signing key") {
		t.Fatalf("signed bundle without key error = %v", err)
	}
	if _, err := NewReviewEvidenceVerifier(filepath.Join(t.TempDir(), "missing.key")); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing configured key error = %v", err)
	}
}

func TestReviewEvidenceComment(t *testing.T) {
	got := ReviewEvidenceComment(" o/r ", 7, "o/r#7@abc", "deadbeef")
	want := "<!-- hive-review-evidence --> Evidence bundle `o/r#7@abc` sha256:deadbeef — download from the dashboard or `hivectl review evidence o/r#7`."
	if got != want {
		t.Fatalf("comment =\n%s\nwant\n%s", got, want)
	}
}

type evidenceMergeFixture struct {
	head      string
	base      string
	author    string
	mergedBy  string
	prStatus  int
	listFail  bool
	postFail  bool
	comments  []string
	posts     int
	edits     int
	prFetches int
}

func newEvidenceMergeServer(t *testing.T, f *evidenceMergeFixture) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
			f.prFetches++
			if f.prStatus != 0 {
				w.WriteHeader(f.prStatus)
				return
			}
			pr := map[string]any{"number": 7, "head": map[string]any{"sha": f.head}}
			if f.mergedBy != "" {
				pr["merged_by"] = map[string]any{"login": f.mergedBy}
			}
			if f.base != "" {
				pr["base"] = map[string]any{"sha": f.base}
			}
			if f.author != "" {
				pr["user"] = map[string]any{"login": f.author, "type": "Bot"}
			}
			_ = json.NewEncoder(w).Encode(pr)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			if f.listFail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			out := []map[string]any{}
			for i, c := range f.comments {
				out = append(out, map[string]any{"id": i + 1, "body": c})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/comments/"):
			f.edits++
			id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/o/r/issues/comments/"))
			var in struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if id >= 1 && id <= len(f.comments) {
				f.comments[id-1] = in.Body
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			f.posts++
			if f.postFail {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			var in struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.comments = append(f.comments, in.Body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": len(f.comments)})
		default:
			t.Logf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func evidenceMergeClient(t *testing.T, f *evidenceMergeFixture, settings *ReviewEvidenceSettings) *Client {
	t.Helper()
	c := newTestClient(t, newEvidenceMergeServer(t, f), "o", []string{"o/r"})
	if settings != nil {
		s := *settings
		c.SetReviewEvidence(func(string) ReviewEvidenceSettings { return s })
	}
	return c
}

func TestRecordPRMergedAuditPostsEvidencePointerOnce(t *testing.T) {
	root := withEvidenceRoot(t)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seed := make([]byte, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	for _, b := range []*evidence.Bundle{
		writeTestEvidence(t, root, "head111", "", t0),
		writeTestEvidence(t, root, "head222", evidence.BundleID("o/r", 7, "head111"), t0.Add(time.Hour)),
	} {
		if err := evidence.Sign(b, priv); err != nil {
			t.Fatal(err)
		}
		if err := writeReviewEvidence(ReviewEvidencePath(root, "o/r", 7, b.HeadSHA), b); err != nil {
			t.Fatal(err)
		}
	}

	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte(strings.Repeat("00", ed25519.SeedSize)), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &evidenceMergeFixture{head: "head111", mergedBy: "hive-app[bot]"}
	c := evidenceMergeClient(t, f, &ReviewEvidenceSettings{Enabled: true, SigningKeyFile: keyFile})

	// Two merge paths (or a sweep re-run) report the same merge.
	c.RecordPRMergedAudit("o/r", 7, "squash", "merge999", PRAuditPathSweep)
	c.RecordPRMergedAudit("o/r", 7, "squash", "merge999", PRAuditPathRelay)

	b := mustLoadBundle(t, root, "head111")
	if b.Merge == nil || b.Merge.Actor != "hive-app[bot]" || b.Merge.Method != "squash" || b.Merge.SHA != "merge999" {
		t.Fatalf("merge = %+v", b.Merge)
	}
	if err := evidence.Verify(b, ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)); err != nil {
		t.Fatalf("merged bundle does not verify: %v", err)
	}
	if other := mustLoadBundle(t, root, "head222"); other.Merge != nil {
		t.Fatalf("merge recorded on the wrong head: %+v", other.Merge)
	}
	if f.posts != 1 || len(f.comments) != 1 {
		t.Fatalf("posts = %d comments = %d, want exactly one", f.posts, len(f.comments))
	}
	if want := ReviewEvidenceComment("o/r", 7, b.ID, b.Hash); f.comments[0] != want {
		t.Fatalf("comment = %q, want %q", f.comments[0], want)
	}
}

func TestRecordReviewEvidenceMergeBranches(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		settings     *ReviewEvidenceSettings
		fixture      evidenceMergeFixture
		seed         []string
		corruptHead  bool
		sha          string
		wantPosts    int
		wantMergeOn  string
		wantActor    string
		wantMergeSHA string
		wantPRFetch  bool
	}{
		{name: "not wired", fixture: evidenceMergeFixture{head: "h1"}, seed: []string{"h1"}},
		{name: "disabled", settings: &ReviewEvidenceSettings{}, fixture: evidenceMergeFixture{head: "h1"}, seed: []string{"h1"}},
		{name: "no bundle, no comment", settings: &ReviewEvidenceSettings{Enabled: true}, fixture: evidenceMergeFixture{head: "h1"}, wantPRFetch: true},
		{
			// hivecommons/hive#12592: the merge outran the swarm; the bundle
			// is started at merge time so the merge is never lost.
			name: "no bundle starts one for the merged head", settings: &ReviewEvidenceSettings{Enabled: true},
			fixture: evidenceMergeFixture{head: "h1", base: "base1", author: "hive-app[bot]"}, sha: "m1",
			wantPosts: 1, wantMergeOn: "h1", wantActor: "hive:sweep", wantMergeSHA: "m1", wantPRFetch: true,
		},
		{
			name: "head without bundle falls back to newest", settings: &ReviewEvidenceSettings{Enabled: true},
			fixture: evidenceMergeFixture{head: "h9"}, seed: []string{"h1", "h2"}, sha: "m1",
			wantPosts: 1, wantMergeOn: "h2", wantActor: "hive:sweep", wantMergeSHA: "m1", wantPRFetch: true,
		},
		{
			name: "PR fetch fails falls back to newest and head sha", settings: &ReviewEvidenceSettings{Enabled: true},
			fixture: evidenceMergeFixture{prStatus: http.StatusInternalServerError}, seed: []string{"h1"},
			wantPosts: 1, wantMergeOn: "h1", wantActor: "hive:sweep", wantMergeSHA: "h1", wantPRFetch: true,
		},
		{
			name: "marker already present", settings: &ReviewEvidenceSettings{Enabled: true},
			fixture: evidenceMergeFixture{head: "h1", comments: []string{"earlier\n" + ReviewEvidenceMarker + " Evidence bundle"}}, seed: []string{"h1"}, sha: "m1",
			wantMergeOn: "h1", wantActor: "hive:sweep", wantMergeSHA: "m1", wantPRFetch: true,
		},
		{
			name: "comment list fails", settings: &ReviewEvidenceSettings{Enabled: true},
			fixture: evidenceMergeFixture{head: "h1", listFail: true}, seed: []string{"h1"}, sha: "m1",
			wantMergeOn: "h1", wantActor: "hive:sweep", wantMergeSHA: "m1", wantPRFetch: true,
		},
		{
			name: "comment post fails", settings: &ReviewEvidenceSettings{Enabled: true},
			fixture: evidenceMergeFixture{head: "h1", postFail: true}, seed: []string{"h1"}, sha: "m1",
			wantPosts: 1, wantMergeOn: "h1", wantActor: "hive:sweep", wantMergeSHA: "m1", wantPRFetch: true,
		},
		{
			name: "unreadable bundle left alone", settings: &ReviewEvidenceSettings{Enabled: true},
			fixture: evidenceMergeFixture{head: "h1"}, corruptHead: true, sha: "m1", wantPRFetch: true,
		},
		{
			name: "bad signing key writes unsigned", settings: &ReviewEvidenceSettings{Enabled: true, SigningKeyFile: "BADKEY"},
			fixture: evidenceMergeFixture{head: "h1"}, seed: []string{"h1"}, sha: "m1",
			wantPosts: 1, wantMergeOn: "h1", wantActor: "hive:sweep", wantMergeSHA: "m1", wantPRFetch: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := withEvidenceRoot(t)
			for i, h := range tt.seed {
				writeTestEvidence(t, root, h, "", t0.Add(time.Duration(i)*time.Hour))
			}
			if tt.corruptHead {
				p := ReviewEvidencePath(root, "o/r", 7, "h1")
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("{broken"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			settings := tt.settings
			if settings != nil && settings.SigningKeyFile == "BADKEY" {
				bad := filepath.Join(t.TempDir(), "bad.key")
				if err := os.WriteFile(bad, []byte("not a key"), 0o600); err != nil {
					t.Fatal(err)
				}
				s := *settings
				s.SigningKeyFile = bad
				settings = &s
			}
			f := tt.fixture
			c := evidenceMergeClient(t, &f, settings)
			c.RecordPRMergedAudit("o/r", 7, "", tt.sha, PRAuditPathSweep)

			if f.posts != tt.wantPosts {
				t.Fatalf("posts = %d, want %d", f.posts, tt.wantPosts)
			}
			if (f.prFetches > 0) != tt.wantPRFetch {
				t.Fatalf("PR fetches = %d, want fetch %v", f.prFetches, tt.wantPRFetch)
			}
			if tt.wantMergeOn == "" {
				for _, h := range tt.seed {
					if b := mustLoadBundle(t, root, h); b.Merge != nil {
						t.Fatalf("bundle %s gained a merge: %+v", h, b.Merge)
					}
				}
				return
			}
			b := mustLoadBundle(t, root, tt.wantMergeOn)
			if b.Merge == nil || b.Merge.Actor != tt.wantActor || b.Merge.SHA != tt.wantMergeSHA || b.Merge.Method != "squash" {
				t.Fatalf("merge on %s = %+v", tt.wantMergeOn, b.Merge)
			}
			if b.Signed {
				t.Fatal("bundle signed without a usable key")
			}
			if h, _ := evidence.Hash(b); h != b.Hash {
				t.Fatalf("hash not resealed: %s vs %s", h, b.Hash)
			}
		})
	}
}

func TestCompleteReviewEvidenceMergeKeepsRecordedMerge(t *testing.T) {
	root := withEvidenceRoot(t)
	b := writeTestEvidence(t, root, "h1", "", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	c := evidenceMergeClient(t, &evidenceMergeFixture{head: "h1"}, &ReviewEvidenceSettings{Enabled: true})
	first := c.completeReviewEvidenceMerge("o/r", 7, "h1", evidence.MergeEvent{Actor: "a", Method: "merge", SHA: "m1", At: time.Now().UTC()}, ReviewEvidenceSettings{}, nil, nil)
	if first == nil || first.Hash == b.Hash {
		t.Fatalf("first merge not recorded: %+v", first)
	}
	second := c.completeReviewEvidenceMerge("o/r", 7, "h1", evidence.MergeEvent{Actor: "b", Method: "squash", SHA: "m2", At: time.Now().UTC()}, ReviewEvidenceSettings{}, nil, nil)
	if second == nil || second.Hash != first.Hash || second.Merge.Actor != "a" {
		t.Fatalf("replayed merge changed the bundle: %+v", second)
	}
	// An invalid merge event (no actor) is refused rather than written.
	writeTestEvidence(t, root, "h2", "", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	if got := c.completeReviewEvidenceMerge("o/r", 7, "h2", evidence.MergeEvent{Method: "merge", SHA: "m"}, ReviewEvidenceSettings{}, nil, nil); got != nil {
		t.Fatalf("invalid merge written: %+v", got.Merge)
	}
	if mustLoadBundle(t, root, "h2").Merge != nil {
		t.Fatal("invalid merge persisted")
	}
}

func TestRecordReviewEvidenceMergeGuards(t *testing.T) {
	withEvidenceRoot(t)
	var nilClient *Client
	nilClient.RecordPRMergedAudit("o/r", 7, "squash", "m", PRAuditPathSweep)
	c := evidenceMergeClient(t, &evidenceMergeFixture{head: "h1"}, &ReviewEvidenceSettings{Enabled: true})
	c.recordReviewEvidenceMerge("o/r", 0, "squash", "m", PRAuditPathSweep, nil)
	// A client with no REST transport still records nothing and posts nothing.
	bare := &Client{logger: c.logger}
	bare.SetReviewEvidence(func(string) ReviewEvidenceSettings { return ReviewEvidenceSettings{Enabled: true} })
	bare.recordReviewEvidenceMerge("o/r", 7, "squash", "m", PRAuditPathSweep, nil)
}

func TestReviewEvidencePublicKey(t *testing.T) {
	if got, err := ReviewEvidencePublicKey(""); got != "" || err != nil {
		t.Fatalf("no key: %q %v", got, err)
	}
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte(strings.Repeat("00", ed25519.SeedSize)), 0o600); err != nil {
		t.Fatal(err)
	}
	want := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	got, err := ReviewEvidencePublicKey(keyFile)
	if err != nil || got != hex.EncodeToString(want) {
		t.Fatalf("public key = %q, %v", got, err)
	}
	bad := filepath.Join(t.TempDir(), "bad")
	if err := os.WriteFile(bad, []byte("nope nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReviewEvidencePublicKey(bad); err == nil {
		t.Fatal("want error for a malformed key")
	}
}

// StartReviewEvidence (hivecommons/hive#12592) starts a missing bundle from
// the PR snapshot so the self-merge lane's shadow observation, CI and merge
// have somewhere to land, and never rewrites an existing one.
func TestStartReviewEvidence(t *testing.T) {
	pr := &gh.PullRequest{
		Number: gh.Ptr(7),
		User:   &gh.User{Login: gh.Ptr("hive-app[bot]"), Type: gh.Ptr("Bot")},
		Base:   &gh.PullRequestBranch{SHA: gh.Ptr("base1")},
		Head:   &gh.PullRequestBranch{SHA: gh.Ptr("h1")},
	}
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		settings  *ReviewEvidenceSettings
		seed      bool
		pr        *gh.PullRequest
		wantNew   bool
		wantExist bool
	}{
		{name: "not wired", pr: pr},
		{name: "disabled", settings: &ReviewEvidenceSettings{}, pr: pr},
		{name: "no PR snapshot", settings: &ReviewEvidenceSettings{Enabled: true}},
		{name: "starts a missing bundle", settings: &ReviewEvidenceSettings{Enabled: true, Policy: evidence.Policy{RequireApproval: true}}, pr: pr, wantNew: true, wantExist: true},
		{name: "existing bundle untouched", settings: &ReviewEvidenceSettings{Enabled: true}, seed: true, pr: pr, wantExist: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := withEvidenceRoot(t)
			var seeded *evidence.Bundle
			if tt.seed {
				seeded = writeTestEvidence(t, root, "h1", "", t0)
			}
			c := evidenceMergeClient(t, &evidenceMergeFixture{head: "h1"}, tt.settings)
			c.StartReviewEvidence("o/r", 7, "h1", tt.pr)
			_, err := os.Stat(ReviewEvidencePath(root, "o/r", 7, "h1"))
			if exists := err == nil; exists != tt.wantExist {
				t.Fatalf("bundle exists = %v, want %v", exists, tt.wantExist)
			}
			if !tt.wantExist {
				return
			}
			b := mustLoadBundle(t, root, "h1")
			if seeded != nil && b.Hash != seeded.Hash {
				t.Fatalf("existing bundle rewritten: %s vs %s", b.Hash, seeded.Hash)
			}
			if tt.wantNew {
				if b.Author.Login != "hive-app[bot]" || b.BaseSHA != "base1" || !b.Policy.RequireApproval || b.Merge != nil {
					t.Fatalf("started bundle = %+v", b)
				}
			}
		})
	}
}
