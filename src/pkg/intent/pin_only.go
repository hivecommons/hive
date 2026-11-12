package intent

import (
	"fmt"
	"path"
	"regexp"
	"slices"
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

// Pin kinds a PinOnlySpec may name. pinLineKinds are the kinds one
// removed/added pair satisfies on its own, in match precedence order.
// pinKindActionSHARef (pin_only_sha_ref.go) is decided across the whole diff,
// and only for a pair every line kind has declined.
const (
	pinKindActionSHA    = "action-sha"
	pinKindImageDigest  = "image-digest"
	pinKindVersionPin   = "version-pin"
	pinKindActionSHARef = "action-sha-ref"
)

var pinLineKinds = []string{pinKindActionSHA, pinKindImageDigest, pinKindVersionPin}

// PinOnlyKinds returns every kind a PinOnlySpec may name, in the order the
// settings editors offer them. It is the one list: config validation, the
// dashboard and the admin MCP all read it, so a kind the classifier knows is
// never missing from an editor and an editor never offers one it refuses.
func PinOnlyKinds() []string {
	return append(slices.Clone(pinLineKinds), pinKindActionSHARef)
}

// ActionPin is one accepted action-sha replacement: the upstream action
// repository ("owner/repo", without any sub-path), the new 40-hex commit, and
// the trailing version comment ("v4", "v4.1.2"; empty when absent).
type ActionPin struct {
	Repo    string
	SHA     string
	Version string
}

// ClassifyPinOnly checks the entire supplied diff, not only guarded files: a
// mixed PR must not acquire a pin-only exception. Every file must be an existing
// modified file in both the actual human-merge matches and the exception paths.
// Missing/truncated/malformed patches fail closed, including mismatched API
// addition/deletion counts. Deliberately supports only narrow line grammars.
//
// Callers MUST separately prove a complete file list bound to the current head,
// authenticated trusted authorship, and that each new action SHA belongs to
// its upstream repository (ClassifyPinOnlyActions returns them). A true result
// proves syntax only, NEVER those facts or merge safety.
func ClassifyPinOnly(files []ChangedFile, matches []string, spec PinOnlySpec) (bool, string) {
	ok, why, _ := ClassifyPinOnlyActions(files, matches, spec)
	return ok, why
}

// ClassifyPinOnlyActions is ClassifyPinOnly that also returns, on acceptance,
// the distinct action-sha pins it accepted. GitHub resolves a `uses:` SHA
// across the whole fork network of owner/repo, so the syntax alone does not
// tie the new SHA to the upstream action; the caller resolves each pin.
//
// With the action-sha-ref kind, a bare `key: <40-hex>` line is also accepted,
// but only when it carries the old and the new commit of an action-sha
// replacement accepted somewhere in the same diff. Such a line adds no pin:
// the returned list is the `uses:` pins alone, exactly as without the kind.
func ClassifyPinOnlyActions(files []ChangedFile, matches []string, spec PinOnlySpec) (bool, string, []ActionPin) {
	if len(files) == 0 || len(matches) == 0 || len(spec.Paths) == 0 || len(spec.Kinds) == 0 {
		return false, "missing pin-only evidence or policy", nil
	}
	known := PinOnlyKinds()
	kinds := make(map[string]bool)
	for _, kind := range spec.Kinds {
		if !slices.Contains(known, kind) {
			return false, "unknown pin kind: " + kind, nil
		}
		kinds[kind] = true
	}
	guarded := make(map[string]bool)
	for _, match := range matches {
		guarded[match] = true
	}
	seen := make(map[string]bool)
	var pins []ActionPin
	// moves holds the old -> new commit of every accepted action-sha
	// replacement; refs holds the action-sha-ref candidates, which are only
	// decided once every file has been read (pin_only_sha_ref.go).
	moves := make(map[pinSHAMove]bool)
	var refs []pinSHARefCandidate
	for _, file := range files {
		name := file.Filename
		if name == "" || path.Clean(name) != name || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") || seen[name] {
			return false, "invalid or duplicate path: " + name, nil
		}
		seen[name] = true
		if !guarded[name] || !PathMatchesAny(name, spec.Paths) {
			return false, name + ": outside approved human-merge paths", nil
		}
		if strings.Contains(strings.ToLower(name), "owners") || file.Status != "modified" || file.Patch == "" {
			return false, name + ": not an existing modified pin file with a patch", nil
		}
		got, err := classifyPinPatch(file, kinds)
		if err != nil {
			return false, name + ": " + err.Error(), nil
		}
		for _, pin := range got.pins {
			if !slices.Contains(pins, pin) {
				pins = append(pins, pin)
			}
		}
		for _, move := range got.moves {
			moves[move] = true
		}
		for _, move := range got.refs {
			refs = append(refs, pinSHARefCandidate{file: name, move: move})
		}
	}
	for match := range guarded {
		if !seen[match] {
			return false, match + ": missing matched file", nil
		}
	}
	if why := pinSHARefsUnmatched(refs, moves); why != "" {
		return false, why, nil
	}
	return true, "all changes are approved pin replacements (syntax only)", pins
}

