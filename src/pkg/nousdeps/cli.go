package nousdeps

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Subcommand is the `hive` subcommand bin/nous-runner.sh invokes before the
// engine starts.
const Subcommand = "nous-setup"

// Exit codes. Refusals and verification failures share one code because
// the runner treats them alike: the campaign does not start.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

const cliUsage = `usage: hive nous-setup --campaign <campaign.yaml> [--run-id <id>] [--root <dir>]
                       [--cache-dir <dir>] [--allowlist host[,host...]] [--timeout <duration>] [--json]

Fetch the campaign's pinned dependencies (repos by commit, files by sha256)
from allowlisted hosts only, verify every pin, and record the resolved
versions plus toolchain versions in <root>/<run-id>/dependencies.json and
dependencies.env. Any dependency outside the allowlist, missing a full pin,
or failing verification stops the campaign before it starts (exit 1).

The allowlist comes from --allowlist, else ` + config.NousFetchAllowlistEnv + `
(exported by the hive from nous.fetch_allowlist), else the default
"github.com". The root comes from --root, else HIVE_NOUS_DATA_DIR,
else /data/nous.
`

// Run implements `hive nous-setup …`. It returns the process exit code: 0
// when the record was written, 1 on a refused or failed fetch, 2 on usage
// errors.
func Run(args []string, stdout, stderr io.Writer) int {
	return RunWith(context.Background(), args, stdout, stderr, Options{})
}

// RunWith is Run with a parent context and base Options; tests inject the
// Runner, HTTP client, clock and environment through base.
func RunWith(ctx context.Context, args []string, stdout, stderr io.Writer, base Options) int {
	fs := flag.NewFlagSet(Subcommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, cliUsage) }
	var (
		campaign  = fs.String("campaign", "", "campaign definition to read dependencies from")
		runID     = fs.String("run-id", "", "campaign run id (default: governor)")
		root      = fs.String("root", "", "durable Nous root (default: HIVE_NOUS_DATA_DIR or /data/nous)")
		cacheDir  = fs.String("cache-dir", "", "dependency cache (default: HIVE_AGENT_CACHE_DIR/deps or <root>/cache/deps)")
		allowlist = fs.String("allowlist", "", "comma-separated hosts (default: "+config.NousFetchAllowlistEnv+")")
		timeout   = fs.Duration("timeout", DefaultFetchTimeout, "overall setup timeout")
		asJSON    = fs.Bool("json", false, "print the dependency record as JSON")
	)
	if err := fs.Parse(args); err != nil {
		// flag already printed the problem (or the usage for -h).
		return exitUsage
	}
	if strings.TrimSpace(*campaign) == "" {
		fmt.Fprintln(stderr, "nous-setup: --campaign is required")
		fs.Usage()
		return exitUsage
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "nous-setup: --timeout must be positive")
		return exitUsage
	}
	opts := base
	opts.CampaignPath = *campaign
	if *runID != "" {
		opts.RunID = *runID
	}
	if *root != "" {
		opts.Root = *root
	}
	if *cacheDir != "" {
		opts.CacheDir = *cacheDir
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if *allowlist != "" {
		opts.Allowlist = NewAllowlist(strings.Split(*allowlist, config.NousFetchAllowlistSeparator))
	} else if opts.Allowlist == nil {
		opts.Allowlist = AllowlistFromEnv(getenv)
	}
	if opts.Logf == nil {
		opts.Logf = func(format string, a ...any) { fmt.Fprintf(stderr, "[nous-setup] "+format+"\n", a...) }
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	opts.Logf("allowlist: %s", opts.Allowlist.describe())
	rec, err := Setup(ctx, opts)
	if err != nil {
		fmt.Fprintf(stderr, "nous-setup: refusing to start campaign: %v\n", err)
		return exitFail
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rec); err != nil {
			fmt.Fprintf(stderr, "nous-setup: %v\n", err)
			return exitFail
		}
		return exitOK
	}
	fmt.Fprintf(stdout, "nous-setup: %d dependencies verified; recorded in %s\n", len(rec.Dependencies), rec.RecordPath)
	return exitOK
}
