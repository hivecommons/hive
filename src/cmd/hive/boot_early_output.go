package main

import (
	"io"
	"log/slog"

	"github.com/hivecommons/hive/crux/logread"
)

// teeEarlyOutput keeps what hive prints before its configured log is set up
// as the hive-output log source (#11944), while still writing it to stdout
// exactly as before. When the source cannot be opened stdout is returned
// unchanged and the reason goes to stdout.
func teeEarlyOutput(stdout io.Writer, getenv func(string) string) io.Writer {
	output, err := logread.OpenOutputLog(logread.OutputDir(getenv), logread.SourceHiveOutput)
	if err != nil {
		slog.New(slog.NewJSONHandler(stdout, nil)).Warn("early hive output is not kept", "error", err)
		return stdout
	}
	return io.MultiWriter(stdout, output.Stream(logread.StreamStdout))
}
