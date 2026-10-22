package main

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

// fieldChange is one old -> new rate.
type fieldChange struct {
	Old *big.Rat
	New *big.Rat
}

// change is every field that moved on one table row.
type change struct {
	Key      string
	SourceID string
	Fields   map[string]fieldChange
}

// unverified is a table row the source could not vouch for.
type unverified struct {
	Key    string
	Reason string
}

// result is the outcome of one reconciliation.
type result struct {
	OldDate string
	// NewDate is the date to write; equal to OldDate when nothing moves.
	NewDate string
	// DateReason explains a date bump that happened without a price change.
	DateReason string
	Changes    []change
	Verified   []string
	Unverified []unverified
	// Notes are per-row remarks that did not block a change (for example a
	// cache-write rate the source publishes with different semantics).
	Notes []string
}

// Changed reports whether applyChanges would alter the source.
func (r *result) Changed() bool {
	return len(r.Changes) > 0 || r.NewDate != r.OldDate
}

// reconcile compares every table row against the catalogue and decides what
// to write. Rows absent from the catalogue keep their current price and are
// listed as unverified; priceTableDate moves to today when any price changed
// or when the table is older than staleAfterDays (a successful "no change"
// reconciliation is still a reconciliation).
func reconcile(t *table, c *catalogue, today string, staleAfterDays int) *result {
	r := &result{OldDate: t.Date, NewDate: t.Date}
	for _, e := range t.Entries {
		if ids, ok := c.Ambiguous[e.Key]; ok {
			r.Unverified = append(r.Unverified, unverified{e.Key, "several catalogue ids normalise to this key: " + strings.Join(ids, ", ")})
			continue
		}
		q, via, ok := lookupQuote(c, e.Key)
		if !ok {
			r.Unverified = append(r.Unverified, unverified{e.Key, "not listed by the source; current value kept"})
			continue
		}
		ch, notes := diffEntry(e, q)
		if via != "" {
			notes = append(notes, fmt.Sprintf("`%s`: not listed by the source; priced via its base model `%s` (%s)", e.Key, via, q.SourceID))
		}
		r.Notes = append(r.Notes, notes...)
		if len(ch.Fields) > 0 {
			r.Changes = append(r.Changes, ch)
		} else {
			r.Verified = append(r.Verified, e.Key)
		}
	}
	sort.Slice(r.Changes, func(i, j int) bool { return r.Changes[i].Key < r.Changes[j].Key })
	sort.Slice(r.Unverified, func(i, j int) bool { return r.Unverified[i].Key < r.Unverified[j].Key })
	sort.Strings(r.Verified)
	sort.Strings(r.Notes)

	switch {
	case len(r.Changes) > 0:
		r.NewDate = today
	case staleAfterDays > 0 && olderThan(t.Date, today, staleAfterDays):
		r.NewDate = today
		r.DateReason = fmt.Sprintf("no price moved, but the table was last reconciled on %s (more than %d days ago); the date is bumped to record this verification", t.Date, staleAfterDays)
	}
	return r
}

// effortSuffixes are reasoning-effort selector spellings some CLIs append to
// a model id; they bill at the base model's rate.
var effortSuffixes = []string{"-high", "-medium", "-low"}

// lookupQuote finds the quote for key, falling back to the base model when
// key is only an effort-selector spelling of one. via names the base key used.
func lookupQuote(c *catalogue, key string) (quote, string, bool) {
	if q, ok := c.Quotes[key]; ok {
		return q, "", true
	}
	for _, suffix := range effortSuffixes {
		base, found := strings.CutSuffix(key, suffix)
		if !found {
			continue
		}
		if _, ambiguous := c.Ambiguous[base]; ambiguous {
			return quote{}, "", false
		}
		if q, ok := c.Quotes[base]; ok {
			return q, base, true
		}
	}
	return quote{}, "", false
}

// olderThan reports whether date is more than days before today. Unparseable
// dates count as stale so a corrupted constant surfaces as a bump.
func olderThan(date, today string, days int) bool {
	d, err := time.Parse(dateLayout, date)
	if err != nil {
		return true
	}
	now, err := time.Parse(dateLayout, today)
	if err != nil {
		return false
	}
	return now.Sub(d).Hours()/hoursPerDay > float64(days)
}

