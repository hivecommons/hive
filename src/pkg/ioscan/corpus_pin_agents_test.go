package ioscan_test

// Drift guard for the pinned coding-agent injection corpus (RepoGuardBench) and
// the benign corpus of real GitHub issue text (#19515, grown in #19461). Hashes live in
// each directory's MANIFEST.json; changing a file means updating its pin on purpose.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	repoGuardDir  = "testdata/classifier-bench/third-party/repoguardbench"
	realIssuesDir = "testdata/classifier-bench/real-issues"
)

func checkPins(t *testing.T, dir string, groups ...map[string]pinnedFile) {
	t.Helper()
	for _, group := range groups {
		if len(group) == 0 {
			t.Fatalf("%s: manifest group is empty", dir)
		}
		for name, pin := range group {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(b)
			if got := hex.EncodeToString(sum[:]); got != pin.SHA256 {
				t.Errorf("%s/%s drifted from pinned hash: got %s want %s", dir, name, got, pin.SHA256)
			}
		}
	}
}

func checkCorpusRows(t *testing.T, path, label string, minRows int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20)
	n, dev, held := 0, 0, 0
	for sc.Scan() {
		var r struct{ ID, Label, Split, Source, Text string }
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		n++
		sum := sha256.Sum256([]byte(r.ID))
		want := "dev"
		if new(big.Int).SetBytes(sum[:]).Bit(0) == 1 {
			want = "heldout"
		}
		if r.Split != want || r.Label != label || r.Text == "" || r.Source == "" {
			t.Errorf("bad row %q: split=%q want %q label=%q", r.ID, r.Split, want, r.Label)
		}
		if r.Split == "heldout" {
			held++
		} else {
			dev++
		}
	}
	if n < minRows || dev == 0 || held == 0 {
		t.Fatalf("%s: want >= %d rows with both splits, got %d (dev %d, heldout %d)", path, minRows, n, dev, held)
	}
}

func TestVendoredRepoGuardBenchCorpusMatchesPinnedHashes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoGuardDir, "MANIFEST.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m pinManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Commit) != 40 || !strings.HasPrefix(m.License, "MIT") {
		t.Fatalf("manifest must pin a full commit and an MIT license, got %q %q", m.Commit, m.License)
	}
	for _, n := range []string{"LICENSE", "LICENSE-DATA"} {
		if _, ok := m.UpstreamFiles[n]; !ok {
			t.Fatalf("upstream %s must be vendored and pinned", n)
		}
	}
	checkPins(t, repoGuardDir, m.UpstreamFiles, m.DerivedFiles)
}

func TestVendoredRepoGuardBenchRowsAreWellFormed(t *testing.T) {
	checkCorpusRows(t, filepath.Join(repoGuardDir, "repoguardbench-attacks.jsonl"), "attack", 50)
}

func TestRealIssueBenignCorpusMatchesPinnedHashes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(realIssuesDir, "MANIFEST.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m pinManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	checkPins(t, realIssuesDir, m.DerivedFiles)
}

func TestRealIssueBenignCorpusRowsAreWellFormed(t *testing.T) {
	checkCorpusRows(t, filepath.Join(realIssuesDir, "github-issues.jsonl"), "benign", 300)
}
