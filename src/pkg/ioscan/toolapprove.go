package ioscan

import "github.com/hivecommons/hive/pkg/toolapprove"

// ToolApproveScanner adapts this package's input/output scanning to
// toolapprove.TextScanner, the payload-scanner seam the tool-approval desk
// runs over tool content, commands and arguments. Hosts install it with
// toolapprove.SetDefaultTextScanner (cmd/hive and pkg/turn do at init); the
// desk itself does not import this package, which keeps pkg/toolapprove
// inside the Crux ring (hivecommons/hive#13536).
type ToolApproveScanner struct{}

var _ toolapprove.TextScanner = ToolApproveScanner{}

func (ToolApproveScanner) ScanInput(text string) toolapprove.TextVerdict {
	return toolApproveVerdict(ScanInput(text))
}

func (ToolApproveScanner) ScanOutput(text string) toolapprove.TextVerdict {
	return toolApproveVerdict(ScanOutput(text))
}

func toolApproveVerdict(v Verdict) toolapprove.TextVerdict {
	out := toolapprove.TextVerdict{Blocked: v.Blocked, CriticalInjection: v.HasCriticalInjection()}
	for _, f := range v.Findings {
		out.Findings = append(out.Findings, toolapprove.TextFinding{
			Kind:    string(f.Kind),
			Snippet: f.Snippet,
			Rule:    f.Rule,
			High:    f.Severity >= SeverityHigh,
		})
	}
	return out
}
