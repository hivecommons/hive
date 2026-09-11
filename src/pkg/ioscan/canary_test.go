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
	cases := map[string]string{
		"base64":           base64.StdEncoding.EncodeToString([]byte(c.Token)),
		"hex":              hex.EncodeToString([]byte(c.Token)),
		"url":              url.QueryEscape(c.Token),
		"split":            spaced,
		"reversed":         reversed,
		"case-insensitive": strings.ToLower(c.Token),
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

func reverseCanaryForTest(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}
