package ioscan

import (
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
)

func TestGenerateCanaryUniqueHighEntropyPrefix(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := GenerateCanary()
		if err != nil {
			t.Fatalf("GenerateCanary: %v", err)
		}
		if !strings.HasPrefix(tok, CanaryPrefix) {
			t.Fatalf("token %q missing prefix %q", tok, CanaryPrefix)
		}
		if len(tok) != len(CanaryPrefix)+canaryRandomBytes*2 {
			t.Fatalf("token length = %d, want %d", len(tok), len(CanaryPrefix)+canaryRandomBytes*2)
		}
		if seen[tok] {
			t.Fatalf("duplicate canary generated: %s", tok)
		}
		seen[tok] = true
	}
}

func TestCanaryRegistryScanByAgent(t *testing.T) {
	r := NewCanaryRegistry("")
	c, err := r.Add("scanner")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, ok := r.Scan("quality", "ordinary report", "artifact"); ok {
		t.Fatal("unexpected leak in benign text")
	}
	leak, ok := r.Scan("scanner", "oops "+c.Token, "artifact")
	if !ok {
		t.Fatal("expected canary leak")
	}
	if leak.Agent != "scanner" || leak.Source != "artifact" || leak.Token != c.Token {
		t.Fatalf("leak = %+v, want scanner/artifact/%s", leak, c.Token)
	}
}

func TestCanaryRegistryScanDetectsEncodedCanaries(t *testing.T) {
	r := NewCanaryRegistry("")
	c, err := r.Add("scanner")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	spaced := strings.Join(strings.Split(c.Token, ""), " \n")
	reversed := reverseCanaryForTest(c.Token)
	b64 := base64.StdEncoding.EncodeToString([]byte(c.Token))
	hexed := hex.EncodeToString([]byte(c.Token))
	hexEscaped := `\x` + sepEveryForTest(hexed, 2, `\x`)
	cases := map[string]string{
		"base64":           b64,
		"hex":              hexed,
		"url":              url.QueryEscape(c.Token),
		"split":            spaced,
		"reversed":         reversed,
		"case-insensitive": strings.ToLower(c.Token),

		// An exfiltrating agent picks whichever spelling of an encoding is at
		// hand, and every one of these reaches a GitHub write body intact.
		"base64-url-safe":    base64.URLEncoding.EncodeToString([]byte(c.Token)),
		"base64-unpadded":    base64.RawStdEncoding.EncodeToString([]byte(c.Token)),
		"base64-wrapped":     wrapForTest(b64, 24),
		"base64-nested":      base64.StdEncoding.EncodeToString([]byte(b64)),
		"base64-reversed":    base64.StdEncoding.EncodeToString([]byte(reversed)),
		"hex-upper":          strings.ToUpper(hexed),
		"hex-separated":      sepEveryForTest(hexed, 2, ":"),
		"hex-escaped":        hexEscaped,
		"url-double-encoded": url.QueryEscape(url.QueryEscape(c.Token)),
		"comma-split":        strings.Join(strings.Split(c.Token, ""), ","),
		"markdown-split":     c.Token[:20] + "`` ``" + c.Token[20:],
		// Homoglyphs and invisible characters are what the input path
		// normalizes away; egress gets the same treatment.
		"confusables": strings.NewReplacer("a", "\u0430", "c", "\u0441", "e", "\u0435").Replace(c.Token),
		"zero-width":  strings.Join(strings.Split(c.Token, ""), "\u200b"),
		"soft-hyphen": strings.Join(strings.Split(c.Token, ""), "\u00ad"),
		"fullwidth":   fullwidthForTest(c.Token),
		// Only a piece of the token reaches this body; an intact window of
		// the random half is already past coincidence.
		"fragment-only": c.Token[12:44],
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			leak, ok := r.Scan("scanner", "egress body: "+body, "proxy")
			if !ok {
				t.Fatalf("encoded canary was not detected in %q", body)
			}
			if leak.Agent != "scanner" || leak.Source != "proxy" || leak.Token != c.Token {
				t.Fatalf("leak = %+v, want scanner/proxy/%s", leak, c.Token)
			}
		})
	}
}

