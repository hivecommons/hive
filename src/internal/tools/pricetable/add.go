package main

import (
	"context"
	"fmt"
	"go/format"
	"math/big"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/tokens"
)

// addition is a modelPrices row --add proposes for a model the table lacks.
type addition struct {
	Key      string
	SourceID string
	// Sibling is the existing row of the same model line the quote was
	// compared against; empty when the table has none.
	Sibling string
	Fields  map[string]*big.Rat
}

// addResult is the outcome of one --add run.
type addResult struct {
	Added []addition
	// Present ids already have a row; --add never reprices (the weekly
	// reconcile does that).
	Present []string
	// Missing ids have no usable quote in the source.
	Missing []unverified
	// Held ids have a quote that tripped a plausibility rule.
	Held []suspect
}

// versionedKey splits a normalised key that ends in a dashed version into the
// model line and the version numbers: claude-fable-5-1 -> (claude-fable, [5 1]).
var versionedKey = regexp.MustCompile(`^(.*?[a-z])-(\d+(?:-\d+)*)$`)

// planAdditions decides, for each requested id, whether a new row can be
// added from the catalogue. A quote must pass the same plausibility rules a
// price move must pass: the move rules against the newest existing row of the
// same model line (a new model priced more than 5× away from its predecessor
// looks like a catalogue defect), and the ratio rules on its own.
func planAdditions(t *table, c *catalogue, ids []string) *addResult {
	r := &addResult{}
	seen := map[string]bool{}
	for _, id := range ids {
		key := tokens.NormalizeModelID(id)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if _, ok := t.entry(key); ok {
			r.Present = append(r.Present, key)
			continue
		}
		if dup, ok := c.Ambiguous[key]; ok {
			r.Missing = append(r.Missing, unverified{key, "several catalogue ids normalise to this key: " + strings.Join(dup, ", ")})
			continue
		}
		q, ok := c.Quotes[key]
		if !ok {
			r.Missing = append(r.Missing, unverified{key, "not listed by the source; add the row by hand from the vendor's price page"})
			continue
		}
		fields, rules := proposeRow(q)
		sib, hasSib := siblingEntry(t, key)
		p := make(map[string]fieldChange, len(priceFields))
		for _, name := range priceFields {
			old := fields[name]
			if hasSib {
				old = sib.Fields[name].Value
			}
			p[name] = fieldChange{Old: old, New: fields[name]}
		}
		if hasSib {
			ch := change{Key: key, SourceID: q.SourceID, Fields: map[string]fieldChange{}}
			for name, fc := range p {
				if fc.Old.Cmp(fc.New) != 0 {
					ch.Fields[name] = fc
				}
			}
			rules = append(rules, moveRules(ch)...)
		}
		rules = append(rules, shapeRules(p)...)
		if len(rules) > 0 {
			r.Held = append(r.Held, suspect{Key: key, SourceID: q.SourceID, Fields: p, Rules: rules})
			continue
		}
		a := addition{Key: key, SourceID: q.SourceID, Fields: fields}
		if hasSib {
			a.Sibling = sib.Key
		}
		r.Added = append(r.Added, a)
	}
	sort.Slice(r.Added, func(i, j int) bool { return r.Added[i].Key < r.Added[j].Key })
	sort.Slice(r.Held, func(i, j int) bool { return r.Held[i].Key < r.Held[j].Key })
	sort.Slice(r.Missing, func(i, j int) bool { return r.Missing[i].Key < r.Missing[j].Key })
	sort.Strings(r.Present)
	return r
}

