package ioscan_test

// Opt-in classifier benchmark (hivecommons/hive#19459).
//
// scorecard_test.go measures the deterministic rules only. This file runs
// ioscan.Classifier.Score over the labelled corpus in testdata/classifier-bench
// and reports, per backend, catch rate at the warn and block thresholds, the
// false-positive rate, errors, and latency. Score applies the production input
// path (prepareClassifierInput) itself and thresholds go through
// ioscan.ActionForScore, so the numbers match what boot would enforce.
//
// The benchmark spends real model calls, so it never runs in the default test
// run. Enable it with IOSCAN_CLASSIFIER_BENCH=1:
//
//	IOSCAN_CLASSIFIER_BENCH=1 IOSCAN_CLASSIFIER_BENCH_OPENAI_ENDPOINT=... \
//	  go test ./pkg/ioscan/ -run TestClassifierBench -v
//
// Rows with split "heldout" are reported separately and must never be used to
// tune thresholds or the classifier prompt.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/ioscan"
)

const (
	classifierBenchDir     = "testdata/classifier-bench"
	classifierBenchEnv     = "IOSCAN_CLASSIFIER_BENCH"
	classifierBenchBackend = "IOSCAN_CLASSIFIER_BENCH_BACKENDS"
	classifierBenchOpenAI  = "IOSCAN_CLASSIFIER_BENCH_OPENAI_ENDPOINT"
	classifierBenchModel   = "IOSCAN_CLASSIFIER_BENCH_MODEL"
)

// benchCase is one labelled row of a classifier-bench JSONL file.
type benchCase struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Split  string `json:"split"`
	Source string `json:"source"`
	Text   string `json:"text"`
}

// parseBenchCases decodes JSONL, rejecting unknown fields, bad labels or
// splits, missing fields and duplicate ids.
func parseBenchCases(name string, data []byte) ([]benchCase, error) {
	var out []benchCase
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		var c benchCase
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", name, line, err)
		}
		switch {
		case c.ID == "":
			return nil, fmt.Errorf("%s:%d: missing id", name, line)
		case c.Label != "attack" && c.Label != "benign":
			return nil, fmt.Errorf("%s:%d: label %q must be attack or benign", name, line, c.Label)
		case c.Split != "dev" && c.Split != "heldout":
			return nil, fmt.Errorf("%s:%d: split %q must be dev or heldout", name, line, c.Split)
		case c.Source == "":
			return nil, fmt.Errorf("%s:%d: missing source", name, line)
		case strings.TrimSpace(c.Text) == "":
			return nil, fmt.Errorf("%s:%d: empty text", name, line)
		case seen[c.ID]:
			return nil, fmt.Errorf("%s:%d: duplicate id %s", name, line, c.ID)
		}
		seen[c.ID] = true
		out = append(out, c)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// loadBenchCorpus reads every *.jsonl under testdata/classifier-bench.
func loadBenchCorpus(dir string) ([]benchCase, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var all []benchCase
	seen := map[string]bool{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		cases, err := parseBenchCases(filepath.Base(f), data)
		if err != nil {
			return nil, err
		}
		for _, c := range cases {
			if seen[c.ID] {
				return nil, fmt.Errorf("%s: id %s repeated across files", filepath.Base(f), c.ID)
			}
			seen[c.ID] = true
			all = append(all, c)
		}
	}
	return all, nil
}

