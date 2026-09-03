package toolapprove

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hivecommons/hive/pkg/ioscan"
)

// SecurityScanner provides pre-execution security analysis of tool requests.
type SecurityScanner interface {
	Scan(ctx context.Context, req ToolRequest) (ScanResult, error)
}

// DefaultSecurityScanner evaluates tool requests against prompt injections,
// destructive directives, secret leaks, and guardrail path modifications.
type DefaultSecurityScanner struct {
	guardrailPathPatterns []string
}

// NewDefaultSecurityScanner returns a configured DefaultSecurityScanner.
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
	destructiveCmdRe  = regexp.MustCompile(`(?i)\b(rm\s+-[rRfF]+\s+(/|\*|/\*|\$HOME|~)|mkfs\b|dd\s+if=/dev/zero|:\(\)\{\s*:\|:&\s*\};:)\b`)
	credentialExfilRe = regexp.MustCompile(`(?i)\b(cat\s+~/\.ssh/|cat\s+/var/run/secrets/|printenv\s+.*TOKEN|echo\s+\$GH_TOKEN|echo\s+\$GITHUB_TOKEN)\b`)
)

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

	for _, text := range textsToScan {
		if text == "" {
			continue
		}
		inVerdict := ioscan.ScanInput(text)
		if inVerdict.Blocked || inVerdict.HasCriticalInjection() {
			for _, f := range inVerdict.Findings {
				if f.Severity >= ioscan.SeverityHigh {
					violations = append(violations, fmt.Sprintf("input scan [%s]: %s (rule: %s)", f.Kind, f.Snippet, f.Rule))
				}
			}
		}
		outVerdict := ioscan.ScanOutput(text)
		if outVerdict.Blocked {
			for _, f := range outVerdict.Findings {
				if f.Severity >= ioscan.SeverityHigh {
					violations = append(violations, fmt.Sprintf("output scan [%s]: %s (rule: %s)", f.Kind, f.Snippet, f.Rule))
				}
			}
		}
	}

	// 2. Command safety analysis for shell/bash tools
	if IsBash(req.Tool) || req.GetCommand() != "" {
		cmd := req.GetCommand()
		if cmd != "" {
			if destructiveCmdRe.MatchString(cmd) {
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
