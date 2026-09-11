package github

import (
	"context"
	"regexp"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Rejected-finding re-file gate (#6463).
//
// A scanner running in issues-only mode files an issue per finding, and the
// policy correctly forbids it from listing issues — so the moment a maintainer
// closes one of those issues as not-planned, the rejection leaves the agent's
// field of view entirely. On the next kick the same finding is rediscovered,
// reworded, and filed again. Observed live on a downstream repo: one false
// positive filed five times in four days, each closed with a rebuttal, each
// re-filed within ~24h.
//
// The existing exact-title dedupe in CreateIssue cannot catch this: every one
// of those five filings was worded differently, and findingEvidenceHash
// (pkg/advisory) is deliberately verbatim-only. What DID stay stable across
// all five was the set of files the finding pointed at — the line numbers
// drifted with unrelated commits, the prose was rephrased every time, but the
// file paths were the finding's actual subject and the producer could not
// rephrase those without changing what it was reporting.
//
// So the gate keys on exactly that: before creating an issue, CreateIssue
// looks for a recently closed issue that
//
//   - was filed by this hive's own App bot (an agent filing, by construction —
//     every agent create flows through this same chokepoint), and
//   - was closed as "not_planned" or "duplicate" — a maintainer's explicit
//     rejection, never "completed" (a completed close means fixed, and a
//     re-report after a fix may be a genuine regression), and
//   - references exactly the same set of files as the pending request.
//
// When one exists, the request is refused and the response names the closed
// issue, so the agent (and its transcript) sees the maintainer's rebuttal
// instead of silently re-litigating it. GitHub's own closed issue IS the
// rejection tombstone — there is deliberately no new persistent store, the
// same posture advisory_suppress.go took for digest suppression: the baseline
// is re-read from the forge, so it survives restarts and is visible to the
// humans it protects.
//
// The gate fails toward FILING, in every direction it can:
//
//   - lookup error → file (same posture as the open-title dedupe lookup, and
//     as advisory's ResolveRef: suppression only ever on positive evidence);
//   - either file set empty → file (nothing to key on);
//   - file sets differ at all → file (exact equality, not overlap — a genuine
//     new defect in an already-argued-about file names a different set the
//     moment it involves any other file);
//   - no App-bot identity on this client → file (a token-authenticated hive
//     cannot distinguish its own filings from a human's, and suppressing
//     against a human-authored issue is not this gate's mandate);
//   - rejection older than issueRejectionWindow → file (a maintainer's "no"
//     from months ago should not silence a world that may have changed).
//
// The first cut of that keying was too literal and the same false positive got
// through a sixth time (#6674): "the set of files the finding pointed at" was
// approximated by every dotted token in the text, so the verification command
// the run happened to use ("ast.parse" in five filings, "py_compile.compile"
// in the sixth), a version string ("3.x"), and a bare basename mentioned in
// one extra sentence all counted as files — and under exact equality any one
// of them is enough to miss. looksLikeFileRef and foldBareBasenames below
// narrow the set back down to what the producer cannot reword: the paths.

// issueRejectionWindow bounds how long a not-planned closure suppresses
// re-filing. Mirrors advisory's refClosedWindow reasoning: the observed
// re-file loop cycles in ~24h, so 30 days is deep coverage for the failure
// mode, while a rejection from a different era eventually stops gating —
// the codebase, and the maintainer's reasoning, may both have moved on.
const issueRejectionWindow = 30 * 24 * time.Hour

// issueRejectionMaxPages bounds the closed-issue scan, same budget as
// findOpenIssueByTitle: agent-filed issues are recent by construction, and
// the scan additionally early-exits at the window boundary (sorted by
// updated desc, and closed_at ≤ updated_at, so past-window updated means
// past-window closed for everything after it).
const issueRejectionMaxPages = 3

// rejectedStateReasons are the GitHub state_reason values that count as a
// maintainer rejecting a finding. "completed" is deliberately absent — see
// the file comment.
var rejectedStateReasons = map[string]bool{
	"not_planned": true,
	"duplicate":   true,
}

// fileRefTokenPattern matches a file-path-like token: path characters ending
// in a dot-extension that starts with a letter, optionally followed by one or
// more :NN line/column suffixes ("client.py", "pkg/advisory/evidence.go:53",
// "a.go:1:2"). The letter-first extension keeps numeric-tailed version
// strings ("v4.23.3") out; it does NOT keep "3.x" or a dotted attribute
// chain out, which is what looksLikeFileRef below is for (#6674).
var fileRefTokenPattern = regexp.MustCompile(`[A-Za-z0-9_][A-Za-z0-9_./-]*\.[A-Za-z][A-Za-z0-9_]*(?::[0-9]+)*`)

// urlPattern strips URLs before token extraction so "peps.python.org/pep-0758"
// inside a link never becomes a "file". Bare schemeless domains are handled by
// domainExtensions below.
var urlPattern = regexp.MustCompile(`https?://[^\s)\]>"']+`)

// lineSuffixPattern strips trailing :NN(:NN...) line/column references so
// "client.py:290" and "client.py:321" — the same site renumbered by unrelated
// commits, exactly the drift observed live — normalize to the same key.
var lineSuffixPattern = regexp.MustCompile(`(:[0-9]+)+$`)

// domainExtensions are dot-suffixes that make a token a hostname rather than a
// file, checked before every other rule so a schemeless "peps.python.org" or
// "example.com/pkg/thing.go" is never mistaken for a path.
var domainExtensions = map[string]bool{
	"com": true, "org": true, "net": true, "io": true,
	"dev": true, "edu": true, "gov": true,
}

// sourceExtensions are the dot-suffixes that make a SLASHLESS token a file
// (#6674). A token containing "/" is a path on its face and needs no
// allowlist; a bare "client.py" does, because "ast.parse" and "3.x" are
// shaped identically and are the tokens that defeated the gate in practice.
//
// A miss here drops a real basename — which is safe in the direction that
// matters: the fuller path almost always appears alongside it in the same
// finding, and if it does not, the set shrinks on BOTH sides equally or the
// gate simply fails toward filing, which is this file's standing posture.
var sourceExtensions = map[string]bool{
	"go": true, "py": true, "pyi": true, "js": true, "mjs": true, "cjs": true,
	"ts": true, "tsx": true, "jsx": true, "rs": true, "java": true, "kt": true,
	"rb": true, "php": true, "c": true, "h": true, "cc": true, "cpp": true,
	"hpp": true, "cs": true, "swift": true, "sh": true, "bash": true,
	"zsh": true, "ps1": true, "bat": true,
	"yml": true, "yaml": true, "json": true, "toml": true, "ini": true,
	"cfg": true, "conf": true, "properties": true, "env": true,
	"md": true, "rst": true, "txt": true, "adoc": true,
	"html": true, "htm": true, "css": true, "scss": true, "less": true,
	"sql": true, "proto": true, "tf": true, "tfvars": true, "gradle": true,
	"mk": true, "cmake": true, "lock": true, "sum": true, "mod": true,
	"service": true, "container": true, "dockerfile": true,
	"containerfile": true, "gitignore": true, "gitattributes": true,
	"editorconfig": true, "csv": true, "tsv": true, "xml": true, "svg": true,
}

// looksLikeFileRef reports whether an extracted token actually names a file.
// Two ways to qualify, neither of which a rewording producer controls: the
// token contains a path separator, or its extension is a known source
// extension. Everything else — dotted attribute chains ("ast.parse",
// "py_compile.compile"), dotted module paths ("custom_components.sensi"),
// version strings ("3.x") — is prose, and prose is exactly what changes
// between two filings of the same finding.
func looksLikeFileRef(tok string) bool {
	dot := strings.LastIndex(tok, ".")
	if dot < 0 {
		return false
	}
	if domainExtensions[strings.ToLower(tok[dot+1:])] {
		return false
	}
	if slash := strings.Index(tok, "/"); slash >= 0 {
		// "example.com/pkg/thing.go": the host, not a path in the repo.
		if hostDot := strings.LastIndex(tok[:slash], "."); hostDot >= 0 &&
			domainExtensions[strings.ToLower(tok[hostDot+1:slash])] {
			return false
		}
		return true
	}
	return sourceExtensions[strings.ToLower(tok[dot+1:])]
}

// foldBareBasenames drops a slashless token when a fuller path for the same
// file is already in the set, so "client.py" mentioned in a sentence and
// "custom_components/sensi/client.py" quoted in the evidence block count once
// (#6674). Whether a finding's prose happens to name the basename in passing
// is the producer's wording, not its subject; without this fold, one such
// sentence changes the set and defeats exact equality.
func foldBareBasenames(set map[string]bool) map[string]bool {
	paths := make([]string, 0, len(set))
	for tok := range set {
		if strings.Contains(tok, "/") {
			paths = append(paths, tok)
		}
	}
	out := make(map[string]bool, len(set))
	for tok := range set {
		if strings.Contains(tok, "/") {
			out[tok] = true
			continue
		}
		covered := false
		for _, p := range paths {
			if strings.HasSuffix(p, "/"+tok) {
				covered = true
				break
			}
		}
		if !covered {
			out[tok] = true
		}
	}
	return out
}

// issueFileRefSet extracts the normalized set of file paths a finding's text
// references.
func issueFileRefSet(text string) map[string]bool {
	text = urlPattern.ReplaceAllString(text, " ")
	out := make(map[string]bool)
	for _, tok := range fileRefTokenPattern.FindAllString(text, -1) {
		tok = lineSuffixPattern.ReplaceAllString(tok, "")
		if !looksLikeFileRef(tok) {
			continue
		}
		out[tok] = true
	}
	return foldBareBasenames(out)
}

// equalFileRefSets reports whether two non-empty file-reference sets are
// exactly equal. Empty on either side never matches: no evidence, no gate.
func equalFileRefSets(a, b map[string]bool) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// findRejectedTwin returns a recently closed, maintainer-rejected, App-bot-
// filed issue whose file-reference set exactly matches the pending request's,
// or nil. An error means the lookup could not tell; the caller files anyway.
func (c *Client) findRejectedTwin(ctx context.Context, owner, repo, title, body string) (*gh.Issue, error) {
	botLogin := strings.TrimSpace(c.appBotLogin)
	if botLogin == "" {
		return nil, nil
	}
	want := issueFileRefSet(title + "\n" + body)
	if len(want) == 0 {
		return nil, nil
	}
	cutoff := time.Now().Add(-issueRejectionWindow)
	opts := &gh.IssueListByRepoOptions{
		State:       "closed",
		Creator:     botLogin,
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	for page := 1; page <= issueRejectionMaxPages; page++ {
		opts.ListOptions.Page = page
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, err
		}
		for _, is := range issues {
			if is.GetUpdatedAt().Time.Before(cutoff) {
				// Sorted by updated desc: everything after this is older, and
				// closed_at ≤ updated_at, so nothing further can qualify.
				return nil, nil
			}
			if is.IsPullRequest() {
				continue
			}
			if !rejectedStateReasons[is.GetStateReason()] {
				continue
			}
			if is.GetClosedAt().Time.Before(cutoff) {
				continue
			}
			if equalFileRefSets(want, issueFileRefSet(is.GetTitle()+"\n"+is.GetBody())) {
				return is, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
	}
	return nil, nil
}
