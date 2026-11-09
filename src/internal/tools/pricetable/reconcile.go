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

// Plausibility thresholds. A quote that trips any of them is held for a human
// instead of applied: a catalogue defect transcribed faithfully looks exactly
// like a real price move otherwise (#17364).
const (
	// maxMoveFactor is the largest move, in either direction, one field may
	// make against the current table value.
	maxMoveFactor = 5
	// maxOutputInputRatio is the largest plausible output/input price ratio.
	maxOutputInputRatio = 30
	// maxCacheReadShareNum/Den express the cache-read share of input (90%) at
	// or above which a cache-read rate is implausible, unless the current row
	// already prices cache reads at the input rate.
	maxCacheReadShareNum = 9
	maxCacheReadShareDen = 10
)

// suspect is a catalogue quote held back because it looks implausible. Fields
// carries every price field, Old being the current table value and New the
// value the quote would have produced.
type suspect struct {
	Key      string
	SourceID string
	Fields   map[string]fieldChange
	Rules    []string
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
	// Suspect rows keep their current value; the quote tripped a plausibility
	// rule and needs a human decision.
	Suspect []suspect
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
// listed as unverified; priceTableDate moves to today when any price changed,
// when any quote was held for review (so the run yields a diff and the held
// section reaches a human in a PR), or when the table is older than
// staleAfterDays (a successful "no change" reconciliation is still a
// reconciliation).
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
		if rules := implausible(e, ch); len(rules) > 0 {
			r.Suspect = append(r.Suspect, suspect{Key: e.Key, SourceID: q.SourceID, Fields: proposedFields(e, ch), Rules: rules})
			r.Notes = append(r.Notes, fmt.Sprintf("`%s`: quote from `%s` held, current value kept: %s", e.Key, q.SourceID, strings.Join(rules, "; ")))
			continue
		}
		if len(ch.Fields) > 0 {
			r.Changes = append(r.Changes, ch)
		} else {
			r.Verified = append(r.Verified, e.Key)
		}
	}
	sort.Slice(r.Changes, func(i, j int) bool { return r.Changes[i].Key < r.Changes[j].Key })
	sort.Slice(r.Unverified, func(i, j int) bool { return r.Unverified[i].Key < r.Unverified[j].Key })
	sort.Slice(r.Suspect, func(i, j int) bool { return r.Suspect[i].Key < r.Suspect[j].Key })
	sort.Strings(r.Verified)
	sort.Strings(r.Notes)

	switch {
	case len(r.Changes) > 0:
		r.NewDate = today
	case len(r.Suspect) > 0:
		// Without a diff the workflow opens no PR and the held section would
		// never reach a reviewer (#17365).
		r.NewDate = today
		r.DateReason = fmt.Sprintf("no price was applied, but %d quote(s) were held for review; the date is bumped so this run opens a PR that puts the held rows in front of a human", len(r.Suspect))
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

// proposedFields lists every price field of e with the value the change would
// give it (equal to the current one when the field does not move).
func proposedFields(e entry, ch change) map[string]fieldChange {
	out := make(map[string]fieldChange, len(priceFields))
	for _, name := range priceFields {
		cur := e.Fields[name].Value
		next := cur
		if fc, ok := ch.Fields[name]; ok {
			next = fc.New
		}
		out[name] = fieldChange{Old: cur, New: next}
	}
	return out
}

// implausible names the plausibility rules the proposed change trips; it is
// empty for a row that does not move or whose move looks like a real one.
func implausible(e entry, ch change) []string {
	if len(ch.Fields) == 0 {
		return nil
	}
	var rules []string
	for _, name := range priceFields {
		fc, ok := ch.Fields[name]
		if !ok {
			continue
		}
		// A move to zero (or, for the two headline rates, away from it) is
		// unbounded, so it trips the same rule as a move past the factor.
		if (fc.Old.Sign() > 0 && fc.New.Sign() <= 0) ||
			(fc.Old.Sign() <= 0 && fc.New.Sign() > 0 && (name == fieldInput || name == fieldOutput)) {
			rules = append(rules, fmt.Sprintf("%s moves more than %d× (%s → %s)", name, maxMoveFactor, formatPrice(fc.Old), formatPrice(fc.New)))
			continue
		}
		if fc.Old.Sign() <= 0 || fc.New.Sign() <= 0 {
			continue
		}
		factor := new(big.Rat).Quo(fc.New, fc.Old)
		if factor.Cmp(big.NewRat(maxMoveFactor, 1)) > 0 || factor.Cmp(big.NewRat(1, maxMoveFactor)) < 0 {
			rules = append(rules, fmt.Sprintf("%s moves more than %d× (%s → %s)", name, maxMoveFactor, formatPrice(fc.Old), formatPrice(fc.New)))
		}
	}
	p := proposedFields(e, ch)
	in, out, read := p[fieldInput].New, p[fieldOutput].New, p[fieldCacheRead].New
	// A zero proposed input would skip the ratio checks, so judge against the
	// current input instead.
	if in.Sign() <= 0 {
		in = p[fieldInput].Old
	}
	if in.Sign() > 0 {
		if new(big.Rat).Quo(out, in).Cmp(big.NewRat(maxOutputInputRatio, 1)) > 0 {
			rules = append(rules, fmt.Sprintf("output/input ratio above %d (%s / %s)", maxOutputInputRatio, formatPrice(out), formatPrice(in)))
		}
		share := new(big.Rat).Quo(read, in)
		sameAsInput := p[fieldCacheRead].Old.Cmp(p[fieldInput].Old) == 0
		if !sameAsInput && share.Cmp(big.NewRat(maxCacheReadShareNum, maxCacheReadShareDen)) >= 0 {
			rules = append(rules, fmt.Sprintf("cache read at or above %d%% of input (%s vs %s)", maxCacheReadShareNum*100/maxCacheReadShareDen, formatPrice(read), formatPrice(in)))
		}
	}
	return rules
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

	if len(r.Suspect) > 0 {
		fmt.Fprintf(&b, "### Held for review (%d)\n\n", len(r.Suspect))
		b.WriteString("The source quoted these rows implausibly, so they keep their current value. Treat each as a question about the source's data, not a change: check the vendor's price page, then edit the table by hand if the quote is right.\n\n")
		b.WriteString("| model | source id | input | output | cache read | cache write | rule |\n")
		b.WriteString("|---|---|---|---|---|---|---|\n")
		for _, s := range r.Suspect {
			fmt.Fprintf(&b, "| `%s` | `%s` |", s.Key, s.SourceID)
			for _, name := range priceFields {
				fc := s.Fields[name]
				if fc.Old.Cmp(fc.New) != 0 {
					fmt.Fprintf(&b, " %s → proposed %s |", formatPrice(fc.Old), formatPrice(fc.New))
				} else {
					fmt.Fprintf(&b, " %s |", formatPrice(fc.Old))
				}
			}
			fmt.Fprintf(&b, " %s |\n", strings.Join(s.Rules, "; "))
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