// Decoding-and-rescanning widens what counts as a match, so the bodies an
// agent legitimately writes have to stay clean: a scanner that trips on
// ordinary review comments, diffs and attachments would be turned off, and a
// canary nobody leaves on detects nothing.
func TestCanaryRegistryScanIgnoresBenignBodies(t *testing.T) {
	r := NewCanaryRegistry("")
	if _, err := r.Add("scanner"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	other, err := GenerateCanary()
	if err != nil {
		t.Fatalf("GenerateCanary: %v", err)
	}
	bodies := map[string]string{
		"review comment":  `{"body":"LGTM — the deadbeefcafe fixture in pkg/proxy still covers this."}`,
		"commit sha":      `{"sha":"9f8c1b2d3e4a5f60718293a4b5c6d7e8f9012345","message":"fix: bound the scan"}`,
		"base64 blob":     `{"content":"` + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("ordinary attachment bytes ", 40))) + `"}`,
		"hex blob":        `{"content":"` + hex.EncodeToString([]byte(strings.Repeat("ordinary attachment bytes ", 40))) + `"}`,
		"prefix only":     `{"body":"HIVE-CANARY-<token> is the shape ioscan plants; see docs/ioscan.md"}`,
		"another canary":  `{"body":"` + other + `"}`,
		"url-encoded doc": `{"body":"` + url.QueryEscape("see https://example.test/a?b=c&d=e for the decode-and-rescan write-up") + `"}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			if leak, ok := r.Scan("scanner", body, "proxy"); ok {
				t.Fatalf("benign body reported leak %+v", leak)
			}
		})
	}
}

// The scan runs synchronously on the proxy's write path against bodies up to
// maxGitHubWriteBodyScan, and the body is attacker-influenced, so an
// adversarial shape must not turn the sweep into a stall.
func TestCanaryRegistryScanStaysBoundedOnLargeBodies(t *testing.T) {
	r := NewCanaryRegistry("")
	var last Canary
	for i := 0; i < 32; i++ {
		c, err := r.Add("scanner")
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		last = c
	}
	const twoMiB = 2 << 20
	line := `{"path":"pkg/proxy/github_proxy.go","sha":"` + hex.EncodeToString([]byte("noise bytes here")) + `","content":"` +
		base64.StdEncoding.EncodeToString([]byte("ordinary patch hunk ")) + `"}` + "\n"
	filler := strings.Repeat(line, twoMiB/len(line))
	if leak, ok := r.Scan("scanner", filler, "proxy"); ok {
		t.Fatalf("filler reported leak %+v", leak)
	}
	leaky := filler + `{"content":"` + base64.StdEncoding.EncodeToString([]byte(last.Token)) + `"}`
	if _, ok := r.Scan("scanner", leaky, "proxy"); !ok {
		t.Fatal("canary encoded at the end of a large body was not detected")
	}
}

func reverseCanaryForTest(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

// wrapForTest breaks s into fixed-width lines, the way a MIME body or a YAML
// block scalar carries base64.
func wrapForTest(s string, width int) string {
	var b strings.Builder
	for i := 0; i < len(s); i += width {
		end := i + width
		if end > len(s) {
			end = len(s)
		}
		b.WriteString(s[i:end])
		b.WriteString("\n")
	}
	return b.String()
}

// fullwidthForTest rewrites the ASCII alphanumerics of s into their
// Halfwidth and Fullwidth Forms twins, the shape #6708 found on the input path.
func fullwidthForTest(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r + 0xfee0)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sepEveryForTest inserts sep after every n characters of s.
func sepEveryForTest(s string, n int, sep string) string {
	var parts []string
	for i := 0; i < len(s); i += n {
		end := i + n
		if end > len(s) {
			end = len(s)
		}
		parts = append(parts, s[i:end])
	}
	return strings.Join(parts, sep)
}

// The scan is synchronous on the proxy's write path, so its cost against a
// full-size body with a canary per live agent is worth keeping visible.
func BenchmarkCanaryRegistryScanLargeBody(b *testing.B) {
	r := NewCanaryRegistry("")
	for i := 0; i < 32; i++ {
		if _, err := r.Add("scanner"); err != nil {
			b.Fatal(err)
		}
	}
	const twoMiB = 2 << 20
	line := `{"body":"a perfectly ordinary review comment about pkg/proxy"}` + "\n"
	body := strings.Repeat(line, twoMiB/len(line))
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := r.Scan("scanner", body, "proxy"); ok {
			b.Fatal("unexpected leak")
		}
	}
}
