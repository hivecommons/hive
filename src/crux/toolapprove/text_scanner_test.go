package toolapprove

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeTextScanner stands in for pkg/ioscan in this package's tests (the
// desk does not import ioscan). It flags the two shapes the scanner tests
// rely on: an "ignore all previous instructions" injection, a github_token
// secret and an "rm -rf" dangerous directive.
type fakeTextScanner struct{}

func (fakeTextScanner) ScanInput(text string) TextVerdict {
	lower := strings.ToLower(text)
	var v TextVerdict
	if strings.Contains(lower, "ignore all previous instructions") {
		v.Blocked, v.CriticalInjection = true, true
		v.Findings = append(v.Findings, TextFinding{Kind: "injection", Snippet: "ignore all previous instructions", Rule: "fake-injection", High: true})
	}
	if strings.Contains(lower, "github_token") {
		v.Blocked = true
		v.Findings = append(v.Findings, TextFinding{Kind: "secret", Snippet: "github_token: ***", Rule: "fake-secret", High: true})
	}
	if strings.Contains(lower, "rm -rf") {
		v.Blocked = true
		v.Findings = append(v.Findings, TextFinding{Kind: "dangerous", Snippet: "rm -rf", Rule: "fake-dangerous", High: true})
	}
	return v
}

func (f fakeTextScanner) ScanOutput(text string) TextVerdict { return f.ScanInput(text) }

func init() { SetDefaultTextScanner(fakeTextScanner{}) }

func TestDefaultSecurityScannerFailsClosedWithoutTextScanner(t *testing.T) {
	SetDefaultTextScanner(nil)
	t.Cleanup(func() { SetDefaultTextScanner(fakeTextScanner{}) })
	if DefaultTextScanner() != nil {
		t.Fatal("nil install must uninstall")
	}
	_, err := NewDefaultSecurityScanner().Scan(context.Background(), ToolRequest{Tool: "Write", Target: "x", Content: "hello"})
	if !errors.Is(err, ErrNoTextScanner) {
		t.Fatalf("err = %v, want ErrNoTextScanner", err)
	}
	// An explicit per-scanner TextScanner wins over the (missing) default.
	res, err := (&DefaultSecurityScanner{Text: fakeTextScanner{}}).Scan(context.Background(), ToolRequest{Tool: "Write", Target: "x", Content: "hello"})
	if err != nil || !res.Passed {
		t.Fatalf("explicit text scanner: res=%+v err=%v", res, err)
	}
}

func TestDefaultSecurityScannerOnlyHighFindingsBecomeViolations(t *testing.T) {
	low := textScannerFunc(func(string) TextVerdict {
		return TextVerdict{Blocked: true, Findings: []TextFinding{{Kind: "secret", Snippet: "x", Rule: "low", High: false}}}
	})
	res, err := (&DefaultSecurityScanner{Text: low}).Scan(context.Background(), ToolRequest{Tool: "Write", Target: "x", Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 0 || !res.Passed {
		t.Fatalf("low-severity finding became a violation: %+v", res)
	}
}

type textScannerFunc func(string) TextVerdict

func (f textScannerFunc) ScanInput(text string) TextVerdict  { return f(text) }
func (f textScannerFunc) ScanOutput(text string) TextVerdict { return f(text) }
