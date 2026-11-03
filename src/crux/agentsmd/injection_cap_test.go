package agentsmd

import (
	"strconv"
	"strings"
	"testing"
)

// #12129: the AGENTS.md block is byte-capped at heading boundaries. Whole
// sections are kept or dropped — a rule is never clipped mid-sentence — and a
// dropped section is named in a visible warning the agent reads before the
// surviving rules.

func section(title string, bodyLen int) string {
	return "## " + title + "\n\n" + strings.Repeat("r", bodyLen) + "\n\n"
}

func TestRender_UnderCapIsCompleteAndWarningFree(t *testing.T) {
	cfg := &AgentsConfig{
		Body:   "Preamble rule.\n\n" + section("Build", 200) + section("Tests", 200),
		Skills: map[string]string{"go-testing": "Use table tests."},
	}
	out, omitted := cfg.Render([]string{"go-testing"}, 0)
	if omitted.Any() {
		t.Fatalf("nothing should be omitted under the cap: %+v", omitted)
	}
	if strings.Contains(out, InjectionTruncatedMarker) {
		t.Errorf("warning rendered without truncation:\n%s", out)
	}
	for _, want := range []string{"Preamble rule.", "## Build", "## Tests", "## Requested Skills", "### go-testing", "Use table tests."} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	if len(out) > DefaultMaxInjectionBytes {
		t.Errorf("render %d bytes exceeds default cap", len(out))
	}
	// InjectionText is the default-capped Render.
	if got := cfg.InjectionText([]string{"go-testing"}); got != out {
		t.Errorf("InjectionText should equal Render at the default cap")
	}
}

func TestRender_CapHitDropsWholeSectionsAndWarns(t *testing.T) {
	cfg := &AgentsConfig{
		Body: section("Build", 300) + section("Huge policy", 3000) + section("Tests", 300) + section("Review etiquette", 300),
	}
	const capBytes = 2048
	out, omitted := cfg.Render(nil, capBytes)
	if len(out) > capBytes {
		t.Errorf("render %d bytes exceeds cap %d", len(out), capBytes)
	}
	if omitted.WholeFile {
		t.Error("smaller sections fit; this is not a whole-file drop")
	}
	if len(omitted.Sections) != 1 || omitted.Sections[0] != "Huge policy" {
		t.Fatalf("expected exactly the oversized section dropped, got %+v", omitted)
	}
	if omitted.Bytes < 3000 {
		t.Errorf("omitted bytes = %d, want the whole section counted", omitted.Bytes)
	}
	if strings.Contains(out, "## Huge policy") || strings.Contains(out, strings.Repeat("r", 3000)) {
		t.Errorf("oversized section must be dropped whole, not clipped:\n%s", out)
	}
	// Later, smaller sections survive a dropped one (document order kept).
	iB, iT, iR := strings.Index(out, "## Build"), strings.Index(out, "## Tests"), strings.Index(out, "## Review etiquette")
	if !(iB >= 0 && iB < iT && iT < iR) {
		t.Errorf("surviving sections missing or out of order (%d %d %d):\n%s", iB, iT, iR, out)
	}
	// The warning is one line, names the cap and the section, tells the agent
	// to read the file, and sits before the surviving rules.
	want := "> " + InjectionTruncatedMarker + " " + strconv.Itoa(capBytes) + " bytes; 1 section(s) omitted (" + strconv.Itoa(omitted.Bytes) + " bytes): \"Huge policy\" — read AGENTS.md in the repository root for the full instructions before you start.\n"
	if !strings.Contains(out, want) {
		t.Errorf("warning line wrong, want %q in:\n%s", want, out)
	}
	if iW := strings.Index(out, InjectionTruncatedMarker); iW > iB {
		t.Errorf("warning must precede the surviving rules:\n%s", out)
	}
	if iP := strings.Index(out, injectionPrecedence); iP > strings.Index(out, InjectionTruncatedMarker) {
		t.Errorf("precedence statement must still lead:\n%s", out)
	}
}

func TestRender_WholeFileDropWhenNothingFits(t *testing.T) {
	cfg := &AgentsConfig{Body: strings.Repeat("One enormous rule with no headings. ", 200)}
	out, omitted := cfg.Render(nil, 2048)
	if !omitted.WholeFile || len(omitted.Sections) != 1 || omitted.Sections[0] != injectionPreambleName {
		t.Fatalf("expected a whole-file drop of the preamble, got %+v", omitted)
	}
	if strings.Contains(out, "enormous rule") {
		t.Errorf("no part of the body may render when it does not fit:\n%s", out)
	}
	if !strings.Contains(out, injectionHeader) || !strings.Contains(out, injectionPrecedence) {
		t.Errorf("header and precedence must still render:\n%s", out)
	}
	if !strings.Contains(out, "the whole file ("+strconv.Itoa(omitted.Bytes)+" bytes) was omitted") {
		t.Errorf("whole-file warning missing:\n%s", out)
	}
	if len(out) > 2048 {
		t.Errorf("render %d bytes exceeds cap", len(out))
	}
}