// diffEntry works out which fields of one row move to match the quote.
//
// Input and output always follow the source. Cache read follows the source
// when published; otherwise a row that priced cache reads at the input rate
// keeps doing so. Cache write follows the source only when it is at least
// the input rate (Anthropic/OpenAI's write premium); a smaller figure is a
// storage-per-hour rate (Google) with different semantics, and an absent one
// means the provider has no write premium. In both of those cases the table's
// documented convention — cache write equals the input rate — is kept: a row
// that already followed it moves with input, any other hand-set value stays.
func diffEntry(e entry, q quote) (change, []string) {
	ch := change{Key: e.Key, SourceID: q.SourceID, Fields: make(map[string]fieldChange)}
	var notes []string
	set := func(name string, want *big.Rat) {
		cur := e.Fields[name].Value
		if want != nil && cur.Cmp(want) != 0 {
			ch.Fields[name] = fieldChange{Old: cur, New: want}
		}
	}
	set(fieldInput, q.Input)
	set(fieldOutput, q.Output)
	oldInput := e.Fields[fieldInput].Value
	oldRead := e.Fields[fieldCacheRead].Value
	oldWrite := e.Fields[fieldCacheWrite].Value
	switch {
	case q.CacheRead != nil:
		set(fieldCacheRead, q.CacheRead)
	case oldRead.Cmp(oldInput) == 0:
		set(fieldCacheRead, q.Input)
	default:
		notes = append(notes, fmt.Sprintf("`%s`: source publishes no cache-read rate; hand-set %s kept", e.Key, formatPrice(oldRead)))
	}

	switch {
	case q.CacheWrite != nil && q.CacheWrite.Cmp(q.Input) >= 0:
		set(fieldCacheWrite, q.CacheWrite)
	case oldWrite.Cmp(oldInput) == 0:
		// Row follows the "cache write = input" convention; keep it in step.
		set(fieldCacheWrite, q.Input)
		if q.CacheWrite != nil {
			notes = append(notes, fmt.Sprintf("`%s`: source cache-write rate %s is below input (storage-rate semantics); cache write kept equal to input", e.Key, formatPrice(q.CacheWrite)))
		}
	default:
		if q.CacheWrite != nil {
			notes = append(notes, fmt.Sprintf("`%s`: source cache-write rate %s is below input (storage-rate semantics); hand-set %s kept", e.Key, formatPrice(q.CacheWrite), formatPrice(oldWrite)))
		} else {
			notes = append(notes, fmt.Sprintf("`%s`: source publishes no cache-write rate; hand-set %s kept", e.Key, formatPrice(oldWrite)))
		}
	}
	return ch, notes
}

// renderReport writes the markdown summary used as the PR body.
func renderReport(r *result, source string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Price table refresh\n\n")
	fmt.Fprintf(&b, "Source: `%s` (OpenRouter public model catalogue; per-token USD list prices).\n\n", source)
	if r.NewDate != r.OldDate {
		fmt.Fprintf(&b, "`priceTableDate`: %s → %s.\n\n", r.OldDate, r.NewDate)
	} else {
		fmt.Fprintf(&b, "`priceTableDate`: %s (unchanged).\n\n", r.OldDate)
	}
	if r.DateReason != "" {
		fmt.Fprintf(&b, "%s.\n\n", r.DateReason)
	}

	fmt.Fprintf(&b, "### Changes (%d)\n\n", len(r.Changes))
	if len(r.Changes) == 0 {
		b.WriteString("No list price moved.\n\n")
	} else {
		b.WriteString("All figures are USD per million tokens.\n\n")
		b.WriteString("| model | source id | input | output | cache read | cache write |\n")
		b.WriteString("|---|---|---|---|---|---|\n")
		for _, c := range r.Changes {
			fmt.Fprintf(&b, "| `%s` | `%s` |", c.Key, c.SourceID)
			for _, name := range priceFields {
				if fc, ok := c.Fields[name]; ok {
					fmt.Fprintf(&b, " %s → **%s** |", formatPrice(fc.Old), formatPrice(fc.New))
				} else {
					b.WriteString(" – |")
				}
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "### Verified unchanged (%d)\n\n", len(r.Verified))
	if len(r.Verified) > 0 {
		b.WriteString(backtickList(r.Verified))
		b.WriteString("\n\n")
	}

	fmt.Fprintf(&b, "### Not verifiable via the source (%d)\n\n", len(r.Unverified))
	if len(r.Unverified) == 0 {
		b.WriteString("Every table row was found in the source.\n\n")
	} else {
		b.WriteString("These rows keep their current value and still need a manual check against the vendor's price page.\n\n")
		for _, u := range r.Unverified {
			fmt.Fprintf(&b, "- `%s` — %s\n", u.Key, u.Reason)
		}
		b.WriteString("\n")
	}

	if len(r.Notes) > 0 {
		fmt.Fprintf(&b, "<details><summary>Notes (%d)</summary>\n\n", len(r.Notes))
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
		b.WriteString("\n</details>\n\n")
	}
	b.WriteString("List prices are a ceiling on what hive's estimate can show, not a bill: subscription seats, self-hosted inference and negotiated rates all bill differently. Review the figures above against the vendor's own page before merging.\n")
	return b.String()
}

// backtickList renders keys as a comma-separated inline-code list.
func backtickList(keys []string) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = "`" + k + "`"
	}
	return strings.Join(parts, ", ")
}
