package hivectl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/forge"
)

const validPresetYAML = `version: 1
name: Bluefin ecosystem
description: Hives Bluefin depends on
revision: "2026-10-17"
hives:
  - url: https://bluefin-hive.example
    description: Why we recommend this Hive
    repositories: all-current
  - url: https://dependency-hive.example
    repositories:
      - repo: dependency-org/component-a
        forge: github
        host: github.com
        description: optional
      - repo: dependency-org/component-b
      - repo: group/subgroup/project
        forge: gitlab
      - repo: org/thing
        forge: gitea
        host: codeberg.org
hubs:
  - url: https://hub.example
    hives: all-current
  - url: https://other-hub.example
    hives:
      - https://bluefin-hive.example
`

func TestPresetForgeKindsMatchForgePackage(t *testing.T) {
	if presetForgeGitHub != string(forge.KindGitHub) || presetForgeGitLab != string(forge.KindGitLab) || presetForgeGitea != string(forge.KindGitea) {
		t.Fatal("preset forge kinds drifted from pkg/forge")
	}
}

func TestParsePresetValid(t *testing.T) {
	p, err := ParsePreset([]byte(validPresetYAML))
	if err != nil {
		t.Fatalf("ParsePreset: %v", err)
	}
	if p.Version != 1 || p.Name != "Bluefin ecosystem" || p.Revision != "2026-10-17" {
		t.Fatalf("header parsed as %+v", p)
	}
	if len(p.Hives) != 2 {
		t.Fatalf("hives = %d, want 2", len(p.Hives))
	}
	if !p.Hives[0].Repositories.AllCurrent || p.Hives[0].Repositories.Repos != nil {
		t.Errorf("first hive should select all-current, got %+v", p.Hives[0].Repositories)
	}
	repos := p.Hives[1].Repositories.Repos
	if len(repos) != 4 {
		t.Fatalf("repos = %+v, want 4", repos)
	}
	if repos[1].Forge != "github" || repos[1].Host != "github.com" {
		t.Errorf("default forge/host not applied: %+v", repos[1])
	}
	if repos[2].Forge != "gitlab" || repos[2].Host != "gitlab.com" {
		t.Errorf("gitlab default host not applied: %+v", repos[2])
	}
	if repos[3].Host != "codeberg.org" {
		t.Errorf("explicit gitea host lost: %+v", repos[3])
	}
	if len(p.Hubs) != 2 || !p.Hubs[0].Hives.AllCurrent || len(p.Hubs[1].Hives.URLs) != 1 {
		t.Errorf("hubs parsed as %+v", p.Hubs)
	}
}

func TestParsePresetMinimalAndEmptySelection(t *testing.T) {
	p, err := ParsePreset([]byte("version: 1\nname: x\nhives:\n  - url: https://h.example\n    repositories: []\n"))
	if err != nil {
		t.Fatalf("ParsePreset: %v", err)
	}
	if p.Hives[0].Repositories.AllCurrent || p.Hives[0].Repositories.Repos == nil || len(p.Hives[0].Repositories.Repos) != 0 {
		t.Errorf("empty list must stay an explicit empty selection, got %+v", p.Hives[0].Repositories)
	}
}

