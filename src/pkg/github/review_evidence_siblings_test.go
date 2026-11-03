package github

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestReviewEvidenceDirsMemoisesTheRepoListing pins the #14681 fix: one
// repo-directory read serves every PR lookup for the TTL, a sibling this
// process writes is visible at once, and one another writer creates shows
// up once the TTL lapses.
func TestReviewEvidenceDirsMemoisesTheRepoListing(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "o", "r")
	for _, d := range []string{"7", "7.uid1001", "8", "8.uidx"} {
		if err := os.MkdirAll(filepath.Join(repo, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	clock := time.Date(2026, 10, 27, 20, 0, 0, 0, time.UTC)
	evidenceSiblingNow = func() time.Time { return clock }
	t.Cleanup(func() {
		evidenceSiblingNow = time.Now
		forgetEvidenceSiblings(filepath.Join(repo, "7"))
	})

	dir7, dir8 := filepath.Join(repo, "7"), filepath.Join(repo, "8")
	if got := reviewEvidenceDirs(dir7); !reflect.DeepEqual(got, []string{dir7, filepath.Join(repo, "7.uid1001")}) {
		t.Fatalf("dirs(7) = %v", got)
	}
	// A non-numeric suffix is not a sibling.
	if got := reviewEvidenceDirs(dir8); !reflect.DeepEqual(got, []string{dir8}) {
		t.Fatalf("dirs(8) = %v", got)
	}

	// Another writer adds a sibling: invisible until the TTL lapses.
	if err := os.MkdirAll(filepath.Join(repo, "8.uid2002"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := reviewEvidenceDirs(dir8); len(got) != 1 {
		t.Fatalf("dirs(8) within TTL = %v, want the memoised listing", got)
	}
	clock = clock.Add(evidenceSiblingListTTL)
	if got := reviewEvidenceDirs(dir8); !reflect.DeepEqual(got, []string{dir8, filepath.Join(repo, "8.uid2002")}) {
		t.Fatalf("dirs(8) after TTL = %v", got)
	}

	// This process creating one drops the memo for its parent.
	if err := os.MkdirAll(filepath.Join(repo, "7.uid3003"), 0o755); err != nil {
		t.Fatal(err)
	}
	forgetEvidenceSiblings(dir7)
	if got := reviewEvidenceDirs(dir7); len(got) != 3 {
		t.Fatalf("dirs(7) after forget = %v, want both siblings", got)
	}

	// The memo is bounded: past the cap it is dropped wholesale, not grown.
	for i := 0; i < evidenceSiblingListMaxParents+1; i++ {
		evidenceSiblingNames(filepath.Join(root, "missing", string(rune('a'+i%26)), time.Duration(i).String()))
	}
	evidenceSiblingMu.Lock()
	n := len(evidenceSiblingLists)
	evidenceSiblingMu.Unlock()
	if n > evidenceSiblingListMaxParents {
		t.Fatalf("memo holds %d parents, cap %d", n, evidenceSiblingListMaxParents)
	}
}
