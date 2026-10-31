package toolapprove

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
)

// TextScanner is the payload scanner DefaultSecurityScanner runs over tool
// content, commands and string arguments: prompt-injection, secret and
// dangerous-directive rules. Hive's implementation is pkg/ioscan's
// ToolApproveScanner, installed by the host through SetDefaultTextScanner
// (cmd/hive and pkg/turn do so at init); this package does not import
// pkg/ioscan so it stays inside the Crux ring.
type TextScanner interface {
	ScanInput(text string) TextVerdict
	ScanOutput(text string) TextVerdict
}

// TextVerdict is a TextScanner's answer for one payload.
type TextVerdict struct {
	// Blocked is the scanner's policy decision for the text.
	Blocked bool
	// CriticalInjection reports a critical-severity injection finding, which
	// blocks input even when the scanner's own policy would not.
	CriticalInjection bool
	// Findings lists every rule hit; only High ones become violations.
	Findings []TextFinding
}

// TextFinding is one rule hit, with the masked snippet and rule id the
// violation text names.
type TextFinding struct {
	Kind    string
	Snippet string
	Rule    string
	// High is true at high severity or above.
	High bool
}

// defaultTextScanner is the TextScanner NewDefaultSecurityScanner installs.
var defaultTextScanner atomic.Pointer[TextScanner]

// SetDefaultTextScanner installs the TextScanner that NewDefaultSecurityScanner
// (and every nil-scanner fallback in Resolve and the Desk) uses. Without one
// a scan fails closed: DefaultSecurityScanner.Scan returns ErrNoTextScanner
// and the scan lane denies, so a host that forgets the wiring sees denials,
// not silently unscanned payloads.
func SetDefaultTextScanner(s TextScanner) {
	if s == nil {
		defaultTextScanner.Store(nil)
		return
	}
	defaultTextScanner.Store(&s)
}

// DefaultTextScanner returns the installed TextScanner, or nil.
func DefaultTextScanner() TextScanner {
	if p := defaultTextScanner.Load(); p != nil {
		return *p
	}
	return nil
}

// ErrNoTextScanner is returned by DefaultSecurityScanner.Scan when no
// TextScanner is installed; the scan lane treats it as a failed scan.
var ErrNoTextScanner = errors.New("toolapprove: no text scanner installed (SetDefaultTextScanner)")

// SecurityScanner provides pre-execution security analysis of tool requests.
type SecurityScanner interface {
	Scan(ctx context.Context, req ToolRequest) (ScanResult, error)
}

// DefaultSecurityScanner evaluates tool requests against prompt injections,
// destructive directives, secret leaks, and guardrail path modifications.
type DefaultSecurityScanner struct {
	// Text scans payload text; nil falls back to DefaultTextScanner at scan
	// time, and a scan with neither fails closed.
	Text                  TextScanner
	guardrailPathPatterns []string
}

// NewDefaultSecurityScanner returns a configured DefaultSecurityScanner that
// scans payload text through the installed DefaultTextScanner.
func NewDefaultSecurityScanner() *DefaultSecurityScanner {
	return &DefaultSecurityScanner{
		guardrailPathPatterns: []string{
			".github/workflows/**",
			"**/.github/workflows/**",
			"**/gh-wrapper*",
			"**/gh_wrapper*",
			"**/proxy/rules*",
			"policies/**",
			"**/policies/**",
			"hive.yaml",
			"**/hive.yaml",
			"OWNERS",
			"**/OWNERS",
			"CODEOWNERS",
			"**/CODEOWNERS",
		},
	}
}

var (
	// Path arguments need shell terminators, not word boundaries: /, * and ~
	// are non-word characters. The root target is one or more path components
	// made only of slashes and an optional . or .. (/, //, /., /.., /./,
	// /../). $HOME, ${HOME} and ~ may carry the same suffix, or /*, and still
	// name the whole home directory. Split flags (-r -f) and surrounding
	// single or double quotes are accepted too. Names such as /.cache or
	// ~/.cache are not matched. The fork bomb likewise needs no word boundary.
	destructiveCmdRe  = regexp.MustCompile(`(?i)(\brm\s+(-[rRfF]+\s+)+["']?(` + rmRootTargetPattern + `)["']?(\s|$|[;&|])|\bmkfs\b|\bdd\s+if=/dev/zero\b|:\(\)\{\s*:\|:&\s*\};:)`)
	credentialExfilRe = regexp.MustCompile(`(?i)\b(cat\s+~/\.ssh/|cat\s+/var/run/secrets/|printenv\s+.*TOKEN|echo\s+\$GH_TOKEN|echo\s+\$GITHUB_TOKEN)\b`)
	// shellSeparatorRe splits a command line into the simple commands that
	// isDestructiveRm inspects one at a time.
	shellSeparatorRe = regexp.MustCompile(`[;&|\n]+`)
	// rmRootTargetRe anchors rmRootTargetPattern so isDestructiveRm can test
	// a single argv token; it shares the target set with destructiveCmdRe so
	// the two detectors cannot drift apart.
	rmRootTargetRe = regexp.MustCompile(`^(` + rmRootTargetPattern + `)$`)
)

// rmRootTargetPattern is the set of paths whose recursive removal wipes the
// filesystem root or the home directory: one or more components made only of
// slashes and an optional . or .. (/, //, /., /.., /./, /../), optionally
// followed by *; a bare * or /*; and $HOME, ${HOME} or ~ optionally carrying
// the same suffix. Names such as /.cache, ~/.cache or /... are not matched.
// Both destructiveCmdRe and rmRootTargetRe are built from it.
const rmRootTargetPattern = `(/+\.{0,2})+\*?|/*\*|(\$HOME|\$\{HOME\}|~)((/+\.{0,2})+\*?)?`