// proposeRow turns a quote into the four table fields. Input and output come
// from the source. Cache read must be published: a new row has no current
// value to keep, so a missing rate is a rule, not a guess. Cache write
// follows the table's documented convention (input rate) unless the source
// publishes a write premium.
func proposeRow(q quote) (map[string]*big.Rat, []string) {
	var rules []string
	if q.Input.Sign() <= 0 || q.Output.Sign() <= 0 {
		rules = append(rules, "input and output must both be published and above zero")
	}
	read := q.CacheRead
	if read == nil {
		rules = append(rules, "source publishes no cache-read rate")
		read = q.Input
	}
	write := q.Input
	if q.CacheWrite != nil && q.CacheWrite.Cmp(q.Input) >= 0 {
		write = q.CacheWrite
	}
	return map[string]*big.Rat{
		fieldInput:      q.Input,
		fieldOutput:     q.Output,
		fieldCacheRead:  read,
		fieldCacheWrite: write,
	}, rules
}

// siblingEntry is the existing row with the highest version in key's model
// line (claude-fable-5-2 -> the newest claude-fable-<n> row).
func siblingEntry(t *table, key string) (entry, bool) {
	m := versionedKey.FindStringSubmatch(key)
	if m == nil {
		return entry{}, false
	}
	var best entry
	var bestVer []int
	for _, e := range t.Entries {
		em := versionedKey.FindStringSubmatch(e.Key)
		if em == nil || em[1] != m[1] {
			continue
		}
		v := versionInts(em[2])
		if bestVer == nil || versionLess(bestVer, v) {
			best, bestVer = e, v
		}
	}
	return best, bestVer != nil
}

