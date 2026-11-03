package intent

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// PinOnlySpec is syntax policy, not a trust or merge authorization decision.
type PinOnlySpec struct {
	Paths []string
	Kinds []string
}

var (
	pinHunkRE    = regexp.MustCompile(`^@@ -[0-9]+(?:,([0-9]+))? \+[0-9]+(?:,([0-9]+))? @@(?: .*)?$`)
	actionPinRE  = regexp.MustCompile(`^([ \t]*(?:- )?uses: [A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@)([0-9a-fA-F]{40})([ \t]*(?:# v[0-9]+(?:\.[0-9]+){0,2})?)$`)
	imagePinRE   = regexp.MustCompile(`^([ \t]*(?:FROM |image: )[A-Za-z0-9][A-Za-z0-9._/:\-]*@sha256:)([0-9a-fA-F]{64})([ \t]*(?:AS [A-Za-z0-9_-]+)?)$`)
	versionPinRE = regexp.MustCompile(`^([ \t]*(?:version: |ARG [A-Z][A-Z0-9_]*_VERSION=))(["']?v?[0-9]+\.[0-9]+\.[0-9]+["']?)([ \t]*)$`)
	unsafePinRE  = regexp.MustCompile(`(?i)(permissions|secrets[.]|\benv\b|\brun:|\bwith:|id-token|cosign|attest|provenance|credential|signing|codeowners)`)
)

// ClassifyPinOnly checks the entire supplied diff, not only guarded files: a
// mixed PR must not acquire a pin-only exception. Every file must be an existing
// modified file in both the actual human-merge matches and the exception paths.
// Missing/truncated/malformed patches fail closed, including mismatched API
// addition/deletion counts. Deliberately supports only narrow line grammars.
//
// Callers MUST separately prove a complete file list bound to the current head,
// authenticated trusted authorship, and review of each referenced upstream
// change. A true result proves syntax only, NEVER those facts or merge safety.
func ClassifyPinOnly(files []ChangedFile, matches []string, spec PinOnlySpec) (bool, string) {
	if len(files) == 0 || len(matches) == 0 || len(spec.Paths) == 0 || len(spec.Kinds) == 0 {
		return false, "missing pin-only evidence or policy"
	}
	kinds := make(map[string]bool)
	for _, kind := range spec.Kinds {
		switch kind {
		case "action-sha", "image-digest", "version-pin":
			kinds[kind] = true
		default:
			return false, "unknown pin kind: " + kind
		}
	}
	guarded := make(map[string]bool)
	for _, match := range matches {
		guarded[match] = true
	}
	seen := make(map[string]bool)
	for _, file := range files {
		name := file.Filename
		if name == "" || path.Clean(name) != name || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") || seen[name] {
			return false, "invalid or duplicate path: " + name
		}
		seen[name] = true
		if !guarded[name] || !PathMatchesAny(name, spec.Paths) {
			return false, name + ": outside approved human-merge paths"
		}
		if strings.Contains(strings.ToLower(name), "owners") || file.Status != "modified" || file.Patch == "" {
			return false, name + ": not an existing modified pin file with a patch"
		}
		if err := classifyPinPatch(file, kinds); err != nil {
			return false, name + ": " + err.Error()
		}
	}
	for match := range guarded {
		if !seen[match] {
			return false, match + ": missing matched file"
		}
	}
	return true, "all changes are approved pin replacements (syntax only)"
}

func classifyPinPatch(file ChangedFile, kinds map[string]bool) error {
	var removed, added []string
	oldLeft, newLeft, additions, deletions, hunks := 0, 0, 0, 0, 0
	flush := func() error {
		if len(removed) != len(added) {
			return fmt.Errorf("unpaired addition/removal")
		}
		for i := range removed {
			if !pinReplacement(removed[i], added[i], kinds) {
				return fmt.Errorf("non-pin or unsafe replacement")
			}
		}
		removed, added = nil, nil
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(file.Patch, "\n"), "\n")
	for i, line := range lines {
		if m := pinHunkRE.FindStringSubmatch(line); m != nil {
			if err := flush(); err != nil {
				return err
			}
			if oldLeft != 0 || newLeft != 0 {
				return fmt.Errorf("incomplete hunk before line %d", i+1)
			}
			oldLeft, newLeft = 1, 1
			var err error
			if m[1] != "" {
				oldLeft, err = strconv.Atoi(m[1])
				if err != nil {
					return fmt.Errorf("invalid old hunk count")
				}
			}
			if m[2] != "" {
				newLeft, err = strconv.Atoi(m[2])
				if err != nil {
					return fmt.Errorf("invalid new hunk count")
				}
			}
			hunks++
			continue
		}
		if hunks == 0 {
			return fmt.Errorf("invalid patch line %d", i+1)
		}
		if line == "" {
			// GitHub renders blank context lines as "" rather than " ".
			line = " "
		}
		switch line[0] {
		case ' ':
			if err := flush(); err != nil {
				return err
			}
			oldLeft--
			newLeft--
		case '-':
			if len(added) != 0 {
				return fmt.Errorf("interleaved replacement")
			}
			removed = append(removed, line[1:])
			deletions++
			oldLeft--
		case '+':
			added = append(added, line[1:])
			additions++
			newLeft--
		default:
			// Even newline/format metadata is outside this bounded grammar.
			return fmt.Errorf("unsupported patch line %d", i+1)
		}
		if oldLeft < 0 || newLeft < 0 {
			return fmt.Errorf("hunk count exceeded")
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if hunks == 0 || oldLeft != 0 || newLeft != 0 || additions == 0 || additions != file.Additions || deletions != file.Deletions {
		return fmt.Errorf("incomplete patch or mismatched changed-line counts")
	}
	return nil
}

func pinReplacement(old, next string, kinds map[string]bool) bool {
	if old == next || unsafePinRE.MatchString(old) || unsafePinRE.MatchString(next) {
		return false
	}
	for _, kind := range []string{"action-sha", "image-digest", "version-pin"} {
		if !kinds[kind] {
			continue
		}
		re := actionPinRE
		switch kind {
		case "image-digest":
			re = imagePinRE
		case "version-pin":
			re = versionPinRE
		}
		a, b := re.FindStringSubmatch(old), re.FindStringSubmatch(next)
		if a == nil || b == nil || a[1] != b[1] || a[2] == b[2] {
			continue
		}
		// Only action version comments may change; all other suffixes (image
		// aliases, whitespace and quoting) must remain byte-identical.
		if kind == "action-sha" {
			if strings.Contains(a[3], "#") != strings.Contains(b[3], "#") {
				continue
			}
			return true
		}
		if kind == "version-pin" {
			// Do not permit quote changes to alter YAML interpretation.
			quote := func(s string) byte {
				if s[0] == '\'' || s[0] == '"' {
					return s[0]
				}
				return 0
			}
			q := quote(a[2])
			if q != quote(b[2]) || (q != 0 && (a[2][len(a[2])-1] != q || b[2][len(b[2])-1] != q)) {
				continue
			}
			if q == 0 && (strings.HasSuffix(a[2], "'") || strings.HasSuffix(a[2], "\"") || strings.HasSuffix(b[2], "'") || strings.HasSuffix(b[2], "\"")) {
				continue
			}
		}
		if a[3] == b[3] {
			return true
		}
	}
	return false
}
