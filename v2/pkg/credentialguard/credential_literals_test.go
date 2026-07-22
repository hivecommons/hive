package credentialguard

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type credentialLiteralPattern struct {
	name    string
	pattern *regexp.Regexp
}

// TestRepositoryHasNoCredentialLiterals prevents realistic provider-shaped
// fixture values from being committed. Tests that exercise credential
// detection must assemble their input at runtime instead.
func TestRepositoryHasNoCredentialLiterals(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve credential guard source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", ".."))

	patterns := credentialLiteralPatterns()
	files, err := trackedCredentialGuardFiles(root)
	if err != nil {
		t.Fatalf("enumerate repository credential guard scope: %v", err)
	}
	var findings []string
	for _, relative := range files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("tracked credential-guard path is missing, linked, or non-regular: %s: %v", relative, statErr)
		}
		if info.Size() > 16<<20 {
			t.Fatalf("tracked credential-guard path exceeds the 16 MiB review bound: %s", relative)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read tracked credential-guard path %s: %v", relative, readErr)
		}
		if bytes.IndexByte(data, 0) >= 0 {
			continue
		}
		if match := privateKeyLiteralPattern().FindIndex(data); match != nil {
			line := bytes.Count(data[:match[0]], []byte{'\n'}) + 1
			findings = append(findings, fmt.Sprintf("%s:%d: private key block", relative, line))
		}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		buffer := make([]byte, 64<<10)
		scanner.Buffer(buffer, 16<<20)
		line := 0
		for scanner.Scan() {
			line++
			for _, candidate := range patterns {
				if candidate.pattern.Match(scanner.Bytes()) {
					findings = append(findings, fmt.Sprintf("%s:%d: %s", relative, line, candidate.name))
				}
			}
		}
		if scanErr := scanner.Err(); scanErr != nil {
			t.Fatalf("scan tracked credential-guard path %s: %v", relative, scanErr)
		}
	}
	if len(findings) != 0 {
		sort.Strings(findings)
		t.Fatalf("credential-shaped literals must be assembled at runtime:\n%s", strings.Join(findings, "\n"))
	}
}

func trackedCredentialGuardFiles(root string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z")
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(output, []byte{0})
	if len(parts) > 100001 {
		return nil, fmt.Errorf("tracked repository inventory exceeds 100000 paths")
	}
	files := make([]string, 0, len(parts))
	for _, raw := range parts {
		if len(raw) == 0 {
			continue
		}
		if !utf8.Valid(raw) {
			return nil, fmt.Errorf("tracked repository path is not UTF-8")
		}
		relative := filepath.ToSlash(string(raw))
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
		if relative != clean || filepath.IsAbs(filepath.FromSlash(relative)) || relative == ".." || strings.HasPrefix(relative, "../") {
			return nil, fmt.Errorf("tracked repository path is unsafe: %q", relative)
		}
		files = append(files, relative)
	}
	sort.Strings(files)
	return files, nil
}

func credentialLiteralPatterns() []credentialLiteralPattern {
	return []credentialLiteralPattern{
		{name: "cloud access key identifier", pattern: regexp.MustCompile(`\b(?:AK` + `IA|AS` + `IA)[A-Z0-9]{16}\b`)},
		{name: "collaboration token", pattern: regexp.MustCompile(`\bxo` + `x[baprs]-[A-Za-z0-9-]{10,}\b`)},
		{name: "source-host token", pattern: regexp.MustCompile(`\b(?:github` + `_pat_[A-Za-z0-9_]{20,}|gh` + `[pousr]_[A-Za-z0-9]{20,})\b`)},
		{name: "alternate source-host token", pattern: regexp.MustCompile(`\bgl` + `pat-[A-Za-z0-9_-]{20,}\b`)},
		{name: "model-provider key", pattern: regexp.MustCompile(`\bs` + `k-(?:(?:proj|svcacct)-)?[A-Za-z0-9_-]{20,}\b`)},
		{name: "browser-platform key", pattern: regexp.MustCompile(`\bAI` + `za[0-9A-Za-z_-]{20,}\b`)},
		{name: "package-registry token", pattern: regexp.MustCompile(`\bnpm` + `_[A-Za-z0-9]{30,}\b`)},
		{name: "python-registry token", pattern: regexp.MustCompile(`\bpyp` + `i-[A-Za-z0-9_-]{20,}\b`)},
		{name: "payment-provider live key", pattern: regexp.MustCompile(`\b(?:s` + `k|rk)_live_[A-Za-z0-9]{16,}\b`)},
		{name: "cloud service account key", pattern: regexp.MustCompile(`\bya` + `29\.[A-Za-z0-9_-]{30,}\b`)},
	}
}

func TestCredentialLiteralPatterns(t *testing.T) {
	fixtures := []string{
		"AK" + "IA" + "ABCDEFGHIJKLMNOP",
		"AS" + "IA" + "ABCDEFGHIJKLMNOP",
		"xo" + "xb-1234567890-ABCDEFGHIJ",
		"github" + "_pat_ABCDEFGHIJKLMNOPQRSTUVWXYZ123456",
		"gh" + "p_ABCDEFGHIJKLMNOPQRSTUVWXYZ123456",
		"gl" + "pat-ABCDEFGHIJKLMNOPQRSTUVWXYZ123456",
		"s" + "k-proj-ABCDEFGHIJKLMNOPQRSTUVWXYZ123456",
		"AI" + "zaABCDEFGHIJKLMNOPQRSTUVWXYZ123456",
		"npm" + "_ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890",
		"pyp" + "i-ABCDEFGHIJKLMNOPQRSTUVWXYZ123456",
		"s" + "k_live_ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		"ya" + "29.ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890",
	}
	patterns := credentialLiteralPatterns()
	for index, fixture := range fixtures {
		matched := false
		for _, candidate := range patterns {
			matched = matched || candidate.pattern.MatchString(fixture)
		}
		if !matched {
			t.Errorf("credential fixture %d was not detected", index)
		}
	}
	pem := "-----BEGIN OPENSSH PRIVATE" + " KEY-----\n" + strings.Repeat("A", 64) + "\n-----END OPENSSH PRIVATE" + " KEY-----"
	if !privateKeyLiteralPattern().MatchString(pem) {
		t.Fatal("private-key block fixture was not detected")
	}
}

func TestCredentialGuardInventoryIncludesRepositoryRootWorkflowsAndExtensionlessFiles(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve credential guard source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", ".."))
	files, err := trackedCredentialGuardFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, path := range files {
		present[path] = true
	}
	for _, required := range []string{".github/workflows/integrated-release.yml", ".github/workflows/v2-ci.yml", ".gitignore"} {
		if !present[required] {
			t.Fatalf("credential guard inventory omitted tracked root path %s", required)
		}
	}
}

func privateKeyLiteralPattern() *regexp.Regexp {
	return regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE` + ` KEY-----[[:space:]]+[A-Za-z0-9+/=\r\n]{40,}-----END (?:RSA |EC |DSA |OPENSSH )?PRIVATE` + ` KEY-----`)
}
