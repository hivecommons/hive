package dashboard

import (
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hivecommons/hive/crux/logscrub"
)

// matrixEntropyRun is the longest run of token-alphabet characters a Matrix
// line may carry; a longer run looks like a key, hash or encoded secret.
const matrixEntropyRun = 32

var matrixLockfiles = map[string]bool{
	"go.sum":              true,
	"package-lock.json":   true,
	"npm-shrinkwrap.json": true,
	"yarn.lock":           true,
	"pnpm-lock.yaml":      true,
	"cargo.lock":          true,
	"gemfile.lock":        true,
	"poetry.lock":         true,
	"pipfile.lock":        true,
	"composer.lock":       true,
	"bun.lockb":           true,
}

// matrixPathDenied reports whether a file must be skipped before its patch is
// parsed: secret-shaped names, lockfiles, vendored or generated code, and the
// operator's dashboard.matrix.exclude_paths globs.
func matrixPathDenied(p string, excludes []string) bool {
	if strings.TrimSpace(p) == "" {
		return true
	}
	lower := strings.ToLower(p)
	base := path.Base(lower)
	switch {
	case strings.HasPrefix(base, ".env"),
		strings.HasSuffix(base, ".pem"),
		strings.HasSuffix(base, ".key"),
		strings.HasSuffix(base, ".p12"),
		strings.HasPrefix(base, "id_"),
		matrixLockfiles[base],
		strings.HasSuffix(base, ".lock"),
		strings.Contains(base, ".min."),
		strings.Contains(lower, "zz_generated"),
		strings.HasSuffix(base, ".pb.go"),
		strings.Contains(base, "_gen."):
		return true
	}
	segments := strings.Split(lower, "/")
	for i, seg := range segments {
		if strings.Contains(seg, "secret") || strings.Contains(seg, "credential") || strings.Contains(seg, "token") {
			return true
		}
		if i < len(segments)-1 && (seg == "vendor" || seg == "node_modules") {
			return true
		}
	}
	for _, glob := range excludes {
		glob = strings.TrimSpace(glob)
		if glob == "" {
			continue
		}
		if ok, _ := path.Match(glob, p); ok {
			return true
		}
		if ok, _ := path.Match(glob, path.Base(p)); ok {
			return true
		}
		if dir := strings.TrimSuffix(glob, "/**"); dir != glob && (p == dir || strings.HasPrefix(p, dir+"/")) {
			return true
		}
	}
	return false
}

// matrixAddedLines returns the candidate lines a patch added: `+` lines
// without the `+++` header, minus anything inside a BEGIN/END armour block
// (a PEM key is only recognisable as a whole, never line by line). A patch
// that is empty or carries NUL bytes is treated as binary and yields nothing.
func matrixAddedLines(patch string) []string {
	if patch == "" || strings.ContainsRune(patch, 0) {
		return nil
	}
	var out []string
	inArmour := false
	for _, raw := range strings.Split(patch, "\n") {
		if strings.HasPrefix(raw, "+++") || !strings.HasPrefix(raw, "+") {
			continue
		}
		line := raw[1:]
		if strings.Contains(line, "-----BEGIN ") {
			inArmour = true
		}
		if inArmour {
			if strings.Contains(line, "-----END ") {
				inArmour = false
			}
			continue
		}
		out = append(out, line)
	}
	return out
}

// matrixAcceptLine trims a candidate line and reports whether it may be
// shown. Every rule drops; nothing is ever masked, so a rejected line never
// leaves a partial secret behind.
func matrixAcceptLine(raw string) (string, bool) {
	line := strings.TrimSpace(raw)
	if line == "" || !utf8.ValidString(line) {
		return "", false
	}
	if utf8.RuneCountInString(line) > matrixMaxLineRunes {
		return "", false
	}
	for _, r := range line {
		if unicode.IsControl(r) && r != '\t' {
			return "", false
		}
	}
	if matrixHasEntropyRun(line) {
		return "", false
	}
	if logscrub.ContainsRedactionMarker(line) {
		return "", false
	}
	if logscrub.ScrubString(line) != line || logscrub.RedactPaneTokens(line) != line {
		return "", false
	}
	return line, true
}

func matrixHasEntropyRun(s string) bool {
	run := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '+' || c == '/' || c == '=' || c == '_' || c == '-' {
			run++
			if run >= matrixEntropyRun {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}

var matrixLangByExt = map[string]string{
	".go":   "go",
	".js":   "javascript",
	".mjs":  "javascript",
	".cjs":  "javascript",
	".jsx":  "javascript",
	".ts":   "typescript",
	".tsx":  "typescript",
	".py":   "python",
	".rb":   "ruby",
	".rs":   "rust",
	".java": "java",
	".kt":   "kotlin",
	".c":    "c",
	".h":    "c",
	".cc":   "cpp",
	".cpp":  "cpp",
	".hpp":  "cpp",
	".cs":   "csharp",
	".sh":   "shell",
	".bash": "shell",
	".yaml": "yaml",
	".yml":  "yaml",
	".json": "json",
	".toml": "toml",
	".md":   "markdown",
	".html": "html",
	".css":  "css",
	".sql":  "sql",
}

// matrixLang names a file's language from its extension; "" when unknown.
func matrixLang(p string) string {
	return matrixLangByExt[strings.ToLower(path.Ext(p))]
}
