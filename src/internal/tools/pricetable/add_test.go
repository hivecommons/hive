package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// addTable is a miniature pricing.go for --add: two Fable rows, one Haiku
// and one Opus row as siblings, and a non-Claude row last so a Claude model
// with no sibling must still land among the Claude rows.
const addTable = `package tokens

const priceTableDate = "2026-11-09"

type ModelPrice struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheReadPerMTok  float64
	CacheWritePerMTok float64
}

var modelPrices = map[string]ModelPrice{
	// ---- Anthropic ----
	"claude-opus-5":    {InputPerMTok: 5.00, OutputPerMTok: 25.00, CacheReadPerMTok: 0.50, CacheWritePerMTok: 6.25},
	"claude-fable-5":   {InputPerMTok: 10.00, OutputPerMTok: 50.00, CacheReadPerMTok: 1.00, CacheWritePerMTok: 12.50},
	"claude-fable-5-1": {InputPerMTok: 10.00, OutputPerMTok: 50.00, CacheReadPerMTok: 0.25, CacheWritePerMTok: 12.50},
	"claude-haiku-4-5": {InputPerMTok: 1.00, OutputPerMTok: 5.00, CacheReadPerMTok: 0.10, CacheWritePerMTok: 1.25},

	// ---- OpenAI ----
	"gpt-5-4": {InputPerMTok: 2.50, OutputPerMTok: 15.00, CacheReadPerMTok: 0.25, CacheWritePerMTok: 2.50},
}
`

const addIDs = "claude-fable-5-2,anthropic/claude-haiku-5.5,claude-opus-6,claude-sonnet-6,claude-nova-1,claude-fable-5-1,claude-fable-9"