func TestParsePresetDeduplicatesRepos(t *testing.T) {
	doc := `version: 1
name: dedup
hives:
  - url: https://h.example
    repositories:
      - repo: Org/Repo
      - repo: org/repo
        forge: GitHub
        host: GitHub.com
      - repo: org/repo
        forge: gitlab
      - repo: org/other
hubs:
  - url: https://hub.example
    hives:
      - https://a.example
      - https://A.example/
`
	p, err := ParsePreset([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePreset: %v", err)
	}
	repos := p.Hives[0].Repositories.Repos
	if len(repos) != 3 {
		t.Fatalf("repos = %+v, want 3 after dedup (same repo on another forge is distinct)", repos)
	}
	if repos[0].Identity() != "github|github.com|org/repo" {
		t.Errorf("identity = %q", repos[0].Identity())
	}
	if got := p.Hubs[0].Hives.URLs; len(got) != 1 {
		t.Errorf("hub hives = %v, want 1 after dedup", got)
	}
}

func TestParsePresetRejectsDeniedFields(t *testing.T) {
	cases := map[string]string{
		"top-level token":          "version: 1\nname: x\ntoken: abc\nhives: []\n",
		"registration token":       "version: 1\nname: x\nhives:\n  - url: https://h.example\n    registration_token: abc\n    repositories: all-current\n",
		"provider credential":      "version: 1\nname: x\nanthropic_api_key: k\nhives: []\n",
		"secret":                   "version: 1\nname: x\nsecrets:\n  a: b\nhives: []\n",
		"env":                      "version: 1\nname: x\nenv:\n  FOO: bar\nhives: []\n",
		"command":                  "version: 1\nname: x\ncommand: rm -rf /\nhives: []\n",
		"script":                   "version: 1\nname: x\nhives:\n  - url: https://h.example\n    script: curl x | sh\n    repositories: all-current\n",
		"hooks":                    "version: 1\nname: x\nhooks:\n  pre: echo\nhives: []\n",
		"backend":                  "version: 1\nname: x\nbackend: claude\nhives: []\n",
		"model":                    "version: 1\nname: x\nhives:\n  - url: https://h.example\n    model: opus\n    repositories: all-current\n",
		"permissions":              "version: 1\nname: x\npermissions: all\nhives: []\n",
		"dangerous skip":           "version: 1\nname: x\ndangerously-skip-permissions: true\nhives: []\n",
		"allowed tools":            "version: 1\nname: x\nallowed_tools: [Bash]\nhives: []\n",
		"nested repo token":        "version: 1\nname: x\nhives:\n  - url: https://h.example\n    repositories:\n      - repo: a/b\n        token: t\n",
		"session":                  "version: 1\nname: x\nsession: review\nhives: []\n",
		"denied field on newer v2": "version: 2\nname: x\npassword: p\nhives: []\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePreset([]byte(doc))
			if err == nil {
				t.Fatalf("expected rejection")
			}
			if !strings.Contains(err.Error(), "is not allowed") {
				t.Fatalf("error should name the forbidden field, got: %v", err)
			}
		})
	}
	_, err := ParsePreset([]byte("version: 1\nname: x\nhives:\n  - url: https://h.example\n    repositories:\n      - repo: a/b\n        token: t\n"))
	if err == nil || !strings.Contains(err.Error(), `"hives[0].repositories[0].token"`) {
		t.Errorf("error should carry the full field path, got: %v", err)
	}
}

func TestParsePresetRejectsUnknownFields(t *testing.T) {
	for name, doc := range map[string]string{
		"top level":  "version: 1\nname: x\nmystery: 1\nhives:\n  - url: https://h.example\n    repositories: all-current\n",
		"hive":       "version: 1\nname: x\nhives:\n  - url: https://h.example\n    mystery: 1\n    repositories: all-current\n",
		"repository": "version: 1\nname: x\nhives:\n  - url: https://h.example\n    repositories:\n      - repo: a/b\n        mystery: 1\n",
		"hub":        "version: 1\nname: x\nhubs:\n  - url: https://hub.example\n    mystery: 1\n    hives: all-current\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePreset([]byte(doc)); err == nil || !strings.Contains(err.Error(), "mystery") {
				t.Fatalf("expected unknown-field error naming mystery, got %v", err)
			}
		})
	}
}

func TestParsePresetVersion(t *testing.T) {
	for name, doc := range map[string]string{
		"missing":  "name: x\nhives: []\n",
		"major 2":  "version: 2\nname: x\nfuture_field: y\nhives: []\n",
		"major 0":  "version: 0\nname: x\nhives: []\n",
		"string":   "version: \"1\"\nname: x\nhives: []\n",
		"float":    "version: 1.1\nname: x\nhives: []\n",
		"negative": "version: -1\nname: x\nhives: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePreset([]byte(doc))
			if err == nil || !strings.Contains(err.Error(), "version") {
				t.Fatalf("expected version refusal, got %v", err)
			}
		})
	}
}

