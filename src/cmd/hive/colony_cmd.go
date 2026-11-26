package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/customers"
)

// colonySubcommand holds the offline colony store tools (#19972):
//
//	hive colony export --format hive-people-json [--dir DIR] [--out DIR] [--force]
//
// export writes the JSON files a colony-only store replaced back out, in
// the shape the json mode reads, for a manual rollback. Stop the hive
// first: a running hive holds colony.db's lock. An existing file is never
// overwritten without --force. Exit status 0 on success, 1 on failure, 2 on
// a usage error.
const colonySubcommand = "colony"

// colonyExportFormatPeople is the spoke's people.json and enrichment.json.
const colonyExportFormatPeople = "hive-people-json"

// colonyExportFormat is one export format: its default data directory and
// the export itself, which prints a counts-only report.
type colonyExportFormat struct {
	defaultDir string
	export     func(dir, out string, force bool, stdout io.Writer) error
}

// colonyExportFormats are the formats colony export knows.
var colonyExportFormats = map[string]colonyExportFormat{
	colonyExportFormatPeople: {defaultDir: customersDataDir, export: exportColonyPeople},
}

func exportColonyPeople(dir, out string, force bool, stdout io.Writer) error {
	rep, err := customers.ExportColony(dir, out, force)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

func runColony(args []string, stdout, stderr io.Writer) int {
	usage := func() {
		fmt.Fprintln(stderr, "usage: hive colony export --format "+strings.Join(colonyFormatNames(), "|")+" [--dir DIR] [--out DIR] [--force]")
	}
	if len(args) == 0 || args[0] != "export" {
		usage()
		return 2
	}
	fs := flag.NewFlagSet(colonySubcommand+" export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "", "what to write: "+strings.Join(colonyFormatNames(), ", "))
	dir := fs.String("dir", "", "the data directory holding colony.db (default: the format's)")
	out := fs.String("out", "", "where to write the files (default: --dir)")
	force := fs.Bool("force", false, "overwrite files that exist")
	fs.Usage = func() { usage(); fs.PrintDefaults() }
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	f, ok := colonyExportFormats[*format]
	if !ok || fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	d := *dir
	if d == "" {
		d = f.defaultDir
	}
	if err := f.export(d, *out, *force, stdout); err != nil {
		fmt.Fprintln(stderr, "hive colony export:", err)
		return 1
	}
	return 0
}

func colonyFormatNames() []string {
	names := make([]string, 0, len(colonyExportFormats))
	for n := range colonyExportFormats {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
