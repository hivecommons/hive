package ioscan_test

// Drift guard for the vendored third-party injection corpus (#19460).
// Files under testdata/classifier-bench/third-party/garak come from a pinned
// upstream commit; any change to them must update MANIFEST.json deliberately.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
)

const garakDir = "testdata/classifier-bench/third-party/garak"

type pinnedFile struct {
	SHA256 string `json:"sha256"`
}

type pinManifest struct {
	Commit        string                `json:"commit"`
	License       string                `json:"license"`
	UpstreamFiles map[string]pinnedFile `json:"upstream_files"`
	DerivedFiles  map[string]pinnedFile `json:"derived_files"`
}

func TestVendoredGarakCorpusMatchesPinnedHashes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(garakDir, "MANIFEST.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m pinManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Commit) != 40 || m.License != "Apache-2.0" {
		t.Fatalf("manifest must pin a full commit and Apache-2.0, got %q %q", m.Commit, m.License)
	}
	if _, ok := m.UpstreamFiles["LICENSE"]; !ok {
		t.Fatal("upstream LICENSE must be vendored and pinned")
	}
	for _, group := range []map[string]pinnedFile{m.UpstreamFiles, m.DerivedFiles} {
		for name, pin := range group {
			b, err := os.ReadFile(filepath.Join(garakDir, name))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(b)
			if got := hex.EncodeToString(sum[:]); got != pin.SHA256 {
				t.Errorf("%s drifted from pinned hash: got %s want %s", name, got, pin.SHA256)
			}
		}
	}
}

func TestVendoredGarakCorpusRowsAreWellFormed(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(garakDir, "garak-latent.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20)
	n := 0
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
		if r.Split != want || r.Label != "attack" || r.Text == "" || r.Source == "" {
			t.Errorf("bad row %q: split=%q want %q label=%q", r.ID, r.Split, want, r.Label)
		}
	}
	if n == 0 {
		t.Fatal("corpus is empty")
	}
}
