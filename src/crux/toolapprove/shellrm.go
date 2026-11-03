package toolapprove

import (
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file reads a shell command the way bash would hand it to rm, without
// running or evaluating any of it, and reports whether a recursive rm removes
// a protected directory. It replaces matching the spelling of the target with
// matching what the target means, so the many ways of writing the same path
// (quotes, escapes, $'…', brace expansion, . and .. components, $HOME, ~)
// are judged as one; see security_equivalence_test.go for the gate that
// keeps it that way.
//
// Semantics. Words are read as GNU bash (not POSIX mode, default shell
// options) reads them: single, double, $'…' and $"…" quoting, backslash
// escapes and line continuations, brace expansion ({a,b} and {x..y[..n]}
// sequences) before tilde and parameter expansion, tilde expansion of a
// leading unquoted ~, and the $HOME, $PWD and $IFS parameters. Command
// separators (; & | newline and ( )) split simple commands, so subshells
// are read in place. A command substitution ($(…) or `…`), quoted or not,
// leaves an unknown value in the word holding it and its text is re-read as
// a nested command, so the enclosing simple command keeps every operand
// around it (rm -rf $(true) / is still rm -rf /). Every word anywhere in a
// simple command may be the rm command word, so sudo rm, env rm, command
// rm, xargs rm and /bin/rm are covered without a list of wrappers. GNU rm
// option parsing is modelled: options may follow operands,
// -- ends them, a short-option cluster holding r or R and any unambiguous
// abbreviation of --recursive (--r, --rec, …) make the removal recursive.
//
// Protected targets. A recursive rm is destructive when an operand, after
// lexical cleaning, names the filesystem root, the home directory or an
// ancestor of it, or the working directory or an ancestor of it ($PWD, ., ..,
// $PWD/../.., …). A final component made only of * and ? with at least one *
// (every non-hidden entry) is read as the directory it is in, so /*, ~/* and
// * stay destructive. Cleaning is lexical: it does not resolve symlinks, so
// /tmp/.. is read as / even on a host where /tmp is a symlink; reading the
// ambiguous form as the destructive one is deliberate.
//
// Conservative readings. A * is read as a glob even when quoted or escaped
// (/\* and "/*" stay flagged). A leading unquoted ~ followed by a quoted
// slash (~\/, ~"/") is read as home although bash leaves it literal. An rm
// option word holding an unknown expansion ($OPTS, $(…)) may expand to -r,
// so it counts as recursive. Comments are read as code. Every word holding a
// quoted or escaped character is also re-read as a command one level deeper
// (bash -c '…', eval "…", ssh host '…' hand that text to another shell),
// and so is the text of a command substitution or a ${…} expansion that is
// not a plain name.
//
// Empty expansions. A tool-issued command has no positional parameters, so
// $1, $@ and $* expand to nothing (rm -rf /$1 is rm -rf /). A command
// substitution glued to a path may print nothing, so it is dropped when
// judging the operand (rm -rf $(true)/ is refused).
//
// Not resolved. The value of any other parameter ($X, ${X:-…}, $0, …),
// command substitution output, other users' homes (~user), bracket globs,
// aliases, functions and commands assembled at run time (x=rm; $x -rf /)
// are unknown to a static reader; an operand holding one is not judged
// destructive. rm is the only command modelled here, and an rm that is the
// subcommand of a version-control tool (git rm -r --cached ., hg rm) is not
// the binary; mkfs and dd from
// /dev/zero are matched by destructiveCmdRe, and other deleting commands
// (find -delete, rsync --delete, shred) are left to the host's TextScanner.
//
// Bounds. Reading is linear in the input, with these explicit limits; a
// command that hits any of them is reported as outside the analysis bounds
// (a violation of its own) instead of being judged clean:
//   - one source word may brace-expand to at most maxBraceWords words, and
//     brace groups may nest maxBraceDepth deep;
//   - quoted text and command substitutions are re-read at most
//     maxShellNesting levels deep;
//   - the whole analysis may process at most shellWorkBudget(len(cmd))
//     bytes, counting re-reads and brace-expanded output.
const (
	maxBraceWords   = 1024
	maxBraceDepth   = 32
	maxShellNesting = 8
)

// shellWorkBudget is the number of bytes one analysis may process: every
// byte read, re-read or produced by brace expansion counts once.
func shellWorkBudget(cmdLen int) int { return 16*cmdLen + 1<<16 }

// shUnit is one character of a shell word after quote removal, or one
// parameter/opaque expansion in it.
type shUnit struct {
	c    byte // the literal byte, or the shParam* code for uParam
	kind uint8
}

const (
	uLit    uint8 = iota // unquoted literal byte: may be a brace, glob or tilde
	uQuoted              // quoted or escaped literal byte
	uParam               // a known parameter: c is shParamHome, shParamPWD or shParamIFS
	uOpaque              // an expansion whose value is unknown statically
	uSubst               // a command substitution: unknown, and may expand to nothing
)

const (
	shParamHome = 'H'
	shParamPWD  = 'P'
	shParamIFS  = 'I'
)

// shParamText spells a known parameter for a nested read.
var shParamText = map[byte]string{shParamHome: "${HOME}", shParamPWD: "${PWD}", shParamIFS: "${IFS}"}

var shOpaqueUnit = shUnit{kind: uOpaque}

var shSubstUnit = shUnit{kind: uSubst}

// rmAnalysis carries the work budget and the verdict across nested reads.
type rmAnalysis struct {
	budget    int
	overBound bool
}

// analyzeRmCommand reports whether cmd holds a recursive rm of a protected
// directory, and whether reading cmd hit one of the analysis bounds.
func analyzeRmCommand(cmd string) (destructive, overBound bool) {
	a := rmAnalysis{budget: shellWorkBudget(len(cmd))}
	destructive = a.command(cmd, 0)
	return destructive, a.overBound
}

// charge spends n bytes of the work budget and reports whether any is left.
func (a *rmAnalysis) charge(n int) bool {
	a.budget -= n
	if a.budget < 0 {
		a.overBound = true
	}
	return !a.overBound
}

// command reads src as a list of simple commands at nesting depth and
// reports whether any of them is a destructive rm.
func (a *rmAnalysis) command(src string, depth int) bool {
	if depth > maxShellNesting {
		a.overBound = true
		return false
	}
	if !a.charge(len(src)) {
		return false
	}
	l := shLexer{a: a, src: src, depth: depth}
	l.run()
	return l.hit
}

// nested re-reads text as a command one level deeper when it can hold one.
func (a *rmAnalysis) nested(text string, depth int) bool {
	if !strings.ContainsAny(text, " \t\n;&|()<>`$'\"\\{}") {
		return false
	}
	return a.command(text, depth+1)
}

// shLexer splits one command string into words and simple commands.
type shLexer struct {
	a      *rmAnalysis
	src    string
	depth  int
	args   [][]shUnit
	word   []shUnit
	inWord bool
	hit    bool
}

func (l *shLexer) stop() bool { return l.hit || l.a.overBound }

func (l *shLexer) add(u shUnit) {
	l.word = append(l.word, u)
	l.inWord = true
}

func (l *shLexer) endWord() {
	if l.inWord {
		l.args = append(l.args, l.word)
	}
	l.word, l.inWord = nil, false
}

func (l *shLexer) endCommand() {
	l.endWord()
	if len(l.args) > 0 && !l.stop() {
		l.hit = l.a.simple(l.args, l.depth)
	}
	l.args = l.args[:0]
}

func (l *shLexer) run() {
	src := l.src
	for i := 0; i < len(src) && !l.stop(); {
		switch c := src[i]; c {
		case ' ', '\t':
			l.endWord()
			i++
		case '\n', ';', '(', ')':
			l.endCommand()
			i++
		case '`':
			i = l.backtick(i + 1)
		case '&':
			if i+1 < len(src) && src[i+1] == '>' { // &> and &>> redirect
				l.endWord()
				i = skipRedirect(src, i+1)
				continue
			}
			l.endCommand()
			i++
		case '|':
			l.endCommand()
			i++
		case '<', '>':
			l.endWord()
			i = skipRedirect(src, i)
		case '\\':
			switch {
			case i+1 == len(src):
				l.add(shUnit{c: c, kind: uLit})
				i++
			case src[i+1] == '\n':
				i += 2
			default:
				l.add(shUnit{c: src[i+1], kind: uQuoted})
				i += 2
			}
		case '\'':
			end := closeIndex(src, i+1, '\'')
			l.addQuoted(src[i+1 : end])
			i = end + 1
		case '"':
			i = l.doubleQuoted(i + 1)
		case '$':
			i = l.dollar(i, false)
		default:
			l.add(shUnit{c: c, kind: uLit})
			i++
		}
	}
	l.endCommand()
}

// skipRedirect returns the index after the redirection operator at i (<, >,
// >>, >&, <&, >|, <>, <<, <<<, &>, &>>); its target is read as an ordinary
// word, which can only add findings.
func skipRedirect(src string, i int) int {
	i++
	for i < len(src) && strings.IndexByte("<>&|", src[i]) >= 0 {
		i++
	}
	return i
}

// closeIndex returns the index of the first quote byte at or after i, or
// len(src) when the quote is unterminated: the rest is read as quoted, which
// keeps every word before it in the analysis.
func closeIndex(src string, i int, quote byte) int {
	if j := strings.IndexByte(src[i:], quote); j >= 0 {
		return i + j
	}
	return len(src)
}

func (l *shLexer) addQuoted(s string) {
	l.inWord = true
	for i := range len(s) {
		l.word = append(l.word, shUnit{c: s[i], kind: uQuoted})
	}
}

// doubleQuoted reads a "…" string starting after the opening quote and
// returns the index after the closing one.
func (l *shLexer) doubleQuoted(i int) int {
	src := l.src
	l.inWord = true
	for i < len(src) && !l.stop() {
		switch c := src[i]; c {
		case '"':
			return i + 1
		case '\\':
			if i+1 < len(src) && strings.IndexByte("$`\"\\\n", src[i+1]) >= 0 {
				if src[i+1] != '\n' {
					l.add(shUnit{c: src[i+1], kind: uQuoted})
				}
				i += 2
				continue
			}
			l.add(shUnit{c: c, kind: uQuoted})
			i++
		case '$':
			i = l.dollar(i, true)
		case '`':
			i = l.backtick(i + 1)
		default:
			l.add(shUnit{c: c, kind: uQuoted})
			i++
		}
	}
	return len(src)
}

// backtick reads a `…` command substitution starting after the opening
// backtick and returns the index after the closing one. The substitution
// leaves an unknown value in the word holding it, so the enclosing simple
// command keeps its other words; its text is re-read as a nested command.
func (l *shLexer) backtick(i int) int {
	end := closeIndex(l.src, i, '`')
	l.hit = l.a.nested(l.src[i:end], l.depth)
	l.add(shSubstUnit)
	return end + 1
}

// dollar reads the expansion starting with the $ at i and returns the index
// after it. inDouble is true inside "…".
func (l *shLexer) dollar(i int, inDouble bool) int {
	src := l.src
	lit := uLit
	if inDouble {
		lit = uQuoted
	}
	if i+1 == len(src) {
		l.add(shUnit{c: '$', kind: lit})
		return i + 1
	}
	switch n := src[i+1]; {
	case n == '\'' && !inDouble:
		return l.ansiC(i + 2)
	case n == '"' && !inDouble:
		return l.doubleQuoted(i + 2)
	case n == '(':
		// The substitution leaves an unknown value in the word holding
		// it, so the enclosing simple command keeps its other words
		// (rm -rf $(true) / is still rm -rf /); its text is re-read as a
		// nested command.
		l.add(shSubstUnit)
		end := closeParen(src, i+2)
		l.hit = l.a.nested(src[i+2:min(end, len(src))], l.depth)
		return end + 1
	case n == '{':
		end := closeBrace(src, i+2)
		inner := src[i+2 : min(end, len(src))]
		if isShellName(inner) {
			l.param(inner, inDouble)
		} else {
			l.add(shOpaqueUnit)
			l.hit = l.a.nested(inner, l.depth)
		}
		return end + 1
	case n == '_' || isASCIILetter(n):
		j := i + 2
		for j < len(src) && (src[j] == '_' || isASCIILetter(src[j]) || isASCIIDigit(src[j])) {
			j++
		}
		l.param(src[i+1:j], inDouble)
		return j
	case (isASCIIDigit(n) && n != '0') || n == '@' || n == '*':
		// A tool-issued command has no positional parameters, so these
		// expand to nothing; the word stays present, as "$1" is "".
		l.inWord = true
		return i + 2
	case isASCIIDigit(n) || strings.IndexByte("#?-$!", n) >= 0:
		l.add(shOpaqueUnit)
		return i + 2
	default:
		l.add(shUnit{c: '$', kind: lit})
		return i + 1
	}
}

// param adds the parameter name. An unquoted $IFS splits the word the way
// default-IFS word splitting does.
func (l *shLexer) param(name string, inDouble bool) {
	switch name {
	case "HOME":
		l.add(shUnit{c: shParamHome, kind: uParam})
	case "PWD":
		l.add(shUnit{c: shParamPWD, kind: uParam})
	case "IFS":
		if !inDouble {
			l.endWord()
			return
		}
		l.add(shUnit{c: shParamIFS, kind: uParam})
	default:
		l.add(shOpaqueUnit)
	}
}

// ansiC decodes a $'…' string starting after the opening quote, as bash
// does, and returns the index after the closing quote. A decoded NUL ends
// the value, as it does in bash.
func (l *shLexer) ansiC(i int) int {
	src := l.src
	l.inWord = true
	ended := false
	emit := func(c byte) {
		if ended = ended || c == 0; !ended {
			l.word = append(l.word, shUnit{c: c, kind: uQuoted})
		}
	}
	for i < len(src) {
		c := src[i]
		if c == '\'' {
			return i + 1
		}
		if c != '\\' || i+1 == len(src) {
			emit(c)
			i++
			continue
		}
		e := src[i+1]
		i += 2
		if b := ansiCSimpleEscape[e]; b != 0 {
			emit(b)
			continue
		}
		switch {
		case e == 'c':
			if i < len(src) {
				emit(src[i] & 0x1f)
				i++
			}
		case e == 'x' || e == 'u' || e == 'U':
			digits := 8
			switch e {
			case 'x':
				digits = 2
			case 'u':
				digits = 4
			}
			j := i
			for j < len(src) && j-i < digits && isHexDigit(src[j]) {
				j++
			}
			if j == i {
				emit('\\')
				emit(e)
				continue
			}
			v, _ := strconv.ParseUint(src[i:j], 16, 32)
			i = j
			if e == 'x' {
				emit(byte(v))
				continue
			}
			var buf [utf8.UTFMax]byte
			for _, b := range buf[:utf8.EncodeRune(buf[:], rune(v))] {
				emit(b)
			}
		case e >= '0' && e <= '7':
			j := i - 1
			for j < len(src) && j-(i-1) < 3 && src[j] >= '0' && src[j] <= '7' {
				j++
			}
			v, _ := strconv.ParseUint(src[i-1:j], 8, 16)
			emit(byte(v))
			i = j
		default:
			emit('\\')
			emit(e)
		}
	}
	return len(src)
}

// ansiCSimpleEscape maps the single-character $'…' escapes to their byte.
var ansiCSimpleEscape = [256]byte{
	'a': 7, 'b': 8, 'e': 27, 'E': 27, 'f': 12, 'n': '\n', 'r': '\r', 't': '\t', 'v': 11,
	'\\': '\\', '\'': '\'', '"': '"', '?': '?',
}

// closeParen returns the index of the ) closing a $( opened before i,
// skipping quoted text and nested parentheses, or len(src).
func closeParen(src string, i int) int {
	depth := 1
	for ; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++
		case '\'', '"':
			i = closeIndex(src, i+1, src[i])
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return len(src)
}

// closeBrace returns the index of the } closing a ${ opened before i, or
// len(src).
func closeBrace(src string, i int) int {
	depth := 1
	for ; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return len(src)
}

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isASCIIDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isHexDigit(c byte) bool {
	return isASCIIDigit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func isShellName(s string) bool {
	if s == "" || isASCIIDigit(s[0]) {
		return false
	}
	for i := range len(s) {
		if c := s[i]; c != '_' && !isASCIILetter(c) && !isASCIIDigit(c) {
			return false
		}
	}
	return true
}

// simple brace-expands one simple command's words, checks every rm in it,
// and re-reads quoted words one level deeper.
func (a *rmAnalysis) simple(words [][]shUnit, depth int) bool {
	argv := make([][]shUnit, 0, len(words))
	for _, w := range words {
		for _, e := range a.braceExpand(w, 0) {
			if len(e) > 0 {
				argv = append(argv, e)
			}
		}
		if a.overBound {
			return false
		}
	}
	for i, w := range argv {
		if isRmCommandWord(w) && !isVCSSubcommand(argv, i) && rmRemovesProtected(argv[i+1:]) {
			return true
		}
	}
	for _, w := range argv {
		if hasQuotedUnit(w) && a.nested(renderUnits(w), depth) {
			return true
		}
		if a.overBound {
			return false
		}
	}
	return false
}

// hasQuotedUnit reports whether w holds a quoted or escaped byte, the only
// words whose text another shell would read differently.
func hasQuotedUnit(w []shUnit) bool {
	for _, u := range w {
		if u.kind == uQuoted {
			return true
		}
	}
	return false
}

// renderUnits spells w as the text a shell receiving it as an argument
// (bash -c, eval) would read.
func renderUnits(w []shUnit) string {
	var b strings.Builder
	for _, u := range w {
		switch u.kind {
		case uLit, uQuoted:
			b.WriteByte(u.c)
		case uParam:
			b.WriteString(shParamText[u.c])
		default:
			b.WriteString("${_}")
		}
	}
	return b.String()
}

// literalWord returns w as a plain string when it holds no expansion.
func literalWord(w []shUnit) (string, bool) {
	b := make([]byte, 0, len(w))
	for _, u := range w {
		if u.kind != uLit && u.kind != uQuoted {
			return "", false
		}
		b = append(b, u.c)
	}
	return string(b), true
}

// isRmCommandWord reports whether w names the rm binary, bare or by path
// (/bin/rm, /usr/bin/rm, ./rm), after quote removal (\rm, "rm", r\m).
func isRmCommandWord(w []shUnit) bool {
	s, ok := literalWord(w)
	return ok && strings.EqualFold(path.Base(s), "rm")
}

// vcsCommands are the version-control tools whose rm subcommand untracks
// files instead of running the rm binary.
var vcsCommands = map[string]bool{
	"git": true, "hg": true, "svn": true, "bzr": true, "brz": true,
	"cvs": true, "fossil": true, "darcs": true, "jj": true,
}

// isVCSSubcommand reports whether the rm word at argv[i] is the rm
// subcommand of a version-control tool (git rm -r --cached ., hg rm,
// svn rm): the tool's word must precede it, with only the tool's own option
// words in between (git -C dir rm, git --no-pager rm). An rm after a
// non-option word is still the binary (git submodule foreach rm -rf /,
// git bisect run rm -rf /, sudo rm -rf /).
func isVCSSubcommand(argv [][]shUnit, i int) bool {
	for j := i - 1; j >= 0; j-- {
		s, ok := literalWord(argv[j])
		if !ok {
			return false
		}
		if vcsCommands[strings.ToLower(path.Base(s))] {
			return true
		}
		if strings.HasPrefix(s, "-") {
			continue
		}
		// -C <path> and -c <name>=<value> take the next word as their operand.
		if j > 0 {
			if prev, ok := literalWord(argv[j-1]); ok && (prev == "-C" || prev == "-c") {
				j--
				continue
			}
		}
		return false
	}
	return false
}

// rmRemovesProtected reports whether args, the words after an rm command
// word, make a recursive removal of a protected directory.
func rmRemovesProtected(args [][]shUnit) bool {
	recursive, protected, endOfOptions := false, false, false
	for _, w := range args {
		if !endOfOptions {
			arg, ok := literalWord(w)
			if !ok && hasOpaqueUnit(w) {
				recursive = true // may expand to -r
			}
			if ok && len(arg) > 1 && arg[0] == '-' {
				switch {
				case arg == "--":
					endOfOptions = true
				case strings.HasPrefix(arg, "--"):
					if len(arg) >= 3 && strings.HasPrefix("--recursive", arg) {
						recursive = true
					}
				case strings.ContainsAny(arg[1:], "rR"):
					recursive = true
				}
				continue
			}
		}
		if isProtectedTarget(w) {
			protected = true
		}
	}
	return recursive && protected
}

func hasOpaqueUnit(w []shUnit) bool {
	for _, u := range w {
		if u.kind == uOpaque || u.kind == uSubst {
			return true
		}
	}
	return false
}

func isSlashUnit(u shUnit) bool { return u.c == '/' && (u.kind == uLit || u.kind == uQuoted) }

// isProtectedTarget reports whether the rm operand w names the root, the
// home directory or the working directory, or an ancestor of one of them.
func isProtectedTarget(w []shUnit) bool {
	// A command substitution may print nothing, so $(true)/ may be /.
	w = slices.DeleteFunc(slices.Clone(w), func(u shUnit) bool { return u.kind == uSubst })
	rest, anchored := w, false
	if len(w) > 0 && (len(w) == 1 || isSlashUnit(w[1])) {
		switch first := w[0]; {
		case first.kind == uLit && first.c == '~',
			first.kind == uParam && (first.c == shParamHome || first.c == shParamPWD):
			rest, anchored = w[1:], true
		}
	}
	p, ok := literalWord(rest)
	if !ok || (!anchored && p == "") {
		return false
	}
	return lexicalTopDepth(p) == 0
}

// lexicalTopDepth returns how many components below its anchor (the root
// for an absolute p, otherwise the anchor the caller stripped) the
// lexically cleaned p names. 0 means the anchor itself or an ancestor. A
// final every-entry glob (*, *?, **) counts as the directory holding it.
func lexicalTopDepth(p string) int {
	comps := strings.Split(p, "/")
	for len(comps) > 0 && (comps[len(comps)-1] == "" || comps[len(comps)-1] == ".") {
		comps = comps[:len(comps)-1]
	}
	depth := 0
	for i, c := range comps {
		switch {
		case c == "" || c == ".":
		case c == "..":
			if depth > 0 {
				depth--
			}
		case i == len(comps)-1 && isEveryEntryGlob(c):
		default:
			depth++
		}
	}
	return depth
}

// isEveryEntryGlob reports whether a path component is a glob that matches
// every non-hidden name: only * and ?, with at least one *.
func isEveryEntryGlob(c string) bool {
	return strings.Contains(c, "*") && strings.Trim(c, "*?") == ""
}
