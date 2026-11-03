package main

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureTable is a miniature pricing.go with the same shape as the real one:
// a priceTableDate const, a modelPrices map with comments, and rows that will
// move, stay, be derived from a base model, or go unverified.
const fixtureTable = `package tokens

// priceTableDate documents when the table was last reconciled.
const priceTableDate = "2026-01-15"

type ModelPrice struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheReadPerMTok  float64
	CacheWritePerMTok float64
}

var modelPrices = map[string]ModelPrice{
	// ---- Anthropic ----
	"claude-opus-4-8": {InputPerMTok: 5.00, OutputPerMTok: 25.00, CacheReadPerMTok: 0.50, CacheWritePerMTok: 6.25},
	// Sonnet tier.
	"claude-sonnet-5": {InputPerMTok: 3.00, OutputPerMTok: 15.00, CacheReadPerMTok: 0.30, CacheWritePerMTok: 3.75},

	// ---- OpenAI ----
	"gpt-5-4":             {InputPerMTok: 1.25, OutputPerMTok: 10.00, CacheReadPerMTok: 0.125, CacheWritePerMTok: 1.25},
	"gpt-oss-120b-medium": {InputPerMTok: 0.59, OutputPerMTok: 0.79, CacheReadPerMTok: 0.59, CacheWritePerMTok: 0.59},
	"gpt-5-6":             {InputPerMTok: 1.25, OutputPerMTok: 10.00, CacheReadPerMTok: 0.125, CacheWritePerMTok: 1.25},

	// ---- Google / Meta ----
	"gemini-3-7-flash":       {InputPerMTok: 0.50, OutputPerMTok: 3.00, CacheReadPerMTok: 0.05, CacheWritePerMTok: 0.50},
	"gemini-3-7-flash-high":  {InputPerMTok: 0.50, OutputPerMTok: 3.00, CacheReadPerMTok: 0.05, CacheWritePerMTok: 0.50},
	"llama-3-3-70b-instruct": {InputPerMTok: 0.59, OutputPerMTok: 0.79, CacheReadPerMTok: 0.59, CacheWritePerMTok: 0.40},
	"dup-model":              {InputPerMTok: 1.00, OutputPerMTok: 2.00, CacheReadPerMTok: 1.00, CacheWritePerMTok: 1.00},
}
`

// fixtureDate is "today" for the tests; the fixture table is 20 days older.
const fixtureDate = "2026-02-04"