func versionInts(s string) []int {
	parts := strings.Split(s, "-")
	out := make([]int, len(parts))
	for i, p := range parts {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

func versionLess(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// applyAdditions inserts each new row after its sibling's line (or after the
// last row of the same vendor prefix, or at the end of the map), with a
// comment naming the source quote, then gofmt's the result. Existing rows and
// priceTableDate are not touched.
func applyAdditions(src []byte, t *table, r *addResult, today string) ([]byte, error) {
	if len(t.Entries) == 0 {
		return nil, fmt.Errorf("table has no rows")
	}
	byOffset := map[int][]addition{}
	for _, a := range r.Added {
		anchor := t.Entries[len(t.Entries)-1]
		if a.Sibling != "" {
			anchor, _ = t.entry(a.Sibling)
		} else {
			vendor, _, _ := strings.Cut(a.Key, "-")
			for _, e := range t.Entries {
				if strings.HasPrefix(e.Key, vendor+"-") {
					anchor = e
				}
			}
		}
		at := strings.IndexByte(string(src[anchor.End:]), '\n')
		if at < 0 {
			return nil, fmt.Errorf("row %s has no line end", anchor.Key)
		}
		off := anchor.End + at + 1
		byOffset[off] = append(byOffset[off], a)
	}
	offsets := make([]int, 0, len(byOffset))
	for off := range byOffset {
		offsets = append(offsets, off)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(offsets)))
	out := append([]byte(nil), src...)
	for _, off := range offsets {
		rows := byOffset[off]
		sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
		var b strings.Builder
		for _, a := range rows {
			fmt.Fprintf(&b, "\t// %s: list price from the source's quote for %s (added %s by pricetable --add).\n", a.Key, a.SourceID, today)
			fmt.Fprintf(&b, "\t%q: {InputPerMTok: %s, OutputPerMTok: %s, CacheReadPerMTok: %s, CacheWritePerMTok: %s},\n",
				a.Key, formatPrice(a.Fields[fieldInput]), formatPrice(a.Fields[fieldOutput]),
				formatPrice(a.Fields[fieldCacheRead]), formatPrice(a.Fields[fieldCacheWrite]))
		}
		out = append(out[:off], append([]byte(b.String()), out[off:]...)...)
	}
	formatted, err := format.Source(out)
	if err != nil {
		return nil, fmt.Errorf("gofmt: %w", err)
	}
	return formatted, nil
}

// renderAddReport writes the markdown summary of an --add run.
func renderAddReport(r *addResult, source string) string {
	var b strings.Builder
	b.WriteString("## New price rows\n\n")
	fmt.Fprintf(&b, "Source: `%s` (OpenRouter public model catalogue; per-token USD list prices). Every quote passed the same plausibility rules as a price refresh, compared with the newest existing row of the same model line.\n\n", source)
	fmt.Fprintf(&b, "### Added (%d)\n\n", len(r.Added))
	if len(r.Added) == 0 {
		b.WriteString("No row added.\n\n")
	} else {
		b.WriteString("All figures are USD per million tokens.\n\n")
		b.WriteString("| model | source id | input | output | cache read | cache write | compared with |\n")
		b.WriteString("|---|---|---|---|---|---|---|\n")
		for _, a := range r.Added {
			sib := "no earlier row of this line; ratio rules only"
			if a.Sibling != "" {
				sib = "`" + a.Sibling + "`"
			}
			fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s | %s | %s | %s |\n", a.Key, a.SourceID,
				formatPrice(a.Fields[fieldInput]), formatPrice(a.Fields[fieldOutput]),
				formatPrice(a.Fields[fieldCacheRead]), formatPrice(a.Fields[fieldCacheWrite]), sib)
		}
		b.WriteString("\n")
	}
	if len(r.Held) > 0 {
		fmt.Fprintf(&b, "### Held for review (%d)\n\n", len(r.Held))
		b.WriteString("The source's quote for these models failed a plausibility rule, so no row was added; the model falls back to its tier price until someone adds the row by hand from the vendor's price page.\n\n")
		b.WriteString("| model | source id | input | output | cache read | cache write | rule |\n")
		b.WriteString("|---|---|---|---|---|---|---|\n")
		for _, s := range r.Held {
			fmt.Fprintf(&b, "| `%s` | `%s` |", s.Key, s.SourceID)
			for _, name := range priceFields {
				fmt.Fprintf(&b, " %s |", formatPrice(s.Fields[name].New))
			}
			fmt.Fprintf(&b, " %s |\n", strings.Join(s.Rules, "; "))
		}
		b.WriteString("\n")
	}
	if len(r.Missing) > 0 {
		fmt.Fprintf(&b, "### No quote (%d)\n\n", len(r.Missing))
		for _, u := range r.Missing {
			fmt.Fprintf(&b, "- `%s` — %s\n", u.Key, u.Reason)
		}
		b.WriteString("\n")
	}
	if len(r.Present) > 0 {
		fmt.Fprintf(&b, "### Already priced (%d)\n\n%s\n\n", len(r.Present), backtickList(r.Present))
	}
	return b.String()
}

// executeAdd runs --add: new rows only, existing rows and the date untouched.
func executeAdd(ctx context.Context, opts options, src []byte, t *table, today string) error {
	var ids []string
	for _, id := range strings.Split(opts.add, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	raw, err := opts.fetch(ctx, opts.source)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", opts.source, err)
	}
	catalogue, err := parseOpenRouter(raw)
	if err != nil {
		return fmt.Errorf("parse catalogue: %w", err)
	}
	result := planAdditions(t, catalogue, ids)
	report := renderAddReport(result, opts.source)
	if opts.report != "" {
		if err := os.WriteFile(opts.report, []byte(report), 0o644); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
	}
	fmt.Fprint(opts.stdout, report)
	if len(result.Added) == 0 {
		fmt.Fprintln(opts.stdout, "pricetable: no new rows")
		return nil
	}
	out, err := applyAdditions(src, t, result, today)
	if err != nil {
		return fmt.Errorf("add rows: %w", err)
	}
	if opts.dryRun {
		fmt.Fprintf(opts.stdout, "pricetable: dry run, %d new row(s) not written\n", len(result.Added))
		return nil
	}
	if err := os.WriteFile(opts.pricing, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", opts.pricing, err)
	}
	fmt.Fprintf(opts.stdout, "pricetable: added %d new row(s) to %s\n", len(result.Added), opts.pricing)
	return nil
}
