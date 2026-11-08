package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/checkout"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/serve"
)

// `crux checkout add|rm|list` is the pinned-checkout work service
// (hivecommons/crux#114 S1). It does not touch crux.Runtime: locally it
// drives checkout.Service on <workdir>/checkouts as the operator; with
// --server it drives the /v1/checkouts routes as whoever the token is.

const checkoutUsage = "usage: crux checkout add -repo URL -commit SHA [-owner O] [-ttl 72h] [-max-bytes N] [-no-branches] | rm ID | list [-owner O]"

// checkoutClient is the one shape the local and remote paths share.
type checkoutClient interface {
	CreateCheckout(ctx context.Context, in api.CheckoutRequest) (api.Checkout, error)
	ListCheckouts(ctx context.Context, owner string) ([]api.Checkout, error)
	DeleteCheckout(ctx context.Context, id string) error
}

// localCheckouts adapts checkout.Service to checkoutClient for the
// in-process path, where the caller is the operator.
type localCheckouts struct{ svc *checkout.Service }

func (l localCheckouts) CreateCheckout(ctx context.Context, in api.CheckoutRequest) (api.Checkout, error) {
	req := checkout.Request{Repo: in.Repo, Commit: in.Commit, Owner: in.Owner, Branches: in.Branches, MaxBytes: in.MaxBytes}
	if req.Owner == "" {
		req.Owner = serve.DefaultOperatorName
	}
	if in.TTL != "" {
		ttl, err := time.ParseDuration(in.TTL)
		if err != nil {
			return api.Checkout{}, fmt.Errorf("%w: ttl: %v", checkout.ErrBadRequest, err)
		}
		req.TTL = ttl
	}
	rec, err := l.svc.Create(ctx, req)
	return checkoutRecordToAPI(rec), err
}

func (l localCheckouts) ListCheckouts(_ context.Context, owner string) ([]api.Checkout, error) {
	recs, err := l.svc.List(owner)
	out := make([]api.Checkout, 0, len(recs))
	for _, rec := range recs {
		out = append(out, checkoutRecordToAPI(rec))
	}
	return out, err
}

func (l localCheckouts) DeleteCheckout(ctx context.Context, id string) error {
	return l.svc.Delete(ctx, id, serve.KindOperator+":"+serve.DefaultOperatorName)
}

func checkoutRecordToAPI(rec checkout.Record) api.Checkout {
	return api.Checkout{ID: rec.ID, Path: rec.Path, Repo: rec.Repo, Commit: rec.Commit, Owner: rec.Owner, Branches: rec.Branches,
		MaxBytes: rec.MaxBytes, Bytes: rec.Bytes, TTL: rec.TTL, CreatedAt: rec.CreatedAt, ExpiresAt: rec.ExpiresAt}
}

// newCheckoutService builds the service from the flag or environment
// allowlist; nil with no error means no allowlist was given, so the
// service stays off. maxBytes is the byte ceiling (zero reads
// CRUX_CHECKOUT_MAX_BYTES, then the package default); the private-repo
// token is read from CRUX_CHECKOUT_TOKEN only. inUse is the contained-run
// service's hook that keeps a checkout with live runs from being deleted;
// nil means never in use.
func newCheckoutService(allow, dir string, maxBytes int64, workDir string, inUse func(id string) bool) (*checkout.Service, error) {
	if allow == "" {
		allow = os.Getenv(config.CheckoutAllowlistEnvVar)
	}
	if strings.TrimSpace(allow) == "" {
		return nil, nil
	}
	dir = checkoutDirFor(dir, workDir)
	if maxBytes == 0 {
		var err error
		if maxBytes, err = checkoutMaxBytesFromEnv(); err != nil {
			return nil, err
		}
	}
	var prefixes []string
	for _, p := range strings.Split(allow, ",") {
		if p = strings.TrimSpace(p); p != "" {
			prefixes = append(prefixes, p)
		}
	}
	return checkout.New(checkout.Config{Dir: dir, AllowedRepos: prefixes, MaxBytes: maxBytes, Token: os.Getenv(config.CheckoutTokenEnvVar), InUse: inUse})
}

// checkoutMaxBytesFromEnv parses CRUX_CHECKOUT_MAX_BYTES; unset is zero,
// which the service turns into its default.
func checkoutMaxBytesFromEnv() (int64, error) {
	raw := strings.TrimSpace(os.Getenv(config.CheckoutMaxBytesEnvVar))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive byte count, got %q", config.CheckoutMaxBytesEnvVar, raw)
	}
	return n, nil
}

// cmdCheckout is the local `crux checkout`: the service on <workdir>.
func (e *Env) cmdCheckout(ctx context.Context, opts globalOptions, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.Stderr, checkoutUsage)
		return ExitUsage
	}
	fs := e.subFlags("checkout " + args[0])
	allow := fs.String("allow", "", "comma-separated repo URL prefixes this command may clone (default $"+config.CheckoutAllowlistEnvVar+")")
	dir := fs.String("dir", "", "where checkouts live (default $"+config.CheckoutDirEnvVar+", then <workdir>/"+checkout.DirName+")")
	ceiling := fs.Int64("max-bytes-ceiling", 0, "largest clone plus worktree this command may deliver (default $"+config.CheckoutMaxBytesEnvVar+", then 5 GiB)")
	return e.checkout(ctx, func() (checkoutClient, error) {
		svc, err := newCheckoutService(*allow, *dir, *ceiling, opts.workDir, nil)
		if err == nil && svc == nil {
			// No allowlist: a service that refuses every clone but can
			// still list and remove what exists.
			svc, err = checkout.New(checkout.Config{Dir: checkoutDirFor(*dir, opts.workDir)})
		}
		if err != nil {
			return nil, err
		}
		return localCheckouts{svc}, nil
	}, fs, args)
}

