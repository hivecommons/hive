package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/hivecommons/hive/crux/sandbox/substratebridge"
)

// JobBridgeCommand is the subcommand a Job on the copy workspace transport
// runs as its container command (hivecommons/hive#19621):
//
//	crux job-bridge [-listen :8477] [-workspace /workspace] [-token-env VAR] [-wait 10m] -- CMD [ARGS...]
//
// It needs no runtime, work dir or definitions, so Env.Run routes it before
// any of those are resolved.
const JobBridgeCommand = "job-bridge"

// JobBridgeTokenEnv is the default variable the bearer token is read from.
const JobBridgeTokenEnv = "HIVE_JOB_BRIDGE_TOKEN" // #nosec G101 -- env var name, not a credential

func (e *Env) cmdJobBridge(ctx context.Context, args []string) int {
	fs := e.subFlags(JobBridgeCommand)
	listen := fs.String("listen", ":"+strconv.Itoa(substratebridge.DefaultPort), "address to serve the bridge on")
	workspace := fs.String("workspace", substratebridge.DefaultWorkspace, "directory the workspace is copied into and out of")
	tokenEnv := fs.String("token-env", JobBridgeTokenEnv, "environment variable holding the bearer token hive must send; removed from CMD's environment")
	wait := fs.Duration("wait", substratebridge.DefaultKickWait, "how long to wait for hive to push the workspace, and to collect it after CMD exits")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(e.Stderr, "usage: crux job-bridge [-listen ADDR] [-workspace DIR] [-token-env VAR] [-wait DURATION] -- CMD [ARGS...]")
		return ExitUsage
	}
	token := os.Getenv(*tokenEnv)
	if token == "" {
		fmt.Fprintf(e.Stderr, "crux: job-bridge: $%s is empty; refusing to serve without a token\n", *tokenEnv)
		return ExitUsage
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(e.Stderr, "crux: job-bridge:", err)
		return ExitError
	}
	k := &substratebridge.KickServer{
		Workspace: *workspace,
		Command:   fs.Args(),
		Token:     token,
		TokenEnv:  *tokenEnv,
		Wait:      *wait,
		Stdout:    e.Stdout,
		Stderr:    e.Stderr,
	}
	return k.Serve(ctx, ln)
}
