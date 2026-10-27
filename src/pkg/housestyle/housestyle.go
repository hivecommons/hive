// Package housestyle resolves the house-style text prepended to every agent
// kick: the one place an operator or a repo owner writes down how changes
// should be made (diff size, abstractions, tests, sign-off) so every backend
// hears the same thing.
//
// Precedence is repo file > hive default > built-in default. The repo file is
// untrusted input — it arrives from the managed repository, not from the
// operator — so callers hand Resolve a scanner and a blocked file falls through
// to the next source rather than reaching the agent.
package housestyle

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// FileName is the repo-level style file, read from the checkout root.
const FileName = "HIVE_STYLE.md"

// MaxBytes is the default size cap. A style file is a short list of rules;
// anything larger is a policy document, and policy has AGENTS.md.
const MaxBytes = 4096

// MinMaxBytes is the floor for an operator-configured cap.
const MinMaxBytes = 512

// Source names where the applied text came from.
type Source string

const (
	// SourceRepo is HIVE_STYLE.md in the managed repository's checkout.
	SourceRepo Source = "repo"
	// SourceHive is the operator's hive-wide default (project.style or
	// project.style_file).
	SourceHive Source = "hive"
	// SourceBuiltin is the text shipped with Hive.
	SourceBuiltin Source = "built-in"
)

// Builtin is the house style every hive applies when nothing overrides it.
const Builtin = `- Make the smallest complete change that fully addresses the task; no speculative abstractions, no drive-by refactors.
- Follow the patterns already in the file and package you are editing before introducing new ones.
- Every behaviour change ships with a test that fails without it.
- Keep existing public names, ids and config keys working; add, do not rename.
- Explain why in the PR body, not what; the diff already says what.
- Sign every commit off (DCO, ` + "`git commit -s`" + `).`

// sectionHeader opens the kick section; the source is recorded in it so the
// dashboard can report which one applied without a second channel.
const sectionHeader = "HOUSE STYLE (source: "

// Input is what Resolve needs. Every field is optional.
type Input struct {
	// RepoRoot is the managed repository's checkout root; "" means no repo
	// file is looked for.
	RepoRoot string
	// HiveInline is project.style; it wins over HiveFile when both are set.
	HiveInline string
	// HiveFile is project.style_file, a path on the hive host.
	HiveFile string
	// MaxBytes caps each candidate; zero means MaxBytes.
	MaxBytes int
	// Scan is run on repo-supplied text only. A true result refuses the file
	// and the reason is recorded on the Resolution.
	Scan func(text string) (blocked bool, reason string)
}

// Resolution is the chosen text plus how it was chosen.
type Resolution struct {
	Text   string
	Source Source
	// Path is the file the text came from, when it came from one.
	Path string
	// Skipped records sources that were present but not used, in precedence
	// order, each with the reason.
	Skipped []Skip
}

// Skip is one candidate that lost: too large, blocked by the scanner, or
// unreadable.
type Skip struct {
	Source Source
	Path   string
	Reason string
}

// ErrOversize is the Skip reason prefix for a candidate above the cap.
var ErrOversize = errors.New("exceeds size cap")

// ErrBlocked is the Skip reason prefix for a repo file the scanner refused.
var ErrBlocked = errors.New("blocked by ioscan")

// Cap returns the effective size cap for a configured value.
func Cap(configured int) int {
	switch {
	case configured <= 0:
		return MaxBytes
	case configured < MinMaxBytes:
		return MinMaxBytes
	}
	return configured
}

// Resolve picks the house-style text by precedence. It never fails: the
// built-in text is the floor.
func Resolve(in Input) Resolution {
	capBytes := Cap(in.MaxBytes)
	var res Resolution

	if in.RepoRoot != "" {
		path := filepath.Join(in.RepoRoot, FileName)
		if text, ok := res.readCandidate(SourceRepo, path, capBytes); ok {
			if in.Scan != nil {
				if blocked, reason := in.Scan(text); blocked {
					res.Skipped = append(res.Skipped, Skip{SourceRepo, path, ErrBlocked.Error() + ": " + reason})
					text = ""
				}
			}
			if text != "" {
				res.Text, res.Source, res.Path = text, SourceRepo, path
				return res
			}
		}
	}

	if text := strings.TrimSpace(in.HiveInline); text != "" {
		if len(text) > capBytes {
			res.Skipped = append(res.Skipped, Skip{SourceHive, "", ErrOversize.Error()})
		} else {
			res.Text, res.Source = text, SourceHive
			return res
		}
	} else if in.HiveFile != "" {
		if text, ok := res.readCandidate(SourceHive, in.HiveFile, capBytes); ok {
			res.Text, res.Source, res.Path = text, SourceHive, in.HiveFile
			return res
		}
	}

	res.Text, res.Source = Builtin, SourceBuiltin
	return res
}

// readCandidate reads one file, recording a Skip when it exists but cannot be
// used. A missing file is silent: that is the normal state for most repos.
func (r *Resolution) readCandidate(src Source, path string, capBytes int) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			r.Skipped = append(r.Skipped, Skip{src, path, err.Error()})
		}
		return "", false
	}
	if len(raw) > capBytes {
		r.Skipped = append(r.Skipped, Skip{src, path, ErrOversize.Error()})
		return "", false
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "", false
	}
	return text, true
}

// Section renders the kick section for a resolution.
func (r Resolution) Section() string {
	if r.Text == "" {
		return ""
	}
	return sectionHeader + string(r.Source) + ")\n" + r.Text
}

// SourceFromKick reads the source back out of a kick's text, or "" when the
// kick carries no house-style section (pre-upgrade kicks, empty kicks).
func SourceFromKick(kick string) Source {
	i := strings.Index(kick, sectionHeader)
	if i < 0 {
		return ""
	}
	rest := kick[i+len(sectionHeader):]
	end := strings.IndexByte(rest, ')')
	if end < 0 {
		return ""
	}
	switch src := Source(rest[:end]); src {
	case SourceRepo, SourceHive, SourceBuiltin:
		return src
	}
	return ""
}
