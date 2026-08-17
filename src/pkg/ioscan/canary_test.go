package ioscan

import (
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