func addCatalogue(t *testing.T) *catalogue {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "openrouter-add.json"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := parseOpenRouter(raw)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPlanAdditions(t *testing.T) {
	tb, err := parseTable([]byte(addTable))
	if err != nil {
		t.Fatal(err)
	}
	r := planAdditions(tb, addCatalogue(t), strings.Split(addIDs, ","))

	var added []string
	sib := map[string]string{}
	for _, a := range r.Added {
		added = append(added, a.Key)
		sib[a.Key] = a.Sibling
	}
	if got, want := strings.Join(added, ","), "claude-fable-5-2,claude-haiku-5-5,claude-nova-1"; got != want {
		t.Fatalf("added = %s, want %s", got, want)
	}
	for key, want := range map[string]string{"claude-fable-5-2": "claude-fable-5-1", "claude-haiku-5-5": "claude-haiku-4-5", "claude-nova-1": ""} {
		if sib[key] != want {
			t.Errorf("%s compared with %q, want %q", key, sib[key], want)
		}
	}
	if got := strings.Join(r.Present, ","); got != "claude-fable-5-1" {
		t.Errorf("present = %s, want claude-fable-5-1 (an existing row is never repriced by --add)", got)
	}
	if len(r.Missing) != 1 || r.Missing[0].Key != "claude-fable-9" {
		t.Errorf("missing = %+v, want only claude-fable-9", r.Missing)
	}
	held := map[string]string{}
	for _, s := range r.Held {
		held[s.Key] = strings.Join(s.Rules, "; ")
	}
	if len(held) != 2 {
		t.Fatalf("held = %v, want claude-opus-6 and claude-sonnet-6", held)
	}
	if !strings.Contains(held["claude-opus-6"], "InputPerMTok moves more than 5× (5.00 → 100.00)") {
		t.Errorf("claude-opus-6 must be held against its sibling claude-opus-5: %s", held["claude-opus-6"])
	}
	if !strings.Contains(held["claude-sonnet-6"], "source publishes no cache-read rate") {
		t.Errorf("claude-sonnet-6 must be held for its missing cache-read rate: %s", held["claude-sonnet-6"])
	}
}

func TestRunAddWritesRowsAndIsIdempotent(t *testing.T) {
	pricing := filepath.Join(t.TempDir(), "pricing.go")
	if err := os.WriteFile(pricing, []byte(addTable), 0o644); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(t.TempDir(), "add.md")
	args := []string{"--source", filepath.Join("testdata", "openrouter-add.json"), "--pricing", pricing, "--date", "2026-11-17", "--add", addIDs}
	var stdout, stderr bytes.Buffer
	if code := run(append(args, "--report", report), &stdout, &stderr, fixedNow); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "added 3 new row(s)") {
		t.Fatalf("stdout = %s", stdout.String())
	}
	got, err := os.ReadFile(pricing)
	if err != nil {
		t.Fatal(err)
	}
	src := string(got)
	tb, err := parseTable(got)
	if err != nil {
		t.Fatalf("rewritten table does not parse: %v\n%s", err, src)
	}
	if tb.Date != "2026-11-09" {
		t.Errorf("priceTableDate = %s; --add must not move it", tb.Date)
	}
	row := func(key string) string {
		e, ok := tb.entry(key)
		if !ok {
			t.Fatalf("%s missing from:\n%s", key, src)
		}
		return formatPrice(e.Fields[fieldInput].Value) + "/" + formatPrice(e.Fields[fieldOutput].Value) + "/" +
			formatPrice(e.Fields[fieldCacheRead].Value) + "/" + formatPrice(e.Fields[fieldCacheWrite].Value)
	}
	for key, want := range map[string]string{
		"claude-fable-5-2": "10.00/50.00/1.00/12.50",
		"claude-haiku-5-5": "1.00/5.00/0.10/1.25",
		"claude-nova-1":    "2.00/8.00/0.20/2.50",
		"claude-fable-5-1": "10.00/50.00/0.25/12.50",
	} {
		if got := row(key); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	for _, key := range []string{"claude-opus-6", "claude-sonnet-6", "claude-fable-9"} {
		if _, ok := tb.entry(key); ok {
			t.Errorf("%s must not be added", key)
		}
	}
	// Placement: after the sibling's line, and a sibling-less Claude row stays
	// among the Claude rows, before the OpenAI section.
	if !(strings.Index(src, `"claude-fable-5-1"`) < strings.Index(src, `"claude-fable-5-2"`) &&
		strings.Index(src, `"claude-fable-5-2"`) < strings.Index(src, `"claude-haiku-4-5"`)) {
		t.Errorf("claude-fable-5-2 not placed after claude-fable-5-1:\n%s", src)
	}
	if !(strings.Index(src, `"claude-nova-1"`) < strings.Index(src, "---- OpenAI ----")) {
		t.Errorf("claude-nova-1 not placed among the Claude rows:\n%s", src)
	}
	if !strings.Contains(src, "// claude-fable-5-2: list price from the source's quote for anthropic/claude-fable-5.2 (added 2026-11-17 by pricetable --add).") {
		t.Errorf("provenance comment missing:\n%s", src)
	}
	if md, err := os.ReadFile(report); err != nil || !strings.Contains(string(md), "### Added (3)") || !strings.Contains(string(md), "### Held for review (2)") {
		t.Fatalf("report: err=%v body=%s", err, md)
	}

	// Second run with the same ids: nothing to add, file byte-identical.
	stdout.Reset()
	if code := run(args, &stdout, &stderr, fixedNow); code != 0 {
		t.Fatalf("second run exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "pricetable: no new rows") {
		t.Fatalf("second run stdout = %s", stdout.String())
	}
	again, _ := os.ReadFile(pricing)
	if !bytes.Equal(got, again) {
		t.Fatal("second --add run modified pricing.go")
	}
}

func TestRunAddDryRunLeavesFile(t *testing.T) {
	pricing := filepath.Join(t.TempDir(), "pricing.go")
	if err := os.WriteFile(pricing, []byte(addTable), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"--source", filepath.Join("testdata", "openrouter-add.json"), "--pricing", pricing, "--dry-run", "--add", "claude-fable-5-2"}, &stdout, &stderr, fixedNow)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "dry run, 1 new row(s) not written") {
		t.Fatalf("stdout = %s", stdout.String())
	}
	got, _ := os.ReadFile(pricing)
	if string(got) != addTable {
		t.Fatal("dry run modified pricing.go")
	}
}
