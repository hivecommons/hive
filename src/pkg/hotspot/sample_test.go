package hotspot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSampleDryRun runs Analyze over a saved GraphQL sample of merged+open
// PRs with their files (HOTSPOT_SAMPLE=<json>, HOTSPOT_CHECKOUT=<repo root>
// for sizes/symbols) and prints the top-10 table used as PR evidence. It
// skips without the sample so CI never needs GitHub.
func TestSampleDryRun(t *testing.T) {
	sample := os.Getenv("HOTSPOT_SAMPLE")
	if sample == "" {
		t.Skip("HOTSPOT_SAMPLE not set")
	}
	raw, err := os.ReadFile(sample)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Data map[string]struct {
			Nodes []struct {
				Number    int       `json:"number"`
				CreatedAt time.Time `json:"createdAt"`
				MergedAt  time.Time `json:"mergedAt"`
				ClosedAt  time.Time `json:"closedAt"`
				State     string    `json:"state"`
				Files     struct {
					Nodes []struct {
						Path string `json:"path"`
					} `json:"nodes"`
				} `json:"files"`
			} `json:"nodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	const repo = "hivecommons/hive"
	var prs []PRFiles
	for _, bucket := range doc.Data {
		for _, n := range bucket.Nodes {
			pr := PRFiles{Repo: repo, Number: n.Number, OpenedAt: n.CreatedAt, SeenAt: time.Now()}
			if !n.MergedAt.IsZero() {
				pr.ClosedAt, pr.Merged = n.MergedAt, true
			} else if !n.ClosedAt.IsZero() {
				pr.ClosedAt = n.ClosedAt
			}
			for _, f := range n.Files.Nodes {
				pr.Files = append(pr.Files, f.Path)
			}
			prs = append(prs, pr)
		}
	}
	root := os.Getenv("HOTSPOT_CHECKOUT")
	read := func(_, p string) (FileInfo, bool) {
		if root == "" {
			return FileInfo{}, false
		}
		src, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			return FileInfo{}, false
		}
		return FileInfo{Lines: CountLines(src), Source: src}, true
	}
	hs := Analyze(repo, prs, nil, read, Options{}, time.Now())
	var b strings.Builder
	fmt.Fprintf(&b, "%d PRs in sample, %d hotspots\n\n", len(prs), len(hs))
	b.WriteString("| file | PRs | overlaps | lines | groups | kind | recommendation |\n|---|---|---|---|---|---|---|\n")
	for i, h := range hs {
		if i >= 10 {
			break
		}
		fmt.Fprintf(&b, "| `%s` | %d | %d | %d | %d | %s | %s |\n", h.File, h.PRs, h.Overlaps, h.Lines, len(h.SymbolGroups), h.Kind, h.Recommendation)
	}
	t.Log("\n" + b.String())
	if out := os.Getenv("HOTSPOT_TABLE_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