func TestParsePresetRejectsInvalidDocuments(t *testing.T) {
	cases := map[string]struct{ doc, want string }{
		"display name":        {"version: 1\nname: x\nhives:\n  - url: https://h.example\n    repositories:\n      - repo: Component A\n", "display name"},
		"bare name":           {"version: 1\nname: x\nhives:\n  - url: https://h.example\n    repositories:\n      - repo: component-a\n", "owner/name"},
		"plain string repo":   {"version: 1\nname: x\nhives:\n  - url: https://h.example\n    repositories:\n      - dependency-org/component-a\n", "cannot unmarshal"},
		"empty owner":         {"version: 1\nname: x\nhives:\n  - url: https://h.example\n    repositories:\n      - repo: /b\n", "owner/name"},
		"github nested":       {"version: 1\nname: x\nhives:\n  - url: https://h.example\n    repositories:\n      - repo: a/b/c\n", "owner/name"},
		"unknown forge":       {"version: 1\nname: x\nhives:\n  - url: https://h.example\n    repositories:\n      - repo: a/b\n        forge: bitbucket\n", "not a known forge"},
		"gitea without host":  {"version: 1\nname: x\nhives:\n  - url: https://h.example\n    repositories:\n      - repo: a/b\n        forge: gitea\n", "host is required"},
		"host with scheme":    {"version: 1\nname: x\nhives:\n  - url: https://h.example\n    repositories:\n      - repo: a/b\n        host: https://github.com\n", "bare host"},
		"all keyword":         {"version: 1\nname: x\nhives:\n  - url: https://h.example\n    repositories: all\n", "all-current"},
		"follow keyword":      {"version: 1\nname: x\nhives:\n  - url: https://h.example\n    repositories: follow\n", "all-current"},
		"hub all keyword":     {"version: 1\nname: x\nhubs:\n  - url: https://hub.example\n    hives: all\n", "all-current"},
		"missing repos":       {"version: 1\nname: x\nhives:\n  - url: https://h.example\n", "repositories is required"},
		"missing hub hives":   {"version: 1\nname: x\nhubs:\n  - url: https://hub.example\n", "hives is required"},
		"missing name":        {"version: 1\nhives:\n  - url: https://h.example\n    repositories: all-current\n", "name is required"},
		"nothing recommended": {"version: 1\nname: x\n", "recommends nothing"},
		"duplicate hive":      {"version: 1\nname: x\nhives:\n  - url: https://h.example\n    repositories: all-current\n  - url: https://H.example/\n    repositories: all-current\n", "more than once"},
		"http hive":           {"version: 1\nname: x\nhives:\n  - url: http://h.example\n    repositories: all-current\n", "https"},
		"wss hive":            {"version: 1\nname: x\nhives:\n  - url: wss://h.example/contribute\n    repositories: all-current\n", "https"},
		"ws hub":              {"version: 1\nname: x\nhubs:\n  - url: ws://hub.example\n    hives: all-current\n", "https"},
		"http hub hive":       {"version: 1\nname: x\nhubs:\n  - url: https://hub.example\n    hives:\n      - http://h.example\n", "https"},
		"credentials in url":  {"version: 1\nname: x\nhives:\n  - url: https://user:pw@h.example\n    repositories: all-current\n", "credentials"},
		"alias":               {"version: 1\nname: x\nhives:\n  - url: &u https://h.example\n    repositories: all-current\n  - url: *u\n    repositories: all-current\n", "alias"},
		"two documents":       {"version: 1\nname: x\nhives:\n  - url: https://h.example\n    repositories: all-current\n---\nversion: 1\n", "exactly one"},
		"not a mapping":       {"- a\n- b\n", "mapping"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePreset([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestParsePresetAllowsLoopbackHTTP(t *testing.T) {
	doc := "version: 1\nname: x\nhives:\n  - url: http://127.0.0.1:3001\n    repositories: all-current\n  - url: http://localhost:3002\n    repositories: all-current\n"
	if _, err := ParsePreset([]byte(doc)); err != nil {
		t.Fatalf("loopback http should be allowed: %v", err)
	}
}

func TestParsePresetSizeLimits(t *testing.T) {
	big := "version: 1\nname: x\ndescription: \"" + strings.Repeat("a", MaxPresetBytes) + "\"\nhives: []\n"
	if _, err := ParsePreset([]byte(big)); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected size refusal, got %v", err)
	}

	var b strings.Builder
	b.WriteString("version: 1\nname: x\nhives:\n  - url: https://h.example\n    repositories:\n")
	for i := 0; i <= MaxPresetEntries; i++ {
		b.WriteString("      - repo: o/r" + strconv.Itoa(i) + "\n")
	}
	if b.Len() > MaxPresetBytes {
		t.Fatalf("entry-cap fixture is %d bytes; it must stay under the byte cap to exercise the entry cap", b.Len())
	}
	if _, err := ParsePreset([]byte(b.String())); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("expected entry-cap refusal, got %v", err)
	}
}

func TestLoadPresetLocalFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bluefin.preset.yml")
	if err := os.WriteFile(path, []byte(validPresetYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPreset(context.Background(), path)
	if err != nil {
		t.Fatalf("LoadPreset: %v", err)
	}
	if p.Name != "Bluefin ecosystem" {
		t.Errorf("name = %q", p.Name)
	}

	bigPath := filepath.Join(dir, "big.yml")
	if err := os.WriteFile(bigPath, make([]byte, MaxPresetBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPreset(context.Background(), bigPath); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("expected size refusal for large file, got %v", err)
	}
	if _, err := LoadPreset(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Errorf("expected directory refusal, got %v", err)
	}
	if _, err := LoadPreset(context.Background(), filepath.Join(dir, "missing.yml")); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoadPresetRefusesOtherSchemes(t *testing.T) {
	for _, src := range []string{
		"file:///etc/passwd",
		"ftp://example.com/p.yml",
		"wss://example.com/p.yml",
		"ws://example.com/p.yml",
		"data://text/plain,version: 1",
		"http://example.com/p.yml",
		"",
	} {
		if _, err := LoadPreset(context.Background(), src); err == nil {
			t.Errorf("LoadPreset(%q) should be refused", src)
		}
	}
}

func TestLoadPresetHTTPS(t *testing.T) {
	var sawAuth, sawCookie atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth.Store(true)
		}
		if r.Header.Get("Cookie") != "" {
			sawCookie.Store(true)
		}
		switch r.URL.Path {
		case "/preset.yml":
			http.SetCookie(w, &http.Cookie{Name: "track", Value: "1"})
			_, _ = w.Write([]byte(validPresetYAML))
		case "/hop":
			http.Redirect(w, r, "/preset.yml", http.StatusFound)
		case "/downgrade":
			http.Redirect(w, r, "http://127.0.0.1:1/preset.yml", http.StatusFound)
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("#", MaxPresetBytes+10)))
		case "/bad":
			_, _ = w.Write([]byte("version: 1\nname: x\ntoken: t\nhives: []\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := newPresetHTTPClient(srv.Client().Transport)
	ctx := context.Background()

	p, err := loadPreset(ctx, srv.URL+"/preset.yml", client)
	if err != nil {
		t.Fatalf("loadPreset: %v", err)
	}
	if p.Name != "Bluefin ecosystem" {
		t.Errorf("name = %q", p.Name)
	}
	if _, err := loadPreset(ctx, srv.URL+"/hop", client); err != nil {
		t.Errorf("https→https redirect should be followed: %v", err)
	}
	if sawAuth.Load() || sawCookie.Load() {
		t.Errorf("preset fetch must not send credentials or cookies (auth=%v cookie=%v)", sawAuth.Load(), sawCookie.Load())
	}
	if _, err := loadPreset(ctx, srv.URL+"/downgrade", client); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("https→http redirect should be refused, got %v", err)
	}
	if _, err := loadPreset(ctx, srv.URL+"/big", client); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("oversized response should be refused, got %v", err)
	}
	if _, err := loadPreset(ctx, srv.URL+"/bad", client); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("fetched preset with a token should be refused naming the field, got %v", err)
	}
	if _, err := loadPreset(ctx, srv.URL+"/missing", client); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("non-200 should be an error, got %v", err)
	}
	if client.Jar != nil {
		t.Error("preset client must not carry a cookie jar")
	}
}

func TestLoadPresetLoopbackHTTPRedirectPolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/preset.yml":
			_, _ = w.Write([]byte(validPresetYAML))
		case "/external":
			http.Redirect(w, r, "http://example.com/preset.yml", http.StatusFound)
		case "/scheme":
			http.Redirect(w, r, "ftp://127.0.0.1/preset.yml", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := newPresetHTTPClient(srv.Client().Transport)
	ctx := context.Background()

	if _, err := loadPreset(ctx, srv.URL+"/preset.yml", client); err != nil {
		t.Fatalf("loopback http preset should load: %v", err)
	}
	if _, err := loadPreset(ctx, srv.URL+"/external", client); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("redirect to non-loopback http should be refused, got %v", err)
	}
	if _, err := loadPreset(ctx, srv.URL+"/scheme", client); err == nil {
		t.Error("redirect to another scheme should be refused")
	}
}
