package logread

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The hive log is slog JSON. A secret value that holds a quote, a backslash
// or a control character is written there only in its JSON-escaped form, so
// the plain value never matched and the secret came back readable.
func TestOwnSecretIsRedactedInItsJSONEscapedForm(t *testing.T) {
	for name, secret := range map[string]string{
		"plain":             `plain-secret-0123456789`,
		"quote, backslash":  `pa"ss\word-0123456789`,
		"tab and newline":   "line1-secret\tcol\nline2-0123",
		"escape character":  "esc\x1bseq-secret-0123456789",
		"html characters":   `a<b>&c-secret-0123456789`,
		"non-ASCII letters": `pässwörd-geheim-0123456789`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var buf bytes.Buffer
			lg := slog.New(slog.NewJSONHandler(&buf, nil))
			lg.Info("connecting", "password", secret)
			lg.Info("tail")
			if err := os.WriteFile(filepath.Join(dir, HiveLogFilename), buf.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			res := Read(Request{}, Options{LogDir: dir, Secrets: NewSecretScrubber([]string{secret})})
			if res.LineCount != 2 {
				t.Fatalf("read %d lines, want 2", res.LineCount)
			}
			redacted := 0
			for _, line := range res.Lines {
				var record map[string]any
				if err := json.Unmarshal([]byte(line.Text), &record); err == nil && record["password"] == secret {
					t.Errorf("the secret is readable in the returned line: %s", line.Text)
				}
				redacted += strings.Count(line.Text, secretValueMarker)
			}
			if redacted != 1 {
				t.Errorf("redaction markers in the returned lines = %d, want 1 (redactions=%v)", redacted, res.Redactions)
			}
		})
	}
}

// A record written with encoding/json's default HTML escaping (as the output
// sources' Go writer does) is covered too.
func TestOwnSecretIsRedactedInHTMLEscapedJSON(t *testing.T) {
	const secret = `a<b>&c "quoted" secret-0123456789`
	record, err := json.Marshal(map[string]string{"text": "token is " + secret + " end"})
	if err != nil {
		t.Fatal(err)
	}
	out, n := NewSecretScrubber([]string{secret}).Replace(string(record))
	if n != 1 || strings.Contains(out, "secret-0123456789") {
		t.Fatalf("replaced %d, result %s", n, out)
	}
}

// Ordinary text is left alone, and a value with nothing to escape gains no
// new form.
func TestJSONEscapedFormsAddNothingForPlainValues(t *testing.T) {
	const secret = "plain-secret-0123456789"
	for _, form := range jsonEscapedForms(secret) {
		if form != secret {
			t.Errorf("plain value gained the form %q", form)
		}
	}
	text := `{"msg":"nothing secret here","path":"C:\\temp\\file \"quoted\""}`
	if out, n := NewSecretScrubber([]string{`pa"ss\word-0123456789`}).Replace(text); n != 0 || out != text {
		t.Errorf("unrelated text changed: %d replacements, %s", n, out)
	}
}