// isDestructiveRm reports whether cmd contains a recursive rm whose target is
// the filesystem root or the home directory. Unlike destructiveCmdRe it reads
// the argv the way rm does, so the option tokens may come in any order and
// any spelling: -r/-R or --recursive mark the removal recursive, -- ends the
// options, and every other option (--force, --no-preserve-root, -v, ...) is
// skipped. GNU rm refuses "rm -rf /" by default; "rm -rf --no-preserve-root /"
// is the spelling that actually removes it, and it must not pass clean. The
// command word is matched by its base name, so /bin/rm and /usr/bin/rm are
// read the same way as a bare rm.
func isDestructiveRm(cmd string) bool {
	for _, simple := range shellSeparatorRe.Split(cmd, -1) {
		fields := strings.Fields(simple)
		for i, f := range fields {
			if !isRmCommandWord(f) {
				continue
			}
			if rmRemovesRoot(fields[i+1:]) {
				return true
			}
		}
	}
	return false
}

var shellQuoteReplacer = strings.NewReplacer(`"`, "", `'`, "")

// isRmCommandWord reports whether field names the rm binary, either bare or
// by path (/bin/rm, /usr/bin/rm, ./rm), ignoring shell quotes.
func isRmCommandWord(field string) bool {
	return strings.EqualFold(path.Base(shellQuoteReplacer.Replace(field)), "rm")
}

func rmRemovesRoot(args []string) bool {
	recursive, rootTarget, endOfOptions := false, false, false
	for _, arg := range args {
		if !endOfOptions && len(arg) > 1 && strings.HasPrefix(arg, "-") {
			switch {
			case arg == "--":
				endOfOptions = true
			case strings.HasPrefix(arg, "--"):
				if arg == "--recursive" {
					recursive = true
				}
			default:
				if strings.ContainsAny(arg[1:], "rR") {
					recursive = true
				}
			}
			continue
		}
		if rmRootTargetRe.MatchString(shellQuoteReplacer.Replace(arg)) {
			rootTarget = true
		}
	}
	return recursive && rootTarget
}

// Scan analyzes the tool request for security violations.
func (s *DefaultSecurityScanner) Scan(ctx context.Context, req ToolRequest) (ScanResult, error) {
	var violations []string

	// 1. Text payload & arguments scanning via ioscan
	textsToScan := []string{req.GetContent(), req.GetCommand()}
	for k, v := range req.Arguments {
		if str, ok := v.(string); ok && str != "" && k != "tool" {
			textsToScan = append(textsToScan, str)
		}
	}

	text := s.Text
	if text == nil {
		text = DefaultTextScanner()
	}
	if text == nil {
		return ScanResult{}, ErrNoTextScanner
	}
	for _, t := range textsToScan {
		if t == "" {
			continue
		}
		inVerdict := text.ScanInput(t)
		if inVerdict.Blocked || inVerdict.CriticalInjection {
			for _, f := range inVerdict.Findings {
				if f.High {
					violations = append(violations, fmt.Sprintf("input scan [%s]: %s (rule: %s)", f.Kind, f.Snippet, f.Rule))
				}
			}
		}
		outVerdict := text.ScanOutput(t)
		if outVerdict.Blocked {
			for _, f := range outVerdict.Findings {
				if f.High {
					violations = append(violations, fmt.Sprintf("output scan [%s]: %s (rule: %s)", f.Kind, f.Snippet, f.Rule))
				}
			}
		}
	}

	// 2. Command safety analysis for shell/bash tools
	if IsBash(req.Tool) || req.GetCommand() != "" {
		cmd := req.GetCommand()
		if cmd != "" {
			if destructiveCmdRe.MatchString(cmd) || isDestructiveRm(cmd) {
				violations = append(violations, fmt.Sprintf("destructive shell command detected: %q", cmd))
			}
			if credentialExfilRe.MatchString(cmd) {
				violations = append(violations, fmt.Sprintf("credential exfiltration attempt detected: %q", cmd))
			}
		}
	}

	// 3. Guardrail path protection for local write tools
	if IsLocalWrite(req.Tool) {
		filePath := req.GetFilePath()
		if filePath != "" && s.touchesGuardrailPath(filePath) {
			violations = append(violations, fmt.Sprintf("tampering with guardrail-critical path: %q", filePath))
		}
	}

	if len(violations) > 0 {
		return ScanResult{
			Passed:     false,
			Violations: violations,
			Details:    strings.Join(violations, "; "),
		}, nil
	}

	return ScanResult{
		Passed:  true,
		Details: "clean security scan",
	}, nil
}

func (s *DefaultSecurityScanner) touchesGuardrailPath(path string) bool {
	for _, pattern := range s.guardrailPathPatterns {
		if matchPattern(pattern, path) {
			return true
		}
	}
	return false
}

func matchPattern(pattern, path string) bool {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	path = filepath.ToSlash(strings.TrimPrefix(strings.TrimSpace(path), "./"))

	if ok, _ := filepath.Match(pattern, path); ok {
		return true
	}

	if strings.HasPrefix(pattern, "**/") {
		trimmed := strings.TrimPrefix(pattern, "**/")
		if matchPattern(trimmed, path) {
			return true
		}
		segments := strings.Split(path, "/")
		for i := 1; i < len(segments); i++ {
			sub := strings.Join(segments[i:], "/")
			if matchPattern(trimmed, sub) {
				return true
			}
		}
	}

	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		if path == prefix || strings.HasPrefix(path, prefix+"/") || strings.Contains(path, "/"+prefix+"/") {
			return true
		}
	}

	if ok, _ := filepath.Match(pattern, filepath.Base(path)); ok {
		return true
	}

	return false
}
