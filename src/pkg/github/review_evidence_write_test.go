package github

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

func TestWriteReviewEvidenceCreatesGroupWritableDirs(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "owner", "repo", "42", "abc.json")
	if err := writeReviewEvidence(path, &evidence.Bundle{ID: "x"}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"owner", "owner/repo", "owner/repo/42"} {
		info, err := os.Stat(filepath.Join(root, d))
		if err != nil {
			t.Fatal(err)
		}
		mode := info.Mode() & (os.ModePerm | os.ModeSetgid)
		if mode&0o020 == 0 {
			t.Errorf("%s: mode %v is not group-writable", d, mode)
		}
		if mode&os.ModeSetgid == 0 {
			t.Logf("%s: setgid not preserved on this filesystem", d)
		}
	}
}

// An existing PR directory this uid owns but created 0o755 (before #13836)
// is widened so writers sharing its group can add bundles.
func TestWriteReviewEvidenceWidensOwnedExistingDir(t *testing.T) {
	dir := ReviewEvidenceDir(t.TempDir(), "o/r", 7)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeReviewEvidence(filepath.Join(dir, "abc.json"), &evidence.Bundle{ID: "x"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if uid := os.Getuid(); uid < 0 || fileOwnerUID(info) != uid {
		t.Skip("file ownership is not observable on this platform")
	}
	if info.Mode().Perm()&0o020 == 0 {
		t.Fatalf("owned PR dir mode %v was not widened to group-writable", info.Mode())
	}
}

// The #13836 condition: the PR directory exists but this uid cannot write
// into it (foreign-owned in production; simulated here with a read-only dir,
// which widenOwnedEvidenceDir deliberately leaves alone). The bundle lands in the writer-owned <pr>.uid<N> sibling and the
// readers still find it.
func TestWriteReviewEvidenceUnwritableDirFallsBackToUIDSibling(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	root := t.TempDir()
	dir := ReviewEvidenceDir(root, "o/r", 7)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, nil, 0o644); err == nil {
		_ = os.Remove(probe)
		t.Skip("chmod 0o555 does not block writes here")
	}

	created := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	b := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion,
		ID:            evidence.BundleID("o/r", 7, "abc1234"),
		Repo:          "o/r",
		Number:        7,
		HeadSHA:       "abc1234",
		CreatedAt:     created,
		UpdatedAt:     created,
	}
	path := ReviewEvidencePath(root, "o/r", 7, "abc1234")
	if err := writeReviewEvidence(path, b); err != nil {
		t.Fatalf("write with unwritable PR dir: %v", err)
	}

	want := filepath.Join(dir+".uid"+strconv.Itoa(os.Getuid()), "abc1234.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("bundle not in fallback sibling %s: %v", want, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("bundle unexpectedly at %s: %v", path, err)
	}

	got, err := LoadReviewEvidence(path)
	if err != nil || got.ID != b.ID {
		t.Fatalf("LoadReviewEvidence(%s) = %+v, %v; want the fallback bundle", path, got, err)
	}
	entries, err := ListReviewEvidence(root, "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != want || entries[0].Bundle.ID != b.ID {
		t.Fatalf("ListReviewEvidence = %+v; want the one fallback bundle at %s", entries, want)
	}
	lookup, err := FindReviewEvidence(root, "o/r", 7, "abc1234")
	if err != nil || lookup.Entry == nil || lookup.Entry.Bundle.ID != b.ID {
		t.Fatalf("FindReviewEvidence = %+v, %v; want the fallback bundle", lookup, err)
	}

	// A rewrite of the same head (next poll) keeps landing in the sibling.
	b.UpdatedAt = created.Add(time.Hour)
	if err := writeReviewEvidence(path, b); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got, err := LoadReviewEvidence(path); err != nil || !got.UpdatedAt.Equal(b.UpdatedAt) {
		t.Fatalf("rewrite not visible to reader: %+v, %v", got, err)
	}
}

