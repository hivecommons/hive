package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestColonyExportHubFormatRegistered(t *testing.T) {
	f, ok := colonyExportFormats[colonyExportFormatHub]
	if !ok || f.defaultDir != hubDataDir {
		t.Fatalf("hive-hub-json format = %+v, %v", f, ok)
	}
	var out, errOut bytes.Buffer
	// A directory with no colony.db fails cleanly, never writes.
	if code := runColony([]string{"export", "--format", colonyExportFormatHub, "--dir", t.TempDir()}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "colony.db") {
		t.Errorf("stderr = %q, want it to name colony.db", errOut.String())
	}
	if code := runColony([]string{"export", "--format", "bogus"}, &out, &errOut); code != 2 {
		t.Errorf("unknown format exit = %d, want 2", code)
	}
}
