package webstatic

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// static/index.html is a GENERATED artifact. Its sources live in
// src/pkg/dashboard/static-src/ (one file per dashboard area, listed in
// manifest.txt) and are concatenated verbatim by gen/concat.go. The committed
// artifact stays in the tree so `//go:embed static`, the CSP style hashes, the
// node test harnesses and every test that reads static/index.html keep
// working unchanged — but edits belong in static-src/**, never in the
// artifact; TestIndexHTMLIsGenerated fails when the two drift.
//
//go:generate go run ./gen -src ../static-src -out ../static/index.html

// IndexManifestName is the ordered list of source files, one path per line
// relative to the source root; blank lines and lines starting with '#' are
// ignored.
const IndexManifestName = "manifest.txt"

// ParseIndexManifest returns the source paths in concatenation order.
func ParseIndexManifest(data []byte) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if path.IsAbs(line) || line != path.Clean(line) || strings.HasPrefix(line, "..") {
			return nil, fmt.Errorf("%s:%d: %q must be a clean relative path", IndexManifestName, n, line)
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s lists no files", IndexManifestName)
	}
	return out, nil
}

// ConcatIndex builds the index document from the source tree rooted at src:
// the bytes of every file named in manifest.txt, in order, with no separators
// or rewriting. Byte-exact concatenation is the whole contract — it is what
// lets a split source tree reproduce the previously monolithic file without
// touching the served bytes.
func ConcatIndex(src fs.FS) ([]byte, error) {
	manifest, err := fs.ReadFile(src, IndexManifestName)
	if err != nil {
		return nil, err
	}
	files, err := ParseIndexManifest(manifest)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for _, name := range files {
		b, err := fs.ReadFile(src, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", IndexManifestName, err)
		}
		buf.Write(b)
	}
	return buf.Bytes(), nil
}
