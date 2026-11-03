package github

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/sentinel"
)

const sentinelTestApp = "hive-app[bot]"

// sentinelAdoptionBody is the opening of automerge.AdoptionPRBody for a
// superseding PR (TestAdoptionPRBodyNamesTheAdoptedAuthorForSentinel pins
// that the two stay in step).
func sentinelAdoptionBody(origin, author string) string {
	return "<!-- hive:carried-fence reason=contributor-lane:workflow-path from=#" + origin + " -->\n" +
		"Supersedes #" + origin + " (adopted after CI went red and the contributor was unavailable; original commits by @" + author + " preserved).\n\nCloses #918\n"
}

func sentinelAdoptedPR(num int, head, body string, labels ...string) map[string]any {
	pr := sentinelPR(num, sentinelTestApp, head, labels...)
	pr["body"] = body
	return pr
}

// sentinelTrustApp trusts the App ("hive agent") and nobody else, as a hive
// with no trusted-author policy does.
func sentinelTrustApp(_ string, author string) SentinelAuthorTrust {
	if strings.EqualFold(author, sentinelTestApp) {
		return SentinelAuthorTrust{Trusted: true, Reason: "hive agent"}
	}
	return SentinelAuthorTrust{}
}

func sentinelWorkflowFile() []map[string]any {
	return []map[string]any{{"filename": ".github/workflows/ci.yml", "status": "modified", "patch": "+permissions: write-all\n"}}
}

func sentinelAdoptedOpts() SentinelSweepOptions {
	opts := defaultSentinelOpts(nil)
	opts.TrustedAuthor = sentinelTrustApp
	return opts
}

func sentinelAlertFor(head, author string) string {
	return SentinelComment(DecideSentinelAction("sentinel-alert", SentinelAuthorTrust{}, false), head, author, []sentinel.Finding{{Rule: "sensitive_path", Summary: "workflow", Paths: []string{".github/workflows/ci.yml"}}})
}

// The projectbluefin/utah#930 → #936 shape on a same-repo branch, which
// adoption still supersedes after #14306: the contributor's PR carries the
// blocking alert, the App's superseding PR starts at the same head. The
// superseder must get the label and an alert, not a trusted-author notice.
func TestSweepSentinelAdoptedPRJudgedByAdoptedAuthor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing []string // marker comments already on the superseder
	}{
		{name: "first evaluation"},
		{name: "replaces a notice an older hive posted for the head", existing: []string{
			SentinelComment(DecideSentinelAction("sentinel-alert", SentinelAuthorTrust{Trusted: true, Reason: "hive agent"}, false), "h1", sentinelTestApp, nil),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &sentinelFixture{
				prs: []map[string]any{
					sentinelPR(930, "kylerankin", "h1", "sentinel-alert"),
					sentinelAdoptedPR(936, "h1", sentinelAdoptionBody("930", "kylerankin")),
				},
				files:        map[int][]map[string]any{930: sentinelWorkflowFile(), 936: sentinelWorkflowFile()},
				labelExists:  true,
				comments:     map[int][]string{930: {sentinelAlertFor("h1", "kylerankin")}, 936: tc.existing},
				commentUsers: map[int][]string{930: {sentinelTestApp}, 936: {sentinelTestApp}},
			}
			srv := newSentinelServer(t, f)
			defer srv.Close()
			c := newTestClient(t, srv, "o", []string{"o/r"})
			c.SetAppBotLogin(sentinelTestApp)
			res, err := c.SweepSentinel(context.Background(), sentinelAdoptedOpts())
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if len(res.Flagged) != 1 || res.Flagged[0].Number != 936 || len(res.Notified) != 0 {
				t.Fatalf("superseder must be flagged, not notified: %+v", res)
			}
			if got := f.labels[936]; len(got) != 1 || got[0] != "sentinel-alert" {
				t.Fatalf("superseder labels = %v, want sentinel-alert", got)
			}
			if len(f.comments[936]) != 1 {
				t.Fatalf("want one marker comment on the superseder, got %d: %v", len(f.comments[936]), f.comments[936])
			}
			body := f.comments[936][0]
			for _, want := range []string{SentinelMarker + " h1", sentinelAlertHeading, "adoption of #930", "@kylerankin's"} {
				if !strings.Contains(body, want) {
					t.Errorf("alert missing %q:\n%s", want, body)
				}
			}
			if strings.Contains(body, sentinelNoticeHeading) {
				t.Errorf("alert still reads as a trusted-author notice:\n%s", body)
			}
		})
	}
}