func fixtureCatalogue(t *testing.T) *catalogue {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "openrouter.json"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := parseOpenRouter(raw)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeFixtureTable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pricing.go")
	if err := os.WriteFile(path, []byte(fixtureTable), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func fixedNow() time.Time { return time.Date(2026, 2, 4, 12, 0, 0, 0, time.UTC) }

func TestParseOpenRouter(t *testing.T) {
	c := fixtureCatalogue(t)

	q, ok := c.Quotes["claude-opus-4-8"]
	if !ok {
		t.Fatal("claude-opus-4-8 missing from catalogue")
	}
	if q.SourceID != "anthropic/claude-opus-4.8" {
		t.Fatalf("source id = %q, want the non-variant id", q.SourceID)
	}
	if got := formatPrice(q.Input); got != "5.00" {
		t.Fatalf("input = %s, want 5.00 (per-token 0.000005 scaled to per-MTok)", got)
	}
	if got := formatPrice(q.CacheWrite); got != "6.25" {
		t.Fatalf("cache write = %s, want 6.25", got)
	}
	if q, ok := c.Quotes["gpt-5-4"]; !ok || q.CacheWrite != nil {
		t.Fatalf("gpt-5-4: ok=%v cacheWrite=%v, want present with nil cache write", ok, q.CacheWrite)
	}
	if got := formatPrice(c.Quotes["gemini-3-7-flash"].CacheWrite); got != "0.041667" {
		t.Fatalf("repeating decimal rounded to %s, want 0.041667", got)
	}
	if _, ok := c.Quotes["claude-opus-latest"]; ok {
		t.Fatal("~vendor/model-latest alias must be skipped")
	}
	if _, ok := c.Quotes["switchyard"]; ok {
		t.Fatal("variable (-1) pricing must not produce a quote")
	}
	if _, ok := c.Quotes["flint-v1-0"]; ok {
		t.Fatal(":free variant must be skipped")
	}
	if _, ok := c.Quotes["free-only"]; ok {
		t.Fatal("zero-priced model must not produce a quote")
	}
	if ids := c.Ambiguous["dup-model"]; len(ids) != 2 || ids[0] != "vendor-a/dup-model" {
		t.Fatalf("ambiguous dup-model = %v, want both vendor ids sorted", ids)
	}
	if _, ok := c.Quotes["dup-model"]; ok {
		t.Fatal("ambiguous key must not be quoted")
	}
}

func TestParseOpenRouterRejectsGarbage(t *testing.T) {
	if _, err := parseOpenRouter([]byte(`{"data": []}`)); err == nil {
		t.Fatal("empty catalogue should error")
	}
	if _, err := parseOpenRouter([]byte(`not json`)); err == nil {
		t.Fatal("invalid JSON should error")
	}
	if _, err := parseOpenRouter([]byte(`{"data": [{"id": "x/y", "pricing": {"prompt": "abc", "completion": "1"}}]}`)); err == nil {
		t.Fatal("unparseable price should error")
	}
}

func TestParseTable(t *testing.T) {
	tb, err := parseTable([]byte(fixtureTable))
	if err != nil {
		t.Fatal(err)
	}
	if tb.Date != "2026-01-15" {
		t.Fatalf("date = %q", tb.Date)
	}
	if len(tb.Entries) != 9 {
		t.Fatalf("entries = %d, want 9", len(tb.Entries))
	}
	e, ok := tb.entry("gpt-5-4")
	if !ok {
		t.Fatal("gpt-5-4 missing")
	}
	if e.Fields[fieldCacheRead].Text != "0.125" {
		t.Fatalf("cache read literal = %q", e.Fields[fieldCacheRead].Text)
	}
	if got := string([]byte(fixtureTable)[e.Fields[fieldOutput].Start:e.Fields[fieldOutput].End]); got != "10.00" {
		t.Fatalf("output literal span = %q, want 10.00", got)
	}
}

func TestParseTableRejectsMalformed(t *testing.T) {
	for name, src := range map[string]string{
		"no date":       "package tokens\nvar modelPrices = map[string]ModelPrice{\"a\": {InputPerMTok: 1, OutputPerMTok: 1, CacheReadPerMTok: 1, CacheWritePerMTok: 1}}\n",
		"no table":      "package tokens\nconst priceTableDate = \"2026-01-01\"\n",
		"missing field": "package tokens\nconst priceTableDate = \"2026-01-01\"\nvar modelPrices = map[string]ModelPrice{\"a\": {InputPerMTok: 1}}\n",
		"non-literal":   "package tokens\nconst priceTableDate = \"2026-01-01\"\nvar modelPrices = map[string]ModelPrice{\"a\": {InputPerMTok: x, OutputPerMTok: 1, CacheReadPerMTok: 1, CacheWritePerMTok: 1}}\n",
		"syntax":        "package tokens\nconst priceTableDate = ",
	} {
		if _, err := parseTable([]byte(src)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseRealTable(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "pkg", "tokens", "pricing.go"))
	if err != nil {
		t.Skip("real pricing.go not available:", err)
	}
	tb, err := parseTable(src)
	if err != nil {
		t.Fatalf("real pricing.go must stay parseable by the refresh tool: %v", err)
	}
	if _, err := time.Parse(dateLayout, tb.Date); err != nil {
		t.Fatalf("real priceTableDate %q: %v", tb.Date, err)
	}
	if len(tb.Entries) < 10 {
		t.Fatalf("real table parsed only %d entries", len(tb.Entries))
	}
}

func TestReconcile(t *testing.T) {
	tb, err := parseTable([]byte(fixtureTable))
	if err != nil {
		t.Fatal(err)
	}
	r := reconcile(tb, fixtureCatalogue(t), fixtureDate, defaultStaleAfterDays)
	if !r.Changed() || r.NewDate != fixtureDate {
		t.Fatalf("expected changes dated %s, got changed=%v date=%s", fixtureDate, r.Changed(), r.NewDate)
	}
	byKey := map[string]change{}
	for _, c := range r.Changes {
		byKey[c.Key] = c
	}
	if _, ok := byKey["claude-opus-4-8"]; ok {
		t.Fatal("claude-opus-4-8 matches the source and must not change")
	}
	// Anthropic: all four follow the source (cache write >= input).
	son := byKey["claude-sonnet-5"].Fields
	if formatPrice(son[fieldInput].New) != "2.00" || formatPrice(son[fieldOutput].New) != "10.00" ||
		formatPrice(son[fieldCacheRead].New) != "0.20" || formatPrice(son[fieldCacheWrite].New) != "2.50" {
		t.Fatalf("claude-sonnet-5 changes = %v", son)
	}
	// OpenAI: no cache write published; row followed "write = input" so it moves with input.
	g54 := byKey["gpt-5-4"].Fields
	if formatPrice(g54[fieldInput].New) != "2.50" || formatPrice(g54[fieldCacheRead].New) != "0.25" || formatPrice(g54[fieldCacheWrite].New) != "2.50" {
		t.Fatalf("gpt-5-4 changes = %v", g54)
	}
	// Google: cache write below input (storage rate) -> stays equal to input.
	gem := byKey["gemini-3-7-flash"].Fields
	if formatPrice(gem[fieldCacheWrite].New) != "0.75" || formatPrice(gem[fieldCacheRead].New) != "0.075" {
		t.Fatalf("gemini-3-7-flash changes = %v", gem)
	}
	// Effort-suffix spelling priced via its base model.
	if c, ok := byKey["gemini-3-7-flash-high"]; !ok || c.SourceID != "google/gemini-3.7-flash" {
		t.Fatalf("gemini-3-7-flash-high = %+v, want priced via base", c)
	}
	if c, ok := byKey["gpt-oss-120b-medium"]; !ok || formatPrice(c.Fields[fieldInput].New) != "0.15" {
		t.Fatalf("gpt-oss-120b-medium = %+v", c)
	}
	// Llama: no cache rates published; cache read followed input (moves), cache write was hand-set (kept).
	llama := byKey["llama-3-3-70b-instruct"].Fields
	if formatPrice(llama[fieldCacheRead].New) != "0.10" {
		t.Fatalf("llama cache read = %v, want 0.10", llama[fieldCacheRead])
	}
	if _, moved := llama[fieldCacheWrite]; moved {
		t.Fatal("hand-set llama cache write must be kept")
	}
	// Unverified: not listed, and ambiguous.
	want := map[string]bool{"gpt-5-6": true, "dup-model": true}
	for _, u := range r.Unverified {
		if !want[u.Key] {
			t.Errorf("unexpected unverified %s: %s", u.Key, u.Reason)
		}
		delete(want, u.Key)
	}
	if len(want) != 0 {
		t.Fatalf("missing unverified rows: %v", want)
	}
	if len(r.Verified) != 1 || r.Verified[0] != "claude-opus-4-8" {
		t.Fatalf("verified = %v", r.Verified)
	}
	joined := strings.Join(r.Notes, "\n")
	for _, s := range []string{"storage-rate semantics", "priced via its base model", "hand-set 0.40 kept"} {
		if !strings.Contains(joined, s) {
			t.Errorf("notes missing %q:\n%s", s, joined)
		}
	}
}

func TestReconcileNoChangeKeepsDate(t *testing.T) {
	tb, err := parseTable([]byte(fixtureTable))
	if err != nil {
		t.Fatal(err)
	}
	c := &catalogue{Quotes: map[string]quote{}, Ambiguous: map[string][]string{}}
	// Catalogue matching the table exactly.
	raw, _ := os.ReadFile(filepath.Join("testdata", "openrouter.json"))
	full, _ := parseOpenRouter(raw)
	c.Quotes["claude-opus-4-8"] = full.Quotes["claude-opus-4-8"]

	r := reconcile(tb, c, fixtureDate, defaultStaleAfterDays)
	if r.Changed() {
		t.Fatalf("20-day-old table with no price moves must not change: %+v", r)
	}
	// Past the stale threshold the date is bumped even without price changes.
	r = reconcile(tb, c, fixtureDate, 10)
	if !r.Changed() || len(r.Changes) != 0 || r.NewDate != fixtureDate || r.DateReason == "" {
		t.Fatalf("stale table should bump date only: %+v", r)
	}
	// Disabled threshold never bumps.
	if r := reconcile(tb, c, fixtureDate, 0); r.Changed() {
		t.Fatal("stale-after 0 must not bump the date")
	}
}

func TestApplyChangesMinimalAndIdempotent(t *testing.T) {
	tb, err := parseTable([]byte(fixtureTable))
	if err != nil {
		t.Fatal(err)
	}
	r := reconcile(tb, fixtureCatalogue(t), fixtureDate, defaultStaleAfterDays)
	out, err := applyChanges([]byte(fixtureTable), tb, r)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{
		`const priceTableDate = "2026-02-04"`,
		`"claude-opus-4-8": {InputPerMTok: 5.00, OutputPerMTok: 25.00, CacheReadPerMTok: 0.50, CacheWritePerMTok: 6.25},`,
		`"claude-sonnet-5": {InputPerMTok: 2.00, OutputPerMTok: 10.00, CacheReadPerMTok: 0.20, CacheWritePerMTok: 2.50},`,
		`"gemini-3-7-flash":       {InputPerMTok: 0.75, OutputPerMTok: 3.75, CacheReadPerMTok: 0.075, CacheWritePerMTok: 0.75},`,
		`"llama-3-3-70b-instruct": {InputPerMTok: 0.10, OutputPerMTok: 0.32, CacheReadPerMTok: 0.10, CacheWritePerMTok: 0.40},`,
		"// ---- Anthropic ----",
		"// Sonnet tier.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q\n%s", want, text)
		}
	}
	// Running the same reconciliation over the rewritten source finds nothing.
	tb2, err := parseTable(out)
	if err != nil {
		t.Fatal(err)
	}
	r2 := reconcile(tb2, fixtureCatalogue(t), fixtureDate, defaultStaleAfterDays)
	if r2.Changed() {
		t.Fatalf("second pass must be a no-op, got %d changes, date %s", len(r2.Changes), r2.NewDate)
	}
	out2, err := applyChanges(out, tb2, r2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, out2) {
		t.Fatal("applying an empty result must reproduce the source byte for byte")
	}
}

func TestFormatPrice(t *testing.T) {
	for in, want := range map[string]string{
		"5":            "5.00",
		"0.125":        "0.125",
		"0.01875":      "0.01875",
		"0.0416666667": "0.041667",
		"10.5":         "10.50",
		"0":            "0.00",
	} {
		r, _ := new(big.Rat).SetString(in)
		if got := formatPrice(r); got != want {
			t.Errorf("formatPrice(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestRenderReport(t *testing.T) {
	tb, _ := parseTable([]byte(fixtureTable))
	r := reconcile(tb, fixtureCatalogue(t), fixtureDate, defaultStaleAfterDays)
	md := renderReport(r, "testdata/openrouter.json")
	for _, want := range []string{
		"`priceTableDate`: 2026-01-15 → 2026-02-04",
		"| `claude-sonnet-5` | `anthropic/claude-sonnet-5` | 3.00 → **2.00** | 15.00 → **10.00** | 0.30 → **0.20** | 3.75 → **2.50** |",
		"### Not verifiable via the source (2)",
		"- `gpt-5-6` — not listed by the source; current value kept",
		"several catalogue ids normalise to this key: vendor-a/dup-model, vendor-b/dup-model",
		"`claude-opus-4-8`",
		"ceiling",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report missing %q\n%s", want, md)
		}
	}
}

func TestCheckAge(t *testing.T) {
	var out bytes.Buffer
	if err := checkAge("2026-01-15", fixedNow(), 60, &out); err != nil {
		t.Fatalf("20-day-old table within 60 days: %v", err)
	}
	if !strings.Contains(out.String(), "20 day(s) old") {
		t.Fatalf("stdout = %q", out.String())
	}
	if err := checkAge("2026-01-15", fixedNow(), 10, &out); err == nil {
		t.Fatal("20-day-old table must fail a 10-day limit")
	}
	if err := checkAge("not-a-date", fixedNow(), 10, &out); err == nil {
		t.Fatal("unparseable date must fail")
	}
}

func TestRunRefreshWritesFileAndReport(t *testing.T) {
	pricing := writeFixtureTable(t)
	report := filepath.Join(t.TempDir(), "report.md")
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--source", filepath.Join("testdata", "openrouter.json"),
		"--pricing", pricing, "--report", report, "--date", fixtureDate,
	}, &stdout, &stderr, fixedNow)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "wrote 6 price change(s)") {
		t.Fatalf("stdout = %s", stdout.String())
	}
	got, err := os.ReadFile(pricing)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `const priceTableDate = "2026-02-04"`) {
		t.Fatal("pricing.go not rewritten")
	}
	if md, err := os.ReadFile(report); err != nil || !strings.Contains(string(md), "### Changes (6)") {
		t.Fatalf("report: err=%v body=%s", err, md)
	}

	// Second run: no changes, exit 0, file untouched.
	stdout.Reset()
	code = run([]string{"--source", filepath.Join("testdata", "openrouter.json"), "--pricing", pricing, "--date", fixtureDate}, &stdout, &stderr, fixedNow)
	if code != 0 || !strings.Contains(stdout.String(), "pricetable: no changes") {
		t.Fatalf("second run exit %d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	again, _ := os.ReadFile(pricing)
	if !bytes.Equal(got, again) {
		t.Fatal("no-change run modified pricing.go")
	}
}

func TestRunDryRunLeavesFile(t *testing.T) {
	pricing := writeFixtureTable(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--source", filepath.Join("testdata", "openrouter.json"), "--pricing", pricing, "--dry-run"}, &stdout, &stderr, fixedNow)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "dry run, 6 price change(s) not written") {
		t.Fatalf("stdout = %s", stdout.String())
	}
	// Without --date, "today" comes from the injected clock.
	if !strings.Contains(stdout.String(), "2026-01-15 → 2026-02-04") {
		t.Fatalf("expected injected clock date in report:\n%s", stdout.String())
	}
	got, _ := os.ReadFile(pricing)
	if string(got) != fixtureTable {
		t.Fatal("dry run modified pricing.go")
	}
}

func TestRunCheckAgeMode(t *testing.T) {
	pricing := writeFixtureTable(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--pricing", pricing, "--check-age", "60"}, &stdout, &stderr, fixedNow); code != 0 {
		t.Fatalf("check-age 60 exit %d: %s", code, stderr.String())
	}
	if code := run([]string{"--pricing", pricing, "--check-age", "7"}, &stdout, &stderr, fixedNow); code != 1 {
		t.Fatalf("check-age 7 exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "20 days old") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestRunErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--bogus"}, &stdout, &stderr, fixedNow); code != 2 {
		t.Fatalf("bad flag exit %d, want 2", code)
	}
	if code := run([]string{"--pricing", filepath.Join(t.TempDir(), "missing.go")}, &stdout, &stderr, fixedNow); code != 1 {
		t.Fatalf("missing pricing exit %d, want 1", code)
	}
	pricing := writeFixtureTable(t)
	if code := run([]string{"--pricing", pricing, "--source", filepath.Join(t.TempDir(), "missing.json")}, &stdout, &stderr, fixedNow); code != 1 {
		t.Fatalf("missing source exit %d, want 1", code)
	}
	if code := run([]string{"--pricing", pricing, "--source", filepath.Join("testdata", "openrouter.json"), "--date", "yesterday"}, &stdout, &stderr, fixedNow); code != 1 {
		t.Fatalf("bad --date exit %d, want 1", code)
	}
}

func TestFetchSourceLocalFile(t *testing.T) {
	raw, err := fetchSource(context.Background(), filepath.Join("testdata", "openrouter.json"))
	if err != nil || !bytes.Contains(raw, []byte("anthropic/claude-opus-4.8")) {
		t.Fatalf("local fetch: err=%v len=%d", err, len(raw))
	}
	_, err = fetchSource(context.Background(), filepath.Join(t.TempDir(), "nope.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file err = %v", err)
	}
}

func TestFetchSourceHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/down" {
			http.Error(w, "nope", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"a/b","pricing":{"prompt":"0.000001","completion":"0.000002"}}]}`))
	}))
	defer srv.Close()
	raw, err := fetchSource(context.Background(), srv.URL+"/api/v1/models")
	if err != nil || !bytes.Contains(raw, []byte(`"a/b"`)) {
		t.Fatalf("http fetch: err=%v body=%s", err, raw)
	}
	if _, err := fetchSource(context.Background(), srv.URL+"/down"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("503 should error, got %v", err)
	}
	// End to end through run with an HTTP source.
	pricing := writeFixtureTable(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--pricing", pricing, "--source", srv.URL + "/api/v1/models", "--dry-run"}, &stdout, &stderr, fixedNow); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}