func TestParseBenchCasesRejectsBadRows(t *testing.T) {
	good := `{"id":"a","label":"attack","split":"dev","source":"s","text":"x"}`
	if cs, err := parseBenchCases("g", []byte(good+"\n\n")); err != nil || len(cs) != 1 {
		t.Fatalf("good row: %v %v", cs, err)
	}
	bad := map[string]string{
		"label":     `{"id":"a","label":"evil","split":"dev","source":"s","text":"x"}`,
		"split":     `{"id":"a","label":"attack","split":"test","source":"s","text":"x"}`,
		"id":        `{"label":"attack","split":"dev","source":"s","text":"x"}`,
		"source":    `{"id":"a","label":"attack","split":"dev","text":"x"}`,
		"text":      `{"id":"a","label":"attack","split":"dev","source":"s","text":" "}`,
		"unknown":   `{"id":"a","label":"attack","split":"dev","source":"s","text":"x","extra":1}`,
		"duplicate": good + "\n" + good,
	}
	for name, body := range bad {
		if _, err := parseBenchCases(name, []byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestClassifierBenchCorpusLoads(t *testing.T) {
	cases, err := loadBenchCorpus(classifierBenchDir)
	if err != nil {
		t.Fatal(err)
	}
	var attack, benign, dev, heldout int
	for _, c := range cases {
		if c.Label == "attack" {
			attack++
		} else {
			benign++
		}
		if c.Split == "dev" {
			dev++
		} else {
			heldout++
		}
	}
	if attack == 0 || benign == 0 || dev == 0 || heldout == 0 {
		t.Fatalf("corpus needs both labels and both splits: attack=%d benign=%d dev=%d heldout=%d", attack, benign, dev, heldout)
	}
}

// benchStats accumulates one (backend, split) cell.
type benchStats struct {
	attacks, benign         int
	caughtWarn, caughtBlock int
	falseWarn, falseBlock   int
	errors                  int
	latency                 time.Duration
	scored                  int
}

// add runs one case and folds it into s. A classifier error counts as
// an error, not as a catch or a pass.
func (s *benchStats) add(ctx context.Context, cl ioscan.Classifier, c benchCase, th ioscan.Thresholds) {
	if c.Label == "attack" {
		s.attacks++
	} else {
		s.benign++
	}
	start := time.Now()
	score, err := cl.Score(ctx, c.Text)
	s.latency += time.Since(start)
	if err != nil {
		s.errors++
		return
	}
	s.scored++
	warn := ioscan.ActionForScore(score, th, true) != ioscan.ClassifierActionAllow
	block := ioscan.ActionForScore(score, th, true) == ioscan.ClassifierActionBlock
	if c.Label == "attack" {
		if warn {
			s.caughtWarn++
		}
		if block {
			s.caughtBlock++
		}
		return
	}
	if warn {
		s.falseWarn++
	}
	if block {
		s.falseBlock++
	}
}

func benchPct(n, d int) string {
	if d == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(d))
}

// runClassifierBench scores the corpus and renders one markdown table. Rows
// are split by dev and heldout; usage and dollar cost are not reported because
// the chat client does not surface token counts.
func runClassifierBench(ctx context.Context, backends map[string]ioscan.Classifier, cases []benchCase, th ioscan.Thresholds) string {
	names := make([]string, 0, len(backends))
	for n := range backends {
		names = append(names, n)
	}
	sort.Strings(names)
	th = th.Normalize()
	var b strings.Builder
	fmt.Fprintf(&b, "| backend | split | attacks | benign | catch@warn | catch@block | FP@warn | FP@block | errors | mean latency |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|---|---|---|---|\n")
	for _, n := range names {
		for _, split := range []string{"dev", "heldout"} {
			var s benchStats
			for _, c := range cases {
				if c.Split == split {
					s.add(ctx, backends[n], c, th)
				}
			}
			mean := "n/a"
			if s.scored+s.errors > 0 {
				mean = (s.latency / time.Duration(s.scored+s.errors)).Round(time.Millisecond).String()
			}
			fmt.Fprintf(&b, "| %s | %s | %d | %d | %s | %s | %s | %s | %d | %s |\n", n, split, s.attacks, s.benign,
				benchPct(s.caughtWarn, s.attacks), benchPct(s.caughtBlock, s.attacks),
				benchPct(s.falseWarn, s.benign), benchPct(s.falseBlock, s.benign), s.errors, mean)
		}
	}
	return b.String()
}

type fixedClassifier struct {
	byText map[string]float64
	err    error
}

func (f fixedClassifier) Score(_ context.Context, text string) (ioscan.InjectionScore, error) {
	if f.err != nil {
		return ioscan.InjectionScore{}, f.err
	}
	return ioscan.InjectionScore{Score: f.byText[text]}, nil
}

func TestClassifierBenchReportMath(t *testing.T) {
	cases := []benchCase{
		{ID: "a1", Label: "attack", Split: "dev", Source: "s", Text: "a1"},
		{ID: "a2", Label: "attack", Split: "dev", Source: "s", Text: "a2"},
		{ID: "b1", Label: "benign", Split: "dev", Source: "s", Text: "b1"},
		{ID: "h1", Label: "attack", Split: "heldout", Source: "s", Text: "h1"},
	}
	cl := fixedClassifier{byText: map[string]float64{"a1": 0.9, "a2": 0.6, "b1": 0.6, "h1": 0.1}}
	got := runClassifierBench(context.Background(), map[string]ioscan.Classifier{"fake": cl}, cases, ioscan.Thresholds{})
	for _, want := range []string{
		"| fake | dev | 2 | 1 | 100.0% | 50.0% | 100.0% | 0.0% | 0 |",
		"| fake | heldout | 1 | 0 | 0.0% | 0.0% | n/a | n/a | 0 |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q:\n%s", want, got)
		}
	}
	failing := fixedClassifier{err: fmt.Errorf("down")}
	got = runClassifierBench(context.Background(), map[string]ioscan.Classifier{"bad": failing}, cases, ioscan.Thresholds{})
	if !strings.Contains(got, "| bad | dev | 2 | 1 | 0.0% | 0.0% | 0.0% | 0.0% | 3 |") {
		t.Errorf("errors must be counted, not treated as passes or catches:\n%s", got)
	}
}

// TestClassifierBench is the opt-in live benchmark. It is skipped unless
// IOSCAN_CLASSIFIER_BENCH=1.
func TestClassifierBench(t *testing.T) {
	if os.Getenv(classifierBenchEnv) != "1" {
		t.Skipf("set %s=1 to run the live classifier benchmark", classifierBenchEnv)
	}
	cases, err := loadBenchCorpus(classifierBenchDir)
	if err != nil {
		t.Fatal(err)
	}
	model := strings.TrimSpace(os.Getenv(classifierBenchModel))
	backends := map[string]ioscan.Classifier{}
	if ep := strings.TrimSpace(os.Getenv(classifierBenchOpenAI)); ep != "" {
		cl, err := ioscan.NewLLMClassifier(ioscan.LLMClassifierConfig{Endpoint: ep, APIKey: os.Getenv("OPENAI_API_KEY"), Model: model})
		if err != nil {
			t.Fatal(err)
		}
		backends["openai-compatible"] = cl
	}
	for _, name := range strings.Fields(os.Getenv(classifierBenchBackend)) {
		cl, err := ioscan.NewCLIClassifier(name, model, 1_000_000, time.Now)
		if err != nil {
			t.Fatalf("backend %s: %v (toolless backends: %v)", name, err, ioscan.ToollessBackends())
		}
		backends[name] = cl
	}
	if len(backends) == 0 {
		t.Fatalf("no backend configured: set %s or %s", classifierBenchOpenAI, classifierBenchBackend)
	}
	report := runClassifierBench(context.Background(), backends, cases, ioscan.Thresholds{})
	t.Logf("\n%s", report)
	if p := os.Getenv("GITHUB_STEP_SUMMARY"); p != "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
			fmt.Fprintf(f, "### ioscan classifier benchmark\n\n%s\n", report)
			f.Close()
		}
	}
}
