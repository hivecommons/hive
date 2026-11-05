package github

import (
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Danathar/arch-bootc#634's park, reduced: the change it asks for, quoted,
// in the file it names, and the run-failure issue it names.
const park634 = "## Proposed change (`.github/workflows/build.yml`, delete loop, ~line 810)\n\nReplace\n\n```bash\n" +
	"            if ! gh api --method DELETE \"${versions_path}/${id}\" >/dev/null; then\n" +
	"              echo \"buildcache: FAILED to delete version ${id} after removing ${removed}; stopping\" >&2\n" +
	"              exit 1\n            fi\n```\n\nwith\n\n```bash\n            deleted=0\n            for delay in 0 5 15; do\n```\n\n" +
	"Related: the auto-opened run-failure issue #633."

// #635's patch for that file: the delete line the park quotes is removed.
const patch635 = "@@ -803,11 +810,34 @@ jobs:\n" +
	"+          delete_version() {\n" +
	"           removed=0\n" +
	"-            if ! gh api --method DELETE \"${versions_path}/${id}\" >/dev/null; then\n" +
	"+            if ! delete_version \"${id}\"; then\n" +
	"               echo \"buildcache: FAILED to delete version ${id} after removing ${removed}; stopping\" >&2\n"

func TestParkLandedFile(t *testing.T) {
	quoted := parkQuotedLines(park634)
	for _, tc := range []struct {
		name    string
		patches map[string]string
		want    string
	}{
		{name: "#635 removes the quoted delete line", patches: map[string]string{".github/workflows/build.yml": patch635, "docs/ci-cd.md": "-x"}, want: ".github/workflows/build.yml"},
		{name: "same file, the quoted lines untouched", patches: map[string]string{".github/workflows/build.yml": "@@ -1 +1 @@\n-# old comment about cache cleanup timing\n+# new comment"}},
		{name: "the quoted line removed from a file the park does not name", patches: map[string]string{".github/workflows/other.yml": patch635}},
		{name: "a short line is not evidence", patches: map[string]string{".github/workflows/build.yml": "-              exit 1\n-            fi\n"}},
		{name: "added lines are not evidence", patches: map[string]string{".github/workflows/build.yml": "+            if ! gh api --method DELETE \"${versions_path}/${id}\" >/dev/null; then\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parkLandedFile(park634, quoted, tc.patches)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("parkLandedFile = %q, %v; want %q", got, ok, tc.want)
			}
		})
	}
	// A removed line that itself starts with "--" is still a removed line.
	sqlPark := "In `db/schema.sql` replace\n\n```sql\n-- drop the legacy sessions table here\n```\n"
	if f, ok := parkLandedFile(sqlPark, parkQuotedLines(sqlPark), map[string]string{"db/schema.sql": "@@ -1,2 +1,1 @@\n--- drop the legacy sessions table here\n create table x;\n"}); !ok || f != "db/schema.sql" {
		t.Fatalf("a removed \"--\" line did not count: %q %v", f, ok)
	}
	if len(parkQuotedLines("no code here, just `.github/workflows/build.yml`")) != 0 {
		t.Fatal("inline code is not a quoted change")
	}
}

func prFile(name, patch string) map[string]any { return map[string]any{"filename": name, "patch": patch} }

// arch-bootc#634: the named issue #633 closed when #635 merged; #635's patch
// removes the line the park quotes. The note names #635 and the file, and
// carries the marker the notice reads.
func TestParkRecheckNamedIssueClosedByPRThatLandedTheChange(t *testing.T) {
	f := prParkedIssue(634, park634)
	f.timeline = append(f.timeline, prCrossRef("org/repo", 633, false, "closed", parkT0.Add(time.Hour), time.Time{}, "Scheduled run of the daily image build failed on main"))
	f.closedBy = map[int]int{633: 635}
	f.prFiles = map[int][]map[string]any{635: {prFile(".github/workflows/build.yml", patch635), prFile("docs/ci-cd.md", "+doc")}}
	recheck(t, f)
	if len(f.posted) != 1 {
		t.Fatalf("posted %q", f.posted)
	}
	wantNote(t, f.posted[0],
		"- #633 was closed on 2026-10-04 by merged #635, which changed the lines this issue asks to change in `.github/workflows/build.yml` (the park reason names it).",
		"**The change this issue asks for has landed in #635.**", "`/fixed`")
	if got := parkRecheckLandedBy(f.posted[0]); got != "#635" {
		t.Fatalf("landed marker = %q, want #635:\n%s", got, f.posted[0])
	}
	if unparkMentionsRecommendation(f.posted[0]) {
		t.Fatalf("the note must not read as a recommendation for /hive approve:\n%s", f.posted[0])
	}
}