func TestRender_SkillsAreDroppableUnits(t *testing.T) {
	cfg := &AgentsConfig{
		Body:   section("Build", 200),
		Skills: map[string]string{"small": "Short skill.", "big": strings.Repeat("s", 3000)},
	}
	out, omitted := cfg.Render([]string{"big", "small"}, 2048)
	if len(omitted.Sections) != 1 || omitted.Sections[0] != injectionSkillPrefix+"big" {
		t.Fatalf("expected the oversized skill dropped, got %+v", omitted)
	}
	if strings.Contains(out, "### big") || strings.Contains(out, "ssss") {
		t.Errorf("oversized skill must be dropped whole:\n%s", out)
	}
	if !strings.Contains(out, "## Requested Skills\n\n### small\n\nShort skill.\n") {
		t.Errorf("smaller skill should still be kept under its header:\n%s", out)
	}
}

func TestRender_SkillsHeaderOnlyWhenASkillIsKept(t *testing.T) {
	cfg := &AgentsConfig{
		Body:   section("Build", 200),
		Skills: map[string]string{"big": strings.Repeat("s", 3000)},
	}
	out, _ := cfg.Render([]string{"big"}, 2048)
	if strings.Contains(out, injectionSkillsHeader) {
		t.Errorf("skills header must not render when every skill was dropped:\n%s", out)
	}
}

func TestRender_HeadingsInsideCodeFencesAreNotBoundaries(t *testing.T) {
	cfg := &AgentsConfig{Body: "## Setup\n\n```sh\n# not a heading\necho hi\n```\n\n## Next\n\nrule\n"}
	secs := splitBodySections(cfg.Body)
	if len(secs) != 2 || secs[0].name != "Setup" || secs[1].name != "Next" {
		names := make([]string, len(secs))
		for i, s := range secs {
			names[i] = s.name
		}
		t.Errorf("fenced '#' line split a section: %v", names)
	}
	if !strings.Contains(secs[0].text, "# not a heading") {
		t.Errorf("fenced content must stay in its section: %q", secs[0].text)
	}
}

func TestRender_WarningNamesAtMostEightSectionsThenCounts(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 12; i++ {
		body.WriteString(section("S"+strconv.Itoa(i), 2000))
	}
	out, omitted := cfg(body.String()).Render(nil, 2048)
	if len(omitted.Sections) != 12 || !omitted.WholeFile {
		t.Fatalf("expected all 12 dropped: %+v", omitted)
	}
	// A whole-file drop says so without listing names.
	if strings.Contains(out, "\"S0\"") {
		t.Errorf("whole-file drop should not enumerate sections:\n%s", out)
	}
	// Partial drop lists up to eight and counts the rest.
	out, omitted = cfg(section("Keep", 10)+body.String()).Render(nil, 2048)
	if omitted.WholeFile || len(omitted.Sections) != 12 {
		t.Fatalf("expected 12 dropped alongside one kept: %+v", omitted)
	}
	if !strings.Contains(out, "\"S7\"") || strings.Contains(out, "\"S8\"") || !strings.Contains(out, " and 4 more") {
		t.Errorf("warning should name 8 sections then 'and 4 more':\n%s", out)
	}
	if len(out) > 2048 {
		t.Errorf("render %d bytes exceeds cap even with a long warning", len(out))
	}
}

func TestInjectionMaxBytesDefaultsAndFloor(t *testing.T) {
	if got := injectionMaxBytes(0); got != DefaultMaxInjectionBytes {
		t.Errorf("zero -> %d", got)
	}
	if got := injectionMaxBytes(10); got != minInjectionBytes {
		t.Errorf("tiny -> %d, want floor %d", got, minInjectionBytes)
	}
	if got := injectionMaxBytes(32768); got != 32768 {
		t.Errorf("configured not honoured: %d", got)
	}
}

func TestPrecedenceStatementIsOneSentence(t *testing.T) {
	// #12129 shortened the ~1.1 KB paragraph; keep it short and single-sentence
	// so it does not creep back.
	if n := len(injectionPrecedence); n > 600 {
		t.Errorf("precedence statement is %d bytes; keep it under 600", n)
	}
	if strings.Count(strings.TrimSuffix(injectionPrecedence, "."), ". ") != 0 {
		t.Errorf("precedence statement should be one sentence: %q", injectionPrecedence)
	}
}

func cfg(body string) *AgentsConfig { return &AgentsConfig{Body: body} }
