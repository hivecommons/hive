// Command pricetable keeps pkg/tokens/pricing.go's hand-maintained model
// price table reconciled with a machine-readable list-price source.
//
// It reads the current table straight out of the Go source, fetches
// OpenRouter's public model catalogue (https://openrouter.ai/api/v1/models,
// no key required), maps each catalogue id onto the table's normalised keys
// with the exact tokens.NormalizeModelID the dashboard uses for lookups,
// rewrites only the literals whose value moved, bumps priceTableDate, and
// writes a markdown report of every change plus every table row OpenRouter
// could not vouch for. Rows OpenRouter does not list keep their current value.
//
// Usage (from src/):
//
//	go run ./internal/tools/pricetable                      # refresh in place
//	go run ./internal/tools/pricetable --dry-run            # report only
//	go run ./internal/tools/pricetable --check-age 60       # CI staleness guard
//
// The refresh is driven by .github/workflows/price-table-refresh.yml, which
// turns a non-empty diff into a PR for a human to review; see
// src/docs/token-tracking.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// defaultSource is OpenRouter's public model catalogue.
	defaultSource = "https://openrouter.ai/api/v1/models"
	// defaultPricingPath is pkg/tokens/pricing.go relative to the module root.
	defaultPricingPath = "pkg/tokens/pricing.go"
	// defaultStaleAfterDays is how old an otherwise-unchanged table may get
	// before a refresh bumps priceTableDate anyway, so the date keeps meaning
	// "last reconciled" rather than "last moved" and the --check-age guard
	// stays satisfiable through long quiet spells.
	defaultStaleAfterDays = 30
	// fetchTimeout bounds the catalogue download.
	fetchTimeout = 60 * time.Second
	// dateLayout is the priceTableDate format.
	dateLayout = "2006-01-02"
	// hoursPerDay converts durations to whole days.
	hoursPerDay = 24
)

// options are the parsed command-line flags plus the seams tests inject.
type options struct {
	source     string
	pricing    string
	report     string
	date       string
	dryRun     bool
	checkAge   int
	staleAfter int
	stdout     io.Writer
	stderr     io.Writer
	now        func() time.Time
	fetch      func(ctx context.Context, source string) ([]byte, error)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, time.Now))
}

// run parses args and executes; it returns the process exit code.
func run(args []string, stdout, stderr io.Writer, now func() time.Time) int {
	opts := options{stdout: stdout, stderr: stderr, now: now, fetch: fetchSource}
	fs := flag.NewFlagSet("pricetable", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.source, "source", defaultSource, "OpenRouter models endpoint URL, or a local JSON file path")
	fs.StringVar(&opts.pricing, "pricing", defaultPricingPath, "path to pkg/tokens/pricing.go")
	fs.StringVar(&opts.report, "report", "", "write the markdown report to this file (default: stdout only)")
	fs.StringVar(&opts.date, "date", "", "override today's date (YYYY-MM-DD) used for priceTableDate")
	fs.BoolVar(&opts.dryRun, "dry-run", false, "compute and report, but do not rewrite pricing.go")
	fs.IntVar(&opts.checkAge, "check-age", 0, "exit non-zero when priceTableDate is older than this many days (no fetch, no rewrite)")
	fs.IntVar(&opts.staleAfter, "stale-after", defaultStaleAfterDays, "bump priceTableDate even without price changes once it is older than this many days")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := execute(context.Background(), opts); err != nil {
		fmt.Fprintln(stderr, "pricetable:", err)
		return 1
	}
	return 0
}

// execute runs either the --check-age guard or a refresh.
func execute(ctx context.Context, opts options) error {
	src, err := os.ReadFile(opts.pricing)
	if err != nil {
		return fmt.Errorf("read pricing table: %w", err)
	}
	table, err := parseTable(src)
	if err != nil {
		return fmt.Errorf("parse %s: %w", opts.pricing, err)
	}
	if opts.checkAge > 0 {
		return checkAge(table.Date, opts.now(), opts.checkAge, opts.stdout)
	}

	today := opts.now().UTC().Format(dateLayout)
	if opts.date != "" {
		if _, err := time.Parse(dateLayout, opts.date); err != nil {
			return fmt.Errorf("--date: %w", err)
		}
		today = opts.date
	}

	raw, err := opts.fetch(ctx, opts.source)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", opts.source, err)
	}
	catalogue, err := parseOpenRouter(raw)
	if err != nil {
		return fmt.Errorf("parse catalogue: %w", err)
	}

	result := reconcile(table, catalogue, today, opts.staleAfter)
	report := renderReport(result, opts.source)
	if opts.report != "" {
		if err := os.WriteFile(opts.report, []byte(report), 0o644); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
	}
	fmt.Fprint(opts.stdout, report)

	if !result.Changed() {
		fmt.Fprintln(opts.stdout, "pricetable: no changes")
		return nil
	}
	out, err := applyChanges(src, table, result)
	if err != nil {
		return fmt.Errorf("rewrite table: %w", err)
	}
	if opts.dryRun {
		fmt.Fprintf(opts.stdout, "pricetable: dry run, %d price change(s) not written\n", len(result.Changes))
		return nil
	}
	if err := os.WriteFile(opts.pricing, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", opts.pricing, err)
	}
	fmt.Fprintf(opts.stdout, "pricetable: wrote %d price change(s) to %s (priceTableDate %s -> %s)\n",
		len(result.Changes), opts.pricing, table.Date, result.NewDate)
	return nil
}

// checkAge fails when the table date is more than maxDays old at now.
func checkAge(date string, now time.Time, maxDays int, stdout io.Writer) error {
	parsed, err := time.Parse(dateLayout, date)
	if err != nil {
		return fmt.Errorf("priceTableDate %q: %w", date, err)
	}
	age := int(now.UTC().Sub(parsed).Hours() / hoursPerDay)
	if age > maxDays {
		return fmt.Errorf("price table is %d days old (priceTableDate %s, limit %d days); run the price-table-refresh workflow or `go run ./internal/tools/pricetable`", age, date, maxDays)
	}
	fmt.Fprintf(stdout, "pricetable: table dated %s is %d day(s) old (limit %d)\n", date, age, maxDays)
	return nil
}

// fetchSource reads the catalogue from a URL or, when source is not an
// http(s) URL, from a local file so fixtures and offline runs work the same.
func fetchSource(ctx context.Context, source string) ([]byte, error) {
	if !strings.HasPrefix(source, "http://") && !strings.HasPrefix(source, "https://") {
		return os.ReadFile(source)
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "hive-pricetable (+https://github.com/hivecommons/hive)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}
