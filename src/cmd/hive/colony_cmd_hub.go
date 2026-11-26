package main

import (
	"encoding/json"
	"io"

	"github.com/hivecommons/hive/pkg/hub"
)

// colonyExportFormatHub is the hub's user records with their CRM fields and
// its consent files (HIVE_HUB_CUSTOMER_STORE=colony-only, #19972).
const colonyExportFormatHub = "hive-hub-json"

// hubDataDir is the hub's SaaS data directory, holding colony.db, users/
// and consents/.
const hubDataDir = "/data/saas"

func init() {
	colonyExportFormats[colonyExportFormatHub] = colonyExportFormat{defaultDir: hubDataDir, export: exportColonyHub}
}

// exportColonyHub writes <out>/users and <out>/consents (out defaults to the
// data directory itself) and prints a counts-only report.
func exportColonyHub(dir, out string, force bool, stdout io.Writer) error {
	if out == "" {
		out = dir
	}
	rep, err := hub.ExportHubCustomerStore(dir, out, force)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}
