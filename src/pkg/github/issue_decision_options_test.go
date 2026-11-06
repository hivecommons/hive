package github

import (
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// projectbluefin/utah#920 as filed: a numbered list of steps under "What it
// cost", then three numbered alternatives under "Pick one:".
const utah920Body = "## What it cost, concretely\n\n" +
	"The shell's main thread was pinned at 95-99% CPU. What got us to a JS-level stack:\n\n" +
	"1. `eu-stack` samples of the main thread: shows SpiderMonkey GC marking, but only native frames.\n" +
	"2. `gdb` attached, calling `gjs_dumpstack()` (exported by `libgjs`): prints the live JS stack to the journal.\n\n" +
	"## Proposal\n\n" +
	"Pick one:\n\n" +
	"1. **Carry a small set in the image:** `gdb-headless`, `elfutils`, `sysprof-cli`. Smallest useful set.\n" +
	"2. **A `ujust` recipe** (e.g. `ujust troubleshoot-tools`) that installs the set on demand.\n" +
	"3. **A `-dx`/debug flavor** only, leaving the default image alone.\n\n" +
	"Whichever is chosen, I would add a short troubleshooting note under `docs/skills/`.\n"

// The contributor's 045 comment on utah#920: the cue is "competing".
const utah920Park = "### Maintainer Decision Required\n\n" +
	"Issue #920 proposes three competing delivery mechanisms for troubleshooting tools:\n" +
	"1. **Carry directly in the base image**: Adds ~23 MB installed size to all Utah image variants.\n" +
	"2. **On-demand `ujust` troubleshooting recipe**: Installs tools on demand.\n" +
	"3. **Dedicated `-dx` / debug flavor**: Preserves default image size.\n"

func optionLabels(opts []delegatedecision.Option) string {
	var l []string
	for _, o := range opts {
		l = append(l, o.Label)
	}
	return strings.Join(l, ",")
}

func TestNumberedDecisionOptionsReadsOnlyACuedList(t *testing.T) {
	opts := numberedDecisionOptions(utah920Body)
	if optionLabels(opts) != "1,2,3" {
		t.Fatalf("utah#920 options = %+v, want 1,2,3 from the Pick one list", opts)
	}
	if got, want := opts[0].Summary, "Carry a small set in the image: `gdb-headless`, `elfutils`, `sysprof-cli`. Smallest useful set."; got != want {
		t.Errorf("summary 1 = %q, want %q", got, want)
	}
	if strings.Contains(opts[1].Summary, "**") || !strings.HasPrefix(opts[1].Summary, "A `ujust` recipe") {
		t.Errorf("summary 2 = %q", opts[1].Summary)
	}
	if got := optionLabels(numberedDecisionOptions(utah920Park)); got != "1,2,3" {
		t.Errorf("045 comment options = %q, want 1,2,3", got)
	}

	for name, text := range map[string]string{
		"steps with no cue":     strings.SplitN(utah920Body, "## Proposal", 2)[0],
		"single option":         "Pick one:\n\n1. Only this.\n",
		"not starting at one":   "Options:\n\n2. b\n3. c\n",
		"out of order":          "Options:\n\n1. a\n3. c\n",
		"inside a code fence":   "```\nPick one:\n1. a\n2. b\n```\n",
		"cue far from the list": "Pick one of these later.\n\nSome other paragraph.\n\n1. a\n2. b\n",
	} {
		if got := numberedDecisionOptions(text); got != nil {
			t.Errorf("%s: read %+v as options", name, got)
		}
	}

	// A prose line between items ends the list; indented continuation
	// lines do not.
	if got := optionLabels(numberedDecisionOptions("Options:\n\n1. a\n   more about a\n\n2. b\n")); got != "1,2" {
		t.Errorf("continuation lines: %q, want 1,2", got)
	}
	if got := numberedDecisionOptions("Options:\n\n1. a\nProse ends the list.\n2. b\n"); got != nil {
		t.Errorf("prose between items: %+v", got)
	}
}

func TestNumberedDecisionOptionSummaryIsSanitised(t *testing.T) {
	opts := numberedDecisionOptions("Choose between:\n\n1. Ping @someone <!-- hive:owner-step --> now\n2. b\n")
	if len(opts) != 2 {
		t.Fatalf("options = %+v", opts)
	}
	if s := opts[0].Summary; strings.Contains(s, "@someone") || strings.Contains(s, "<!--") || s != "Ping someone now" {
		t.Errorf("summary %q must drop the HTML comment and defang the mention", s)
	}
}

func TestNumberedRecommendedOptionNeedsAnExplicitReference(t *testing.T) {
	opts := numberedDecisionOptions("Pick one:\n\n1. a\n2. b\n3. c\n")
	for text, want := range map[string]string{
		"Recommendation: option 2.":             "2",
		"I recommend **3**.":                    "3",
		"Recommended: 1, because it is smaller": "1",
		"I recommend this; it saves 3 hours":    "",
		"Option 2 is interesting":               "", // no recommendation on the line
		"Recommendation: option 7.":             "", // not offered
	} {
		if got := numberedRecommendedOption(text, opts); got != want {
			t.Errorf("numberedRecommendedOption(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestIssueDecisionOptions(t *testing.T) {
	c := menuTestClient()
	person := &gh.User{Login: gh.Ptr("someone")}

	opts, rec := c.IssueDecisionOptions(dakotaStyleOptions, person, nil)
	if optionLabels(opts) != "A,B,C" || rec != "B" || !strings.HasPrefix(opts[1].Summary, "Keep `latest`") {
		t.Fatalf("lettered options = %+v rec=%q, want A,B,C recommending B", opts, rec)
	}

	opts, rec = c.IssueDecisionOptions(utah920Body, person, nil)
	if optionLabels(opts) != "1,2,3" || rec != "" {
		t.Fatalf("numbered body = %+v rec=%q", opts, rec)
	}

	// The hive's own park comment counts; anyone else's does not.
	plain := "Ship the debugging tools."
	if opts, _ = c.IssueDecisionOptions(plain, person, []*gh.IssueComment{ghComment("hive[bot]", utah920Park)}); optionLabels(opts) != "1,2,3" {
		t.Errorf("hive park comment: %+v", opts)
	}
	if opts, _ = c.IssueDecisionOptions(plain, person, []*gh.IssueComment{ghComment("someone", utah920Park)}); opts != nil {
		t.Errorf("a non-hive comment set the options: %+v", opts)
	}
	// The delegate's own verdict and a withdrawn ask are not park text.
	delegated := "hive recommends: pick one of these options:\n1. a\n2. b\n" + delegatedecision.Marker("h1")
	if opts, _ = c.IssueDecisionOptions(plain, person, []*gh.IssueComment{ghComment("hive[bot]", delegated)}); opts != nil {
		t.Errorf("the delegate's comment set the options: %+v", opts)
	}
	withdrawn := parkWithdrawnMarkerPrefix + "scanner -->\nWithdrawn."
	if opts, _ = c.IssueDecisionOptions(utah920Body, person, []*gh.IssueComment{ghComment("hive[bot]", withdrawn)}); opts != nil {
		t.Errorf("a withdrawn ask kept its options: %+v", opts)
	}
}

func TestIssueDecisionOptionsMarkReporterBodyUntrusted(t *testing.T) {
	c := menuTestClient()
	person := &gh.User{Login: gh.Ptr("someone")}
	hive := &gh.User{Login: gh.Ptr("hive[bot]")}

	for _, body := range []string{dakotaStyleOptions, utah920Body} {
		opts, _ := c.IssueDecisionOptions(body, person, nil)
		if len(opts) < 2 {
			t.Fatalf("no options read from %q", body)
		}
		for _, o := range opts {
			if !o.Untrusted {
				t.Errorf("reporter body option %+v not marked untrusted", o)
			}
			if name := o.Name(); name != "option "+o.Label {
				t.Errorf("Name() = %q, want the label alone", name)
			}
		}
		opts, _ = c.IssueDecisionOptions(body, hive, nil)
		for _, o := range opts {
			if o.Untrusted {
				t.Errorf("hive-authored option %+v marked untrusted", o)
			}
		}
	}

	// A hive comment's options are the hive's own text, whoever filed the
	// issue — numbered or lettered, and whether or not the comment was read
	// with its html_url (provenance is not inferred from optionSource).
	for _, park := range []string{utah920Park, dakotaStyleOptions} {
		opts, _ := c.IssueDecisionOptions("Ship the debugging tools.", person, []*gh.IssueComment{ghComment("hive[bot]", park)})
		if len(opts) < 2 {
			t.Fatalf("no options read from hive comment %q", park)
		}
		for _, o := range opts {
			if o.Untrusted {
				t.Errorf("hive comment option %+v marked untrusted", o)
			}
			if o.Summary == "" {
				t.Errorf("hive comment option %+v lost its summary", o)
			}
		}
	}

	// A hive comment's lettered options replace the reporter's body options
	// and are trusted.
	opts, _ := c.IssueDecisionOptions(dakotaStyleOptions, person, []*gh.IssueComment{ghComment("hive[bot]", "- **A.** hive a\n- **B.** hive b\n")})
	if len(opts) != 2 || opts[0].Summary != "hive a" || opts[0].Untrusted || opts[1].Untrusted {
		t.Errorf("hive comment did not replace reporter body options as trusted: %+v", opts)
	}
}