// A named PR that merged and changed the quoted lines is the landing itself.
func TestParkRecheckNamedPRMergedLandedTheChange(t *testing.T) {
	f := prParkedIssue(634, strings.Replace(park634, "the auto-opened run-failure issue #633", "PR #635", 1))
	f.timeline = append(f.timeline, prCrossRef("org/repo", 635, true, "closed", parkT0.Add(time.Hour), parkT0.Add(time.Hour), "fix(ci): retry a 5xx"))
	f.prFiles = map[int][]map[string]any{635: {prFile(".github/workflows/build.yml", patch635)}}
	recheck(t, f)
	if len(f.posted) != 1 {
		t.Fatalf("posted %q", f.posted)
	}
	wantNote(t, f.posted[0], "- #635 merged on 2026-10-04, changing the lines this issue asks to change in `.github/workflows/build.yml` (the park reason names it).", "has landed in #635")
}

// Everything short of the quoted lines landing keeps the plain note.
func TestParkRecheckNoLandingKeepsPlainNote(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*parkFake)
	}{
		{name: "the closer changed other lines", setup: func(f *parkFake) {
			f.closedBy = map[int]int{633: 635}
			f.prFiles = map[int][]map[string]any{635: {prFile(".github/workflows/build.yml", "-# a different comment entirely here\n")}}
		}},
		{name: "no merged closer", setup: func(f *parkFake) { f.closedBy = map[int]int{} }},
		{name: "the closer's files cannot be read", setup: func(f *parkFake) { f.closedBy = map[int]int{633: 635} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := prParkedIssue(634, park634)
			f.timeline = append(f.timeline, prCrossRef("org/repo", 633, false, "closed", parkT0.Add(time.Hour), time.Time{}, ""))
			f.lookups[633] = map[string]any{"number": 633, "state": "closed", "closed_at": prTS(parkT0.Add(time.Hour))}
			tc.setup(f)
			recheck(t, f)
			if len(f.posted) != 1 {
				t.Fatalf("posted %q", f.posted)
			}
			wantNote(t, f.posted[0], "- #633 was closed on 2026-10-04 (the park reason names it).")
			if strings.Contains(f.posted[0], "has landed") || parkRecheckLandedBy(f.posted[0]) != "" {
				t.Fatalf("no landing, but the note claims one:\n%s", f.posted[0])
			}
		})
	}
}

// The "What to reply" notice leads with `/fixed`, the one recommended reply,
// once the note says the change landed; an accepted reply after it clears it.
func TestUnparkMenuOffersFixedWhenChangeLanded(t *testing.T) {
	c := &Client{}
	c.SetAppBotLogin("hive[bot]")
	hive := &gh.User{Login: gh.Ptr("hive[bot]")}
	body := "Apply the change below.\n\nRecommendation: apply it.\n" + unparkOwnerStepMarker
	note := parkRecheckNote("stage/045-needs-decision", parkT0, []parkChange{{ref: "#633", verb: "was closed", at: parkT0.Add(time.Hour), named: true, landedBy: "#635", landedFile: ".github/workflows/build.yml"}})
	comments := []*gh.IssueComment{{User: hive, Body: gh.Ptr(note)}}
	m := c.unparkMenuFor(body, hive, comments)
	replies := m.replies(false)
	if len(replies) == 0 || replies[0].command != "/fixed" || !replies[0].recommended || !strings.Contains(replies[0].does, "#635") {
		t.Fatalf("first reply = %+v, want /fixed recommended naming #635", replies)
	}
	for _, r := range replies[1:] {
		if r.recommended {
			t.Fatalf("only /fixed is recommended once the change landed: %+v", replies)
		}
	}
	if got := renderUnparkReplies(replies); strings.Count(got, "(recommended)") != 1 {
		t.Fatalf("one bold line expected:\n%s", got)
	}
	comments = append(comments, &gh.IssueComment{User: hive, Body: gh.Ptr("Un-parked.\n" + unparkAcceptedMarker)})
	if m := c.unparkMenuFor(body, hive, comments); m.landedBy != "" {
		t.Fatalf("an accepted reply after the note must clear the offer, got %q", m.landedBy)
	}
	if m := c.unparkMenuFor(body, hive, nil); m.landedBy != "" || m.replies(false)[0].command == "/fixed" {
		t.Fatalf("no note, no /fixed offer: %+v", m.replies(false))
	}
}
