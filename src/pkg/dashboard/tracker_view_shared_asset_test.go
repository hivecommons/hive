package dashboard

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/webassets/trackerview"
)

// TestTrackerViewSharedAssetCompat pins the hub's copy of the Activity
// Monitor URL-view and display-playlist helpers (hivecommons/hive#16064) to
// the spoke: past its header comment, pkg/webassets/trackerview/tracker-view.js
// must be exactly these spoke declarations, in this order, as the committed
// static/index.html serves them. The spoke side is only read, never changed:
// a drift fails here and is fixed by re-copying from the spoke.
func TestTrackerViewSharedAssetCompat(t *testing.T) {
	html := indexHTML(t)
	want := spokeFuncSource(t, html, "stageTrackerDefaultFilter") +
		spokeFuncSource(t, html, "stageTrackerNormalizeFilter") +
		spokeMarkedBlock(t, html, "tracker url view helpers begin", "tracker url view helpers end") +
		spokeMarkedBlock(t, html, "tracker display playlist helpers begin", "tracker display playlist helpers end")

	b, err := fs.ReadFile(trackerview.FS, "tracker-view.js")
	if err != nil {
		t.Fatal(err)
	}
	got := sharedAssetBody(string(b))
	if got != want {
		t.Errorf("pkg/webassets/trackerview/tracker-view.js differs from the spoke's declarations; re-copy them verbatim from static/index.html\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// sharedAssetBody drops the leading block of column-0 `//` header lines.
func sharedAssetBody(src string) string {
	lines := strings.SplitAfter(src, "\n")
	i := 0
	for i < len(lines) && strings.HasPrefix(lines[i], "//") {
		i++
	}
	return strings.Join(lines[i:], "")
}

// spokeFuncSource is one function declaration from the spoke page, from the
// start of its line (indentation included) to its closing brace and newline.
func spokeFuncSource(t *testing.T, html, name string) string {
	t.Helper()
	start := strings.Index(html, "function "+name+"(")
	if start < 0 {
		t.Fatalf("spoke page does not define %s", name)
	}
	lineStart := strings.LastIndex(html[:start], "\n") + 1
	depth := 0
	for i := start + strings.Index(html[start:], "{"); i < len(html); i++ {
		switch html[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return html[lineStart:i+1] + "\n"
			}
		}
	}
	t.Fatalf("unbalanced braces in %s", name)
	return ""
}

// spokeMarkedBlock is a `// begin` … `// end` block, both marker lines included.
func spokeMarkedBlock(t *testing.T, html, begin, end string) string {
	t.Helper()
	m := regexp.MustCompile(`( *// ` + regexp.QuoteMeta(begin) + `\n[\s\S]*?// ` + regexp.QuoteMeta(end) + `\n)`).FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("spoke page has no %q … %q block", begin, end)
	}
	return m[1]
}
