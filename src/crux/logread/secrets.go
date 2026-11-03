package logread

import (
	"encoding/base64"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SecretValueKind is the redaction kind for this hive's own secret values.
const SecretValueKind = "hive-secret-value"

const secretValueMarker = "<redacted:" + SecretValueKind + ">"

// SecretScrubber replaces this hive's own secret values in log text: each
// value of at least MinSecretValueLen characters in its plain, URL-encoded and
// base64 forms, the base64 forms at all three byte alignments so the value is
// found inside a longer encoded string (an "Authorization: Basic" header
// encoding user:value). It is one precompiled multi-value replacer, built in
// process for one read; the values are never returned.
type SecretScrubber struct {
	r *strings.Replacer
}

// NewSecretScrubber builds the replacer over values. Values shorter than
// MinSecretValueLen are skipped: replacing them would redact unrelated text.
func NewSecretScrubber(values []string) *SecretScrubber {
	set := map[string]bool{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < MinSecretValueLen {
			continue
		}
		for _, form := range secretForms(v) {
			if len(form) >= MinSecretValueLen {
				set[form] = true
			}
		}
	}
	if len(set) == 0 {
		return &SecretScrubber{}
	}
	forms := make([]string, 0, len(set))
	for f := range set {
		forms = append(forms, f)
	}
	// strings.Replacer prefers earlier arguments among matches at the same
	// position, so the longest form wins and no tail of a value survives.
	sort.Slice(forms, func(i, j int) bool {
		if len(forms[i]) != len(forms[j]) {
			return len(forms[i]) > len(forms[j])
		}
		return forms[i] < forms[j]
	})
	pairs := make([]string, 0, 2*len(forms))
	for _, f := range forms {
		pairs = append(pairs, f, secretValueMarker)
	}
	return &SecretScrubber{r: strings.NewReplacer(pairs...)}
}

// Replace redacts every own secret value in text and reports how many it
// replaced.
func (s *SecretScrubber) Replace(text string) (string, int) {
	if s == nil || s.r == nil {
		return text, 0
	}
	out := s.r.Replace(text)
	if out == text {
		return text, 0
	}
	return out, strings.Count(out, secretValueMarker) - strings.Count(text, secretValueMarker)
}

// secretForms returns the encodings of v that can appear in a log line.
func secretForms(v string) []string {
	forms := []string{v, url.QueryEscape(v), url.PathEscape(v)}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		forms = append(forms, enc.EncodeToString([]byte(v)))
		raw := enc.WithPadding(base64.NoPadding)
		for k := 0; k < 3; k++ {
			forms = append(forms, base64Fragment(raw, v, k))
		}
	}
	return forms
}

// base64Fragment returns the base64 characters fully determined by v when v
// starts k bytes into the encoded input. The characters that mix bits of v
// with the unknown neighbouring bytes are dropped from both ends.
func base64Fragment(enc *base64.Encoding, v string, k int) string {
	buf := make([]byte, k+len(v))
	copy(buf[k:], v)
	full := enc.EncodeToString(buf)
	start := (8*k + 5) / 6
	end := (8 * len(buf)) / 6
	if start >= end {
		return ""
	}
	return full[start:end]
}

// secretEnvWords mark an environment variable whose value is a secret.
var secretEnvWords = []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "CREDENTIAL", "PASSPHRASE"}

// secretEnvIndirection marks a variable that names where a secret is (a file,
// path, directory or another variable) rather than holding it. The file
// contents are covered by the secret directories instead.
var secretEnvIndirection = []string{"_FILE", "_PATH", "_DIR", "_ENV"}

// SecretEnvName reports whether an environment variable's value is treated as
// one of this hive's secrets.
func SecretEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, suffix := range secretEnvIndirection {
		if strings.HasSuffix(upper, suffix) {
			return false
		}
	}
	for _, w := range secretEnvWords {
		if strings.Contains(upper, w) {
			return true
		}
	}
	return false
}

// SecretSources lists where this hive's own secret values come from.
type SecretSources struct {
	// Values are known secrets: the dashboard token, the terminal login in
	// its user:token form, the terminal key.
	Values []string
	// Environ is the process environment, as from os.Environ.
	Environ []string
	// SecretDirs are the directories holding the hive's secret files.
	SecretDirs []string
}

const (
	maxSecretFiles     = 256
	maxSecretFileBytes = 64 * 1024
	maxSecretDirDepth  = 3
)

// CollectSecretValues gathers the values a read redacts. Secret files
// contribute their whole content and each line of it; PEM armour lines are
// left to the private-key scrubbing.
func CollectSecretValues(src SecretSources) []string {
	out := append([]string(nil), src.Values...)
	for _, kv := range src.Environ {
		name, value, ok := strings.Cut(kv, "=")
		if ok && SecretEnvName(name) {
			out = append(out, value)
		}
	}
	files := 0
	for _, dir := range src.SecretDirs {
		out = append(out, secretFileValues(dir, &files)...)
	}
	return out
}

func secretFileValues(root string, files *int) []string {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}
	var out []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if rel, rerr := filepath.Rel(root, path); rerr == nil && rel != "." && strings.Count(rel, string(filepath.Separator)) >= maxSecretDirDepth-1 {
				return filepath.SkipDir
			}
			return nil
		}
		if *files >= maxSecretFiles {
			return filepath.SkipAll
		}
		st, serr := os.Stat(path)
		if serr != nil || !st.Mode().IsRegular() || st.Size() > maxSecretFileBytes {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		*files++
		content := string(data)
		out = append(out, content)
		for _, line := range strings.Split(content, "\n") {
			if line = strings.TrimSpace(line); !strings.HasPrefix(line, "-----") {
				out = append(out, line)
			}
		}
		return nil
	})
	return out
}