func checkoutDirFor(dir, workDir string) string {
	if dir == "" {
		dir = os.Getenv(config.CheckoutDirEnvVar)
	}
	if dir == "" {
		dir = checkout.DefaultDir(workDir)
	}
	return dir
}

// remoteCheckout is `crux --server … checkout`.
func (e *Env) remoteCheckout(ctx context.Context, c *api.Client, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.Stderr, checkoutUsage)
		return ExitUsage
	}
	return e.checkout(ctx, func() (checkoutClient, error) { return c, nil }, e.subFlags("checkout "+args[0]), args)
}

// checkout dispatches the add/rm/list verb. fs may already carry the
// local-only flags; the verb's own flags are added here, and newClient runs
// after parsing so those flags are in effect.
func (e *Env) checkout(ctx context.Context, newClient func() (checkoutClient, error), fs *flag.FlagSet, args []string) int {
	verb, rest := args[0], args[1:]
	client := func() (checkoutClient, bool) {
		c, err := newClient()
		if err != nil {
			fmt.Fprintln(e.Stderr, "crux: checkout:", err)
		}
		return c, err == nil
	}
	switch verb {
	case "add":
		var in api.CheckoutRequest
		var noBranches bool
		fs.StringVar(&in.Repo, "repo", "", "repository URL (must match the server's allowlist)")
		fs.StringVar(&in.Commit, "commit", "", "full 40-hex commit SHA to pin")
		fs.StringVar(&in.Owner, "owner", "", "owning run or driver (default: the caller)")
		fs.StringVar(&in.TTL, "ttl", "", "duration after which a sweeper may reclaim it, e.g. 72h")
		fs.Int64Var(&in.MaxBytes, "max-bytes", 0, "size cap for clone plus worktree (default 5 GiB); checked after cloning")
		fs.BoolVar(&noBranches, "no-branches", false, "clone a single branch instead of all refs")
		asJSON := fs.Bool("json", false, "emit the record as JSON")
		if code, ok := parseSub(fs, rest); !ok {
			return code
		}
		if in.Repo == "" || in.Commit == "" || fs.NArg() != 0 {
			fmt.Fprintln(e.Stderr, checkoutUsage)
			return ExitUsage
		}
		if noBranches {
			f := false
			in.Branches = &f
		}
		c, ok := client()
		if !ok {
			return ExitUsage
		}
		rec, err := c.CreateCheckout(ctx, in)
		if err != nil {
			return e.checkoutErr("add", err)
		}
		if *asJSON {
			return e.writeJSON(rec)
		}
		fmt.Fprintf(e.Stdout, "%s\t%s\n", rec.ID, rec.Path)
		return ExitOK
	case "rm":
		if code, ok := parseSub(fs, rest); !ok {
			return code
		}
		if fs.NArg() != 1 {
			fmt.Fprintln(e.Stderr, checkoutUsage)
			return ExitUsage
		}
		c, ok := client()
		if !ok {
			return ExitUsage
		}
		if err := c.DeleteCheckout(ctx, fs.Arg(0)); err != nil {
			return e.checkoutErr("rm", err)
		}
		return ExitOK
	case "list":
		owner := fs.String("owner", "", "only this owner's checkouts")
		asJSON := fs.Bool("json", false, "emit a JSON array instead of a table")
		if code, ok := parseSub(fs, rest); !ok {
			return code
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(e.Stderr, checkoutUsage)
			return ExitUsage
		}
		c, ok := client()
		if !ok {
			return ExitUsage
		}
		recs, err := c.ListCheckouts(ctx, *owner)
		if err != nil {
			return e.checkoutErr("list", err)
		}
		if *asJSON {
			return e.writeJSON(recs)
		}
		tw := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tOWNER\tCOMMIT\tCREATED\tEXPIRES\tPATH")
		for _, r := range recs {
			expires := "-"
			if r.ExpiresAt != nil {
				expires = r.ExpiresAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Owner, r.Commit, r.CreatedAt.UTC().Format(time.RFC3339), expires, r.Path)
		}
		if err := tw.Flush(); err != nil {
			fmt.Fprintln(e.Stderr, "crux:", err)
			return ExitError
		}
		return ExitOK
	default:
		fmt.Fprintf(e.Stderr, "crux: checkout: unknown verb %q\n%s\n", verb, checkoutUsage)
		return ExitUsage
	}
}

// checkoutErr prints a failure; malformed requests and refusals are usage
// errors, everything else a runtime failure.
func (e *Env) checkoutErr(verb string, err error) int {
	fmt.Fprintf(e.Stderr, "crux: checkout %s: %v\n", verb, err)
	var se *api.StatusError
	if errors.Is(err, checkout.ErrBadRequest) || errors.Is(err, checkout.ErrForbidden) ||
		(errors.As(err, &se) && (se.Body.Code == api.CodeBadRequest || se.Body.Code == api.CodeForbidden)) {
		return ExitUsage
	}
	if api.IsUnauthorized(err) {
		fmt.Fprintf(e.Stderr, "crux: the server wants a bearer token; pass --token or set %s\n", config.ServeTokenEnvVar)
	}
	return ExitError
}
