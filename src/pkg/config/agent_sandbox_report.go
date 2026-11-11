package config

import "sync"

// SandboxGateReport is the result of AgentSandboxGateReport. OpenShell and
// Substrate are the preflight findings for agents on that runtime; both are
// also included in Warnings.
type SandboxGateReport struct {
	Warnings  []string
	OpenShell []string
	Substrate []string
}

var (
	sandboxGateReportMu sync.RWMutex
	sandboxGateReport   SandboxGateReport
)

// StoreSandboxGateReport records the report computed at boot/reload so
// readers such as the status builder never run a preflight themselves.
func StoreSandboxGateReport(r SandboxGateReport) {
	sandboxGateReportMu.Lock()
	defer sandboxGateReportMu.Unlock()
	sandboxGateReport = r
}

// LastSandboxGateReport returns the report last stored by StoreSandboxGateReport.
func LastSandboxGateReport() SandboxGateReport {
	sandboxGateReportMu.RLock()
	defer sandboxGateReportMu.RUnlock()
	return sandboxGateReport
}

// SandboxPreflightWarningsFor returns the stored preflight warnings that apply
// to an agent on the given sandbox runtime.
func SandboxPreflightWarningsFor(runtime string) []string {
	r := LastSandboxGateReport()
	switch runtime {
	case SandboxRuntimeOpenShell:
		return r.OpenShell
	case SandboxRuntimeSubstrate:
		return r.Substrate
	}
	return nil
}
