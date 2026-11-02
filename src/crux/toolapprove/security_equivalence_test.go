package toolapprove

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The spelling-equivalence gate (hivecommons/hive#16094). The destructive-rm
// detector used to match spellings of a root or home target, and every
// spelling it missed was a separate bypass. This gate derives commands from
// seeds by transformations that are exact equivalences under bash (GNU bash,
// not POSIX mode, default options; HOME is the home directory, PWD the
// working directory), composes them up to depth 2, and requires every
// derivation of a destructive seed to be flagged and every derivation of a
// safe seed to stay clean. A detector change that handles one spelling but
// not its equivalents fails here, not in a later finding.
//
// Only transformations that preserve meaning in their position are used;
// in particular:
//   - a * is never quoted or escaped (that makes it a literal, not a glob);
//   - a ~ is never quoted, and the slash right after it is never quoted
//     (either one stops tilde expansion);
//   - "" is not placed right before or after a lone ~;
//   - $HOME and $PWD are only double-quoted, never single-quoted;
//   - an option list ending in -- is never moved after the target;
//   - commands are wrapped only in constructs that run them (bash -c, eval,
//     subshell, command substitution, group, &&, ;, &), never sh -c: dash
//     reads $'…' and braces differently.
//
// Spellings bash reads differently but the detector deliberately flags
// (~\/, /\*, "/*", an unknown option word) are pinned separately in
// TestDestructiveRmConservativeReadings, so the gate never claims them as
// equivalences.

// equivPieceKind is the meaning of one part of a target word.
type equivPieceKind int

const (
	equivSlash      equivPieceKind = iota // a / separator
	equivTildeSlash                       // the / right after a leading ~
	equivText                             // a literal name, . or ..
	equivGlob                             // an unquoted glob: *, *.tmp
	equivTilde                            // a leading ~
	equivHome                             // a leading $HOME
	equivPWD                              // a leading $PWD
)

type equivPiece struct {
	kind equivPieceKind
	text string
}

var (
	pSlash      = equivPiece{kind: equivSlash}
	pTildeSlash = equivPiece{kind: equivTildeSlash}
	pTilde      = equivPiece{kind: equivTilde}
	pHome       = equivPiece{kind: equivHome}
	pPWD        = equivPiece{kind: equivPWD}
	pStar       = equivPiece{kind: equivGlob, text: "*"}
)

func pText(s string) equivPiece { return equivPiece{kind: equivText, text: s} }
func pGlob(s string) equivPiece { return equivPiece{kind: equivGlob, text: s} }

// spellings lists the bash-equivalent spellings of a piece; the first is
// canonical.
func (p equivPiece) spellings() []string {
	switch p.kind {
	case equivSlash:
		return []string{"/", `\/`, `'/'`, `"/"`, `$'/'`, `$'\x2f'`, `$'\057'`, `$'\u002f'`, "//", `/""`}
	case equivTildeSlash:
		return []string{"/", "//", `/""`}
	case equivText:
		t := p.text
		escaped := `\` + strings.Join(strings.Split(t, ""), `\`)
		return []string{t, "'" + t + "'", `"` + t + `"`, "$'" + t + "'", escaped, t[:1] + `""` + t[1:]}
	case equivHome:
		return []string{"$HOME", "${HOME}", `"$HOME"`, `"${HOME}"`}
	case equivPWD:
		return []string{"$PWD", "${PWD}", `"$PWD"`, `"${PWD}"`}
	default: // equivTilde, equivGlob: any quoting changes the meaning
		if p.kind == equivTilde {
			return []string{"~"}
		}
		return []string{p.text}
	}
}

// equivSeed is one rm invocation by meaning.
type equivSeed struct {
	target []equivPiece
	opts   []string // option spellings, all with the same meaning; first is canonical
}

var (
	// recursiveOpts all make rm recursive (GNU rm: clusters, any order,
	// unambiguous long-option abbreviations, --no-preserve-root, --).
	recursiveOpts = []string{
		"-rf", "-fr", "-r -f", "-R", "-Rf", "-r", "--recursive", "--recursive --force",
		"--rec", "-rf --no-preserve-root", "--no-preserve-root -r", `"-rf"`, `\-rf`,
		"$'-rf'", "-rf --", "-r -f --",
	}
	// nonRecursiveOpts never make rm recursive.
	nonRecursiveOpts = []string{"-f", "--force", "-v", "--preserve-root -f", "--", "-f --", "-d"}

	// equivCommandWords all run rm.
	equivCommandWords = []string{
		"rm", `\rm`, `r\m`, `"rm"`, "'rm'", "$'rm'", `r""m`, "/bin/rm", "/usr/bin/rm", `"/bin/rm"`,
		"command rm", "sudo rm", "env rm", "{rm,}", "r{m..m}",
	}
	// equivSeparators all separate words.
	equivSeparators = []string{" ", "\t", "  ", " \\\n"}
	// equivWrappers all run the command they wrap.
	equivWrappers = []func(string) string{
		func(c string) string { return c },
		func(c string) string { return "cd /tmp && " + c },
		func(c string) string { return c + "; echo done" },
		func(c string) string { return "(" + c + ")" },
		func(c string) string { return "{ " + c + "; }" },
		func(c string) string { return c + " &" },
		func(c string) string { return "echo $(" + c + ")" },
		func(c string) string { return `echo "$(` + c + `)"` },
		func(c string) string { return "bash -c " + singleQuote(c) },
		func(c string) string { return "eval " + singleQuote(c) },
		func(c string) string { return "sudo -u root " + c },
	}
)

// singleQuote quotes s so a shell reading the result receives s exactly.
func singleQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// wordForms lists whole-word transformations that keep a target's meaning:
// a brace expansion that repeats it or adds a safe sibling, and empty quotes
// where they do not touch a leading ~.
func wordForms(w string, tildeLead, tildeOnly bool) []string {
	forms := []string{w, w + "{,}", "{" + w + ",}", "{/tmp/x," + w + "}"}
	if !tildeLead {
		forms = append(forms, `""`+w)
	}
	if !tildeOnly {
		forms = append(forms, w+`""`)
	}
	return forms
}

// equivVariants returns every command derived from seed with at most two
// transformations applied, each from a different axis.
func equivVariants(seed equivSeed) []string {
	n := len(seed.target)
	tildeLead := n > 0 && seed.target[0].kind == equivTilde
	sizes := make([]int, 0, n+6)
	for _, p := range seed.target {
		sizes = append(sizes, len(p.spellings()))
	}
	sizes = append(sizes,
		len(wordForms("x", tildeLead, tildeLead && n == 1)),
		len(equivCommandWords), len(seed.opts), 2, len(equivSeparators), len(equivWrappers))

	render := func(choice []int) (string, bool) {
		var target strings.Builder
		for i, p := range seed.target {
			target.WriteString(p.spellings()[choice[i]])
		}
		word := wordForms(target.String(), tildeLead, tildeLead && n == 1)[choice[n]]
		opts := seed.opts[choice[n+2]]
		parts := []string{equivCommandWords[choice[n+1]], opts, word}
		if choice[n+3] == 1 {
			if strings.HasSuffix(opts, "--") {
				return "", false // operands after -- only
			}
			parts[1], parts[2] = word, opts
		}
		return equivWrappers[choice[n+5]](strings.Join(parts, equivSeparators[choice[n+4]])), true
	}

	var out []string
	choice := make([]int, len(sizes))
	emit := func() {
		if cmd, ok := render(choice); ok {
			out = append(out, cmd)
		}
	}
	emit()
	for a := range sizes {
		for x := 1; x < sizes[a]; x++ {
			choice[a] = x
			emit()
			for b := a + 1; b < len(sizes); b++ {
				for y := 1; y < sizes[b]; y++ {
					choice[b] = y
					emit()
				}
				choice[b] = 0
			}
		}
		choice[a] = 0
	}
	return out
}

func dotdots(n int) []equivPiece {
	var ps []equivPiece
	for range n {
		ps = append(ps, pSlash, pText(".."))
	}
	return ps
}

// destructiveEquivSeeds name the root, the home directory, the working
// directory, or an ancestor of one of them.
var destructiveEquivSeeds = []equivSeed{
	{target: []equivPiece{pSlash}},
	{target: []equivPiece{pSlash, pStar}},
	{target: []equivPiece{pSlash, pText(".")}},
	{target: []equivPiece{pSlash, pText("..")}},
	{target: []equivPiece{pSlash, pText("tmp"), pSlash, pText("..")}},
	{target: []equivPiece{pSlash, pText("home"), pSlash, pText(".."), pSlash}},
	{target: []equivPiece{pSlash, pText("tmp"), pSlash, pText(".."), pSlash, pStar}},
	{target: []equivPiece{pTilde}},
	{target: []equivPiece{pTilde, pTildeSlash}},
	{target: []equivPiece{pTilde, pTildeSlash, pStar}},
	{target: []equivPiece{pTilde, pTildeSlash, pText("..")}},
	{target: []equivPiece{pHome}},
	{target: []equivPiece{pHome, pSlash}},
	{target: []equivPiece{pHome, pSlash, pStar}},
	{target: append([]equivPiece{pHome}, dotdots(2)...)},
	{target: append([]equivPiece{pPWD}, dotdots(6)...)},
	{target: []equivPiece{pPWD}},
	{target: []equivPiece{pStar}},
	{target: []equivPiece{pText("."), pSlash, pStar}},
	{target: []equivPiece{pText("..")}},
}

// safeEquivSeeds are recursive removals of ordinary paths, and non-recursive
// removals of protected ones; none may be flagged.
var safeEquivSeeds = []equivSeed{
	{target: []equivPiece{pSlash, pText("tmp"), pSlash, pText("x")}},
	{target: []equivPiece{pSlash, pText("tmp"), pSlash, pText("x"), pSlash, pText("..")}},
	{target: []equivPiece{pSlash, pText("tmp"), pSlash, pText("x"), pSlash, pStar}},
	{target: []equivPiece{pSlash, pText(".cache")}},
	{target: []equivPiece{pSlash, pText("...")}},
	{target: []equivPiece{pSlash, pGlob("*.tmp")}},
	{target: []equivPiece{pGlob("*.tmp")}},
	{target: []equivPiece{pText("."), pSlash, pText("build")}},
	{target: []equivPiece{pText("build"), pSlash, pText(".."), pSlash, pText("out")}},
	{target: []equivPiece{pTilde, pTildeSlash, pText("build")}},
	{target: []equivPiece{pTilde, pTildeSlash, pText(".cache")}},
	{target: []equivPiece{pHome, pSlash, pText("build")}},
	{target: []equivPiece{pHome, pSlash, pGlob("*.tmp")}},
	{target: []equivPiece{pPWD, pSlash, pText("build")}},
	{target: []equivPiece{pSlash}, opts: nonRecursiveOpts},
	{target: []equivPiece{pTilde}, opts: nonRecursiveOpts},
	{target: []equivPiece{pHome, pSlash, pStar}, opts: nonRecursiveOpts},
}

func equivCommands(seeds []equivSeed) []string {
	var out []string
	for _, s := range seeds {
		if s.opts == nil {
			s.opts = recursiveOpts
		}
		out = append(out, equivVariants(s)...)
	}
	return out
}

// commandOnlyScanner isolates the built-in command check from the host's
// TextScanner, which would otherwise mask a missed command.
func commandOnlyScanner() *DefaultSecurityScanner {
	s := NewDefaultSecurityScanner()
	s.Text = textScannerFunc(func(string) TextVerdict { return TextVerdict{} })
	return s
}

func scanCommand(t *testing.T, s *DefaultSecurityScanner, cmd string) ScanResult {
	t.Helper()
	res, err := s.Scan(context.Background(), ToolRequest{Tool: "bash", Command: cmd})
	if err != nil {
		t.Fatalf("Scan(%q): %v", cmd, err)
	}
	return res
}

func destructiveViolation(cmd string) string {
	return fmt.Sprintf("destructive shell command detected: %q", cmd)
}

func boundsViolation(cmd string) string {
	return fmt.Sprintf("shell command exceeds destructive-command analysis bounds: %q", cmd)
}

// checkVerdicts scans every command and reports the ones whose violations
// differ from want(cmd) (nil: clean), listing at most 20.
func checkVerdicts(t *testing.T, cmds []string, want func(string) []string) {
	t.Helper()
	s := commandOnlyScanner()
	var bad []string
	for _, cmd := range cmds {
		res := scanCommand(t, s, cmd)
		w := want(cmd)
		if res.Passed != (len(w) == 0) || fmt.Sprint(res.Violations) != fmt.Sprint(w) {
			bad = append(bad, fmt.Sprintf("%q: got %q", cmd, res.Violations))
		}
	}
	if len(bad) > 0 {
		t.Errorf("%d of %d commands misjudged; first %d:\n%s", len(bad), len(cmds), min(len(bad), 20), strings.Join(bad[:min(len(bad), 20)], "\n"))
	}
}

func flagged(cmd string) []string { return []string{destructiveViolation(cmd)} }
func clean(string) []string       { return nil }

func TestDestructiveRmSpellingEquivalenceGate(t *testing.T) {
	positives, negatives := equivCommands(destructiveEquivSeeds), equivCommands(safeEquivSeeds)
	if len(positives) < 10000 || len(negatives) < 5000 {
		t.Fatalf("generator shrank: %d destructive, %d safe derivations", len(positives), len(negatives))
	}
	t.Run("destructive", func(t *testing.T) { checkVerdicts(t, positives, flagged) })
	t.Run("safe", func(t *testing.T) { checkVerdicts(t, negatives, clean) })
}

// TestDestructiveRmShellForms covers shell forms outside the generator's
// per-word axes: words split by braces or $IFS, nested commands, redirects
// and decoded escapes.
func TestDestructiveRmShellForms(t *testing.T) {
	checkVerdicts(t, []string{
		"{rm,-rf,/}",
		"rm${IFS}-rf${IFS}/",
		"rm -rf {/tmp,/}",
		"rm -rf /tmp/../../..",
		"rm -rf $'/\\0tmp'",
		"rm -rf $'\\x2f'tmp/..",
		"rm -rf \"$PWD\"",
		"rm -rf .",
		"rm -rf $PWD/x/../..",
		"rm -rf />/dev/null",
		"rm -rf / 2>&1",
		"rm -rf x &>/dev/null /",
		"`rm -rf /`",
		"x=$(rm -rf ~)",
		"echo \"$(rm -rf /)\"",
		"echo \"`rm -rf /`\"",
		"echo ${X:-$(rm -rf /)}",
		"rm -rf $(true) /",
		"rm -rf `true` /",
		"rm -rf $(true) ~",
		"rm $(true) -rf /",
		"rm -rf $(echo x; rm -rf /) y",
		"bash -c \"rm -rf $HOME\"",
		"bash -c 'bash -c \"rm -rf /\"'",
		"ssh host 'rm -rf ~/'",
		"find . -name x | xargs rm -rf /",
		"git submodule foreach rm -rf /",
		"git bisect run rm -rf ~",
		"git status && rm -rf .",
		"rm -rf / 'unterminated",
		"rm -rf \\\n/",
	}, flagged)
	checkVerdicts(t, []string{
		"rm -rf \\$HOME",
		"rm -rf '$HOME'",
		"rm -rf \\~",
		"rm -rf \"~\"",
		"rm -rf '~'/",
		"rm -rf $HOME_backup",
		"rm -rf ${HOME}x",
		"rm -rf $HOME.bak",
		"rm -rf $HOMEDIR/",
		"rm -rf $PWD/build/../out",
		"rm -rf /tmp/x/../y",
		"rm -rf ''",
		"rm \\-- -rf /",
		"rmdir -- /",
		"rm -f /",
		"rm -f '~'",
		"a=rm -rf /",
		"echo {/,}",
		"echo rm -rf /tmp",
		"rm -rf {/tmp/a,/tmp/b}",
		"rm -rf /tmp/{a..c}",
		"bash -c 'rm -rf /tmp/x'",
		"echo \"$(rm -rf /tmp/x)\"",
		"rm -rf $(true)/",
		"rm -f $(true) /tmp/x",
		"git rm -r --cached .",
		"git rm -r .",
		"git -C . rm -r --cached .",
		"git --no-pager rm -rf .",
		"sudo git rm -r --cached .",
		"/usr/bin/git rm -r --cached .",
		"hg rm -r .",
		"svn rm -r .",
	}, clean)
}

// TestDestructiveRmConservativeReadings pins the documented cases where the
// detector deliberately reads a command as destructive although bash would
// not, or cannot know: a quoted * is still read as a glob, a ~ before a
// quoted slash is still read as home, and an unknown word in rm's options
// may expand to -r.
func TestDestructiveRmConservativeReadings(t *testing.T) {
	checkVerdicts(t, []string{
		"rm -rf /\\*",
		"rm -rf \"/*\"",
		"rm -rf ~\\/",
		"rm -rf ~\"/\"",
		"rm $OPTS /",
		"rm $OPTS -- ~",
	}, flagged)
}

// TestDestructiveRmAnalysisBounds checks the explicit limits: inputs within
// them are judged normally, inputs past them are reported as outside the
// analysis bounds instead of passing clean.
func TestDestructiveRmAnalysisBounds(t *testing.T) {
	nestBash := func(cmd string, levels int) string {
		for range levels {
			cmd = "bash -c " + singleQuote(cmd)
		}
		return cmd
	}
	nestedBraces := func(levels int) string {
		return strings.Repeat("{a,", levels) + "b" + strings.Repeat("}", levels)
	}
	within := []string{
		"echo {1.." + fmt.Sprint(maxBraceWords) + "}",
		"echo " + strings.Repeat("{a,b}", 10), // 1024 words
		"echo " + nestedBraces(maxBraceDepth/2),
		nestBash("echo hi", maxShellNesting),
		// Reaches MaxInt64 without wrapping past it: 8 ordinary words.
		"rm -rf {9223372036854775800..9223372036854775807}",
	}
	checkVerdicts(t, within, clean)
	checkVerdicts(t, []string{
		nestBash("rm -rf /", maxShellNesting),
		"rm -rf " + strings.Repeat("{/,/}", 10), // 1024 words
	}, flagged)

	past := []string{
		"echo {1.." + fmt.Sprint(maxBraceWords+1) + "}",
		"echo " + strings.Repeat("{a,b}", 11), // 2048 words
		"echo " + nestedBraces(maxBraceDepth+2),
		nestBash("echo hi", maxShellNesting+1),
		// Extreme sequence bounds overflow int64 arithmetic; each must be
		// refused rather than looping forever.
		"rm -rf {-9223372036854775808..9223372036854775807}",
		"rm -rf {-9223372036854775000..9223372036854775807}",
		"rm -rf {1..2..-9223372036854775808}",
	}
	checkVerdicts(t, past, func(cmd string) []string { return []string{boundsViolation(cmd)} })

	// A long benign script (a heredoc with quotes, braces and $-words, as an
	// agent writing a file sends it) stays well inside the work budget.
	line := `fmt.Printf("it's {%s,%d} $x\n", a, b) // don't` + "\n"
	script := "cat > main.go <<'EOF'\n" + strings.Repeat(line, 4000) + "EOF\n"
	checkVerdicts(t, []string{script}, clean)
}