// pinPatchResult is what one file's patch contributes: the action pins it
// accepted, the old -> new commit of each of them, and the action-sha-ref
// candidates, which the caller accepts or refuses against every file's moves.
type pinPatchResult struct {
	pins  []ActionPin
	moves []pinSHAMove
	refs  []pinSHAMove
}

func classifyPinPatch(file ChangedFile, kinds map[string]bool) (*pinPatchResult, error) {
	res := &pinPatchResult{}
	var removed, added []string
	oldLeft, newLeft, additions, deletions, hunks := 0, 0, 0, 0, 0
	flush := func() error {
		if len(removed) != len(added) {
			return fmt.Errorf("unpaired addition/removal")
		}
		for i := range removed {
			pin, oldSHA, ok := matchPinReplacement(removed[i], added[i], kinds)
			if !ok {
				// Only a pair every line kind declined may be a companion
				// ref, and it stays a candidate until a uses: pin vouches.
				ref, isRef := pinSHARefMove(removed[i], added[i], kinds)
				if !isRef {
					return fmt.Errorf("non-pin or unsafe replacement")
				}
				res.refs = append(res.refs, ref)
				continue
			}
			if pin != nil {
				res.pins = append(res.pins, *pin)
				res.moves = append(res.moves, pinSHAMove{old: oldSHA, next: pin.SHA})
			}
		}
		removed, added = nil, nil
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(file.Patch, "\n"), "\n")
	for i, line := range lines {
		if m := pinHunkRE.FindStringSubmatch(line); m != nil {
			if err := flush(); err != nil {
				return nil, err
			}
			if oldLeft != 0 || newLeft != 0 {
				return nil, fmt.Errorf("incomplete hunk before line %d", i+1)
			}
			oldLeft, newLeft = 1, 1
			var err error
			if m[1] != "" {
				oldLeft, err = strconv.Atoi(m[1])
				if err != nil {
					return nil, fmt.Errorf("invalid old hunk count")
				}
			}
			if m[2] != "" {
				newLeft, err = strconv.Atoi(m[2])
				if err != nil {
					return nil, fmt.Errorf("invalid new hunk count")
				}
			}
			hunks++
			continue
		}
		if hunks == 0 {
			return nil, fmt.Errorf("invalid patch line %d", i+1)
		}
		if line == "" {
			// GitHub renders blank context lines as "" rather than " ".
			line = " "
		}
		switch line[0] {
		case ' ':
			if err := flush(); err != nil {
				return nil, err
			}
			oldLeft--
			newLeft--
		case '-':
			if len(added) != 0 {
				return nil, fmt.Errorf("interleaved replacement")
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
			return nil, fmt.Errorf("unsupported patch line %d", i+1)
		}
		if oldLeft < 0 || newLeft < 0 {
			return nil, fmt.Errorf("hunk count exceeded")
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if hunks == 0 || oldLeft != 0 || newLeft != 0 || additions == 0 || additions != file.Additions || deletions != file.Deletions {
		return nil, fmt.Errorf("incomplete patch or mismatched changed-line counts")
	}
	return res, nil
}

func pinReplacement(old, next string, kinds map[string]bool) bool {
	_, _, ok := matchPinReplacement(old, next, kinds)
	return ok
}

// matchPinReplacement reports whether old -> next is an accepted pin
// replacement and, for an action-sha replacement, the new pin and the
// lowercased commit it replaces. The old commit is returned beside the pin,
// not in it: ActionPin is what callers resolve upstream, and it stays the new
// commit only.
func matchPinReplacement(old, next string, kinds map[string]bool) (*ActionPin, string, bool) {
	if old == next || unsafePinRE.MatchString(old) || unsafePinRE.MatchString(next) {
		return nil, "", false
	}
	for _, kind := range pinLineKinds {
		if !kinds[kind] {
			continue
		}
		re := actionPinRE
		switch kind {
		case pinKindImageDigest:
			re = imagePinRE
		case pinKindVersionPin:
			re = versionPinRE
		}
		a, b := re.FindStringSubmatch(old), re.FindStringSubmatch(next)
		if a == nil || b == nil || a[1] != b[1] || a[2] == b[2] {
			continue
		}
		// Only action version comments may change; all other suffixes (image
		// aliases, whitespace and quoting) must remain byte-identical.
		if kind == pinKindActionSHA {
			if strings.Contains(a[3], "#") != strings.Contains(b[3], "#") {
				continue
			}
			return actionPinFrom(b), strings.ToLower(a[2]), true
		}
		if kind == pinKindVersionPin {
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
			return nil, "", true
		}
	}
	return nil, "", false
}

// actionPinFrom builds the pin from an actionPinRE match of the new line.
func actionPinFrom(m []string) *ActionPin {
	ref := strings.TrimSpace(m[1])
	ref = strings.TrimPrefix(ref, "- ")
	ref = strings.TrimSuffix(strings.TrimPrefix(ref, "uses: "), "@")
	parts := strings.SplitN(ref, "/", 3)
	return &ActionPin{
		Repo:    parts[0] + "/" + parts[1],
		SHA:     strings.ToLower(m[2]),
		Version: strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(m[3]), "#")),
	}
}
