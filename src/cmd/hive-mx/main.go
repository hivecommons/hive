// Command hive-mx is hive's receive-only mail receiver: it accepts mail for
// hive@<hive host>, keeps only calendar invites, mail-in files and text
// bodies, and proof codes, and posts them to the hub that routes them to
// each hive. Messages are spooled to disk (HIVE_MX_SPOOL_DIR), never held
// in memory whole. It never sends mail. Configuration is environment only; see pkg/mailin and
// src/docs/meetings.md ("Inviting Betty by email").
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata" // the static image has no zoneinfo; invites need IANA zones

	"github.com/hivecommons/hive/pkg/mailin"
)

// run is replaced in tests.
var run = mailin.Run

func main() { os.Exit(runMain(os.Args[1:], os.Getenv, os.Stderr)) }

func runMain(args []string, getenv func(string) string, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "-version" || args[0] == "--version") {
		fmt.Fprintln(stderr, "hive-mx 1")
		return 0
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	cfg, err := mailin.ConfigFromEnv(getenv)
	if err != nil {
		log.Error("hive-mx: bad configuration", "err", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, cfg, log); err != nil {
		log.Error("hive-mx: stopped", "err", err)
		return 1
	}
	return 0
}