// A maintainer who already cleared the alert on the adopted PR at this head
// is not asked again; a hive commit on top (a new head) is judged afresh.
func TestSweepSentinelAdoptedPRCarriesOriginClearanceForSameHead(t *testing.T) {
	f := &sentinelFixture{
		prs: []map[string]any{
			sentinelPR(796, "kylerankin", "h1"),
			sentinelAdoptedPR(877, "h1", sentinelAdoptionBody("796", "kylerankin")),
		},
		files:        map[int][]map[string]any{796: sentinelWorkflowFile(), 877: sentinelWorkflowFile()},
		labelExists:  true,
		comments:     map[int][]string{796: {sentinelAlertFor("h1", "kylerankin")}},
		commentUsers: map[int][]string{796: {sentinelTestApp}, 877: {sentinelTestApp}},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	c.SetAppBotLogin(sentinelTestApp)
	res, err := c.SweepSentinel(context.Background(), sentinelAdoptedOpts())
	if err != nil || len(res.Flagged) != 0 {
		t.Fatalf("cleared head must not be re-alerted: res=%+v err=%v", res, err)
	}
	if len(f.labels[877]) != 0 {
		t.Fatalf("superseder labelled despite the clearance on #796: %v", f.labels[877])
	}
	if len(f.comments[877]) != 1 || !strings.Contains(f.comments[877][0], sentinelAlertHeading) || !strings.Contains(f.comments[877][0], "from #796 after Hive alerted on this same head") {
		t.Fatalf("superseder must record the carried clearance: %v", f.comments[877])
	}

	// The record survives a restart: a fresh client stays quiet.
	c = newTestClient(t, srv, "o", []string{"o/r"})
	c.SetAppBotLogin(sentinelTestApp)
	if res, err := c.SweepSentinel(context.Background(), sentinelAdoptedOpts()); err != nil || len(res.Flagged) != 0 || len(f.labels[877]) != 0 || len(f.comments[877]) != 1 {
		t.Fatalf("restart re-alerted a carried clearance: res=%+v err=%v labels=%v comments=%d", res, err, f.labels[877], len(f.comments[877]))
	}

	// The hive pushes a fix on top: #796 never saw h2, so h2 is alerted.
	f.mu.Lock()
	f.prs[1] = sentinelAdoptedPR(877, "h2", sentinelAdoptionBody("796", "kylerankin"))
	f.mu.Unlock()
	c = newTestClient(t, srv, "o", []string{"o/r"})
	c.SetAppBotLogin(sentinelTestApp)
	res, err = c.SweepSentinel(context.Background(), sentinelAdoptedOpts())
	if err != nil || len(res.Flagged) != 1 || len(f.labels[877]) != 1 {
		t.Fatalf("new head on the superseder must be alerted: res=%+v err=%v labels=%v", res, err, f.labels[877])
	}

	// The adopted PR still carrying the label is no clearance either.
	f.mu.Lock()
	f.prs = []map[string]any{
		sentinelPR(796, "kylerankin", "h1", "sentinel-alert"),
		sentinelAdoptedPR(878, "h1", sentinelAdoptionBody("796", "kylerankin")),
	}
	f.files[878] = sentinelWorkflowFile()
	f.mu.Unlock()
	c = newTestClient(t, srv, "o", []string{"o/r"})
	c.SetAppBotLogin(sentinelTestApp)
	if res, err := c.SweepSentinel(context.Background(), sentinelAdoptedOpts()); err != nil || len(res.Flagged) != 1 || len(f.labels[878]) != 1 {
		t.Fatalf("an uncleared original must not clear the superseder: res=%+v err=%v labels=%v", res, err, f.labels[878])
	}
}

// What did not change: a trusted adopted author keeps the notice, the App's
// own PR keeps the notice, and the supersedes line counts only on a PR the
// App opened.
func TestSweepSentinelAdoptedPRUnchangedCases(t *testing.T) {
	trusted := func(_ string, author string) SentinelAuthorTrust {
		if strings.EqualFold(author, "carol") {
			return SentinelAuthorTrust{Trusted: true, Reason: "trusted author"}
		}
		return sentinelTrustApp("", author)
	}
	humanCopy := sentinelPR(51, "dave", "h1")
	humanCopy["body"] = sentinelAdoptionBody("50", "kylerankin")
	for _, tc := range []struct {
		name      string
		pr        map[string]any
		appLogin  string
		trust     func(string, string) SentinelAuthorTrust
		wantBlock bool
		wantIn    string
		wantNote  bool
	}{
		{name: "trusted adopted author", pr: sentinelAdoptedPR(51, "h1", sentinelAdoptionBody("50", "carol")), appLogin: sentinelTestApp, trust: trusted, wantIn: "author is trusted (trusted author)", wantNote: true},
		{name: "hive's own PR", pr: sentinelAdoptedPR(51, "h1", "Fixes #9\n"), appLogin: sentinelTestApp, trust: sentinelTrustApp, wantIn: "author is trusted (hive agent)"},
		{name: "line in a PR the App did not open", pr: humanCopy, appLogin: sentinelTestApp, trust: func(_ string, a string) SentinelAuthorTrust {
			return SentinelAuthorTrust{Trusted: a == "dave", Reason: "owner"}
		}, wantIn: "author is trusted (owner)"},
		{name: "App login unknown", pr: sentinelAdoptedPR(51, "h1", sentinelAdoptionBody("50", "kylerankin")), trust: sentinelTrustApp, wantIn: "author is trusted (hive agent)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &sentinelFixture{
				prs:         []map[string]any{tc.pr},
				files:       map[int][]map[string]any{51: sentinelWorkflowFile()},
				labelExists: true,
			}
			srv := newSentinelServer(t, f)
			defer srv.Close()
			c := newTestClient(t, srv, "o", []string{"o/r"})
			c.SetAppBotLogin(tc.appLogin)
			opts := defaultSentinelOpts(nil)
			opts.TrustedAuthor = tc.trust
			res, err := c.SweepSentinel(context.Background(), opts)
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if blocked := len(f.labels[51]) > 0; blocked != tc.wantBlock || len(res.Flagged) > 0 != tc.wantBlock {
				t.Fatalf("blocked=%v, want %v: res=%+v labels=%v", blocked, tc.wantBlock, res, f.labels[51])
			}
			if len(f.comments[51]) != 1 || !strings.Contains(f.comments[51][0], tc.wantIn) {
				t.Fatalf("comment missing %q: %v", tc.wantIn, f.comments[51])
			}
			if got := strings.Contains(f.comments[51][0], "adoption of #50"); got != tc.wantNote {
				t.Fatalf("adoption note present=%v, want %v: %s", got, tc.wantNote, f.comments[51][0])
			}
		})
	}
}

func TestAdoptedFrom(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		number int
		author string
	}{
		{name: "fence marker first", body: sentinelAdoptionBody("930", "kylerankin"), number: 930, author: "kylerankin"},
		{name: "a line added above it later", body: "**Depends on o/x#276.**\n\n" + sentinelAdoptionBody("912", "hanthor"), number: 912, author: "hanthor"},
		{name: "re-home cause", body: "Supersedes #7 (adopted in place, re-homed for DCO repair; original commits by @renovate[bot] preserved).\n", number: 7, author: "renovate[bot]"},
		{name: "quoted, not at line start", body: "> Supersedes #7 (x; original commits by @eve preserved).\n"},
		{name: "prose mention", body: "This supersedes #7 by @eve.\n"},
		{name: "no author", body: "Supersedes #7. `bot` opened it with the workflow `GITHUB_TOKEN`.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, author, ok := AdoptedFrom(tc.body)
			if ok != (tc.number != 0) || n != tc.number || author != tc.author {
				t.Fatalf("AdoptedFrom = (%d, %q, %v), want (%d, %q)", n, author, ok, tc.number, tc.author)
			}
		})
	}
}
