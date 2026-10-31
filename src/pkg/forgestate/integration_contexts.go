package forgestate

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

type IntegrationContextReceipt struct {
	JobID      int64  `json:"jobID"`
	CheckID    int64  `json:"checkID"`
	AppID      int64  `json:"appID"`
	SuiteID    int64  `json:"suiteID"`
	Conclusion string `json:"conclusion"`
}

func IntegrationRequiredContexts() []string {
	return []string{"gate", "DCO", "build-and-test", "test", "coverage", "golangci-lint",
		"gosec (insecure patterns)", "govulncheck (known vulnerabilities)", "changelog-fragment-guard",
		"verify PR contents", "Sign-off survives the squash"}
}

func IntegrationContextsPassed(contexts map[string]IntegrationContextReceipt, metadata string) bool {
	raw, err := hex.DecodeString(metadata)
	if err != nil || len(raw) != sha256.Size || metadata != strings.ToLower(metadata) || strings.Trim(metadata, "0") == "" ||
		len(contexts) != len(IntegrationRequiredContexts()) {
		return false
	}
	jobs, checks := map[int64]bool{}, map[int64]bool{}
	var appID, suiteID int64
	for _, name := range IntegrationRequiredContexts() {
		record, exists := contexts[name]
		if !exists || record.JobID <= 0 || record.CheckID <= 0 || record.AppID <= 0 || record.SuiteID <= 0 ||
			record.Conclusion != "success" || jobs[record.JobID] || checks[record.CheckID] {
			return false
		}
		if appID != 0 && (record.AppID != appID || record.SuiteID != suiteID) {
			return false
		}
		appID, suiteID = record.AppID, record.SuiteID
		jobs[record.JobID], checks[record.CheckID] = true, true
	}
	return true
}