func TestReviewEvidenceCanonicalDir(t *testing.T) {
	root := filepath.Join("ev", "o", "r")
	for in, want := range map[string]string{
		filepath.Join(root, "7"):              filepath.Join(root, "7"),
		filepath.Join(root, "7.uid1001"):      filepath.Join(root, "7"),
		filepath.Join(root, "7.uid1001.uid2"): filepath.Join(root, "7.uid1001.uid2"),
		filepath.Join(root, "7.uidx"):         filepath.Join(root, "7.uidx"),
		filepath.Join(root, ".uid7"):          filepath.Join(root, ".uid7"),
	} {
		if got := reviewEvidenceCanonicalDir(in); got != want {
			t.Errorf("reviewEvidenceCanonicalDir(%q) = %q, want %q", in, got, want)
		}
	}
}

// A later writer (merge, outcome, change-failure) rewrites the bundle at the
// Entry.Path ListReviewEvidence handed it, which may be another writer's
// <pr>.uid<M>/ sibling. When that dir is unwritable too the bundle must land
// in this writer's <pr>.uid<N>/, not in <pr>.uid<M>.uid<N>/ where no reader
// looks.
func TestWriteReviewEvidenceFallbackFromForeignSiblingUsesOwnSibling(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	root := t.TempDir()
	dir := ReviewEvidenceDir(root, "o/r", 7)
	foreign := dir + ".uid" + strconv.Itoa(os.Getuid()+1)
	for _, d := range []string{dir, foreign} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(d, 0o755) })
	}
	probe := filepath.Join(foreign, "probe")
	if err := os.WriteFile(probe, nil, 0o644); err == nil {
		_ = os.Remove(probe)
		t.Skip("chmod 0o555 does not block writes here")
	}

	created := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	b := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion,
		ID:            evidence.BundleID("o/r", 7, "abc1234"),
		Repo:          "o/r",
		Number:        7,
		HeadSHA:       "abc1234",
		CreatedAt:     created,
		UpdatedAt:     created,
	}
	entryPath := filepath.Join(foreign, "abc1234.json")
	if err := writeReviewEvidence(entryPath, b); err != nil {
		t.Fatalf("write with unwritable foreign sibling: %v", err)
	}

	want := filepath.Join(dir+".uid"+strconv.Itoa(os.Getuid()), "abc1234.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("bundle not in own fallback sibling %s: %v", want, err)
	}
	if _, err := os.Stat(foreign + ".uid" + strconv.Itoa(os.Getuid())); !os.IsNotExist(err) {
		t.Fatalf("fallback-of-fallback dir was created: %v", err)
	}

	canonical := ReviewEvidencePath(root, "o/r", 7, "abc1234")
	if got, err := LoadReviewEvidence(canonical); err != nil || got.ID != b.ID {
		t.Fatalf("LoadReviewEvidence(%s) = %+v, %v; want the fallback bundle", canonical, got, err)
	}
	entries, err := ListReviewEvidence(root, "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != want || entries[0].Bundle.ID != b.ID {
		t.Fatalf("ListReviewEvidence = %+v; want the one fallback bundle at %s", entries, want)
	}
	lookup, err := FindReviewEvidence(root, "o/r", 7, "abc1234")
	if err != nil || lookup.Entry == nil || lookup.Entry.Bundle.ID != b.ID {
		t.Fatalf("FindReviewEvidence = %+v, %v; want the fallback bundle", lookup, err)
	}
}

func TestEvidenceWriteDeniedDedupesPerPRDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "o", "r", "88")
	key := reviewEvidenceCanonicalDir(filepath.Dir(filepath.Join(dir, "sha1.json")))
	for _, name := range []string{"sha1.json", "sha2.json"} {
		got := reviewEvidenceCanonicalDir(filepath.Dir(filepath.Join(dir+".uid5", name)))
		if got != key {
			t.Errorf("%s: key %q, want %q", name, got, key)
		}
	}
}
