package github

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/priorart"
)

func priorArtTestCheckout(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init")
	for path, text := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
	return root
}

func priorArtTestIndex(checkouts ...PriorArtCheckout) []priorart.Source {
	return NewPriorArtCodeIndex(func() []PriorArtCheckout { return checkouts }, 0).Sources()
}

func priorArtTestQuery() priorart.Query {
	return priorart.Query{
		Repo:        "org/a",
		Terms:       []priorart.Term{{Text: "widget_cap", Weight: priorart.WeightIdentifier, Identifier: true, FromTitle: true}, {Text: "widget", Weight: priorart.WeightTitle, FromTitle: true}},
		Identifiers: []string{"widget_cap"},
	}
}

func priorArtRefs(cands []priorart.Candidate) []string {
	var out []string
	for _, c := range cands {
		out = append(out, string(c.Kind)+" "+c.Ref)
	}
	sort.Strings(out)
	return out
}

// TestPriorArtCodeIndexCoversEveryRegisteredRepo pins #13878: the docs and
// code sources search every registered checkout locally, so they answer
// with no GitHub client at all.
func TestPriorArtCodeIndexCoversEveryRegisteredRepo(t *testing.T) {
	var nilClient *Client
	if srcs := nilClient.PriorArtSources(); srcs != nil {
		t.Fatalf("nil client forge sources = %v, want nil", srcs)
	}
	a := priorArtTestCheckout(t, map[string]string{
		"pkg/gate/gate.go":      "package gate\n\n// widget_cap bounds the widget.\nconst widget_cap = 3\n",
		"pkg/gate/gate_test.go": "package gate\n// widget_cap test\n",
		"README.md":             "The widget_cap setting bounds widgets.\n",
	})
	b := priorArtTestCheckout(t, map[string]string{
		"lib/widget.py":   "widget_cap = 5  # widget limit\n",
		"docs/widgets.md": "# Widgets\nSet widget_cap to bound the widget.\n",
		"other.go":        "package other\n",
	})
	srcs := priorArtTestIndex(PriorArtCheckout{Repo: "org/a", Root: a}, PriorArtCheckout{Repo: "org/b", Root: b})
	if len(srcs) != 2 || srcs[0].Name() != "docs" || srcs[1].Name() != "code" {
		t.Fatalf("sources = %v, want docs and code", srcs)
	}
	ctx := context.Background()

	docs, err := srcs[0].Search(ctx, priorArtTestQuery())
	if err != nil {
		t.Fatalf("docs search: %v", err)
	}
	if got, want := priorArtRefs(docs), []string{"doc org/a/README.md", "doc org/b/docs/widgets.md"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("docs = %v, want %v", got, want)
	}

	code, err := srcs[1].Search(ctx, priorArtTestQuery())
	if err != nil {
		t.Fatalf("code search: %v", err)
	}
	if got, want := priorArtRefs(code), []string{"code org/a/pkg/gate/gate.go", "code org/b/lib/widget.py"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("code = %v, want %v", got, want)
	}
	for _, c := range code {
		if !strings.HasPrefix(c.URL, "https://github.com/org/") || !strings.Contains(c.URL, "/blob/") {
			t.Fatalf("candidate URL %q is not revision-pinned", c.URL)
		}
		if f, ok := priorart.Rank(priorArtTestQuery().Terms, c); !ok || f.Confidence < priorart.DefaultMinConfidence {
			t.Fatalf("candidate %s does not rank: %+v ok=%v", c.Ref, f, ok)
		}
	}

	none, err := srcs[1].Search(ctx, priorart.Query{Repo: "org/a", Terms: []priorart.Term{{Text: "widget"}}})
	if err != nil || len(none) != 0 {
		t.Fatalf("code search without identifiers = %v, %v; want nothing", none, err)
	}
}

// TestPriorArtCodeIndexReportsMissingCheckouts pins that a missing, unset or
// dirty checkout is a coverage gap on the report, not silent absence.
func TestPriorArtCodeIndexReportsMissingCheckouts(t *testing.T) {
	a := priorArtTestCheckout(t, map[string]string{"pkg/gate.go": "// widget_cap bounds the widget\n"})
	dirty := priorArtTestCheckout(t, map[string]string{"main.go": "package main\n"})
	if err := os.WriteFile(filepath.Join(dirty, "main.go"), []byte("package main // edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcs := priorArtTestIndex(
		PriorArtCheckout{Repo: "org/a", Root: a},
		PriorArtCheckout{Repo: "org/gone", Root: filepath.Join(t.TempDir(), "absent")},
		PriorArtCheckout{Repo: "org/unset"},
		PriorArtCheckout{Repo: "org/dirty", Root: dirty},
	)

	cands, err := srcs[1].Search(context.Background(), priorArtTestQuery())
	var gap *priorart.CoverageError
	if !errors.As(err, &gap) {
		t.Fatalf("err = %v, want *priorart.CoverageError", err)
	}
	joined := strings.Join(gap.Gaps, "\n")
	for _, want := range []string{"checkout missing for org/gone", "checkout missing for org/unset", "checkout partial for org/dirty"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("gaps %q missing %q", gap.Gaps, want)
		}
	}
	if strings.Contains(joined, "org/a") {
		t.Fatalf("gaps %q name the healthy checkout", gap.Gaps)
	}
	if got := priorArtRefs(cands); len(got) != 1 || got[0] != "code org/a/pkg/gate.go" {
		t.Fatalf("candidates beside the gaps = %v, want the healthy repo's hit", got)
	}

	report := priorart.NewSearcher(priorart.Options{Sources: srcs}).Find(context.Background(), priorart.Issue{
		Repo:  "org/a",
		Title: "Add `widget_cap` to bound the widget",
		Body:  "The widget needs a `widget_cap` so the widget stays bounded.",
	})
	if report.Errors != 0 || len(report.Sources) != 2 {
		t.Fatalf("report sources=%v errors=%d, want both answered", report.Sources, report.Errors)
	}
	if len(report.Coverage) != len(gap.Gaps) || !strings.Contains(strings.Join(report.Coverage, "\n"), "checkout missing for org/gone") {
		t.Fatalf("report coverage = %q, want the deduplicated gaps %q", report.Coverage, gap.Gaps)
	}
	if body := priorart.RenderParkComment(report); !strings.Contains(body, "Search coverage incomplete") || !strings.Contains(body, "checkout missing for org/gone") {
		t.Fatalf("park comment does not surface the gap:\n%s", body)
	}
}
