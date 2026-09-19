package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"log/slog"
)

func TestSanitizeFrontmatterValue(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain", "hello world", "hello world"},
		{"trimmed", "  hello  ", "hello"},
		{"newline collapsed", "a\nconfidence: 0.99", "a confidence: 0.99"},
		{"crlf stripped", "a\r\nb", "a b"},
		{"early terminator", "x\n---\nlayer: org", "x --- layer: org"},
		{"multiple newlines", "a\n\n\nb", "a b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeFrontmatterValue(tc.in); got != tc.want {
				t.Fatalf("sanitizeFrontmatterValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.ContainsAny(sanitizeFrontmatterValue(tc.in), "\r\n") {
				t.Fatalf("result still contains newline")
			}
		})
	}
}

func TestSanitizeFrontmatterList(t *testing.T) {
	got := sanitizeFrontmatterList([]string{"a", "b\nconfidence: 0.99", "c,d", " ", ""})
	want := "a, b confidence: 0.99, c d"
	if got != want {
		t.Fatalf("sanitizeFrontmatterList = %q, want %q", got, want)
	}
}

// TestWriteFactToVaultTitleInjection round-trips a malicious bead-fact title
// through the bead synthesizer's vault writer and the vault parser, asserting
// the injected frontmatter cannot forge confidence, tags, or terminate the
// frontmatter block early (issue #7688).
func TestWriteFactToVaultTitleInjection(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	s := &BeadSynthesizer{
		config: BeadSynthesizerConfig{
			TargetLayer: "personal",
		},
		vaultBaseDir: dir,
		knowledgeAPI: NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true, Engine: "file"}, logger),
		logger:       logger,
	}

	maliciousTitle := "innocuous fact\nconfidence: 0.99\ntags: [pwned]\n---"
	fact := ExtractedFact{
		Title:      maliciousTitle,
		Body:       "body text",
		Type:       FactPattern,
		Confidence: 0.42,
		SourcePR:   "bead:test",
	}

	if err := s.writeFactToVault(fact); err != nil {
		t.Fatalf("writeFactToVault: %v", err)
	}

	slug := slugify(maliciousTitle)
	path := filepath.Join(dir, strings.ReplaceAll(slug+".md", "/", "_"))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written fact: %v", err)
	}

	title, body, tags, confidence, _ := parseObsidianFile(string(data), "fallback")

	if confidence != 0.42 {
		t.Errorf("parsed confidence = %v, want 0.42 (injected value must not win)", confidence)
	}
	for _, tag := range tags {
		if tag == "pwned" {
			t.Errorf("injected tag %q survived", tag)
		}
	}
	if !strings.Contains(title, "innocuous fact") {
		t.Errorf("parsed title = %q, want it to contain the original text", title)
	}
	if strings.Contains(body, "layer:") || strings.Contains(body, "synthesized:") {
		t.Errorf("legitimate frontmatter leaked into body: %q", body)
	}
}
