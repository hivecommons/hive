package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/firsttimer"
)

func TestFirstTimerWelcomeDefaults(t *testing.T) {
	var f FirstTimerWelcomeConfig
	if !f.IsEnabled() {
		t.Fatal("omitted block must default ON")
	}
	off := false
	if (FirstTimerWelcomeConfig{Enabled: &off}).IsEnabled() {
		t.Fatal("enabled: false must opt out")
	}
	on := true
	if !(FirstTimerWelcomeConfig{Enabled: &on}).IsEnabled() {
		t.Fatal("enabled: true must stay on")
	}
	text, err := f.TemplateText()
	if err != nil || text != "" {
		t.Fatalf("no template_path: text=%q err=%v", text, err)
	}
	if err := ValidateFirstTimerWelcome(f); err != nil {
		t.Fatalf("zero value must validate: %v", err)
	}
	var cfg Config
	if !cfg.Governor.FirstTimerWelcome.IsEnabled() {
		t.Fatal("zero Config must have the welcome on")
	}
}

func TestFirstTimerWelcomeTemplatePath(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "welcome.tmpl")
	if err := os.WriteFile(good, []byte("{{ .Marker }}\nHi @{{ .Author }}"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := FirstTimerWelcomeConfig{TemplatePath: good}
	text, err := f.TemplateText()
	if err != nil || !strings.Contains(text, "{{ .Author }}") {
		t.Fatalf("TemplateText = %q, %v", text, err)
	}
	if err := ValidateFirstTimerWelcome(f); err != nil {
		t.Fatalf("valid template rejected: %v", err)
	}

	bad := filepath.Join(dir, "nomarker.tmpl")
	if err := os.WriteFile(bad, []byte("Hi @{{ .Author }}"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = ValidateFirstTimerWelcome(FirstTimerWelcomeConfig{TemplatePath: bad})
	if err == nil || !strings.Contains(err.Error(), firsttimer.Marker) {
		t.Fatalf("template without marker must be rejected: %v", err)
	}
	if err := ValidateFirstTimerWelcome(FirstTimerWelcomeConfig{TemplatePath: filepath.Join(dir, "missing.tmpl")}); err == nil {
		t.Fatal("unreadable template_path must be rejected")
	}
}
